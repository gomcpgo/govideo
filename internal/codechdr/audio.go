package codechdr

import (
	"encoding/binary"

	"github.com/gomcpgo/govideo/internal/bits"
)

// readDescriptor reads an MPEG-4 descriptor header (tag plus expandable
// length) and returns the tag, the body and the bytes after the body.
func readDescriptor(b []byte) (tag byte, body, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, ErrTruncated
	}
	tag = b[0]
	p := 1
	size := 0
	for i := 0; ; i++ {
		if i == 4 {
			return 0, nil, nil, invalid("descriptor length too long")
		}
		if p >= len(b) {
			return 0, nil, nil, ErrTruncated
		}
		c := b[p]
		p++
		size = size<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			break
		}
	}
	if size > len(b)-p {
		// Some writers overstate descriptor sizes; clamp to what is there.
		size = len(b) - p
	}
	return tag, b[p : p+size], b[p+size:], nil
}

// ParseESDS parses an esds box payload (FullBox version and flags
// included). It returns the objectTypeIndication (0x40 = MPEG-4 audio,
// 0x6B/0x69 = MP3, ...), the DecoderSpecificInfo bytes (nil when absent)
// and the bitrates of the DecoderConfigDescriptor.
func ParseESDS(b []byte) (objectTypeIndication byte, decoderSpecific []byte, avgBitrate, maxBitrate uint32, err error) {
	if len(b) < 4 {
		return 0, nil, 0, 0, ErrTruncated
	}
	tag, es, _, err := readDescriptor(b[4:])
	if err != nil {
		return 0, nil, 0, 0, err
	}
	if tag != 0x03 {
		return 0, nil, 0, 0, invalid("esds: expected ES_Descriptor, got tag %#x", tag)
	}
	if len(es) < 3 {
		return 0, nil, 0, 0, ErrTruncated
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 { // streamDependenceFlag
		if len(es) < 2 {
			return 0, nil, 0, 0, ErrTruncated
		}
		es = es[2:]
	}
	if flags&0x40 != 0 { // URL_Flag
		if len(es) < 1 || len(es) < 1+int(es[0]) {
			return 0, nil, 0, 0, ErrTruncated
		}
		es = es[1+int(es[0]):]
	}
	if flags&0x20 != 0 { // OCRstreamFlag
		if len(es) < 2 {
			return 0, nil, 0, 0, ErrTruncated
		}
		es = es[2:]
	}
	for len(es) > 0 {
		var body []byte
		tag, body, es, err = readDescriptor(es)
		if err != nil {
			return 0, nil, 0, 0, err
		}
		if tag != 0x04 { // DecoderConfigDescriptor
			continue
		}
		if len(body) < 13 {
			return 0, nil, 0, 0, ErrTruncated
		}
		objectTypeIndication = body[0]
		maxBitrate = binary.BigEndian.Uint32(body[5:])
		avgBitrate = binary.BigEndian.Uint32(body[9:])
		body = body[13:]
		for len(body) > 0 {
			var dsi []byte
			tag, dsi, body, err = readDescriptor(body)
			if err != nil {
				break // the fixed fields are still good
			}
			if tag == 0x05 {
				decoderSpecific = dsi
				break
			}
		}
		return objectTypeIndication, decoderSpecific, avgBitrate, maxBitrate, nil
	}
	return 0, nil, 0, 0, invalid("esds: no DecoderConfigDescriptor")
}

var aacSampleRates = [16]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func aacObjectType(r *bits.Reader) int {
	t := int(r.U(5))
	if t == 31 {
		t = 32 + int(r.U(6))
	}
	return t
}

func aacSampleRate(r *bits.Reader) int {
	i := r.U(4)
	if i == 15 {
		return int(r.U(24))
	}
	return aacSampleRates[i]
}

func aacProfileName(aot int) string {
	switch aot {
	case 1:
		return "Main"
	case 2:
		return "LC"
	case 3:
		return "SSR"
	case 4:
		return "LTP"
	case 5:
		return "HE-AAC"
	case 29:
		return "HE-AACv2"
	case 23:
		return "LD"
	case 39:
		return "ELD"
	case 42:
		return "xHE-AAC"
	}
	return ""
}

// aacChannels maps channelConfiguration to a channel count and ffprobe's
// layout name.
func aacChannels(cfg int) (int, string) {
	switch cfg {
	case 1:
		return 1, "mono"
	case 2:
		return 2, "stereo"
	case 3:
		return 3, "3.0"
	case 4:
		return 4, "4.0"
	case 5:
		return 5, "5.0"
	case 6:
		return 6, "5.1"
	case 7:
		return 8, "7.1(wide)"
	case 11:
		return 7, "6.1(back)"
	case 12:
		return 8, "7.1"
	case 14:
		return 8, "5.1.2"
	}
	return 0, ""
}

// aacProgramConfig parses a program_config_element far enough to count
// its channels.
func aacProgramConfig(r *bits.Reader) int {
	r.Skip(4 + 2 + 4) // element_instance_tag, object_type, sampling_frequency_index
	front, side, back := int(r.U(4)), int(r.U(4)), int(r.U(4))
	lfe := int(r.U(2))
	assoc := int(r.U(3))
	cc := int(r.U(4))
	if r.Flag() { // mono_mixdown_present
		r.Skip(4)
	}
	if r.Flag() { // stereo_mixdown_present
		r.Skip(4)
	}
	if r.Flag() { // matrix_mixdown_idx_present
		r.Skip(3)
	}
	ch := lfe
	for i := 0; i < front+side+back; i++ {
		if r.Flag() { // is_cpe
			ch++
		}
		ch++
		r.Skip(4)
	}
	r.Skip(4*lfe + 4*assoc + 5*cc)
	return ch
}

// ParseAudioSpecificConfig parses an MPEG-4 AudioSpecificConfig. Explicit
// (hierarchical and backward-compatible) SBR/PS signalling is honoured:
// the profile becomes "HE-AAC"/"HE-AACv2" and SampleRate the extension
// rate. Implicitly signalled SBR cannot be seen without decoding.
func ParseAudioSpecificConfig(b []byte) (*Audio, error) {
	r := bits.NewReader(b)
	a := &Audio{Codec: "aac"}
	aot := aacObjectType(r)
	rate := aacSampleRate(r)
	chanCfg := int(r.U(4))
	if r.Err() != nil {
		return nil, ErrTruncated
	}
	sbr, ps := false, false
	extRate := 0
	if aot == 5 || aot == 29 {
		sbr = true
		ps = aot == 29
		extRate = aacSampleRate(r)
		aot = aacObjectType(r)
		if aot == 22 {
			r.Skip(4) // extensionChannelConfiguration
		}
	}
	a.ObjectType = aot
	a.FrameSize = 1024
	pceChannels := 0
	switch aot {
	case 1, 2, 3, 4, 6, 7, 17, 19, 20, 21, 22, 23:
		// GASpecificConfig
		if r.Flag() { // frameLengthFlag
			a.FrameSize = 960
		}
		if r.Flag() { // dependsOnCoreCoder
			r.Skip(14)
		}
		ext := r.Flag()
		if chanCfg == 0 {
			pceChannels = aacProgramConfig(r)
		}
		if aot == 6 || aot == 20 {
			r.Skip(3)
		}
		if ext {
			switch aot {
			case 22:
				r.Skip(16)
			case 17, 19, 20, 23:
				r.Skip(3)
			}
			r.Skip(1) // extensionFlag3
		}
		if aot >= 17 {
			r.Skip(2) // epConfig
		}
		if r.Err() != nil {
			return nil, ErrTruncated
		}
		// Backward-compatible explicit signalling.
		if !sbr && r.BitsLeft() >= 16 && r.U(11) == 0x2b7 {
			if aacObjectType(r) == 5 {
				if r.Flag() { // sbrPresentFlag
					sbr = true
					extRate = aacSampleRate(r)
					if r.BitsLeft() >= 12 && r.U(11) == 0x548 {
						ps = r.Flag()
					}
				}
			}
			if r.Err() != nil {
				// A cut-off extension does not invalidate the core config.
				sbr, ps = false, false
			}
		}
	}
	a.Channels, a.ChannelLayout = aacChannels(chanCfg)
	if chanCfg == 0 {
		a.Channels = pceChannels
	}
	a.SampleRate = rate
	a.Profile = aacProfileName(aot)
	if sbr {
		a.Profile = "HE-AAC"
		if extRate > 0 {
			a.SampleRate = extRate
			if extRate > rate {
				a.FrameSize *= 2
			}
		}
		if ps {
			a.Profile = "HE-AACv2"
			if a.Channels == 1 {
				a.Channels, a.ChannelLayout = 2, "stereo"
			}
		}
	}
	if a.SampleRate == 0 {
		return nil, invalid("AAC sample rate")
	}
	return a, nil
}

func opusLayout(family, channels int) string {
	switch {
	case channels == 1:
		return "mono"
	case channels == 2:
		return "stereo"
	case family != 1:
		return ""
	}
	return vorbisLayout(channels)
}

// vorbisLayout names the Vorbis channel orders used by Opus family 1 and
// FLAC for more than two channels.
func vorbisLayout(channels int) string {
	switch channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 3:
		return "3.0"
	case 4:
		return "quad"
	case 5:
		return "5.0"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	}
	return ""
}

