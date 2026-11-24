package process

import (
	"image/color"
	"math"
	"math/rand"
	"testing"

	"github.com/gomcpgo/govideo/codec"
)

// flat returns a w×h frame of one Y'CbCr value.
func flat(w, h int, y, cb, cr byte) *codec.VideoFrame {
	f := codec.NewVideoFrame(w, h)
	fillFrame(f, y, cb, cr)
	return f
}

func TestBlendInto(t *testing.T) {
	a, b := randomFrame(34, 21, 1), randomFrame(34, 21, 2)
	if got := BlendInto(nil, a, b, 0); !sameFrame(got, a) {
		t.Error("weight 0 is not the first picture")
	}
	if got := BlendInto(nil, a, b, 1); !sameFrame(got, b) {
		t.Error("weight 1 is not the second picture")
	}
	// Out-of-range and undefined weights are clamped, not wrapped.
	if got := BlendInto(nil, a, b, -3); !sameFrame(got, a) {
		t.Error("a negative weight is not the first picture")
	}
	if got := BlendInto(nil, a, b, math.NaN()); !sameFrame(got, a) {
		t.Error("an undefined weight is not the first picture")
	}
	if got := BlendInto(nil, a, b, 7); !sameFrame(got, b) {
		t.Error("a weight above 1 is not the second picture")
	}
	for _, w := range []float64{0.1, 0.25, 0.5, 0.9} {
		got := BlendInto(nil, a, b, w)
		for _, pl := range [][3][]byte{{got.Y, a.Y, b.Y}, {got.Cb, a.Cb, b.Cb}, {got.Cr, a.Cr, b.Cr}} {
			for i, v := range pl[0] {
				want := float64(pl[1][i])*(1-w) + float64(pl[2][i])*w
				if math.Abs(float64(v)-want) > 0.51 {
					t.Fatalf("weight %v: sample %d is %d, want %.2f", w, i, v, want)
				}
			}
		}
		// A picture mixed with itself does not change.
		if self := BlendInto(nil, a, a, w); !sameFrame(self, a) {
			t.Errorf("weight %v: a picture mixed with itself changed", w)
		}
	}
	// The destination may be one of the inputs, and is reused.
	want := BlendInto(nil, a, b, 0.3)
	into := codec.NewVideoFrame(34, 21)
	if got := BlendInto(into, a, b, 0.3); got != into || !sameFrame(got, want) {
		t.Error("the destination frame was not reused, or differs")
	}
	ac := *a
	ac.Y, ac.Cb, ac.Cr = append([]byte(nil), a.Y...), append([]byte(nil), a.Cb...), append([]byte(nil), a.Cr...)
	if got := BlendInto(&ac, &ac, b, 0.3); got != &ac || !sameFrame(got, want) {
		t.Error("mixing in place differs")
	}
	// Strides wider than the picture.
	wide := &codec.VideoFrame{Width: 34, Height: 21, YStride: 40, CStride: 23, Matrix: 1,
		Y: make([]byte, 40*21), Cb: make([]byte, 23*11), Cr: make([]byte, 23*11)}
	for y := 0; y < 21; y++ {
		copy(wide.Y[y*40:], b.Y[y*34:(y+1)*34])
	}
	for y := 0; y < 11; y++ {
		copy(wide.Cb[y*23:], b.Cb[y*17:(y+1)*17])
		copy(wide.Cr[y*23:], b.Cr[y*17:(y+1)*17])
	}
	if got := BlendInto(nil, a, wide, 0.3); !sameFrame(got, want) {
		t.Error("a second picture with padded rows gives a different mix")
	}
	if got := BlendInto(nil, wide, a, 0.7); !sameFrame(got, BlendInto(nil, b, a, 0.7)) {
		t.Error("a first picture with padded rows gives a different mix")
	}
	// Other sizes cannot be mixed.
	if got := BlendInto(nil, a, randomFrame(30, 21, 3), 0.5); got != a {
		t.Error("pictures of different sizes: want the first back")
	}
	// A BT.601 picture is converted before it is mixed.
	c601 := FromImage(solid(16, 16, color.NRGBA{255, 0, 0, 255}))
	conv := *c601
	conv.Matrix = 6
	red := ConvertTo709Limited(&conv)
	got := BlendInto(nil, &conv, flat(16, 16, 16, 128, 128), 0)
	if !sameFrame(got, red) || got.Matrix != 1 {
		t.Error("a BT.601 input was mixed without conversion")
	}
}

