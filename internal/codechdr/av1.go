package codechdr

import "github.com/gomcpgo/govideo/internal/bits"

func av1ProfileName(p int) string {
	switch p {
	case 0:
		return "Main"
	case 1:
		return "High"
	case 2:
		return "Professional"
	}
	return ""
}

// ParseAV1C parses an av1C box payload (AV1CodecConfigurationRecord). The
// fixed fields give profile, level, bit depth and chroma format; when the
// record embeds a sequence header OBU, size, timing and colour come from
// it.
func ParseAV1C(b []byte) (*Video, error) {
	if len(b) < 4 {
		return nil, ErrTruncated
	}
	if b[0]&0x80 == 0 || b[0]&0x7f != 1 {
		return nil, invalid("av1C marker/version %#x", b[0])
	}
	v := newVideo("av1")
	profile := int(b[1] >> 5)
	v.Profile = av1ProfileName(profile)
	v.Level = int(b[1] & 0x1f)
	high, twelve, mono := b[2]&0x40 != 0, b[2]&0x20 != 0, b[2]&0x10 != 0
	ssx, ssy := b[2]&0x08 != 0, b[2]&0x04 != 0
	v.BitDepth = 8
	if high {
		v.BitDepth = 10
		if twelve && profile == 2 {
			v.BitDepth = 12
		}
	}
	v.Chroma = av1Chroma(mono, ssx, ssy)

	// configOBUs: look for a sequence header.
	p := b[4:]
	for len(p) > 0 {
		typ, obu, rest, err := av1SplitOBU(p)
		if err != nil {
			break // tolerate trailing garbage; the fixed fields stand
		}
		if typ == 1 {
			if sh, err := ParseAV1SequenceHeader(obu); err == nil {
				return sh, nil
			}
			break
		}
		p = rest
	}
	return v, nil
}

func av1Chroma(mono, ssx, ssy bool) string {
	switch {
	case mono:
		return "4:0:0"
	case ssx && ssy:
		return "4:2:0"
	case ssx:
		return "4:2:2"
	}
	return "4:4:4"
}