// ParseOpusHead parses an Ogg-style identification header ("OpusHead",
// RFC 7845 §5.1), as stored in Matroska CodecPrivate. SampleRate is always
// 48000, the rate Opus decodes at.
func ParseOpusHead(b []byte) (*Audio, error) {
	if len(b) < 19 {
		return nil, ErrTruncated
	}
	if string(b[:8]) != "OpusHead" {
		return nil, invalid("missing OpusHead magic")
	}
	if b[8]>>4 != 0 {
		return nil, invalid("OpusHead version %d", b[8])
	}
	return opusAudio(int(b[9]), int(binary.LittleEndian.Uint16(b[10:])), int(b[18]))
}

// ParseDOps parses a dOps box payload (OpusSpecificBox, Opus in ISO BMFF).
func ParseDOps(b []byte) (*Audio, error) {
	if len(b) < 11 {
		return nil, ErrTruncated
	}
	if b[0] != 0 {
		return nil, invalid("dOps version %d", b[0])
	}
	return opusAudio(int(b[1]), int(binary.BigEndian.Uint16(b[2:])), int(b[10]))
}

func opusAudio(channels, preSkip, family int) (*Audio, error) {
	if channels == 0 {
		return nil, invalid("Opus channel count 0")
	}
	return &Audio{
		Codec: "opus", SampleRate: 48000, Channels: channels,
		ChannelLayout: opusLayout(family, channels), PreSkip: preSkip,
	}, nil
}