func TestFadeInto(t *testing.T) {
	blue := color.NRGBA{0, 0, 255, 255}
	f := randomFrame(34, 21, 4)
	if got := FadeInto(nil, f, blue, 1); !sameFrame(got, f) {
		t.Error("full opacity changed the picture")
	}
	checkSolid(t, "opacity 0", FadeInto(nil, f, blue, 0), 32, 240, 118, 0)
	checkSolid(t, "opacity 0 over black", FadeInto(nil, f, nil, 0), 16, 128, 128, 0)
	// White over blue at a quarter: a quarter of the way from the colour.
	white := flat(34, 21, 235, 128, 128)
	checkSolid(t, "opacity 0.25", FadeInto(nil, white, blue, 0.25), 83, 212, 121, 1)
	// It is the mix with a frame of the colour.
	bg := flat(34, 21, 32, 240, 118)
	for _, g := range []float64{0.2, 0.5, 0.8} {
		if got, want := FadeInto(nil, f, blue, g), BlendInto(nil, bg, f, g); !sameFrame(got, want) {
			t.Errorf("opacity %v differs from the mix with a frame of the colour", g)
		}
	}
	// In place.
	want := FadeInto(nil, f, blue, 0.4)
	if got := FadeInto(f, f, blue, 0.4); got != f || !sameFrame(got, want) {
		t.Error("fading in place differs")
	}
}

func TestFitSize(t *testing.T) {
	for _, c := range []struct {
		sw, sh int
		mode   FitMode
		w, h   float64
	}{
		{1920, 1080, Contain, 640, 360},
		{1000, 1000, Contain, 360, 360},
		{1000, 1000, Cover, 640, 640},
		{1000, 1000, Stretch, 640, 360},
		{300, 100, Contain, 640, 640.0 / 3},
		{300, 100, Cover, 1080, 360},
		{0, 100, Contain, 0, 0},
	} {
		w, h := FitSize(c.sw, c.sh, 640, 360, c.mode)
		if math.Abs(w-c.w) > 1e-9 || math.Abs(h-c.h) > 1e-9 {
			t.Errorf("FitSize(%dx%d, mode %d) = %vx%v, want %vx%v", c.sw, c.sh, c.mode, w, h, c.w, c.h)
		}
	}
}

func TestPlace(t *testing.T) {
	var p Placer
	src := randomFrame(96, 54, 5)

	// Over the whole canvas it is Scale.
	for _, size := range [][2]int{{96, 54}, {64, 36}, {160, 90}, {250, 30}} {
		w, h := size[0], size[1]
		got := p.Place(nil, src, w, h, 0, 0, float64(w), float64(h), nil)
		if !sameFrame(got, Scale(src, w, h)) {
			t.Errorf("placed over a %dx%d canvas: differs from Scale", w, h)
		}
	}

	// At its own size on whole pixels it is a copy, with the background round it.
	red := color.NRGBA{255, 0, 0, 255}
	got := p.Place(nil, src, 160, 90, 20, 10, 96, 54, red)
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			want := byte(63)
			if x >= 20 && x < 116 && y >= 10 && y < 64 {
				want = src.Y[(y-10)*src.YStride+x-20]
			}
			if v := got.Y[y*got.YStride+x]; v != want {
				t.Fatalf("copy: Y(%d,%d) = %d, want %d", x, y, v, want)
			}
		}
	}
	for y := 0; y < 45; y++ {
		for x := 0; x < 80; x++ {
			wcb, wcr := byte(102), byte(240)
			if x >= 10 && x < 58 && y >= 5 && y < 32 {
				wcb, wcr = src.Cb[(y-5)*src.CStride+x-10], src.Cr[(y-5)*src.CStride+x-10]
			}
			if cb, cr := got.Cb[y*got.CStride+x], got.Cr[y*got.CStride+x]; cb != wcb || cr != wcr {
				t.Fatalf("copy: CbCr(%d,%d) = %d,%d, want %d,%d", x, y, cb, cr, wcb, wcr)
			}
		}
	}

	// An edge inside a pixel mixes that pixel with the background by the
	// part covered.
	white := flat(40, 20, 235, 128, 128)
	got = p.Place(nil, white, 100, 60, 10.25, 20.5, 40, 20, nil)
	row := func(y int) []byte { return got.Y[y*got.YStride : y*got.YStride+100] }
	for _, c := range []struct {
		x, y int
		want float64 // coverage
	}{
		{9, 30, 0}, {10, 30, 0.75}, {11, 30, 1}, {49, 30, 1}, {50, 30, 0.25}, {51, 30, 0},
		{30, 19, 0}, {30, 20, 0.5}, {30, 21, 1}, {30, 39, 1}, {30, 40, 0.5}, {30, 41, 0},
		{10, 20, 0.375}, {50, 40, 0.125},
	} {
		want := 16 + 219*c.want
		if v := float64(row(c.y)[c.x]); math.Abs(v-want) > 1 {
			t.Errorf("edge: Y(%d,%d) = %v, want %.1f (coverage %v)", c.x, c.y, v, want, c.want)
		}
	}

	// A quarter of a pixel at a time, a ramp moves a quarter of its slope
	// at a time: the motion is not rounded to whole pixels.
	ramp := codec.NewVideoFrame(64, 16)
	for y := 0; y < 16; y++ {
		for x := 0; x < 64; x++ {
			ramp.Y[y*64+x] = byte(3 * x)
		}
	}
	var last float64
	for i := 0; i <= 12; i++ {
		got = p.Place(got, ramp, 64, 16, -float64(i)/4, 0, 64, 16, nil)
		v := float64(got.Y[8*64+30])
		if want := 3 * (30 + float64(i)/4); math.Abs(v-want) > 1 {
			t.Errorf("shifted by %v: Y = %v, want %.2f", float64(i)/4, v, want)
		}
		if i > 0 && (v < last || v > last+2) {
			t.Errorf("shifted by %v: Y went from %v to %v", float64(i)/4, last, v)
		}
		last = v
	}

	// Magnified about the centre, the middle of the picture stays put.
	quad := codec.NewVideoFrame(64, 64)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x < 32) != (y < 32) {
				quad.Y[y*64+x] = 235
			}
		}
	}
	for _, s := range []float64{1, 1.3, 2, 3.7} {
		w := 64 * s
		got = p.Place(got, quad, 64, 64, 32-w/2, 32-w/2, w, w, nil)
		for _, c := range [][3]int{{16, 16, 16}, {48, 16, 235}, {16, 48, 235}, {48, 48, 16}} {
			if v := got.Y[c[1]*64+c[0]]; !near(v, byte(c[2]), 1) {
				t.Errorf("scale %v: Y(%d,%d) = %d, want %d", s, c[0], c[1], v, c[2])
			}
		}
	}

	// Out of sight, or of no size, leaves the background.
	for _, r := range [][4]float64{{-50, 0, 40, 40}, {0, 70, 40, 40}, {200, 200, 5, 5}, {10, 10, 0, 20}, {10, 10, 20, -3},
		{math.NaN(), 0, 10, 10}, {0, 0, math.Inf(1), 10}, {0, math.Inf(-1), 10, 10}} {
		checkSolid(t, "out of sight", p.Place(got, src, 100, 60, r[0], r[1], r[2], r[3], red), 63, 102, 240, 0)
	}
	checkSolid(t, "no picture", p.Place(nil, nil, 100, 60, 0, 0, 100, 60, red), 63, 102, 240, 0)
	if f := p.Place(nil, src, 0, 0, 0, 0, 10, 10, nil); f.Width != 0 || f.Height != 0 {
		t.Error("an empty canvas is not empty")
	}
}

