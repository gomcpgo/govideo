package process

import (
	"bytes"
	"testing"

	"github.com/gomcpgo/govideo/codec"
)

// wovenFrame returns a frame whose rows are set by fn(plane, row), plane 0
// being luma.
func wovenFrame(w, h int, fn func(plane, y int) byte) *codec.VideoFrame {
	f := codec.NewVideoFrame(w, h)
	f.Matrix, f.FullRange = 6, true
	cw, ch := f.ChromaSize()
	fill := func(p []byte, stride, w, h, plane int) {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				p[y*stride+x] = fn(plane, y)
			}
		}
	}
	fill(f.Y, f.YStride, w, h, 0)
	fill(f.Cb, f.CStride, cw, ch, 1)
	fill(f.Cr, f.CStride, cw, ch, 2)
	return f
}

func TestBob(t *testing.T) {
	// Two flat pictures woven together: each field comes back flat, in all
	// three planes, whatever the size.
	for _, size := range [][2]int{{16, 16}, {17, 9}, {6, 5}, {2, 2}, {3, 1}, {1, 1}, {4, 3}} {
		w, h := size[0], size[1]
		src := wovenFrame(w, h, func(plane, y int) byte { return byte(40 + 50*plane + 100*(y%2)) })
		keep := *src
		keep.Y, keep.Cb, keep.Cr = bytes.Clone(src.Y), bytes.Clone(src.Cb), bytes.Clone(src.Cr)
		for _, field := range []Field{TopField, BottomField} {
			got := Bob(src, field)
			if got.Width != w || got.Height != h || got.Matrix != 6 || !got.FullRange {
				t.Fatalf("%dx%d: result is %dx%d matrix %d full %v", w, h, got.Width, got.Height, got.Matrix, got.FullRange)
			}
			cw, ch := got.ChromaSize()
			planes := []struct {
				p         []byte
				w, h, row int
			}{{got.Y, w, h, got.YStride}, {got.Cb, cw, ch, got.CStride}, {got.Cr, cw, ch, got.CStride}}
			for i, pl := range planes {
				parity := int(field)
				if pl.h < 2 {
					parity = 0 // a plane of one row has only the one
				}
				want := byte(40 + 50*i + 100*parity)
				for y := 0; y < pl.h; y++ {
					for x := 0; x < pl.w; x++ {
						if v := pl.p[y*pl.row+x]; v != want {
							t.Fatalf("%dx%d field %d plane %d: (%d,%d) = %d, want %d", w, h, field, i, x, y, v, want)
						}
					}
				}
			}
		}
		if !bytes.Equal(src.Y, keep.Y) || !bytes.Equal(src.Cb, keep.Cb) || !bytes.Equal(src.Cr, keep.Cr) {
			t.Errorf("%dx%d: the source frame was modified", w, h)
		}
	}
	if Bob(nil, TopField) != nil {
		t.Error("Bob(nil) is not nil")
	}
}

// TestBobInterpolation checks the rows that are filled in: exact on a
// ramp, and the nearest field row repeated at the edge of the picture.
// Next to the edge the outer tap has no row of its own and takes the
// nearest, which bends a ramp by a level.
func TestBobInterpolation(t *testing.T) {
	const w, h = 4, 12
	// Luma rises 10 per row; the bottom field is offset so that taking it
	// for the top one would show.
	src := wovenFrame(w, h, func(plane, y int) byte {
		if y%2 == 1 {
			return byte(200 - 10*y)
		}
		return byte(20 + 10*y)
	})
	top := Bob(src, TopField)
	for y := 0; y < h; y++ {
		want := byte(20 + 10*y)
		if y == h-1 {
			want = byte(20 + 10*(h-2)) // no row below: the one above again
		}
		tol := 0
		if y == 1 || y == h-3 {
			tol = 1
		}
		if got := top.Y[y*top.YStride]; int(got) < int(want)-tol || int(got) > int(want)+tol {
			t.Errorf("top field, row %d: %d, want %d", y, got, want)
		}
	}
	bottom := Bob(src, BottomField)
	for y := 0; y < h; y++ {
		want := byte(200 - 10*y)
		if y == 0 {
			want = 190
		}
		tol := 0
		if y == 2 || y == h-2 {
			tol = 1
		}
		if got := bottom.Y[y*bottom.YStride]; int(got) < int(want)-tol || int(got) > int(want)+tol {
			t.Errorf("bottom field, row %d: %d, want %d", y, got, want)
		}
	}
	// A step stays within range: the cubic's overshoot is clipped.
	step := wovenFrame(w, h, func(plane, y int) byte {
		if y < 6 {
			return 0
		}
		return 255
	})
	for y, want := range []byte{0, 0, 0, 0, 0, 0, 128, 255, 255, 255, 255, 255} {
		if got := Bob(step, BottomField).Y[y*w]; got != want {
			t.Errorf("step, row %d: %d, want %d", y, got, want)
		}
	}
}
