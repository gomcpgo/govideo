package native

import (
	"bytes"
	"image"
	"image/jpeg"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
	"github.com/gomcpgo/govideo/process"
)

// MJPEGDecoder decodes Motion JPEG: each packet is one baseline or
// progressive JPEG image (ITU-T T.81), decoded with the Go standard library.
// Frames without Huffman tables, as AVI files carry them, get the standard
// ones.
type MJPEGDecoder struct{}

// Decode returns the picture as a BT.709 limited-range 4:2:0 frame.
func (MJPEGDecoder) Decode(p []byte) (*codec.VideoFrame, error) {
	img, err := jpeg.Decode(bytes.NewReader(withHuffmanTables(p)))
	if err != nil {
		return nil, err
	}
	return process.FromImage(img), nil
}

// MJPEGEncoder encodes frames as JPEG images. It is the video codec of the
// intermediate export profile.
type MJPEGEncoder struct {
	Quality int // 1..100; 0 selects 90
	conv    *kernels.YUVConv
	buf     bytes.Buffer
}

// Encode compresses one frame. JPEG carries full-range BT.601 Y'CbCr, so the
// BT.709 limited-range working format is converted first.
func (e *MJPEGEncoder) Encode(f *codec.VideoFrame) ([]byte, error) {
	f = process.ConvertTo709Limited(f)
	if e.conv == nil {
		c := kernels.NewYUVConv(0.2126, 0.0722, false, 0.299, 0.114, true)
		e.conv = &c
	}
	cw, ch := f.ChromaSize()
	img := image.NewYCbCr(image.Rect(0, 0, f.Width, f.Height), image.YCbCrSubsampleRatio420)
	for y := 0; y < f.Height; y++ {
		c := (y / 2) * f.CStride
		kernels.ConvertLumaRow(img.Y[y*img.YStride:], f.Y[y*f.YStride:], f.Cb[c:], f.Cr[c:], f.Width, e.conv)
	}
	for y := 0; y < ch; y++ {
		kernels.ConvertChromaRow(img.Cb[y*img.CStride:], img.Cr[y*img.CStride:], f.Cb[y*f.CStride:], f.Cr[y*f.CStride:], cw, e.conv)
	}
	q := e.Quality
	if q == 0 {
		q = 90
	}
	e.buf.Reset()
	if err := jpeg.Encode(&e.buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return bytes.Clone(e.buf.Bytes()), nil
}
