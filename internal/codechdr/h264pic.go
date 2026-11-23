package codechdr

import "github.com/gomcpgo/govideo/internal/bits"

// H264Pictures reads, from the samples of an H.264 track, how each coded
// picture is structured: as a frame or as fields, and in which order the
// fields are shown. A deinterlacer needs this for every picture, and the
// stream says it nowhere else: the sequence parameter set only tells that
// fields may occur.
//
// It parses parameter sets, the first syntax elements of slice headers and
// picture timing SEI messages (H.264 §7.3.2.2, §7.3.3, §D.1.3). No picture
// data is decoded.
type H264Pictures struct {
	lenSize int // bytes in the length prefix of each NAL unit
	sps     [32]*h264SPS
	pps     [256]h264PPS
	fields  bool // a field picture has been seen
}

// h264PPS holds the picture parameter set fields slice headers depend on.
type h264PPS struct {
	ok  bool
	sps uint32
	// bottomPOC is bottom_field_pic_order_in_frame_present_flag: the slice
	// headers of frame pictures carry the order count of the bottom field.
	bottomPOC bool
}

// H264Picture describes the coded picture, or pictures, in one sample.
type H264Picture struct {
	// Found reports that the sample holds a slice whose parameter sets are
	// known; the other fields mean nothing without it.
	Found bool
	// FieldsAllowed is the negation of frame_mbs_only_flag: the sequence may
	// contain field pictures and field macroblocks.
	FieldsAllowed bool
	// FieldPicture reports a picture coded as a field (field_pic_flag), and
	// Bottom which one. BothFields reports that the sample also holds the
	// field of the other parity, so that it is a whole frame.
	FieldPicture, Bottom, BothFields bool
	// MBAFF reports a frame picture whose macroblock pairs choose between
	// frame and field coding.
	MBAFF bool
	// FieldsSeen reports that the stream has had a field picture, in this
	// sample or an earlier one.
	FieldsSeen bool
	// PicStruct is the pic_struct of the sample's first picture timing SEI
	// message (H.264 table D-1), or -1 when it has none.
	PicStruct int
	// POCDelta is the picture order count of the bottom field less that of
	// the top field of a frame picture, when the stream states it
	// (HasPOCDelta). The field with the lower count is shown first.
	POCDelta    int32
	HasPOCDelta bool
}

// Values of H264Picture.PicStruct (H.264 table D-1).
const (
	PicStructFrame           = 0
	PicStructTop             = 1
	PicStructBottom          = 2
	PicStructTopBottom       = 3
	PicStructBottomTop       = 4
	PicStructTopBottomTop    = 5
	PicStructBottomTopBottom = 6
	PicStructDoubling        = 7
	PicStructTripling        = 8
)

// NewH264Pictures returns a reader for a track whose configuration is the
// avcC box payload given.
func NewH264Pictures(avcC []byte) (*H264Pictures, error) {
	if len(avcC) < 6 {
		return nil, ErrTruncated
	}
	if avcC[0] != 1 {
		return nil, invalid("avcC version %d", avcC[0])
	}
	p := &H264Pictures{lenSize: int(avcC[4]&3) + 1}
	b := avcC[5:]
	for _, mask := range []byte{0x1f, 0xff} { // SPS list, then PPS list
		if len(b) < 1 {
			return nil, ErrTruncated
		}
		n := int(b[0] & mask)
		b = b[1:]
		for ; n > 0; n-- {
			if len(b) < 2 {
				return nil, ErrTruncated
			}
			l := int(b[0])<<8 | int(b[1])
			if l > len(b)-2 {
				return nil, ErrTruncated
			}
			p.parameterSet(b[2 : 2+l])
			b = b[2+l:]
		}
	}
	return p, nil
}