// ParseFLACStreamInfo parses a FLAC STREAMINFO block (RFC 9639 §8.2). It
// accepts the bare 34-byte block, a block preceded by its 4-byte metadata
// block header, a dfLa box payload (FullBox header first) and a
// "fLaC"-prefixed stream header as found in Matroska CodecPrivate.
func ParseFLACStreamInfo(b []byte) (*Audio, error) {
	isHeader := func(p []byte) bool {
		return len(p) >= 38 && p[0]&0x7f == 0 && p[1] == 0 && p[2] == 0 && p[3] == 34
	}
	switch {
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		b = b[4:]
		if !isHeader(b) {
			if len(b) < 38 {
				return nil, ErrTruncated
			}
			return nil, invalid("fLaC marker not followed by STREAMINFO")
		}
		b = b[4:]
	case len(b) >= 42 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0 && isHeader(b[4:]):
		b = b[8:]
	case len(b) > 34 && isHeader(b):
		b = b[4:]
	}
	if len(b) < 34 {
		return nil, ErrTruncated
	}
	v := binary.BigEndian.Uint64(b[10:])
	a := &Audio{
		Codec:        "flac",
		SampleRate:   int(v >> 44),
		Channels:     int(v>>41&7) + 1,
		BitDepth:     int(v>>36&0x1f) + 1,
		TotalSamples: int64(v & (1<<36 - 1)),
	}
	if a.SampleRate == 0 {
		return nil, invalid("FLAC sample rate 0")
	}
	a.ChannelLayout = vorbisLayout(a.Channels)
	if minBlock, maxBlock := int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:])); minBlock == maxBlock {
		a.FrameSize = maxBlock
	}
	return a, nil
}

