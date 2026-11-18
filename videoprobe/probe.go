// Package videoprobe reports timing, codec and resolution information about
// media files: a pure-Go subset of ffprobe.
//
// It reads container structure and codec configuration headers only. It never
// decodes audio or video, has no third-party dependencies, and behaves the
// same on every platform. Input is treated as untrusted: parsing is
// bounds-checked and allocation is capped.
package videoprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gomcpgo/govideo/internal/container/avi"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/internal/container/matroska"
	"github.com/gomcpgo/govideo/internal/container/wav"
)

// ErrUnknownFormat reports a file whose container format is not recognised.
var ErrUnknownFormat = errors.New("videoprobe: unrecognised container format")

// Rational is a rational number (frame rates, aspect ratios, time bases).
type Rational struct{ Num, Den int64 }

// Float returns the value as a float64 (0 if Den is 0).
func (r Rational) Float() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Kind is the kind of a stream.
type Kind int

const (
	Video Kind = iota
	Audio
	Subtitle
	Data
)

func (k Kind) String() string {
	if k < Video || k > Data {
		return "data"
	}
	return [...]string{"video", "audio", "subtitle", "data"}[k]
}

// Info describes a media file.
type Info struct {
	Format     string // "mp4", "mov", "matroska", "webm", "wav", "avi"
	Duration   time.Duration
	StartTime  time.Duration
	BitRate    int64 // bits per second, overall
	Size       int64
	FastStart  bool // MP4/MOV: moov precedes mdat
	Fragmented bool // MP4: movie fragments present
	Tags       map[string]string
	Streams    []Stream
}

// Stream describes one track.
type Stream struct {
	Index     int
	ID        int64  // container track ID (MP4), track number (Matroska) or stream number (AVI)
	Kind      Kind   // Video, Audio, Subtitle, Data
	Codec     string // "h264", "hevc", "av1", "vp9", "aac", "opus", ...; "" if unknown
	CodecTag  string // "avc1", "hvc1", "mp4a", "V_VP9", ...
	Profile   string
	Level     string // e.g. "4.1"
	TimeBase  Rational
	StartTime time.Duration
	Duration  time.Duration
	BitRate   int64
	Frames    int64 // 0 if unknown
	Language  string
	Default   bool
	Video     *VideoInfo
	Audio     *AudioInfo

	level int // codec-native level number, as ffprobe prints it
}

// VideoInfo holds the video-specific fields of a stream.
type VideoInfo struct {
	Width, Height           int // display size after cropping
	CodedWidth, CodedHeight int
	SAR, DAR                Rational
	FrameRate, AvgFrameRate Rational
	VFR                     bool
	Rotation                int // degrees clockwise to apply for display: 0, 90, 180, 270
	BitDepth                int
	Chroma                  string // "4:2:0", "4:2:2", "4:4:4"
	Interlaced              bool
	ColorPrimaries          string
	ColorTransfer           string
	ColorMatrix             string
	ColorRange              string // "tv", "pc" or ""
	HDR                     string // "", "hlg", "pq"
	DolbyVision             *DolbyVisionInfo
}

// DolbyVisionInfo is the Dolby Vision configuration of a video stream.
type DolbyVisionInfo struct {
	Profile       int
	Level         int
	Compatibility int // bl_signal_compatibility_id: 1 = HDR10, 2 = SDR, 4 = HLG
	RPU, EL, BL   bool
	VersionMajor  int
	VersionMinor  int
}

// AudioInfo holds the audio-specific fields of a stream.
type AudioInfo struct {
	SampleRate, Channels, BitDepth int
	ChannelLayout                  string
}

// ProbeFile inspects the media file at path.
func ProbeFile(ctx context.Context, path string) (*Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return Probe(ctx, f, st.Size())
}

// Probe inspects a media file of the given size read through r.
func Probe(ctx context.Context, r io.ReaderAt, size int64) (*Info, error) {
	head := make([]byte, 64)
	n, err := r.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	head = head[:n]
	var info *Info
	switch {
	case matroska.Sniff(head):
		f, err := matroska.Open(ctx, r, size)
		if err != nil {
			return nil, err
		}
		info = fromMatroska(f)
	case wav.Sniff(head):
		f, err := wav.Open(ctx, r, size)
		if err != nil {
			return nil, err
		}
		info = fromWAV(f)
	case avi.Sniff(head):
		f, err := avi.Open(ctx, r, size)
		if err != nil {
			return nil, err
		}
		info = fromAVI(f)
	case isobmff.Sniff(head):
		f, err := isobmff.Open(ctx, r, size)
		if err != nil {
			return nil, err
		}
		info = fromISOBMFF(f)
	default:
		return nil, ErrUnknownFormat
	}
	info.Size = size
	if info.Duration > 0 {
		info.BitRate = int64(float64(size) * 8 / info.Duration.Seconds())
	}
	for i := range info.Streams {
		info.Streams[i].Index = i
	}
	return info, nil
}

// --- helpers ----------------------------------------------------------------

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func reduce(n, d int64) Rational {
	if d == 0 {
		return Rational{}
	}
	if g := gcd(n, d); g > 1 {
		n, d = n/g, d/g
	}
	return Rational{n, d}
}

// ticks converts n ticks of 1/scale seconds to a Duration without overflow.
func ticks(n int64, scale int64) time.Duration {
	if scale <= 0 {
		return 0
	}
	sec := n / scale
	rem := n % scale
	const maxSec = int64(1) << 33
	if sec > maxSec || sec < -maxSec {
		return 0
	}
	return time.Duration(sec)*time.Second + time.Duration(float64(rem)/float64(scale)*1e9+0.5*sign(rem))
}

func sign(v int64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// approxRate turns a measured frame rate into a tidy rational, preferring
// the standard integer and NTSC (x/1001) rates.
func approxRate(fps float64) Rational {
	if !(fps > 0) || fps > 1e6 {
		return Rational{}
	}
	if r := float64(int64(fps + 0.5)); r > 0 && abs(fps-r)/r < 0.0005 {
		return Rational{int64(r), 1}
	}
	if r := float64(int64(fps*1.001 + 0.5)); r > 0 && abs(fps*1.001-r)/r < 0.0005 {
		return Rational{int64(r) * 1000, 1001}
	}
	return reduce(int64(fps*1000+0.5), 1000)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func hdrKind(transfer int) string {
	switch transfer {
	case 16:
		return "pq"
	case 18:
		return "hlg"
	}
	return ""
}

func defaultLayout(ch int) string {
	switch ch {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return ""
}

func (v *VideoInfo) setAspect(sarN, sarD int64) {
	if sarN <= 0 || sarD <= 0 {
		sarN, sarD = 1, 1
	}
	v.SAR = reduce(sarN, sarD)
	if v.Width > 0 && v.Height > 0 {
		v.DAR = reduce(int64(v.Width)*v.SAR.Num, int64(v.Height)*v.SAR.Den)
	}
}

func levelString(codec string, level int) string {
	if level <= 0 && codec != "av1" || level < 0 {
		return ""
	}
	switch codec {
	case "h264":
		if level == 9 {
			return "1b"
		}
		return fmt.Sprintf("%d.%d", level/10, level%10)
	case "hevc":
		return fmt.Sprintf("%d.%d", level/30, level%30/3)
	case "av1":
		return fmt.Sprintf("%d.%d", 2+level>>2, level&3)
	case "vp9":
		return fmt.Sprintf("%d.%d", level/10, level%10)
	}
	return fmt.Sprint(level)
}
