package codechdr

import "github.com/gomcpgo/govideo/internal/bits"

const (
	hevcNALVPS = 32
	hevcNALSPS = 33
)

func hevcProfileName(idc int, compat uint32) string {
	if idc == 0 {
		// general_profile_idc 0: fall back on the compatibility flags.
		for i := 1; i <= 4; i++ {
			if compat&(1<<uint(31-i)) != 0 {
				idc = i
				break
			}
		}
	}
	switch idc {
	case 1:
		return "Main"
	case 2:
		return "Main 10"
	case 3:
		return "Main Still Picture"
	case 4:
		return "Rext"
	case 5:
		return "High Throughput"
	case 6:
		return "Multiview Main"
	case 7:
		return "Scalable Main"
	case 9:
		return "SCC"
	}
	return ""
}

// ParseHVCC parses an hvcC box payload (HEVCDecoderConfigurationRecord).
// It describes the base-layer SPS when the record carries one, uses the VPS
// for timing when the SPS has none, and otherwise falls back on the
// record's own profile, level, chroma and bit depth fields.
func ParseHVCC(b []byte) (*Video, error) {
	if len(b) < 23 {
		return nil, ErrTruncated
	}
	rec := newVideo("hevc")
	compat := uint32(b[2])<<24 | uint32(b[3])<<16 | uint32(b[4])<<8 | uint32(b[5])
	rec.Profile = hevcProfileName(int(b[1]&0x1f), compat)
	rec.Level = int(b[12])
	rec.Chroma = chromaName(int(b[16] & 3))
	rec.BitDepth = int(b[17]&7) + 8
	// general_progressive_source_flag / general_interlaced_source_flag
	rec.Interlaced = b[6]&0x80 == 0 && b[6]&0x40 != 0

	var sps, vps *Video
	p := 23
	arrays := int(b[22])
	for a := 0; a < arrays; a++ {
		if p+3 > len(b) {
			return nil, ErrTruncated
		}
		typ := int(b[p] & 0x3f)
		count := int(b[p+1])<<8 | int(b[p+2])
		p += 3
		for i := 0; i < count; i++ {
			if p+2 > len(b) {
				return nil, ErrTruncated
			}
			l := int(b[p])<<8 | int(b[p+1])
			p += 2
			if p+l > len(b) {
				return nil, ErrTruncated
			}
			nal := b[p : p+l]
			p += l
			if l < 2 || hevcLayerID(nal) != 0 {
				continue
			}
			switch {
			case typ == hevcNALSPS && sps == nil:
				v, err := ParseHEVCSPS(nal)
				if err != nil {
					return nil, err
				}
				sps = v
			case typ == hevcNALVPS && vps == nil:
				// A broken VPS only costs the timing fallback.
				vps, _ = parseHEVCVPS(nal)
			}
		}
	}
	if sps == nil {
		return rec, nil
	}
	if !sps.HasTiming && vps != nil && vps.HasTiming {
		sps.HasTiming, sps.TimeNum, sps.TimeDen = true, vps.TimeNum, vps.TimeDen
	}
	return sps, nil
}

func hevcLayerID(nal []byte) int { return int(nal[0]&1)<<5 | int(nal[1]>>3) }

type hevcPTL struct {
	profile     int
	compat      uint32
	level       int
	progressive bool
	interlaced  bool
}

// hevcProfileTierLevel parses profile_tier_level(1, maxSubLayersMinus1).
func hevcProfileTierLevel(r *bits.Reader, maxSub int) hevcPTL {
	var p hevcPTL
	r.Skip(3) // general_profile_space, general_tier_flag
	p.profile = int(r.U(5))
	p.compat = r.U(32)
	p.progressive = r.Flag()
	p.interlaced = r.Flag()
	r.Skip(46) // non_packed, frame_only, 43 reserved/constraint bits, inbld/reserved
	p.level = int(r.U(8))
	var profPresent, levelPresent [8]bool
	for i := 0; i < maxSub; i++ {
		profPresent[i] = r.Flag()
		levelPresent[i] = r.Flag()
	}
	if maxSub > 0 {
		r.Skip(2 * (8 - maxSub))
	}
	for i := 0; i < maxSub; i++ {
		if profPresent[i] {
			r.Skip(88)
		}
		if levelPresent[i] {
			r.Skip(8)
		}
	}
	return p
}

func hevcNALPayload(nal []byte, want int) (*bits.Reader, error) {
	if len(nal) < 3 {
		return nil, ErrTruncated
	}
	if nal[0]&0x80 != 0 || int(nal[0]>>1&0x3f) != want {
		return nil, invalid("unexpected HEVC NAL unit type %d", nal[0]>>1&0x3f)
	}
	return bits.NewReader(bits.RemoveEmulationPrevention(nal[2:])), nil
}