// TestPlaceAnywhere draws pictures at arbitrary positions and sizes: every
// sample must lie between the darkest of the picture and background and the
// lightest (the filter overshoots by a little), and nothing may be read or
// written out of bounds.
func TestPlaceAnywhere(t *testing.T) {
	var p Placer
	rng := rand.New(rand.NewSource(6))
	var into *codec.VideoFrame
	for i := 0; i < 400; i++ {
		sw, sh := 1+rng.Intn(70), 1+rng.Intn(70)
		src := flat(sw, sh, 100, 90, 160)
		cw, ch := 1+rng.Intn(90), 1+rng.Intn(90)
		w, h := math.Exp(rng.Float64()*9-2), math.Exp(rng.Float64()*9-2)
		x, y := (rng.Float64()*3-1.5)*float64(cw)-w/2, (rng.Float64()*3-1.5)*float64(ch)-h/2
		into = p.Place(into, src, cw, ch, x, y, w, h, color.NRGBA{255, 255, 255, 255})
		if into.Width != cw || into.Height != ch {
			t.Fatalf("canvas %dx%d: got a %dx%d frame", cw, ch, into.Width, into.Height)
		}
		for j, v := range into.Y {
			if v < 100 || v > 235 {
				t.Fatalf("case %d (%dx%d at %.2f,%.2f size %.2fx%.2f on %dx%d): Y[%d] = %d", i, sw, sh, x, y, w, h, cw, ch, j, v)
			}
		}
		for j := range into.Cb {
			if cb, cr := into.Cb[j], into.Cr[j]; cb < 90 || cb > 128 || cr < 128 || cr > 160 {
				t.Fatalf("case %d: CbCr[%d] = %d,%d", i, j, cb, cr)
			}
		}
	}
}

func BenchmarkPlace(b *testing.B) {
	src := randomFrame(2304, 1296, 7) // a still reduced for a zoom to 1.2 on 1080p
	var p Placer
	var into *codec.VideoFrame
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		s := 1 + 0.2*float64(i%100)/100
		w, h := 1920*s, 1080*s
		into = p.Place(into, src, 1920, 1080, 960-w/2, 540-h/2, w, h, nil)
	}
}

func BenchmarkBlend(b *testing.B) {
	x, y := randomFrame(1920, 1080, 8), randomFrame(1920, 1080, 9)
	var into *codec.VideoFrame
	b.Run("dissolve", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			into = BlendInto(into, x, y, float64(i%30)/30)
		}
	})
	b.Run("fade", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			into = FadeInto(into, x, nil, float64(i%30)/30)
		}
	})
}
