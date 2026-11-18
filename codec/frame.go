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

// ChromaSize returns the dimensions of the chroma planes.
func (f *VideoFrame) ChromaSize() (w, h int) { return (f.Width + 1) / 2, (f.Height + 1) / 2 }

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