// parseHEVCVPS extracts the timing information of a video parameter set.
func parseHEVCVPS(nal []byte) (*Video, error) {
	r, err := hevcNALPayload(nal, hevcNALVPS)
	if err != nil {
		return nil, err
	}
	v := newVideo("hevc")
	r.Skip(12) // vps id, base layer flags, max_layers_minus1
	maxSub := int(r.U(3))
	r.Skip(17) // temporal_id_nesting, reserved 0xffff
	if maxSub > 6 {
		return nil, invalid("vps_max_sub_layers_minus1 %d", maxSub)
	}
	hevcProfileTierLevel(r, maxSub)
	i := maxSub
	if r.Flag() {
		i = 0
	}
	for ; i <= maxSub; i++ {
		r.UE()
		r.UE()
		r.UE()
	}
	maxLayerID := int(r.U(6))
	sets := r.UE()
	if sets > 1023 {
		return nil, invalid("vps_num_layer_sets_minus1 %d", sets)
	}
	r.Skip(int(sets) * (maxLayerID + 1))
	if r.Flag() { // vps_timing_info_present_flag
		units := r.U(32)
		scale := r.U(32)
		if units != 0 && scale != 0 && r.Err() == nil {
			v.HasTiming, v.TimeNum, v.TimeDen = true, scale, units
		}
	}
	if r.Err() != nil {
		return nil, ErrTruncated
	}
	return v, nil
}

// ParseHEVCSPS parses an H.265 sequence parameter set NAL unit, including
// its two-byte NAL header.
func ParseHEVCSPS(nal []byte) (*Video, error) {
	r, err := hevcNALPayload(nal, hevcNALSPS)
	if err != nil {
		return nil, err
	}
	if hevcLayerID(nal) != 0 {
		return nil, invalid("SPS of an enhancement layer")
	}
	v := newVideo("hevc")
	r.Skip(4) // sps_video_parameter_set_id
	maxSub := int(r.U(3))
	r.Skip(1) // sps_temporal_id_nesting_flag
	if maxSub > 6 {
		return nil, invalid("sps_max_sub_layers_minus1 %d", maxSub)
	}
	ptl := hevcProfileTierLevel(r, maxSub)
	v.Profile = hevcProfileName(ptl.profile, ptl.compat)
	v.Level = ptl.level
	v.Interlaced = ptl.interlaced && !ptl.progressive

	r.UE() // sps_seq_parameter_set_id
	chroma := int(r.UE())
	if chroma > 3 {
		return nil, invalid("chroma_format_idc %d", chroma)
	}
	separatePlanes := false
	if chroma == 3 {
		separatePlanes = r.Flag()
	}
	v.Chroma = chromaName(chroma)
	w, h := r.UE(), r.UE()
	if w == 0 || h == 0 || w > 1<<16 || h > 1<<16 {
		if r.Err() != nil {
			return nil, ErrTruncated
		}
		return nil, invalid("picture size %dx%d", w, h)
	}
	v.CodedWidth, v.CodedHeight = int(w), int(h)
	v.Width, v.Height = v.CodedWidth, v.CodedHeight
	if r.Flag() { // conformance_window_flag
		l, rt, t, bt := r.UE(), r.UE(), r.UE(), r.UE()
		ux, uy := 1, 1
		if !separatePlanes {
			switch chroma {
			case 1:
				ux, uy = 2, 2
			case 2:
				ux = 2
			}
		}
		cw := (uint64(l) + uint64(rt)) * uint64(ux)
		ch := (uint64(t) + uint64(bt)) * uint64(uy)
		if cw >= uint64(w) || ch >= uint64(h) {
			return nil, invalid("conformance window exceeds picture")
		}
		v.Width -= int(cw)
		v.Height -= int(ch)
	}
	bd, bdc := r.UE(), r.UE()
	if bd > 8 || bdc > 8 {
		return nil, invalid("bit depth")
	}
	v.BitDepth = int(bd) + 8
	log2MaxPOC := r.UE() + 4
	if log2MaxPOC > 16 {
		return nil, invalid("log2_max_pic_order_cnt_lsb")
	}
	i := maxSub
	if r.Flag() { // sps_sub_layer_ordering_info_present_flag
		i = 0
	}
	for ; i <= maxSub; i++ {
		r.UE()
		reorder := r.UE()
		r.UE()
		if r.Err() == nil && reorder <= 16 {
			v.HasBFrames = int(reorder)
		}
	}
	for j := 0; j < 6; j++ { // coding block / transform block sizes and depths
		r.UE()
	}
	if r.Flag() && r.Flag() { // scaling_list_enabled_flag, sps_scaling_list_data_present_flag
		hevcScalingListData(r)
	}
	r.Skip(2)     // amp_enabled_flag, sample_adaptive_offset_enabled_flag
	if r.Flag() { // pcm_enabled_flag
		r.Skip(8)
		r.UE()
		r.UE()
		r.Skip(1)
	}
	if err := hevcShortTermRefPicSets(r); err != nil {
		return nil, err
	}
	if r.Flag() { // long_term_ref_pics_present_flag
		n := r.UE()
		if n > 32 {
			return nil, invalid("num_long_term_ref_pics_sps %d", n)
		}
		r.Skip(int(n) * (int(log2MaxPOC) + 1))
	}
	r.Skip(2)     // sps_temporal_mvp_enabled_flag, strong_intra_smoothing_enabled_flag
	if r.Flag() { // vui_parameters_present_flag
		vuiCommon(r, v)
		r.Skip(1)     // neutral_chroma_indication_flag
		if r.Flag() { // field_seq_flag
			v.Interlaced = true
		}
		r.Skip(1)     // frame_field_info_present_flag
		if r.Flag() { // default_display_window_flag
			r.UE()
			r.UE()
			r.UE()
			r.UE()
		}
		if r.Flag() { // vui_timing_info_present_flag
			units := r.U(32)
			scale := r.U(32)
			if units != 0 && scale != 0 && r.Err() == nil {
				v.HasTiming, v.TimeNum, v.TimeDen = true, scale, units
			}
		}
		// The rest (HRD, bitstream restriction) carries nothing we report.
	}
	if r.Err() != nil {
		return nil, ErrTruncated
	}
	return v, nil
}