// parameterSet records an SPS or PPS NAL unit. One that does not parse is
// ignored: pictures that refer to it are then reported as not found.
func (p *H264Pictures) parameterSet(nal []byte) {
	if len(nal) < 2 || nal[0]&0x80 != 0 {
		return
	}
	switch nal[0] & 0x1f {
	case 7:
		if _, sps, err := parseH264SPS(nal); err == nil && sps.id < uint32(len(p.sps)) {
			p.sps[sps.id] = sps
		}
	case 8:
		r := bits.NewReader(bits.RemoveEmulationPrevention(nal[1:min(len(nal), 33)]))
		id, sps := r.UE(), r.UE()
		r.Skip(1) // entropy_coding_mode_flag
		bottom := r.Flag()
		if r.Err() == nil && id < uint32(len(p.pps)) && sps < uint32(len(p.sps)) {
			p.pps[id] = h264PPS{ok: true, sps: sps, bottomPOC: bottom}
		}
	}
}

// maxNALs caps the NAL units looked at in one sample. A picture has at most
// a few hundred slices.
const maxNALs = 4096

// Picture describes the sample given: NAL units, each preceded by its
// length, as MP4 and Matroska store them. Parameter sets found in the sample
// replace the ones held. It never fails: a sample it cannot read comes back
// with Found unset.
func (p *H264Pictures) Picture(sample []byte) H264Picture {
	pic := H264Picture{PicStruct: -1}
	var (
		sps *h264SPS
		sei [][]byte
	)
	b := sample
	for count := 0; len(b) >= p.lenSize && count < maxNALs; count++ {
		n := 0
		for _, c := range b[:p.lenSize] {
			n = n<<8 | int(c)
		}
		b = b[p.lenSize:]
		if n < 0 || n > len(b) {
			break
		}
		nal := b[:n]
		b = b[n:]
		if n < 2 || nal[0]&0x80 != 0 {
			continue
		}
		switch t := nal[0] & 0x1f; t {
		case 7, 8:
			p.parameterSet(nal)
		case 6:
			if !pic.Found && len(sei) < 16 {
				// Read once the slice that follows says which SPS applies.
				sei = append(sei, nal)
			}
		case 1, 5:
			s, hdr, ok := p.sliceHeader(nal, t == 5)
			if !ok {
				continue
			}
			p.fields = p.fields || hdr.field
			if !pic.Found {
				pic.Found = true
				sps = s
				pic.FieldsAllowed = !s.frameMbsOnly
				pic.FieldPicture, pic.Bottom = hdr.field, hdr.bottom
				pic.MBAFF = s.mbaff && !hdr.field
				pic.POCDelta, pic.HasPOCDelta = hdr.pocDelta, hdr.hasPOCDelta
			} else if pic.FieldPicture && hdr.field && hdr.bottom != pic.Bottom {
				pic.BothFields = true
			}
		}
	}
	if !pic.Found {
		return pic
	}
	pic.FieldsSeen = p.fields
	if sps.picStructPresent {
		for _, nal := range sei {
			if ps, ok := picTimingStruct(nal, sps); ok {
				pic.PicStruct = ps
				break
			}
		}
	}
	return pic
}

type h264Slice struct {
	field, bottom bool
	pocDelta      int32
	hasPOCDelta   bool
}

// sliceHeader reads the start of a slice header (H.264 §7.3.3), as far as
// the picture order count.
func (p *H264Pictures) sliceHeader(nal []byte, idr bool) (*h264SPS, h264Slice, bool) {
	var h h264Slice
	// The elements read here fit in a few bytes; the slice data that follows
	// is left alone.
	r := bits.NewReader(bits.RemoveEmulationPrevention(nal[1:min(len(nal), 65)]))
	r.UE() // first_mb_in_slice
	r.UE() // slice_type
	id := r.UE()
	if r.Err() != nil || id >= uint32(len(p.pps)) || !p.pps[id].ok {
		return nil, h, false
	}
	pps := p.pps[id]
	sps := p.sps[pps.sps]
	if sps == nil || sps.frameNumBits > 16 || sps.pocLsbBits > 16 {
		return nil, h, false
	}
	if sps.separatePlanes {
		r.Skip(2) // colour_plane_id
	}
	r.Skip(int(sps.frameNumBits))
	if !sps.frameMbsOnly {
		if h.field = r.Flag(); h.field {
			h.bottom = r.Flag()
		}
	}
	if idr {
		r.UE() // idr_pic_id
	}
	switch sps.pocType {
	case 0:
		r.Skip(int(sps.pocLsbBits))
		if pps.bottomPOC && !h.field {
			h.pocDelta, h.hasPOCDelta = r.SE(), true
		}
	case 1:
		if !h.field {
			h.pocDelta, h.hasPOCDelta = sps.topToBottom, true
		}
		if !sps.deltaPOCAlwaysZero {
			r.SE() // delta_pic_order_cnt[0]
			if pps.bottomPOC && !h.field {
				h.pocDelta += r.SE()
			}
		}
	}
	if r.Err() != nil {
		return nil, h, false
	}
	return sps, h, true
}

