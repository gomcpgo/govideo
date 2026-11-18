// Package codechdr parses codec configuration headers (SPS, sequence
// headers, AudioSpecificConfig, ...) into stream metadata. It only reads
// metadata: no audio or video is decoded (SPEC §10.2).
//
// The parsers are written from the public specifications (ITU-T H.264 and
// H.265, the AV1 and VP9 bitstream specifications, RFC 6386, RFC 7845,
// RFC 9639, ISO/IEC 14496-3 syntax) and are safe on hostile input: every
// read is bounds-checked and every loop count is capped.
package codechdr

import (
	"errors"
	"fmt"
)

// Errors returned by the parsers.
var (
	// ErrTruncated is returned when the input ends before the header does.
	ErrTruncated = errors.New("codechdr: truncated data")
	// ErrInvalid is returned when the input is not a valid header.
	ErrInvalid = errors.New("codechdr: invalid data")
	// ErrNotKeyframe is returned by the VP8/VP9 frame parsers when the frame
	// carries no sequence-level information.
	ErrNotKeyframe = errors.New("codechdr: not a keyframe")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// LevelUnknown is the Level value used when a header carries no level.
const LevelUnknown = -99

// Video describes a video stream as far as its configuration header tells.
type Video struct {
	Codec   string // "h264", "hevc", "av1", "vp8", "vp9"
	Profile string // ffprobe vocabulary, e.g. "High", "Main 10", "Profile 0"
	// Level is the integer ffprobe prints: H.264 level_idc (40), HEVC
	// general_level_idc (120), AV1 seq_level_idx, VP9 level from vpcC, or
	// LevelUnknown.
	Level int

	CodedWidth, CodedHeight int // before cropping
	Width, Height           int // after cropping / conformance window
	SARNum, SARDen          int // 0/0 when unspecified
	BitDepth                int
	Chroma                  string // "4:0:0", "4:2:0", "4:2:2", "4:4:4"
	Interlaced              bool

	// HasTiming reports that the header carries timing; the frame rate is
	// then TimeNum/TimeDen frames per second.
	HasTiming        bool
	TimeNum, TimeDen uint32

	// H.273 code points; 2 means unspecified.
	ColorPrimaries, ColorTransfer, ColorMatrix int
	ColorRangeKnown                            bool
	FullRange                                  bool

	RefFrames int // H.264 max_num_ref_frames
	// HasBFrames is the reorder depth where the header states it
	// (H.264 max_num_reorder_frames, HEVC sps_max_num_reorder_pics), or -1.
	HasBFrames int
}

func newVideo(codec string) *Video {
	return &Video{Codec: codec, Level: LevelUnknown, ColorPrimaries: 2, ColorTransfer: 2, ColorMatrix: 2, HasBFrames: -1}
}

// Audio describes an audio stream as far as its configuration header tells.
type Audio struct {
	Codec         string // "aac", "opus", "flac", "mp3", "mp2", "mp1"
	Profile       string // AAC: "LC", "HE-AAC", "HE-AACv2", "Main", "LTP", ...
	ObjectType    int    // AAC audio object type
	SampleRate    int    // output rate (the SBR extension rate when signalled)
	Channels      int
	ChannelLayout string // ffprobe vocabulary where derivable
	BitDepth      int    // FLAC
	FrameSize     int    // samples per frame where known
	BitRate       int    // MPEG audio frame header, bits per second
	PreSkip       int    // Opus
	TotalSamples  int64  // FLAC
}

// DolbyVision is the content of a dvcC/dvvC configuration record.
type DolbyVision struct {
	VersionMajor, VersionMinor       int
	Profile, Level                   int
	RPUPresent, ELPresent, BLPresent bool
	BLSignalCompatibilityID          int
}

// ParseDOVIConfig parses a dvcC/dvvC/dvwC box payload.
func ParseDOVIConfig(b []byte) (*DolbyVision, error) {
	if len(b) < 5 {
		return nil, ErrTruncated
	}
	v := uint16(b[2])<<8 | uint16(b[3])
	return &DolbyVision{
		VersionMajor:            int(b[0]),
		VersionMinor:            int(b[1]),
		Profile:                 int(v >> 9),
		Level:                   int(v >> 3 & 0x3f),
		RPUPresent:              v&4 != 0,
		ELPresent:               v&2 != 0,
		BLPresent:               v&1 != 0,
		BLSignalCompatibilityID: int(b[4] >> 4),
	}, nil
}

// ParseColr parses a 'colr' box payload of type nclx (ISO BMFF) or nclc
// (QuickTime). ok is false for other colour types (ICC profiles) or short
// data. fullRange is only meaningful for nclx.
func ParseColr(b []byte) (prim, trc, matrix int, fullRange, ok bool) {
	if len(b) < 10 {
		return 2, 2, 2, false, false
	}
	typ := string(b[:4])
	if typ != "nclx" && typ != "nclc" {
		return 2, 2, 2, false, false
	}
	prim = int(b[4])<<8 | int(b[5])
	trc = int(b[6])<<8 | int(b[7])
	matrix = int(b[8])<<8 | int(b[9])
	if typ == "nclx" && len(b) >= 11 {
		fullRange = b[10]&0x80 != 0
	}
	return prim, trc, matrix, fullRange, true
}

// PrimariesName returns ffprobe's name for an H.273 colour primaries code
// point, or "" when unspecified or unknown.
func PrimariesName(v int) string {
	switch v {
	case 1:
		return "bt709"
	case 4:
		return "bt470m"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "film"
	case 9:
		return "bt2020"
	case 10:
		return "smpte428"
	case 11:
		return "smpte431"
	case 12:
		return "smpte432"
	case 22:
		return "ebu3213"
	}
	return ""
}

// TransferName returns ffprobe's name for an H.273 transfer characteristics
// code point, or "" when unspecified or unknown.
func TransferName(v int) string {
	switch v {
	case 1:
		return "bt709"
	case 4:
		return "bt470m"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "linear"
	case 9:
		return "log100"
	case 10:
		return "log316"
	case 11:
		return "iec61966-2-4"
	case 12:
		return "bt1361e"
	case 13:
		return "iec61966-2-1"
	case 14:
		return "bt2020-10"
	case 15:
		return "bt2020-12"
	case 16:
		return "smpte2084"
	case 17:
		return "smpte428"
	case 18:
		return "arib-std-b67"
	}
	return ""
}

// MatrixName returns ffprobe's name for an H.273 matrix coefficients code
// point, or "" when unspecified or unknown.
func MatrixName(v int) string {
	switch v {
	case 0:
		return "gbr"
	case 1:
		return "bt709"
	case 4:
		return "fcc"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "ycgco"
	case 9:
		return "bt2020nc"
	case 10:
		return "bt2020c"
	case 11:
		return "smpte2085"
	case 12:
		return "chroma-derived-nc"
	case 13:
		return "chroma-derived-c"
	case 14:
		return "ictcp"
	}
	return ""
}

// sarTable is Table E-1 of H.264 / H.265 (aspect_ratio_idc 1..16).
var sarTable = [17][2]int{
	{0, 0}, {1, 1}, {12, 11}, {10, 11}, {16, 11}, {40, 33}, {24, 11}, {20, 11}, {32, 11},
	{80, 33}, {18, 11}, {15, 11}, {64, 33}, {160, 99}, {4, 3}, {3, 2}, {2, 1},
}

func chromaName(idc int) string {
	switch idc {
	case 0:
		return "4:0:0"
	case 1:
		return "4:2:0"
	case 2:
		return "4:2:2"
	case 3:
		return "4:4:4"
	}
	return ""
}