// av1SplitOBU returns the type of the first OBU in b, the OBU itself
// (header included) and the bytes following it.
func av1SplitOBU(b []byte) (typ int, obu, rest []byte, err error) {
	hdr, size, err := av1OBUHeader(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return int(b[0] >> 3 & 0xf), b[:hdr+size], b[hdr+size:], nil
}

// av1OBUHeader returns the header length and payload length of the OBU at
// the start of b.
func av1OBUHeader(b []byte) (hdr, size int, err error) {
	if len(b) < 1 {
		return 0, 0, ErrTruncated
	}
	if b[0]&0x80 != 0 {
		return 0, 0, invalid("OBU forbidden bit set")
	}
	hdr = 1
	if b[0]&4 != 0 { // obu_extension_flag
		hdr++
	}
	if b[0]&2 == 0 { // no obu_has_size_field: the OBU runs to the end
		if hdr > len(b) {
			return 0, 0, ErrTruncated
		}
		return hdr, len(b) - hdr, nil
	}
	var v uint64
	for i := 0; ; i++ {
		if i == 8 {
			return 0, 0, invalid("leb128 too long")
		}
		if hdr >= len(b) {
			return 0, 0, ErrTruncated
		}
		c := b[hdr]
		hdr++
		v |= uint64(c&0x7f) << uint(7*i)
		if c&0x80 == 0 {
			break
		}
	}
	if v > uint64(len(b)-hdr) {
		return 0, 0, ErrTruncated
	}
	return hdr, int(v), nil
}

func av1UVLC(r *bits.Reader) uint32 {
	lz := 0
	for r.Err() == nil && !r.Flag() {
		lz++
		if lz >= 32 {
			return 0xFFFFFFFF
		}
	}
	return r.U(lz) + (1<<uint(lz) - 1)
}

// ParseAV1SequenceHeader parses a sequence header OBU, including its OBU
// header.
func ParseAV1SequenceHeader(obu []byte) (*Video, error) {
	hdr, size, err := av1OBUHeader(obu)
	if err != nil {
		return nil, err
	}
	if obu[0]>>3&0xf != 1 {
		return nil, invalid("OBU type %d is not a sequence header", obu[0]>>3&0xf)
	}
	r := bits.NewReader(obu[hdr : hdr+size])
	v := newVideo("av1")
	profile := int(r.U(3))
	v.Profile = av1ProfileName(profile)
	r.Skip(1) // still_picture
	reduced := r.Flag()
	if reduced {
		v.Level = int(r.U(5))
	} else {
		decoderModel := false
		bufferDelayLen := 0
		if r.Flag() { // timing_info_present_flag
			units := r.U(32)
			scale := r.U(32)
			ticks := uint64(1)
			if r.Flag() { // equal_picture_interval
				ticks = uint64(av1UVLC(r)) + 1
			}
			if den := uint64(units) * ticks; units != 0 && scale != 0 && den <= 0xFFFFFFFF && r.Err() == nil {
				v.HasTiming, v.TimeNum, v.TimeDen = true, scale, uint32(den)
			}
			decoderModel = r.Flag()
			if decoderModel {
				bufferDelayLen = int(r.U(5)) + 1
				r.Skip(32 + 5 + 5)
			}
		}
		displayDelay := r.Flag()
		ops := int(r.U(5)) + 1
		for i := 0; i < ops; i++ {
			r.Skip(12) // operating_point_idc
			level := int(r.U(5))
			if level > 7 {
				r.Skip(1) // seq_tier
			}
			if i == 0 {
				v.Level = level
			}
			if decoderModel && r.Flag() {
				r.Skip(2*bufferDelayLen + 1)
			}
			if displayDelay && r.Flag() {
				r.Skip(4)
			}
		}
	}
	wBits := int(r.U(4)) + 1
	hBits := int(r.U(4)) + 1
	v.Width = int(r.U(wBits)) + 1
	v.Height = int(r.U(hBits)) + 1
	v.CodedWidth, v.CodedHeight = v.Width, v.Height
	if !reduced && r.Flag() { // frame_id_numbers_present_flag
		r.Skip(7)
	}
	r.Skip(3) // use_128x128_superblock, enable_filter_intra, enable_intra_edge_filter
	if !reduced {
		r.Skip(4) // interintra, masked compound, warped motion, dual filter
		orderHint := r.Flag()
		if orderHint {
			r.Skip(2)
		}
		forceSCT := uint32(2)
		if !r.Flag() { // seq_choose_screen_content_tools
			forceSCT = r.U(1)
		}
		if forceSCT > 0 {
			if !r.Flag() { // seq_choose_integer_mv
				r.Skip(1)
			}
		}
		if orderHint {
			r.Skip(3)
		}
	}
	r.Skip(3) // enable_superres, enable_cdef, enable_restoration

	// color_config()
	high := r.Flag()
	v.BitDepth = 8
	if profile == 2 && high {
		v.BitDepth = 10
		if r.Flag() {
			v.BitDepth = 12
		}
	} else if high {
		v.BitDepth = 10
	}
	mono := false
	if profile != 1 {
		mono = r.Flag()
	}
	if r.Flag() { // color_description_present_flag
		v.ColorPrimaries = int(r.U(8))
		v.ColorTransfer = int(r.U(8))
		v.ColorMatrix = int(r.U(8))
	}
	ssx, ssy := true, true
	switch {
	case mono:
		v.FullRange = r.Flag()
	case v.ColorPrimaries == 1 && v.ColorTransfer == 13 && v.ColorMatrix == 0:
		v.FullRange = true
		ssx, ssy = false, false
	default:
		v.FullRange = r.Flag()
		switch profile {
		case 0:
		case 1:
			ssx, ssy = false, false
		default:
			if v.BitDepth == 12 {
				ssx = r.Flag()
				ssy = ssx && r.Flag()
			} else {
				ssy = false
			}
		}
	}
	v.ColorRangeKnown = true
	v.Chroma = av1Chroma(mono, ssx, ssy)
	if r.Err() != nil {
		return nil, ErrTruncated
	}
	return v, nil
}