// picTimingStruct returns the pic_struct of the picture timing message in an
// SEI NAL unit (H.264 §7.3.2.3.1, §D.1.3), if it has one.
func picTimingStruct(nal []byte, sps *h264SPS) (int, bool) {
	// Picture timing comes first in practice; a long message ahead of it
	// (user data) is looked past only within this many bytes.
	b := bits.RemoveEmulationPrevention(nal[1:min(len(nal), 4097)])
	for len(b) >= 2 {
		var typ, size int
		for {
			if len(b) == 0 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			typ += int(c)
			if c != 0xff {
				break
			}
		}
		for {
			if len(b) == 0 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			size += int(c)
			if c != 0xff {
				break
			}
		}
		if size > len(b) {
			return 0, false
		}
		if typ == 1 {
			r := bits.NewReader(b[:size])
			if sps.cpbDpbDelays {
				r.Skip(sps.cpbDelayBits + sps.dpbDelayBits)
			}
			ps := int(r.U(4))
			return ps, r.Err() == nil
		}
		b = b[size:]
	}
	return 0, false
}

// FieldOrder says what the frame decoded from a sample holds.
type FieldOrder int

const (
	// Progressive is one picture, or a sample whose structure is not known.
	Progressive FieldOrder = iota
	// TopFieldFirst and BottomFieldFirst are two fields taken at different
	// times, woven into one frame; the field named is the earlier one.
	TopFieldFirst
	BottomFieldFirst
	// SingleField is a sample that holds one field only, its partner being
	// in the next sample (as transport stream remuxes store field pictures).
	SingleField
)

// FieldOrder interprets the picture's structure.
//
// A stream whose sequence parameter set rules fields out is progressive. In
// one that allows them, the picture timing message decides where there is
// one: it is the encoder's statement of how the picture is to be shown, and
// it tells a progressive frame in an interlaced stream (pic_struct 0, or a
// film frame shown for three fields) from a pair of fields.
//
// Without it, a frame whose fields have different picture order counts is
// two fields, the lower count first. Where the counts say nothing, a frame
// is two fields, top first, if the encoder is using the field tools at all
// (macroblock-adaptive coding, or field pictures elsewhere in the stream):
// that is what cameras and broadcast encoders write. A stream that allows
// fields and never uses them holds progressive frames (encoders write those
// for players that insist on an interlaced format).
func (p H264Picture) FieldOrder() FieldOrder {
	switch {
	case !p.Found || !p.FieldsAllowed:
		return Progressive
	case p.FieldPicture && !p.BothFields:
		return SingleField
	case p.FieldPicture:
		// The fields are shown in the order they are coded.
		if p.Bottom {
			return BottomFieldFirst
		}
		return TopFieldFirst
	}
	switch p.PicStruct {
	case PicStructFrame, PicStructDoubling, PicStructTripling, PicStructTopBottomTop, PicStructBottomTopBottom:
		return Progressive
	case PicStructTopBottom:
		return TopFieldFirst
	case PicStructBottomTop:
		return BottomFieldFirst
	}
	switch {
	case p.HasPOCDelta && p.POCDelta < 0:
		return BottomFieldFirst
	case p.HasPOCDelta && p.POCDelta > 0, p.MBAFF, p.FieldsSeen:
		return TopFieldFirst
	}
	return Progressive
}
