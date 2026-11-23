package codechdr

import "github.com/gomcpgo/govideo/internal/bits"

// ParseAVCC parses an avcC box payload (AVCDecoderConfigurationRecord) and
// returns the description of its first sequence parameter set.
func ParseAVCC(b []byte) (*Video, error) {
	if len(b) < 6 {
		return nil, ErrTruncated
	}
	if b[0] != 1 {
		return nil, invalid("avcC version %d", b[0])
	}
	n := int(b[5] & 0x1f)
	p := 6
	if n == 0 {
		return nil, invalid("avcC has no SPS")
	}
	if p+2 > len(b) {
		return nil, ErrTruncated
	}
	l := int(b[p])<<8 | int(b[p+1])
	p += 2
	if l == 0 || p+l > len(b) {
		return nil, ErrTruncated
	}
	return ParseH264SPS(b[p : p+l])
}

func h264ProfileName(profile int, constraints uint32) string {
	cs1 := constraints&0x40 != 0
	cs3 := constraints&0x10 != 0
	switch profile {
	case 66:
		if cs1 {
			return "Constrained Baseline"
		}
		return "Baseline"
	case 77:
		return "Main"
	case 88:
		return "Extended"
	case 100:
		return "High"
	case 110:
		if cs3 {
			return "High 10 Intra"
		}
		return "High 10"
	case 122:
		if cs3 {
			return "High 4:2:2 Intra"
		}
		return "High 4:2:2"
	case 244:
		if cs3 {
			return "High 4:4:4 Intra"
		}
		return "High 4:4:4 Predictive"
	case 44:
		return "CAVLC 4:4:4"
	case 118:
		return "Multiview High"
	case 128:
		return "Stereo High"
	}
	return ""
}

// ParseH264SPS parses an H.264 sequence parameter set NAL unit, including
// its one-byte NAL header.
func ParseH264SPS(nal []byte) (*Video, error) {
	v, _, err := parseH264SPS(nal)
	return v, err
}

// h264SPS holds the sequence parameter set fields that the syntax of slice
// headers and picture timing SEI messages depends on (H.264 §7.3.3, §D.1.3).
type h264SPS struct {
	id                 uint32
	separatePlanes     bool
	frameNumBits       uint32 // log2_max_frame_num
	pocType            uint32
	pocLsbBits         uint32 // log2_max_pic_order_cnt_lsb
	deltaPOCAlwaysZero bool
	topToBottom        int32 // offset_for_top_to_bottom_field
	frameMbsOnly       bool
	mbaff              bool
	picStructPresent   bool
	// cpbDpbDelays reports that picture timing SEI messages start with
	// cpb_removal_delay and dpb_output_delay, of the widths given.
	cpbDpbDelays               bool
	cpbDelayBits, dpbDelayBits int
}

