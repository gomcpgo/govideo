//go:build !darwin

package oscodec

import "github.com/gomcpgo/govideo/codec"

// Load reports whether the operating-system codec frameworks are usable.
func Load() error { return ErrUnsupported }

// NewVideoDecoder returns a decoder for an H.264 or HEVC stream.
func NewVideoDecoder(VideoConfig) (codec.VideoStreamDecoder, error) { return nil, ErrUnsupported }

// NewH264Encoder returns an H.264 High profile encoder.
func NewH264Encoder(EncodeConfig) (codec.VideoStreamEncoder, error) { return nil, ErrUnsupported }

// NewAudioDecoder returns a decoder for an AAC stream.
func NewAudioDecoder(AudioConfig) (codec.AudioDecoder, error) { return nil, ErrUnsupported }

// NewAACEncoder returns an AAC-LC encoder.
func NewAACEncoder(AudioEncodeConfig) (codec.AudioStreamEncoder, error) { return nil, ErrUnsupported }
