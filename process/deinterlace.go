package process

import (
	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

// Field names one of the two fields of an interlaced frame.
type Field int

const (
	TopField    Field = iota // rows 0, 2, 4, ...
	BottomField              // rows 1, 3, 5, ...
)

// Bob returns one field of an interlaced frame as a picture of the frame's
// full height (SPEC §7.1). An interlaced frame holds two pictures taken half
// a frame apart, one on its even rows and one on its odd rows; shown whole,
// anything that moved between them is drawn twice, as a comb. Bob keeps the
// rows of the chosen field where they are and replaces the other field's
// rows with rows interpolated from the ones above and below (a four-tap
// cubic), so the two fields of a frame give two pictures that line up.
//
// The chroma planes are treated the same way: in interlaced 4:2:0 each
// field has its own chroma rows, and they sit where a progressive frame's
// would. f is not modified.
func Bob(f *codec.VideoFrame, field Field) *codec.VideoFrame {
	if f == nil {
		return nil
	}
	cw, ch := f.ChromaSize()
	out := &codec.VideoFrame{
		Width: f.Width, Height: f.Height,
		Y: make([]byte, f.Width*f.Height), Cb: make([]byte, cw*ch), Cr: make([]byte, cw*ch),
		YStride: f.Width, CStride: cw, Matrix: f.Matrix, FullRange: f.FullRange,
	}
	bobPlane(out.Y, out.YStride, f.Y, f.YStride, f.Width, f.Height, int(field))
	bobPlane(out.Cb, out.CStride, f.Cb, f.CStride, cw, ch, int(field))
	bobPlane(out.Cr, out.CStride, f.Cr, f.CStride, cw, ch, int(field))
	return out
}

// bobPlane copies the rows of one parity (0 even, 1 odd) and interpolates
// the others from them. Beyond the first and last rows of the field, the
// nearest one is repeated.
func bobPlane(dst []byte, dstride int, src []byte, sstride, w, h, parity int) {
	if h < 2 {
		parity = 0
	}
	// row returns the field row nearest to y, which has the field's parity.
	row := func(y int) []byte {
		if y < 0 {
			y = parity
		} else if y >= h {
			y = h - 1 - (h-1-parity)%2
		}
		return src[y*sstride : y*sstride+w]
	}
	for y := 0; y < h; y++ {
		d := dst[y*dstride : y*dstride+w]
		switch {
		case y%2 == parity:
			copy(d, row(y))
		case y == 0:
			copy(d, row(1)) // the edge of the picture: only one neighbour
		case y == h-1:
			copy(d, row(h-2))
		default:
			kernels.FieldMidRow(d, row(y-3), row(y-1), row(y+1), row(y+3))
		}
	}
}
