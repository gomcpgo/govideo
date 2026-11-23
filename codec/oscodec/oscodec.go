// Package oscodec reaches the codecs of the operating system: VideoToolbox
// and AudioToolbox on macOS (SPEC §6). It is how govideo handles the
// patent-pooled formats (H.264, HEVC, AAC) that it never implements itself.
// On platforms without a backend every constructor returns ErrUnsupported.
//
// The bindings use purego; there is no cgo.
package oscodec

import (
	"errors"
	"sync/atomic"
)

// ErrUnsupported reports that this platform has no operating-system codec
// backend, or that the backend lacks the requested codec.
var ErrUnsupported = errors.New("oscodec: no operating-system codec for this format on this platform")

// ErrNoQualityMode reports that the H.264 encoder of this machine has no
// quality-targeted rate control (the software encoder used where there is
// no hardware one, for example). Ask for a bit rate instead.
var ErrNoQualityMode = errors.New("oscodec: the H.264 encoder has no quality-targeted mode")

// VideoConfig describes a compressed video stream to decode.
type VideoConfig struct {
	Codec         string // "h264" or "hevc"
	Width, Height int    // coded picture size from the container
	// Record is the decoder configuration record from the container: avcC
	// for H.264, hvcC for HEVC.
	Record []byte
	// Matrix is the H.273 matrix coefficients code signalled by the stream
	// or container (1 = BT.709, 5 or 6 = BT.601, 9 = BT.2020). 0 leaves it
	// to the decoder, which may not know either.
	Matrix int
}

// Restarter is a video decoder that can go on to decode another stream
// without being closed and opened again, which spares setting up a decoder
// session for every clip of a timeline. The decoders NewVideoDecoder returns
// implement it where the platform has a backend.
type Restarter interface {
	// Restart makes the decoder ready for the stream c describes, as a newly
	// opened decoder would be, and reports whether it could. The pictures of
	// the stream before are released. On false the decoder is as it was.
	Restart(c VideoConfig) bool
}

// DecoderStats counts what the video decoders of this process have done. It
// is for tests and diagnostics.
type DecoderStats struct {
	Sessions int64 // decoder sessions opened
	Reused   int64 // streams decoded on the session of an earlier stream
	Pictures int64 // decoded pictures handed out and not yet released
}

var stats struct{ sessions, reused, pictures atomic.Int64 }

// VideoDecoderStats returns the counts so far.
func VideoDecoderStats() DecoderStats {
	return DecoderStats{Sessions: stats.sessions.Load(), Reused: stats.reused.Load(), Pictures: stats.pictures.Load()}
}

// EncodeConfig describes an H.264 encode.
//
// Rate control is chosen by Quality and Bitrate. Quality alone asks for a
// constant picture quality and lets the size follow the content. Bitrate
// alone asks for a long-term average rate. MaxBitrate may be added to either
// as a ceiling.
type EncodeConfig struct {
	Width, Height      int
	FrameNum, FrameDen int64 // frame rate
	// Quality, above 0 and up to 1, is the quality the encoder holds; 0
	// leaves rate control to Bitrate.
	Quality float64
	Bitrate int // average bits per second; 0 = no target
	// MaxBitrate is a hard limit on the bits in any one second; 0 = none.
	MaxBitrate  int
	KeyInterval int // maximum frames between keyframes
	// Reorder lets the encoder use B-frames. Samples then come out in decode
	// order, which differs from presentation order: EncodedFrame.Index tells
	// them apart, and the container needs composition offsets.
	Reorder bool
}

// AudioConfig describes a compressed audio stream to decode.
type AudioConfig struct {
	Codec      string // "aac"
	SampleRate int    // output rate (the extension rate for HE-AAC)
	Channels   int
	// Config is the AudioSpecificConfig.
	Config []byte
}

// AudioEncodeConfig describes an AAC-LC encode.
type AudioEncodeConfig struct {
	SampleRate, Channels int
	Bitrate              int // bits per second
}
