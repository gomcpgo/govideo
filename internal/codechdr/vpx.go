package codechdr

import (
	"strconv"

	"github.com/gomcpgo/govideo/internal/bits"
)

// ParseVPCC parses a vpcC box payload (VPCodecConfigurationRecord),
// including the FullBox version and flags. The record carries no picture
// size.
func ParseVPCC(b []byte) (*Video, error) {
	if len(b) < 10 {
		return nil, ErrTruncated
	}
	if b[0] != 1 {
		return nil, invalid("vpcC version %d", b[0])
	}
	v := newVideo("vp9")
	if b[4] > 3 {
		return nil, invalid("VP9 profile %d", b[4])
	}
	v.Profile = "Profile " + strconv.Itoa(int(b[4]))
	if b[5] != 0 {
		v.Level = int(b[5])
	}
	v.BitDepth = int(b[6] >> 4)
	switch b[6] >> 1 & 7 {
	case 0, 1:
		v.Chroma = "4:2:0"
	case 2:
		v.Chroma = "4:2:2"
	case 3:
		v.Chroma = "4:4:4"
	}
	v.FullRange = b[6]&1 != 0
	v.ColorRangeKnown = true
	v.ColorPrimaries = int(b[7])
	v.ColorTransfer = int(b[8])
	v.ColorMatrix = int(b[9])
	return v, nil
}

// ParseVP9Frame parses the uncompressed header at the start of a VP9
// keyframe. A few dozen bytes are enough. Non-key frames return
// ErrNotKeyframe.
func ParseVP9Frame(frame []byte) (*Video, error) {
	r := bits.NewReader(frame)
	if r.U(2) != 2 {
		if r.Err() != nil {
			return nil, ErrTruncated
		}
		return nil, invalid("VP9 frame marker")
	}
	profile := int(r.U(1))
	profile |= int(r.U(1)) << 1
	if profile == 3 {
		r.Skip(1)
	}
	if r.Flag() { // show_existing_frame
		return nil, ErrNotKeyframe
	}
	if r.U(1) != 0 { // frame_type
		if r.Err() != nil {
			return nil, ErrTruncated
		}
		return nil, ErrNotKeyframe
	}
	r.Skip(2) // show_frame, error_resilient_mode
	if sync := r.U(24); sync != 0x498342 {
		if r.Err() != nil {
			return nil, ErrTruncated
		}
		return nil, invalid("VP9 sync code %#x", sync)
	}
	v := newVideo("vp9")
	v.Profile = "Profile " + strconv.Itoa(profile)
	v.BitDepth = 8
	if profile >= 2 {
		v.BitDepth = 10
		if r.Flag() {
			v.BitDepth = 12
		}
	}
	cs := r.U(3)
	ssx, ssy := true, true
	if cs != 7 {
		v.FullRange = r.Flag()
		if profile == 1 || profile == 3 {
			ssx, ssy = r.Flag(), r.Flag()
			r.Skip(1)
		}
	} else {
		v.FullRange = true
		if profile == 1 || profile == 3 {
			ssx, ssy = false, false
			r.Skip(1)
		}
	}
	v.ColorRangeKnown = true
	switch cs { // color_space → H.273 matrix coefficients
	case 1:
		v.ColorMatrix = 5
	case 2:
		v.ColorMatrix = 1
	case 3:
		v.ColorMatrix = 6
	case 4:
		v.ColorMatrix = 7
	case 5:
		v.ColorMatrix = 9
	case 7:
		v.ColorMatrix = 0
	}
	switch {
	case ssx && ssy:
		v.Chroma = "4:2:0"
	case ssx:
		v.Chroma = "4:2:2"
	case !ssy:
		v.Chroma = "4:4:4"
	default:
		v.Chroma = "4:4:0"
	}
	v.Width = int(r.U(16)) + 1
	v.Height = int(r.U(16)) + 1
	v.CodedWidth, v.CodedHeight = v.Width, v.Height
	if r.Err() != nil {
		return nil, ErrTruncated
	}
	return v, nil
}

// ParseVP8Frame parses the start of a VP8 keyframe (RFC 6386 §9.1): the
// frame tag, the start code and the 14-bit dimensions. Interframes return
// ErrNotKeyframe.
func ParseVP8Frame(frame []byte) (*Video, error) {
	if len(frame) < 3 {
		return nil, ErrTruncated
	}
	if frame[0]&1 != 0 {
		return nil, ErrNotKeyframe
	}
	if len(frame) < 10 {
		return nil, ErrTruncated
	}
	if frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return nil, invalid("VP8 start code")
	}
	v := newVideo("vp8")
	v.Profile = strconv.Itoa(int(frame[0] >> 1 & 7))
	v.Width = int(frame[6]) | int(frame[7]&0x3f)<<8
	v.Height = int(frame[8]) | int(frame[9]&0x3f)<<8
	if v.Width == 0 || v.Height == 0 {
		return nil, invalid("VP8 picture size")
	}
	v.CodedWidth, v.CodedHeight = v.Width, v.Height
	v.BitDepth = 8
	v.Chroma = "4:2:0"
	return v, nil
}
