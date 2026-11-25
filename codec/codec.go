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

// Picture is a decoded picture that is still in its decoder's memory. A
// stream decoder hands out pictures rather than frames so that a picture
// nobody looks at costs no copy: one decoded only because later pictures
// refer to it (everything between a sync sample and a cut), or one that a
// lower output frame rate passes over.
type Picture interface {
	// Frame copies the picture into a frame and returns it. The planes of
	// into are reused when they are large enough (see Reuse), so a caller
	// that passes back the frame it got last time does not allocate; into
	// may be nil. The frame returned shares no memory with the decoder.
	Frame(into *VideoFrame) (*VideoFrame, error)
	// Release gives the picture back to the decoder. It is called once the
	// picture is no longer needed, and the picture is not used afterwards.
	// Closing the decoder releases the pictures still out.
	Release()
}

// TimedPicture is a decoded picture with its presentation time.
type TimedPicture struct {
	Picture Picture
	PTS     time.Duration
}

// VideoStreamDecoder decodes inter-frame video. Packets are sent in decode
// order; pictures come back in decode order too, each tagged with the
// presentation time of its packet, so the caller reorders them. The caller
// releases every picture it is given.
type VideoStreamDecoder interface {
	// Decode returns the pictures the packet completes. The slice is only
	// valid until the next call to Decode or Flush.
	Decode(packet []byte, pts time.Duration) ([]TimedPicture, error)
	// Flush returns the pictures still held by the decoder at end of stream.
	Flush() ([]TimedPicture, error)
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
	// Bitrate is the bit rate the encoder aims at, in bits per second, which
	// need not be the one it was asked for; 0 if it is not known.
	Bitrate() int
	Close() error
}
