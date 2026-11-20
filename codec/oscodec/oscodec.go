// Package oscodec reaches the codecs of the operating system: VideoToolbox
// and AudioToolbox on macOS (SPEC §6). It is how govideo handles the
// patent-pooled formats (H.264, HEVC, AAC) that it never implements itself.
// On platforms without a backend every constructor returns ErrUnsupported.
//
// The bindings use purego; there is no cgo.
package oscodec

import "errors"

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
