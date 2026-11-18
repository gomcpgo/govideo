// Package codec defines the frame types and the decoder and encoder
// interfaces shared by the native codecs (codec/native) and, later, the
// operating-system codec backends (codec/oscodec).
package codec

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
