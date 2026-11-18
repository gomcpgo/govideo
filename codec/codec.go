// Package codec defines the frame types and the decoder and encoder
// interfaces shared by the native codecs (codec/native) and, later, the
// operating-system codec backends (codec/oscodec).
package codec

import "time"

// VideoDecoder turns compressed packets into frames in presentation order.
// A decoder may return nil when a packet produces no frame yet.
type VideoDecoder interface {
	Decode(packet []byte) (*VideoFrame, error)
}

// AudioDecoder turns compressed packets into PCM blocks.
type AudioDecoder interface {
	Decode(packet []byte) (*AudioFrame, error)
}

// VideoEncoder compresses frames; every returned packet is one sample.
type VideoEncoder interface {
	Encode(f *VideoFrame) ([]byte, error)
}

// TimedFrame is a decoded picture with its presentation time.
type TimedFrame struct {
	Frame *VideoFrame
	PTS   time.Duration
}

// VideoStreamDecoder decodes inter-frame video. Packets are sent in decode
// order; frames come back in decode order too, each tagged with the
// presentation time of its packet, so the caller reorders them.
type VideoStreamDecoder interface {
	Decode(packet []byte, pts time.Duration) ([]TimedFrame, error)
	// Flush returns the frames still held by the decoder at end of stream.
	Flush() ([]TimedFrame, error)
	Close() error
}

// EncodedFrame is one compressed video sample. Index is the number of the
// input frame it carries, counting from 0.
type EncodedFrame struct {
	Data  []byte
	Index int64
	Sync  bool
}

// VideoStreamEncoder compresses frames with an inter-frame codec. Output may
// lag the input; samples are returned in decode order.
type VideoStreamEncoder interface {
	Encode(f *VideoFrame) ([]EncodedFrame, error)
	// Flush ends the stream and returns the remaining samples.
	Flush() ([]EncodedFrame, error)
	// Config returns the decoder configuration record (avcC for H.264). It is
	// available once the first sample has been returned.
	Config() []byte
	Close() error
}

// AudioStreamEncoder compresses interleaved float32 PCM into packets of a
// fixed number of sample frames.
type AudioStreamEncoder interface {
	Encode(samples []float32) ([][]byte, error)
	// Flush ends the stream and returns the remaining packets.
	Flush() ([][]byte, error)
	// Config returns the decoder configuration (AudioSpecificConfig for AAC).
	Config() []byte
	// FrameSize is the number of sample frames per packet; Priming is the
	// number of leading sample frames a player must discard.
	FrameSize() int
	Priming() int
	Close() error
}
