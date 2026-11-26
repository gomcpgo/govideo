package codechdr

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Small valid records captured from oracle fixtures (configuration headers
// only, a few dozen bytes each). They keep the basic tests and the fuzz
// seeds independent of ffmpeg being installed.
const (
	seedAVCCHigh       = "0164000dffe100196764000dacd94141fb011000000300100000030320f142996001000668ebe3cb22c0fdf8f800"
	seedAVCCHLG        = "016e000dffe1001b676e000da6cb40a0fd80b509120940000003004000000c83c50aa801000468ce0fc8fdfafa00"
	seedAVCCInterlaced = "0164001effe100186764001eacd940b424d8088000000300800000190f8a14cb01000668fba3cb22c0fdf8f800"
	seedAVCCScaling    = "0164000dffe100196764000dacd94141fb011000000300100000030320f142996001000768ebe3cb3002c0fdf8f800"
	seedHVCCHLG        = "0102200000009000000000003cf000fcfdfafa00000f03a00001001840010c01ffff02200000030090000003000003003c959409a10001002d42010102200000030090000003000003003ca00a080f136595952930bc05a84890482000000300200000030321a2000100064401c073c18927"
	seedHVCCRPS        = "0101600000009000000000003cf000fcfdf8f800000f03a00001001840010c01ffff01600000030090000003000003003c959809a10001002a42010101600000030090000003000003003ca00a080f165959a4932bc05a020000030002000003003210a2000100074401c172b4624027"
	seedAV1C10         = "81004c000a0b0200000521e7fde017c202"
	seedAV1CHLG        = "81004c000a0e0200000521e7fde017c284890482"
	seedVPCC0          = "010000000014820202020000"
	seedVPCC2          = "010000000214a20912090000"
	seedVP9Frame0      = "824983420013f00ef60638241c184a0009305062"
	seedVP9Frame2      = "924983420009f8077b031c120e0c2c0004585062"
	seedVP8Frame       = "105f009d012a4001f00000470885858899848802"
	seedESDSLC         = "000000000380808025000100048080801740150000000001f84b0001f84b0580808005119056e500068080800102"
	seedESDSHEv2       = "0000000003808080270001000480808019401500000000007d0000007a2e0580808007138856e5a54880068080800102"
	seedASCLC          = "119056e500"
	seedASCLC51        = "11b056e500"
	seedASCHE          = "139056e5a0"
	seedASCHEv2        = "138856e5a54880"
	seedASCHEExplicit  = "2b920800" // AOT 5 hierarchical: 22.05 kHz core, 44.1 kHz SBR, stereo
	seedOpusHead       = "4f707573486561640102380180bb0000000000"
	seedOpusHead51     = "4f707573486561640106380180bb00000000010402000401020305"
	seedDOps           = "000201380000bb80000000"
	seedDOps51         = "000601380000bb800000010402000401020305"
	seedFLAC           = "664c614300000022040004000000fa0001840ac442f000005622ba7ed542b5cf1c48269e1b81681b54ee"
	seedDFLA           = "0000000080000022040004000000fa0001840ac442f000005622ba7ed542b5cf1c48269e1b81681b54ee"
	seedMP3            = "fffb9064"
	seedDOVI           = "010010254000000000000000000000000000000000000000" // profile 8, level 4, RPU+BL, compat 4
	seedColrNCLX       = "6e636c78000900120009" + "00"
)

func unhex(tb testing.TB, s string) []byte {
	tb.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func TestSeedsVideo(t *testing.T) {
	type want struct {
		profile       string
		level         int
		w, h, cw, ch  int
		depth         int
		chroma        string
		interlaced    bool
		fpsNum, fpsDn uint32
		prim, trc, mx int
	}
	cases := []struct {
		name  string
		parse func([]byte) (*Video, error)
		seed  string
		want  want
	}{
		{"avcC high", ParseAVCC, seedAVCCHigh, want{"High", 13, 320, 240, 320, 240, 8, "4:2:0", false, 50, 2, 2, 2, 2}},
		{"avcC hlg", ParseAVCC, seedAVCCHLG, want{"High 10", 13, 320, 240, 320, 240, 10, "4:2:0", false, 50, 2, 9, 18, 9}},
		{"avcC interlaced", ParseAVCC, seedAVCCInterlaced, want{"High", 30, 720, 576, 720, 576, 8, "4:2:0", true, 50, 2, 2, 2, 2}},
		{"avcC scaling lists", ParseAVCC, seedAVCCScaling, want{"High", 13, 320, 240, 320, 240, 8, "4:2:0", false, 50, 2, 2, 2, 2}},
		{"hvcC hlg", ParseHVCC, seedHVCCHLG, want{"Main 10", 60, 320, 240, 320, 240, 10, "4:2:0", false, 25, 1, 9, 18, 9}},
		{"hvcC rps", ParseHVCC, seedHVCCRPS, want{"Main", 60, 320, 240, 320, 240, 8, "4:2:0", false, 25, 1, 2, 2, 2}},
		{"av1C 10-bit", ParseAV1C, seedAV1C10, want{"Main", 0, 320, 240, 320, 240, 10, "4:2:0", false, 0, 0, 2, 2, 2}},
		{"av1C hlg", ParseAV1C, seedAV1CHLG, want{"Main", 0, 320, 240, 320, 240, 10, "4:2:0", false, 0, 0, 9, 18, 9}},
		{"vpcC p0", ParseVPCC, seedVPCC0, want{"Profile 0", 20, 0, 0, 0, 0, 8, "4:2:0", false, 0, 0, 2, 2, 2}},
		{"vpcC p2", ParseVPCC, seedVPCC2, want{"Profile 2", 20, 0, 0, 0, 0, 10, "4:2:0", false, 0, 0, 9, 18, 9}},
		{"vp9 frame p0", ParseVP9Frame, seedVP9Frame0, want{"Profile 0", LevelUnknown, 320, 240, 320, 240, 8, "4:2:0", false, 0, 0, 2, 2, 2}},
		{"vp9 frame p2", ParseVP9Frame, seedVP9Frame2, want{"Profile 2", LevelUnknown, 320, 240, 320, 240, 10, "4:2:0", false, 0, 0, 2, 2, 2}},
		{"vp8 frame", ParseVP8Frame, seedVP8Frame, want{"0", LevelUnknown, 320, 240, 320, 240, 8, "4:2:0", false, 0, 0, 2, 2, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := c.parse(unhex(t, c.seed))
			if err != nil {
				t.Fatal(err)
			}
			got := want{v.Profile, v.Level, v.Width, v.Height, v.CodedWidth, v.CodedHeight, v.BitDepth, v.Chroma,
				v.Interlaced, v.TimeNum, v.TimeDen, v.ColorPrimaries, v.ColorTransfer, v.ColorMatrix}
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestSeedsAudio(t *testing.T) {
	cases := []struct {
		name  string
		parse func([]byte) (*Audio, error)
		seed  string
		want  Audio
	}{
		{"asc lc", ParseAudioSpecificConfig, seedASCLC, Audio{Codec: "aac", Profile: "LC", ObjectType: 2, SampleRate: 48000, Channels: 2, ChannelLayout: "stereo", FrameSize: 1024}},
		{"asc lc 5.1", ParseAudioSpecificConfig, seedASCLC51, Audio{Codec: "aac", Profile: "LC", ObjectType: 2, SampleRate: 48000, Channels: 6, ChannelLayout: "5.1", FrameSize: 1024}},
		{"asc he", ParseAudioSpecificConfig, seedASCHE, Audio{Codec: "aac", Profile: "HE-AAC", ObjectType: 2, SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", FrameSize: 2048}},
		{"asc hev2", ParseAudioSpecificConfig, seedASCHEv2, Audio{Codec: "aac", Profile: "HE-AACv2", ObjectType: 2, SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", FrameSize: 2048}},
		{"asc he hierarchical", ParseAudioSpecificConfig, seedASCHEExplicit, Audio{Codec: "aac", Profile: "HE-AAC", ObjectType: 2, SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", FrameSize: 2048}},
		{"OpusHead", ParseOpusHead, seedOpusHead, Audio{Codec: "opus", SampleRate: 48000, Channels: 2, ChannelLayout: "stereo", PreSkip: 312}},
		{"OpusHead 5.1", ParseOpusHead, seedOpusHead51, Audio{Codec: "opus", SampleRate: 48000, Channels: 6, ChannelLayout: "5.1", PreSkip: 312}},
		{"dOps", ParseDOps, seedDOps, Audio{Codec: "opus", SampleRate: 48000, Channels: 2, ChannelLayout: "stereo", PreSkip: 312}},
		{"dOps 5.1", ParseDOps, seedDOps51, Audio{Codec: "opus", SampleRate: 48000, Channels: 6, ChannelLayout: "5.1", PreSkip: 312}},
		{"fLaC", ParseFLACStreamInfo, seedFLAC, Audio{Codec: "flac", SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", BitDepth: 16, FrameSize: 1024, TotalSamples: 22050}},
		{"dfLa", ParseFLACStreamInfo, seedDFLA, Audio{Codec: "flac", SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", BitDepth: 16, FrameSize: 1024, TotalSamples: 22050}},
		{"mp3", ParseMPEGAudioHeader, seedMP3, Audio{Codec: "mp3", SampleRate: 44100, Channels: 2, ChannelLayout: "stereo", FrameSize: 1152, BitRate: 128000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := c.parse(unhex(t, c.seed))
			if err != nil {
				t.Fatal(err)
			}
			if *a != c.want {
				t.Errorf("got  %+v\nwant %+v", *a, c.want)
			}
		})
	}
}

func TestESDS(t *testing.T) {
	oti, dsi, avg, maxBR, err := ParseESDS(unhex(t, seedESDSLC))
	if err != nil {
		t.Fatal(err)
	}
	if oti != 0x40 || hex.EncodeToString(dsi) != seedASCLC || avg != 129099 || maxBR != 129099 {
		t.Errorf("got oti=%#x dsi=%x avg=%d max=%d", oti, dsi, avg, maxBR)
	}
	// Single-byte descriptor lengths, no DecoderSpecificInfo (e.g. MP3 in MP4).
	short := unhex(t, "00000000"+"0312"+"000100"+"040d"+"6b15000000"+"0001f400"+"0001f400")
	oti, dsi, avg, _, err = ParseESDS(short)
	if err != nil || oti != 0x6b || dsi != nil || avg != 128000 {
		t.Errorf("short esds: oti=%#x dsi=%x avg=%d err=%v", oti, dsi, avg, err)
	}
}

func TestDOVI(t *testing.T) {
	d, err := ParseDOVIConfig(unhex(t, seedDOVI))
	if err != nil {
		t.Fatal(err)
	}
	want := DolbyVision{VersionMajor: 1, Profile: 8, Level: 4, RPUPresent: true, BLPresent: true, BLSignalCompatibilityID: 4}
	if *d != want {
		t.Errorf("got %+v, want %+v", *d, want)
	}
	if _, err := ParseDOVIConfig([]byte{1, 0, 0}); !errors.Is(err, ErrTruncated) {
		t.Errorf("short record: %v", err)
	}
}

func TestColr(t *testing.T) {
	p, tr, m, full, ok := ParseColr(unhex(t, seedColrNCLX))
	if !ok || p != 9 || tr != 18 || m != 9 || full {
		t.Errorf("nclx: %d %d %d %v %v", p, tr, m, full, ok)
	}
	_, _, _, full, ok = ParseColr(unhex(t, "6e636c7800010001000180"))
	if !ok || !full {
		t.Errorf("nclx full range: %v %v", full, ok)
	}
	p, tr, m, _, ok = ParseColr(unhex(t, "6e636c63000100010006"))
	if !ok || p != 1 || tr != 1 || m != 6 {
		t.Errorf("nclc: %d %d %d %v", p, tr, m, ok)
	}
	if _, _, _, _, ok = ParseColr([]byte("prof\x00\x00\x00\x00\x00\x00\x00")); ok {
		t.Errorf("ICC profile reported as nclx")
	}
	if PrimariesName(9) != "bt2020" || TransferName(18) != "arib-std-b67" || TransferName(16) != "smpte2084" ||
		MatrixName(9) != "bt2020nc" || PrimariesName(2) != "" || MatrixName(2) != "" {
		t.Errorf("H.273 names")
	}
}

func TestNotKeyframe(t *testing.T) {
	if _, err := ParseVP8Frame([]byte{0x11, 0, 0, 1, 2, 3, 4, 5, 6, 7}); !errors.Is(err, ErrNotKeyframe) {
		t.Errorf("VP8 interframe: %v", err)
	}
	if _, err := ParseVP9Frame([]byte{0x86, 0, 0, 0, 0, 0, 0, 0}); !errors.Is(err, ErrNotKeyframe) {
		t.Errorf("VP9 interframe: %v", err)
	}
	if _, err := ParseVP9Frame([]byte{0x88, 0}); !errors.Is(err, ErrNotKeyframe) {
		t.Errorf("VP9 show_existing_frame: %v", err)
	}
}

type parser struct {
	name string
	fn   func([]byte) error
	seed []string
}

func vid(f func([]byte) (*Video, error)) func([]byte) error {
	return func(b []byte) error { _, err := f(b); return err }
}
func aud(f func([]byte) (*Audio, error)) func([]byte) error {
	return func(b []byte) error { _, err := f(b); return err }
}

// nalFromAVCC / nalFromHVCC pull the SPS out of a seed record.
func spsFromAVCC(s string) string { return s[16 : 16+2*0x19] }
func spsFromHVCC(s string) string {
	i := strings.Index(s, "a10001") + 10
	return s[i : i+2*0x2a]
}

var parsers = []parser{
	{"AVCC", vid(ParseAVCC), []string{seedAVCCHigh, seedAVCCHLG, seedAVCCInterlaced, seedAVCCScaling}},
	{"H264SPS", vid(ParseH264SPS), []string{spsFromAVCC(seedAVCCHigh), spsFromAVCC(seedAVCCScaling)}},
	{"HVCC", vid(ParseHVCC), []string{seedHVCCHLG, seedHVCCRPS}},
	{"HEVCSPS", vid(ParseHEVCSPS), []string{spsFromHVCC(seedHVCCRPS)}},
	{"AV1C", vid(ParseAV1C), []string{seedAV1C10, seedAV1CHLG}},
	{"AV1SequenceHeader", vid(ParseAV1SequenceHeader), []string{seedAV1C10[8:], seedAV1CHLG[8:]}},
	{"VPCC", vid(ParseVPCC), []string{seedVPCC0, seedVPCC2}},
	{"VP9Frame", vid(ParseVP9Frame), []string{seedVP9Frame0, seedVP9Frame2}},
	{"VP8Frame", vid(ParseVP8Frame), []string{seedVP8Frame}},
	{"ESDS", func(b []byte) error { _, _, _, _, err := ParseESDS(b); return err }, []string{seedESDSLC, seedESDSHEv2}},
	{"AudioSpecificConfig", aud(ParseAudioSpecificConfig), []string{seedASCLC, seedASCLC51, seedASCHE, seedASCHEv2, seedASCHEExplicit}},
	{"OpusHead", aud(ParseOpusHead), []string{seedOpusHead, seedOpusHead51}},
	{"DOps", aud(ParseDOps), []string{seedDOps, seedDOps51}},
	{"FLACStreamInfo", aud(ParseFLACStreamInfo), []string{seedFLAC, seedDFLA, seedFLAC[16:]}},
	{"MPEGAudioHeader", aud(ParseMPEGAudioHeader), []string{seedMP3}},
	{"FLACFrameHeader", func(b []byte) error {
		if _, ok := FLACFrameHeader(b); !ok {
			return invalid("FLAC frame header")
		}
		return nil
	}, []string{seedFLACFrame1, seedFLACFrame2, seedFLACFrame3, seedFLACFrame4}},
	{"DOVIConfig", func(b []byte) error { _, err := ParseDOVIConfig(b); return err }, []string{seedDOVI}},
	{"Colr", func(b []byte) error { ParseColr(b); return nil }, []string{seedColrNCLX}},
}

// TestSeedsParse checks that every seed is accepted by its parser, and that
// truncating or corrupting it never panics.
func TestSeedsParse(t *testing.T) {
	for _, p := range parsers {
		for _, s := range p.seed {
			b := unhex(t, s)
			if err := p.fn(b); err != nil {
				t.Errorf("%s: seed %s: %v", p.name, s, err)
			}
			for n := 0; n < len(b); n++ {
				p.fn(b[:n])
			}
			for i := range b {
				for _, x := range []byte{0x01, 0x80, 0xff} {
					c := append([]byte(nil), b...)
					c[i] ^= x
					p.fn(c)
				}
			}
		}
	}
}

func TestTruncatedErrors(t *testing.T) {
	for _, p := range parsers {
		if p.name == "Colr" {
			continue
		}
		if err := p.fn(nil); err == nil {
			t.Errorf("%s: empty input accepted", p.name)
		}
	}
	sps := unhex(t, spsFromAVCC(seedAVCCHigh))
	if _, err := ParseH264SPS(sps[:8]); !errors.Is(err, ErrTruncated) {
		t.Errorf("truncated SPS: %v", err)
	}
	hsps := unhex(t, spsFromHVCC(seedHVCCRPS))
	if _, err := ParseHEVCSPS(hsps[:20]); !errors.Is(err, ErrTruncated) {
		t.Errorf("truncated HEVC SPS: %v", err)
	}
}

func fuzz(f *testing.F, name string) {
	for _, p := range parsers {
		if p.name != name {
			continue
		}
		for _, s := range p.seed {
			f.Add(unhex(f, s))
		}
		f.Fuzz(func(t *testing.T, b []byte) { p.fn(b) })
		return
	}
	f.Fatalf("no parser %q", name)
}

func FuzzAVCC(f *testing.F)                { fuzz(f, "AVCC") }
func FuzzH264SPS(f *testing.F)             { fuzz(f, "H264SPS") }
func FuzzHVCC(f *testing.F)                { fuzz(f, "HVCC") }
func FuzzHEVCSPS(f *testing.F)             { fuzz(f, "HEVCSPS") }
func FuzzAV1C(f *testing.F)                { fuzz(f, "AV1C") }
func FuzzAV1SequenceHeader(f *testing.F)   { fuzz(f, "AV1SequenceHeader") }
func FuzzVPCC(f *testing.F)                { fuzz(f, "VPCC") }
func FuzzVP9Frame(f *testing.F)            { fuzz(f, "VP9Frame") }
func FuzzVP8Frame(f *testing.F)            { fuzz(f, "VP8Frame") }
func FuzzESDS(f *testing.F)                { fuzz(f, "ESDS") }
func FuzzAudioSpecificConfig(f *testing.F) { fuzz(f, "AudioSpecificConfig") }
func FuzzOpusHead(f *testing.F)            { fuzz(f, "OpusHead") }
func FuzzDOps(f *testing.F)                { fuzz(f, "DOps") }
func FuzzFLACStreamInfo(f *testing.F)      { fuzz(f, "FLACStreamInfo") }
func FuzzMPEGAudioHeader(f *testing.F)     { fuzz(f, "MPEGAudioHeader") }
func FuzzFLACFrameHeader(f *testing.F)     { fuzz(f, "FLACFrameHeader") }
func FuzzDOVIConfig(f *testing.F)          { fuzz(f, "DOVIConfig") }
func FuzzColr(f *testing.F)                { fuzz(f, "Colr") }
