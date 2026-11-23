package codec

import "testing"

func TestReuse(t *testing.T) {
	var none *VideoFrame
	f := none.Reuse(7, 5)
	if f == nil || f.Width != 7 || f.Height != 5 || f.YStride != 7 || f.CStride != 4 ||
		len(f.Y) != 35 || len(f.Cb) != 12 || len(f.Cr) != 12 || f.Matrix != 1 || f.FullRange {
		t.Fatalf("Reuse of nil gave %+v", f)
	}
	// A smaller picture, and the first size again, fit in the same planes;
	// whatever the frame was marked as before is forgotten.
	f.Matrix, f.FullRange, f.YStride = 6, true, 64
	y := &f.Y[0]
	for _, size := range [][2]int{{4, 4}, {7, 5}, {1, 1}, {6, 5}} {
		g := f.Reuse(size[0], size[1])
		cw, ch := (size[0]+1)/2, (size[1]+1)/2
		if g != f || &g.Y[0] != y {
			t.Fatalf("%dx%d: the planes were not reused", size[0], size[1])
		}
		if g.Width != size[0] || g.Height != size[1] || g.YStride != size[0] || g.CStride != cw ||
			len(g.Y) != size[0]*size[1] || len(g.Cb) != cw*ch || len(g.Cr) != cw*ch || g.Matrix != 1 || g.FullRange {
			t.Fatalf("%dx%d: Reuse gave %dx%d, strides %d and %d, planes %d, %d, %d, matrix %d", size[0], size[1],
				g.Width, g.Height, g.YStride, g.CStride, len(g.Y), len(g.Cb), len(g.Cr), g.Matrix)
		}
	}
	// A larger one gets a frame of its own and leaves the old one alone.
	g := f.Reuse(8, 6)
	if g == f || len(g.Y) != 48 || len(g.Cb) != 12 || f.Width != 6 || len(f.Y) != 30 {
		t.Errorf("Reuse for a larger picture: new frame %v, planes %d and %d; old frame %dx%d", g != f, len(g.Y), len(g.Cb), f.Width, f.Height)
	}
	// Larger in chroma only: 7x5 has 4x3 chroma planes, 8x4 has 4x2 but more luma.
	h := none.Reuse(2, 16) // 32 luma, 8 chroma
	if k := h.Reuse(5, 5); k == h || len(k.Cb) != 9 {
		t.Errorf("a frame whose chroma planes are too small was reused")
	}
}