func hevcScalingListData(r *bits.Reader) {
	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		for m := 0; m < 6; m += step {
			if !r.Flag() { // scaling_list_pred_mode_flag
				r.UE()
				continue
			}
			n := 1 << uint(4+sizeID<<1)
			if n > 64 {
				n = 64
			}
			if sizeID > 1 {
				r.SE()
			}
			for i := 0; i < n; i++ {
				r.SE()
			}
		}
	}
}

// hevcShortTermRefPicSets skips the st_ref_pic_set() structures of an SPS.
// Inter-predicted sets need the picture counts of the previous set, so the
// delta POC lists are tracked (H.265 §7.4.8).
func hevcShortTermRefPicSets(r *bits.Reader) error {
	num := r.UE()
	if num > 64 {
		return invalid("num_short_term_ref_pic_sets %d", num)
	}
	var prevS0, prevS1 []int32
	for idx := uint32(0); idx < num; idx++ {
		var s0, s1 []int32
		if idx != 0 && r.Flag() { // inter_ref_pic_set_prediction_flag
			sign := r.Flag()
			abs := r.UE()
			if abs >= 1<<15 {
				return invalid("abs_delta_rps_minus1")
			}
			deltaRps := int32(abs) + 1
			if sign {
				deltaRps = -deltaRps
			}
			nNeg, nPos := len(prevS0), len(prevS1)
			use := make([]bool, nNeg+nPos+1)
			for j := range use {
				use[j] = true
				if !r.Flag() { // used_by_curr_pic_flag
					use[j] = r.Flag() // use_delta_flag
				}
			}
			for j := nPos - 1; j >= 0; j-- {
				if d := prevS1[j] + deltaRps; d < 0 && use[nNeg+j] {
					s0 = append(s0, d)
				}
			}
			if deltaRps < 0 && use[nNeg+nPos] {
				s0 = append(s0, deltaRps)
			}
			for j := 0; j < nNeg; j++ {
				if d := prevS0[j] + deltaRps; d < 0 && use[j] {
					s0 = append(s0, d)
				}
			}
			for j := nNeg - 1; j >= 0; j-- {
				if d := prevS0[j] + deltaRps; d > 0 && use[j] {
					s1 = append(s1, d)
				}
			}
			if deltaRps > 0 && use[nNeg+nPos] {
				s1 = append(s1, deltaRps)
			}
			for j := 0; j < nPos; j++ {
				if d := prevS1[j] + deltaRps; d > 0 && use[nNeg+j] {
					s1 = append(s1, d)
				}
			}
			if len(s0)+len(s1) > 32 {
				return invalid("st_ref_pic_set too large")
			}
		} else {
			nNeg, nPos := r.UE(), r.UE()
			if nNeg > 16 || nPos > 16 {
				return invalid("st_ref_pic_set picture count")
			}
			var poc int32
			for i := uint32(0); i < nNeg; i++ {
				poc -= int32(r.UE()&0x7fff) + 1
				r.Skip(1)
				s0 = append(s0, poc)
			}
			poc = 0
			for i := uint32(0); i < nPos; i++ {
				poc += int32(r.UE()&0x7fff) + 1
				r.Skip(1)
				s1 = append(s1, poc)
			}
		}
		if r.Err() != nil {
			return ErrTruncated
		}
		prevS0, prevS1 = s0, s1
	}
	return nil
}
