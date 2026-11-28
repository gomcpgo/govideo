package codec

// VideoFrame is a planar YUV 4:2:0, 8-bit picture: the pipeline's working
// format (SPEC §7.1). Chroma planes are (Width+1)/2 by (Height+1)/2.
type VideoFrame struct {
	Width, Height int
	Y, Cb, Cr     []byte
	YStride       int
	CStride       int
	// Matrix is the H.273 matrix coefficient code (1 = BT.709, 5/6 = BT.601).
	Matrix int
	// FullRange reports full-range (0..255) rather than limited-range samples.
	FullRange bool
}

// NewVideoFrame allocates a BT.709 limited-range frame filled with black.
func NewVideoFrame(w, h int) *VideoFrame {
	cw, ch := (w+1)/2, (h+1)/2
	f := &VideoFrame{
		Width: w, Height: h,
		Y: make([]byte, w*h), Cb: make([]byte, cw*ch), Cr: make([]byte, cw*ch),
		YStride: w, CStride: cw, Matrix: 1,
	}
	for i := range f.Y {
		f.Y[i] = 16
	}
	for i := range f.Cb {
		f.Cb[i] = 128
		f.Cr[i] = 128
	}
	return f
}

// Reuse returns a w×h frame for a decoder or filter to write a picture into:
// f itself, resized, when its planes are large enough, else a new frame (f
// may be nil). The planes are tightly packed and their contents are
// unspecified; the frame is marked BT.709 limited range. Passing the frame of
// the previous picture each time keeps a steady stream of pictures of one
// size from allocating.
func (f *VideoFrame) Reuse(w, h int) *VideoFrame {
	cw, ch := (w+1)/2, (h+1)/2
	if f == nil || cap(f.Y) < w*h || cap(f.Cb) < cw*ch || cap(f.Cr) < cw*ch {
		f = &VideoFrame{Y: make([]byte, w*h), Cb: make([]byte, cw*ch), Cr: make([]byte, cw*ch)}
	}
	f.Width, f.Height, f.YStride, f.CStride = w, h, w, cw
	f.Y, f.Cb, f.Cr = f.Y[:w*h], f.Cb[:cw*ch], f.Cr[:cw*ch]
	f.Matrix, f.FullRange = 1, false
	return f
}

// ChromaSize returns the dimensions of the chroma planes.
func (f *VideoFrame) ChromaSize() (w, h int) { return (f.Width + 1) / 2, (f.Height + 1) / 2 }

// DeepFrame is a planar YUV 4:2:0 picture with samples of more than 8 bits,
// as a decoder gives it for a 10-bit stream. It is not a working format:
// the pipeline has it only between the decoder and the step that brings a
// high dynamic range picture to the working format (SPEC §7.1), which needs
// the bits a VideoFrame has dropped.
//
// A sample fills its 16 bits from the top, as P010 stores it: a 10-bit value
// v is held as v<<6. Black and white of a limited-range picture are then
// 4096 and 60160 whatever the depth, and a chroma sample of no colour is
// 32768.
type DeepFrame struct {
	Width, Height int
	Y, Cb, Cr     []uint16
	YStride       int
	CStride       int
	// Depth is the number of bits of a sample that mean something.
	Depth int
	// Matrix is the H.273 matrix coefficient code (9 = BT.2020).
	Matrix int
	// FullRange reports full-range rather than limited-range samples.
	FullRange bool
}

// Reuse returns a w×h deep frame to write a picture into, as
// VideoFrame.Reuse does: f itself when its planes are large enough, else a
// new one (f may be nil). The contents are unspecified and the tags are
// those of 10-bit BT.2020, limited range.
func (f *DeepFrame) Reuse(w, h int) *DeepFrame {
	cw, ch := (w+1)/2, (h+1)/2
	if f == nil || cap(f.Y) < w*h || cap(f.Cb) < cw*ch || cap(f.Cr) < cw*ch {
		f = &DeepFrame{Y: make([]uint16, w*h), Cb: make([]uint16, cw*ch), Cr: make([]uint16, cw*ch)}
	}
	f.Width, f.Height, f.YStride, f.CStride = w, h, w, cw
	f.Y, f.Cb, f.Cr = f.Y[:w*h], f.Cb[:cw*ch], f.Cr[:cw*ch]
	f.Depth, f.Matrix, f.FullRange = 10, 9, false
	return f
}

// AudioFrame is a block of interleaved float32 PCM in the range [-1, 1].
type AudioFrame struct {
	SampleRate int
	Channels   int
	Samples    []float32 // len = frames * Channels
}

// Frames returns the number of sample frames in the block.
func (a *AudioFrame) Frames() int {
	if a.Channels == 0 {
		return 0
	}
	return len(a.Samples) / a.Channels
}
