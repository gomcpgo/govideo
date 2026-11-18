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
type EncodeConfig struct {
	Width, Height      int
	FrameNum, FrameDen int64 // frame rate
	Bitrate            int   // average bits per second
	KeyInterval        int   // maximum frames between keyframes
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