var mpaBitrates = [5][16]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // MPEG-1 layer I
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // MPEG-1 layer II
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // MPEG-1 layer III
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},    // MPEG-2/2.5 layer I
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // MPEG-2/2.5 layer II, III
}

// MPEGAudioFrame is what the 4-byte header of an MPEG-1, 2 or 2.5 audio
// frame (layers I to III) says about the frame.
type MPEGAudioFrame struct {
	Layer      int  // 1 to 3
	LowRate    bool // MPEG-2 or 2.5: the half and quarter sample rates
	Protected  bool // a 16-bit CRC follows the header
	Mono       bool
	SampleRate int
	BitRate    int // bits per second; 0 for a free-format stream
	Samples    int // sample frames in the frame
	Size       int // bytes in the frame, header included; 0 for free format
}

// MPEGAudioFrameHeader decodes a frame header; ok is false where b does not
// start with a valid one.
func MPEGAudioFrameHeader(b []byte) (h MPEGAudioFrame, ok bool) {
	if len(b) < 4 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return h, false
	}
	version := b[1] >> 3 & 3 // 0 = 2.5, 2 = MPEG-2, 3 = MPEG-1
	layerBits := b[1] >> 1 & 3
	brIdx := int(b[2] >> 4)
	srIdx := int(b[2] >> 2 & 3)
	if version == 1 || layerBits == 0 || brIdx == 15 || srIdx == 3 {
		return h, false
	}
	h.Layer = 4 - int(layerBits)
	h.LowRate = version != 3
	h.Protected = b[1]&1 == 0
	h.Mono = b[3]>>6 == 3
	h.SampleRate = [...]int{44100, 48000, 32000}[srIdx]
	row := h.Layer - 1
	switch version {
	case 2:
		h.SampleRate /= 2
	case 0:
		h.SampleRate /= 4
	}
	if h.LowRate {
		row = 3
		if h.Layer > 1 {
			row = 4
		}
	}
	h.BitRate = mpaBitrates[row][brIdx] * 1000
	switch {
	case h.Layer == 1:
		h.Samples = 384
	case h.Layer == 3 && h.LowRate:
		h.Samples = 576
	default:
		h.Samples = 1152
	}
	if h.BitRate > 0 {
		// A frame holds Samples/SampleRate seconds at BitRate, in slots of
		// four bytes (layer I) or one; the padding bit adds a slot.
		pad := int(b[2] >> 1 & 1)
		if h.Layer == 1 {
			h.Size = (h.Samples/32*h.BitRate/h.SampleRate + pad) * 4
		} else {
			h.Size = h.Samples/8*h.BitRate/h.SampleRate + pad
		}
	}
	return h, true
}

// ParseMPEGAudioHeader parses a 4-byte MPEG-1/2/2.5 audio frame header
// (layers I to III). BitRate is 0 for free-format streams.
func ParseMPEGAudioHeader(b []byte) (*Audio, error) {
	if len(b) < 4 {
		return nil, ErrTruncated
	}
	if b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return nil, invalid("MPEG audio sync word")
	}
	h, ok := MPEGAudioFrameHeader(b)
	if !ok {
		return nil, invalid("MPEG audio header")
	}
	a := &Audio{Codec: [...]string{"mp1", "mp2", "mp3"}[h.Layer-1], SampleRate: h.SampleRate,
		BitRate: h.BitRate, FrameSize: h.Samples, Channels: 2, ChannelLayout: "stereo"}
	if h.Mono {
		a.Channels, a.ChannelLayout = 1, "mono"
	}
	return a, nil
}