func parseH264SPS(nal []byte) (*Video, *h264SPS, error) {
	if len(nal) < 4 {
		return nil, nil, ErrTruncated
	}
	if nal[0]&0x80 != 0 || nal[0]&0x1f != 7 {
		return nil, nil, invalid("not an H.264 SPS NAL unit")
	}
	r := bits.NewReader(bits.RemoveEmulationPrevention(nal[1:]))
	v := newVideo("h264")
	sps := &h264SPS{}
	profile := int(r.U(8))
	constraints := r.U(8)
	v.Level = int(r.U(8))
	v.Profile = h264ProfileName(profile, constraints)
	sps.id = r.UE()

	chroma := 1
	separatePlanes := false
	v.BitDepth = 8
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = int(r.UE())
		if chroma > 3 {
			return nil, nil, invalid("chroma_format_idc %d", chroma)
		}
		if chroma == 3 {
			separatePlanes = r.Flag()
		}
		bd := r.UE()
		bdc := r.UE()
		if bd > 6 || bdc > 6 {
			return nil, nil, invalid("bit depth")
		}
		v.BitDepth = int(bd) + 8
		r.Skip(1) // qpprime_y_zero_transform_bypass_flag
		if r.Flag() {
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if !r.Flag() {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for j := 0; j < size; j++ {
					if next != 0 {
						next = (last + r.SE() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	v.Chroma = chromaName(chroma)

	sps.separatePlanes = separatePlanes
	sps.frameNumBits = r.UE() + 4
	sps.pocType = r.UE()
	switch sps.pocType {
	case 0:
		sps.pocLsbBits = r.UE() + 4
	case 1:
		sps.deltaPOCAlwaysZero = r.Flag()
		r.SE()
		sps.topToBottom = r.SE()
		n := r.UE()
		if n > 255 {
			return nil, nil, invalid("num_ref_frames_in_pic_order_cnt_cycle %d", n)
		}
		for i := uint32(0); i < n; i++ {
			r.SE()
		}
	}
	v.RefFrames = int(r.UE())
	r.Skip(1) // gaps_in_frame_num_value_allowed_flag
	wMbs := r.UE()
	hUnits := r.UE()
	if wMbs >= 4096 || hUnits >= 4096 {
		return nil, nil, invalid("picture size")
	}
	frameMbsOnly := r.Flag()
	sps.frameMbsOnly = frameMbsOnly
	fieldMul := 1
	if !frameMbsOnly {
		fieldMul = 2
		sps.mbaff = r.Flag()
		v.Interlaced = true
	}
	r.Skip(1) // direct_8x8_inference_flag
	v.CodedWidth = int(wMbs+1) * 16
	v.CodedHeight = int(hUnits+1) * 16 * fieldMul
	v.Width, v.Height = v.CodedWidth, v.CodedHeight
	if r.Flag() { // frame_cropping_flag
		l, rt, t, bt := r.UE(), r.UE(), r.UE(), r.UE()
		ux, uy := 1, fieldMul
		if !separatePlanes {
			switch chroma {
			case 1:
				ux, uy = 2, 2*fieldMul
			case 2:
				ux = 2
			}
		}
		cw := (uint64(l) + uint64(rt)) * uint64(ux)
		ch := (uint64(t) + uint64(bt)) * uint64(uy)
		if cw >= uint64(v.CodedWidth) || ch >= uint64(v.CodedHeight) {
			return nil, nil, invalid("cropping exceeds picture")
		}
		v.Width -= int(cw)
		v.Height -= int(ch)
	}
	if r.Flag() { // vui_parameters_present_flag
		h264VUI(r, v, sps)
	}
	if err := r.Err(); err != nil {
		return nil, nil, ErrTruncated
	}
	return v, sps, nil
}

// vuiCommon parses the part of the VUI that H.264 and H.265 share:
// aspect ratio, overscan, video signal type and chroma location.
func vuiCommon(r *bits.Reader, v *Video) {
	if r.Flag() { // aspect_ratio_info_present_flag
		idc := int(r.U(8))
		if idc == 255 {
			v.SARNum = int(r.U(16))
			v.SARDen = int(r.U(16))
			if v.SARNum == 0 || v.SARDen == 0 {
				v.SARNum, v.SARDen = 0, 0
			}
		} else if idc < len(sarTable) {
			v.SARNum, v.SARDen = sarTable[idc][0], sarTable[idc][1]
		}
	}
	if r.Flag() { // overscan_info_present_flag
		r.Skip(1)
	}
	if r.Flag() { // video_signal_type_present_flag
		r.Skip(3) // video_format
		v.FullRange = r.Flag()
		v.ColorRangeKnown = true
		if r.Flag() { // colour_description_present_flag
			v.ColorPrimaries = int(r.U(8))
			v.ColorTransfer = int(r.U(8))
			v.ColorMatrix = int(r.U(8))
		}
	}
	if r.Flag() { // chroma_loc_info_present_flag
		r.UE()
		r.UE()
	}
}

// h264HRD reads hrd_parameters (H.264 §E.1.2), keeping the widths of the two
// delays that picture timing SEI messages carry.
func h264HRD(r *bits.Reader, sps *h264SPS) {
	n := r.UE()
	if n > 31 {
		r.Skip(1 << 30) // force an error
		return
	}
	r.Skip(8)
	for i := uint32(0); i <= n; i++ {
		r.UE()
		r.UE()
		r.Skip(1)
	}
	r.Skip(5) // initial_cpb_removal_delay_length_minus1
	sps.cpbDpbDelays = true
	sps.cpbDelayBits = int(r.U(5)) + 1
	sps.dpbDelayBits = int(r.U(5)) + 1
	r.Skip(5) // time_offset_length
}

func h264VUI(r *bits.Reader, v *Video, sps *h264SPS) {
	vuiCommon(r, v)
	if r.Flag() { // timing_info_present_flag
		units := r.U(32)
		scale := r.U(32)
		r.Skip(1) // fixed_frame_rate_flag
		if units != 0 && scale != 0 && r.Err() == nil {
			v.HasTiming = true
			v.TimeNum = scale
			if units > 0x7fffffff {
				// Avoid overflow of 2*units; halve the numerator instead.
				v.TimeNum = scale / 2
				v.TimeDen = units
			} else {
				v.TimeDen = units * 2
			}
		}
	}
	nal := r.Flag()
	if nal {
		h264HRD(r, sps)
	}
	vcl := r.Flag()
	if vcl {
		h264HRD(r, sps)
	}
	if nal || vcl {
		r.Skip(1) // low_delay_hrd_flag
	}
	sps.picStructPresent = r.Flag()
	if r.Flag() { // bitstream_restriction_flag
		r.Skip(1)
		r.UE()
		r.UE()
		r.UE()
		r.UE()
		reorder := r.UE()
		r.UE()
		if r.Err() == nil && reorder <= 16 {
			v.HasBFrames = int(reorder)
		}
	}
}
