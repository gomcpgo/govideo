package process

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"

	"github.com/gomcpgo/govideo/codec"
)

func near(a, b byte, tol int) bool {
	d := int(a) - int(b)
	return d >= -tol && d <= tol
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

var knownColours = []struct {
	name      string
	c         color.NRGBA
	y, cb, cr byte
}{
	{"white", color.NRGBA{255, 255, 255, 255}, 235, 128, 128},
	{"black", color.NRGBA{0, 0, 0, 255}, 16, 128, 128},
	{"red", color.NRGBA{255, 0, 0, 255}, 63, 102, 240},
	{"green", color.NRGBA{0, 255, 0, 255}, 173, 42, 26},
	{"blue", color.NRGBA{0, 0, 255, 255}, 32, 240, 118},
	{"grey", color.NRGBA{128, 128, 128, 255}, 126, 128, 128},
	{"transparent red", color.NRGBA{255, 0, 0, 0}, 16, 128, 128},
}

func checkSolid(t *testing.T, name string, f *codec.VideoFrame, y, cb, cr byte, tol int) {
	t.Helper()
	if f.Matrix != 1 || f.FullRange {
		t.Errorf("%s: frame tagged matrix=%d full=%v, want 709 limited", name, f.Matrix, f.FullRange)
	}
	for i, v := range f.Y {
		if !near(v, y, tol) {
			t.Fatalf("%s: Y[%d]=%d want %d", name, i, v, y)
		}
	}
	for i := range f.Cb {
		if !near(f.Cb[i], cb, tol) || !near(f.Cr[i], cr, tol) {
			t.Fatalf("%s: CbCr[%d]=%d,%d want %d,%d", name, i, f.Cb[i], f.Cr[i], cb, cr)
		}
	}
}

func TestFromImageKnownColours(t *testing.T) {
	for _, k := range knownColours {
		src := solid(5, 3, k.c)
		checkSolid(t, k.name+"/nrgba", FromImage(src), k.y, k.cb, k.cr, 1)

		rgba := image.NewRGBA(src.Bounds())
		pal := image.NewPaletted(src.Bounds(), color.Palette{color.Black, k.c})
		var generic image.Image = image.NewNRGBA64(src.Bounds())
		for y := 0; y < 3; y++ {
			for x := 0; x < 5; x++ {
				rgba.Set(x, y, k.c)
				pal.SetColorIndex(x, y, 1)
				generic.(*image.NRGBA64).Set(x, y, k.c)
			}
		}
		checkSolid(t, k.name+"/rgba", FromImage(rgba), k.y, k.cb, k.cr, 1)
		checkSolid(t, k.name+"/generic", FromImage(generic), k.y, k.cb, k.cr, 1)
		if k.c.A == 255 {
			checkSolid(t, k.name+"/paletted", FromImage(pal), k.y, k.cb, k.cr, 1)
		}
	}
}

func TestFromImageYCbCrAndGray(t *testing.T) {
	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422,
		image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio440,
	}
	for _, k := range knownColours {
		if k.c.A != 255 {
			continue
		}
		yy, cb, cr := color.RGBToYCbCr(k.c.R, k.c.G, k.c.B)
		for _, ratio := range ratios {
			img := image.NewYCbCr(image.Rect(0, 0, 7, 5), ratio)
			for i := range img.Y {
				img.Y[i] = yy
			}
			for i := range img.Cb {
				img.Cb[i], img.Cr[i] = cb, cr
			}
			checkSolid(t, k.name+"/ycbcr", FromImage(img), k.y, k.cb, k.cr, 2)
			// A sub-image with odd origin exercises the offset maths.
			sub := img.SubImage(image.Rect(1, 1, 6, 4))
			f := FromImage(sub)
			if f.Width != 5 || f.Height != 3 {
				t.Fatalf("subimage size %dx%d", f.Width, f.Height)
			}
			checkSolid(t, k.name+"/ycbcr-sub", f, k.y, k.cb, k.cr, 2)
		}
	}
	g := image.NewGray(image.Rect(0, 0, 3, 3))
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	checkSolid(t, "gray", FromImage(g), 235, 128, 128, 0)
	if f := FromImage(image.NewRGBA(image.Rectangle{})); f.Width != 0 || f.Height != 0 {
		t.Fatal("empty image should give empty frame")
	}
}

func TestConvertTo709Limited(t *testing.T) {
	f := codec.NewVideoFrame(4, 4)
	if ConvertTo709Limited(f) != f {
		t.Fatal("709 limited frame should be returned unchanged")
	}
	// Unspecified matrix, HD height, limited: retagged without conversion.
	hd := codec.NewVideoFrame(2, 720)
	hd.Matrix = 2
	if g := ConvertTo709Limited(hd); g.Matrix != 1 || &g.Y[0] != &hd.Y[0] {
		t.Fatal("unspecified HD frame should be retagged, sharing planes")
	}
	// Full-range 709 white/black → 235/16.
	full := codec.NewVideoFrame(4, 4)
	full.FullRange = true
	for i := range full.Y {
		full.Y[i] = 255
	}
	checkSolid(t, "full white", ConvertTo709Limited(full), 235, 128, 128, 0)
	for i := range full.Y {
		full.Y[i] = 0
	}
	checkSolid(t, "full black", ConvertTo709Limited(full), 16, 128, 128, 0)
	// BT.601 limited red (81, 90, 240) → BT.709 limited red.
	sd := codec.NewVideoFrame(4, 4)
	sd.Matrix = 6
	for i := range sd.Y {
		sd.Y[i] = 81
	}
	for i := range sd.Cb {
		sd.Cb[i], sd.Cr[i] = 90, 240
	}
	checkSolid(t, "601 red", ConvertTo709Limited(sd), 63, 102, 240, 1)
}

func TestToRGBARoundTrip(t *testing.T) {
	for _, k := range knownColours {
		if k.c.A != 255 {
			continue
		}
		img := ToRGBA(FromImage(solid(4, 4, k.c)))
		got := img.RGBAAt(2, 2)
		if !near(got.R, k.c.R, 2) || !near(got.G, k.c.G, 2) || !near(got.B, k.c.B, 2) || got.A != 255 {
			t.Errorf("%s: round trip gave %v", k.name, got)
		}
	}
}

func randomFrame(w, h int, seed int64) *codec.VideoFrame {
	r := rand.New(rand.NewSource(seed))
	f := codec.NewVideoFrame(w, h)
	r.Read(f.Y)
	r.Read(f.Cb)
	r.Read(f.Cr)
	return f
}

func sameFrame(a, b *codec.VideoFrame) bool {
	return a.Width == b.Width && a.Height == b.Height &&
		bytes.Equal(a.Y, b.Y) && bytes.Equal(a.Cb, b.Cb) && bytes.Equal(a.Cr, b.Cr)
}

func TestScale(t *testing.T) {
	src := randomFrame(17, 9, 1)
	same := Scale(src, 17, 9)
	if same == src || !sameFrame(same, src) {
		t.Fatal("identity scale must return an equal copy")
	}
	// Source with padded strides.
	padded := &codec.VideoFrame{Width: 17, Height: 9, YStride: 20, CStride: 11, Matrix: 1}
	padded.Y = make([]byte, 20*9)
	padded.Cb = make([]byte, 11*5)
	padded.Cr = make([]byte, 11*5)
	for y := 0; y < 9; y++ {
		copy(padded.Y[y*20:], src.Y[y*17:(y+1)*17])
	}
	for y := 0; y < 5; y++ {
		copy(padded.Cb[y*11:], src.Cb[y*9:(y+1)*9])
		copy(padded.Cr[y*11:], src.Cr[y*9:(y+1)*9])
	}
	if !sameFrame(Scale(padded, 40, 30), Scale(src, 40, 30)) {
		t.Fatal("stride must not affect scaling")
	}
	// Constant images stay constant at any size.
	flat := FromImage(solid(13, 7, color.NRGBA{200, 40, 90, 255}))
	for _, sz := range [][2]int{{1, 1}, {3, 5}, {13, 7}, {64, 48}, {200, 3}, {5, 121}} {
		g := Scale(flat, sz[0], sz[1])
		if g.Width != sz[0] || g.Height != sz[1] || len(g.Y) != sz[0]*sz[1] {
			t.Fatalf("size %v: got %dx%d", sz, g.Width, g.Height)
		}
		checkSolid(t, "flat scale", g, flat.Y[0], flat.Cb[0], flat.Cr[0], 0)
	}
	// Downscaling a fine checkerboard must average, not alias.
	cb := codec.NewVideoFrame(64, 64)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x+y)&1 == 0 {
				cb.Y[y*64+x] = 235
			}
		}
	}
	small := Scale(cb, 16, 16)
	for i, v := range small.Y {
		if !near(v, 125, 6) {
			t.Fatalf("checkerboard downscale Y[%d]=%d, want ≈125", i, v)
		}
	}
	// A 1×1 source and zero-size targets must not panic.
	Scale(randomFrame(1, 1, 2), 9, 9)
	if z := Scale(src, 0, 0); z.Width != 0 || len(z.Y) != 0 {
		t.Fatal("zero-size scale")
	}
}

// region reports whether every luma sample in the rectangle satisfies ok.
func region(f *codec.VideoFrame, x0, y0, x1, y1 int, ok func(byte) bool) bool {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if !ok(f.Y[y*f.YStride+x]) {
				return false
			}
		}
	}
	return true
}

func TestFitGeometry(t *testing.T) {
	white := color.NRGBA{255, 255, 255, 255}
	isWhite := func(v byte) bool { return v >= 233 }
	isBlack := func(v byte) bool { return v == 16 }

	// Portrait 90×160 in landscape 320×180: scaled to 102×180 (101.25 → 101
	// → even 102), pillarboxed at x=108 (even).
	f := Fit(FromImage(solid(90, 160, white)), 320, 180, Contain, color.Black)
	if f.Width != 320 || f.Height != 180 {
		t.Fatalf("canvas size %dx%d", f.Width, f.Height)
	}
	if !region(f, 0, 0, 108, 180, isBlack) || !region(f, 210, 0, 320, 180, isBlack) {
		t.Error("portrait: side bars not background")
	}
	if !region(f, 108, 0, 210, 180, isWhite) {
		t.Error("portrait: picture area wrong")
	}
	for i := range f.Cb {
		if f.Cb[i] != 128 || f.Cr[i] != 128 {
			t.Fatal("portrait: chroma fringe")
		}
	}

	// Landscape 160×90 in portrait 180×320: 180×102 letterboxed at y=108.
	f = Fit(FromImage(solid(160, 90, white)), 180, 320, Contain, color.Black)
	if !region(f, 0, 0, 180, 108, isBlack) || !region(f, 0, 210, 180, 320, isBlack) ||
		!region(f, 0, 108, 180, 210, isWhite) {
		t.Error("landscape in portrait: wrong layout")
	}

	// Coloured background.
	f = Fit(FromImage(solid(10, 10, white)), 40, 10, Contain, color.NRGBA{255, 0, 0, 255})
	if f.Y[0] != 63 || f.Cb[0] != 102 || f.Cr[0] != 240 {
		t.Errorf("background = %d,%d,%d, want red", f.Y[0], f.Cb[0], f.Cr[0])
	}

	// Cover and Stretch fill the canvas entirely.
	for _, mode := range []FitMode{Cover, Stretch} {
		for _, src := range [][2]int{{90, 160}, {160, 90}, {33, 17}} {
			for _, cv := range [][2]int{{320, 180}, {180, 320}, {31, 47}, {1, 1}} {
				g := Fit(FromImage(solid(src[0], src[1], white)), cv[0], cv[1], mode, color.Black)
				if g.Width != cv[0] || g.Height != cv[1] || !region(g, 0, 0, cv[0], cv[1], isWhite) {
					t.Errorf("mode %d src %v canvas %v: not filled", mode, src, cv)
				}
			}
		}
	}

	// Cover crops the centre: left half black, right half white, tall source.
	half := image.NewNRGBA(image.Rect(0, 0, 100, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 100; x++ {
			if x >= 50 {
				half.SetNRGBA(x, y, white)
			} else {
				half.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 255})
			}
		}
	}
	g := Fit(FromImage(half), 200, 100, Cover, nil)
	if !region(g, 0, 0, 96, 100, isBlack) || !region(g, 104, 0, 200, 100, isWhite) {
		t.Error("cover: centre crop wrong")
	}

	// Odd sizes everywhere, plus exact fit, never panic and give exact size.
	for _, src := range [][2]int{{1, 1}, {3, 7}, {7, 3}, {101, 57}, {64, 64}} {
		for _, cv := range [][2]int{{1, 1}, {2, 2}, {5, 9}, {9, 5}, {101, 57}, {127, 33}} {
			for _, mode := range []FitMode{Contain, Cover, Stretch} {
				g := Fit(randomFrame(src[0], src[1], 3), cv[0], cv[1], mode, color.White)
				if g.Width != cv[0] || g.Height != cv[1] || len(g.Y) != cv[0]*cv[1] {
					t.Fatalf("src %v canvas %v mode %d: bad size", src, cv, mode)
				}
			}
		}
	}
	if g := Fit(nil, 8, 8, Contain, nil); !region(g, 0, 0, 8, 8, isBlack) {
		t.Error("nil frame should give a background canvas")
	}
	// Same-size contain is a straight copy.
	src := randomFrame(32, 18, 4)
	if !sameFrame(Fit(src, 32, 18, Contain, nil), src) {
		t.Error("same-size contain should copy the frame")
	}
}

func TestRotate(t *testing.T) {
	for _, sz := range [][2]int{{6, 4}, {7, 5}, {1, 3}} {
		f := randomFrame(sz[0], sz[1], 5)
		r90 := Rotate(f, 90)
		if r90.Width != sz[1] || r90.Height != sz[0] {
			t.Fatalf("rotate 90 size %dx%d", r90.Width, r90.Height)
		}
		if !sameFrame(Rotate(r90, 270), f) || !sameFrame(Rotate(r90, -90), f) {
			t.Error("90 then 270 is not identity")
		}
		if !sameFrame(Rotate(Rotate(f, 180), 180), f) {
			t.Error("180 twice is not identity")
		}
		if !sameFrame(Rotate(Rotate(Rotate(Rotate(f, 90), 90), 90), 90), f) {
			t.Error("four quarter turns is not identity")
		}
		if !sameFrame(Rotate(Rotate(f, 90), 90), Rotate(f, 180)) {
			t.Error("two quarter turns differ from a half turn")
		}
		if Rotate(f, 0) != f || Rotate(f, 360) != f || Rotate(f, 45) != f {
			t.Error("0/360/unsupported should return the frame")
		}
	}
	// Clockwise: top-left goes to top-right.
	f := codec.NewVideoFrame(4, 2)
	f.Y[0] = 200
	if r := Rotate(f, 90); r.Y[r.Width-1] != 200 {
		t.Error("rotate 90 is not clockwise")
	}

	// RotateInto writes where it is told and gives the same picture; an
	// angle that is no turn returns the source and leaves the target alone.
	src := randomFrame(8, 6, 3)
	src.Matrix, src.FullRange = 6, true
	into := codec.NewVideoFrame(16, 16)
	y := &into.Y[0]
	for _, d := range []int{90, 180, 270, -90} {
		got := RotateInto(into, src, d)
		if got != into || &got.Y[0] != y || got.Matrix != 6 || !got.FullRange || !sameFrame(got, Rotate(src, d)) {
			t.Errorf("RotateInto by %d: reused %v, same picture %v", d, got == into, sameFrame(got, Rotate(src, d)))
		}
	}
	before := *into
	if RotateInto(into, src, 0) != src || RotateInto(into, src, 30) != src || into.Width != before.Width {
		t.Error("RotateInto by an angle that is no turn should return the source")
	}
}

// jpegWithOrientation encodes a small JPEG and splices in an EXIF APP1
// segment carrying the orientation tag.
func jpegWithOrientation(t testing.TB, o uint16, bo binary.ByteOrder) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	tiff := make([]byte, 8+2+2*12+4)
	if bo == binary.LittleEndian {
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	bo.PutUint16(tiff[2:], 42)
	bo.PutUint32(tiff[4:], 8)
	bo.PutUint16(tiff[8:], 2)
	// Entry 0: ImageWidth (0x0100) LONG 1 = 8. Entry 1: Orientation.
	bo.PutUint16(tiff[10:], 0x0100)
	bo.PutUint16(tiff[12:], 4)
	bo.PutUint32(tiff[14:], 1)
	bo.PutUint32(tiff[18:], 8)
	bo.PutUint16(tiff[22:], 0x0112)
	bo.PutUint16(tiff[24:], 3)
	bo.PutUint32(tiff[26:], 1)
	bo.PutUint16(tiff[30:], o)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	seg = append(seg, payload...)
	// An unrelated APP0-style segment first, to exercise skipping.
	skip := []byte{0xFF, 0xE0, 0, 4, 'x', 'y'}
	j := buf.Bytes()
	out := append([]byte{}, j[:2]...)
	out = append(out, skip...)
	out = append(out, seg...)
	return append(out, j[2:]...)
}

func TestEXIFOrientation(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for o := 1; o <= 8; o++ {
			data := jpegWithOrientation(t, uint16(o), bo)
			if got := EXIFOrientation(bytes.NewReader(data)); got != o {
				t.Errorf("%v orientation %d: got %d", bo, o, got)
			}
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("test JPEG is not decodable: %v", err)
			}
		}
		if got := EXIFOrientation(bytes.NewReader(jpegWithOrientation(t, 9, bo))); got != 1 {
			t.Errorf("out-of-range orientation gave %d", got)
		}
	}
	var plain bytes.Buffer
	jpeg.Encode(&plain, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
	for name, data := range map[string][]byte{
		"plain jpeg": plain.Bytes(),
		"empty":      nil,
		"png":        []byte("\x89PNG\r\n\x1a\n"),
		"truncated":  jpegWithOrientation(t, 6, binary.BigEndian)[:20],
		"fill bytes": bytes.Repeat([]byte{0xFF}, 500),
	} {
		if got := EXIFOrientation(bytes.NewReader(data)); got != 1 {
			t.Errorf("%s: got %d, want 1", name, got)
		}
	}
	if EXIFOrientation(nil) != 1 {
		t.Error("nil reader")
	}
}

func FuzzEXIFOrientation(f *testing.F) {
	f.Add(jpegWithOrientation(f, 6, binary.BigEndian))
	f.Add(jpegWithOrientation(f, 3, binary.LittleEndian))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, 2})
	f.Add([]byte("Exif\x00\x00II*\x00\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := EXIFOrientation(bytes.NewReader(data)); o < 1 || o > 8 {
			t.Fatalf("orientation %d out of range", o)
		}
		if o := parseEXIFOrientation(data); o < 0 || o > 8 {
			t.Fatalf("parsed orientation %d out of range", o)
		}
	})
}

func TestOrient(t *testing.T) {
	// 3×2 source:  a b c
	//              d e f
	src := image.NewGray(image.Rect(10, 20, 13, 22)) // non-zero origin on purpose
	copy(src.Pix, "abcdef")
	want := map[int][]string{
		1: {"abc", "def"},
		2: {"cba", "fed"},
		3: {"fed", "cba"},
		4: {"def", "abc"},
		5: {"ad", "be", "cf"},
		6: {"da", "eb", "fc"},
		7: {"fc", "eb", "da"},
		8: {"cf", "be", "ad"},
	}
	for o := 1; o <= 8; o++ {
		img := Orient(src, o)
		b := img.Bounds()
		rows := want[o]
		if b.Min != (image.Point{}) && o != 1 {
			t.Errorf("orientation %d: bounds %v not zero-based", o, b)
		}
		if b.Dx() != len(rows[0]) || b.Dy() != len(rows) {
			t.Fatalf("orientation %d: bounds %v", o, b)
		}
		for y, row := range rows {
			for x := range row {
				got := img.At(b.Min.X+x, b.Min.Y+y).(color.Gray).Y
				if got != row[x] {
					t.Errorf("orientation %d at (%d,%d): got %c want %c", o, x, y, got, row[x])
				}
			}
		}
	}
	if Orient(src, 0) != image.Image(src) || Orient(src, 9) != image.Image(src) {
		t.Error("invalid orientation should return the image unchanged")
	}

	// The fast path in FromImage must agree with materialising the view.
	// 2×2 colour blocks keep the 4:2:0 chroma exact under every transform.
	blocks := image.NewNRGBA(image.Rect(0, 0, 6, 4))
	r := rand.New(rand.NewSource(7))
	for by := 0; by < 2; by++ {
		for bx := 0; bx < 3; bx++ {
			c := color.NRGBA{byte(r.Intn(256)), byte(r.Intn(256)), byte(r.Intn(256)), 255}
			for i := 0; i < 4; i++ {
				blocks.SetNRGBA(bx*2+i%2, by*2+i/2, c)
			}
		}
	}
	for o := 1; o <= 8; o++ {
		view := Orient(blocks, o)
		flat := image.NewNRGBA(view.Bounds())
		for y := 0; y < flat.Rect.Dy(); y++ {
			for x := 0; x < flat.Rect.Dx(); x++ {
				flat.Set(x, y, view.At(x, y))
			}
		}
		if !sameFrame(FromImage(view), FromImage(flat)) {
			t.Errorf("orientation %d: fast path differs from pixel path", o)
		}
	}
}

// Lossy WebP stores limited-range BT.601 samples in an image.YCbCr; they
// must not be stretched as if they were JPEG's full range.
func TestFromVideoYCbCr(t *testing.T) {
	m := image.NewYCbCr(image.Rect(0, 0, 8, 8), image.YCbCrSubsampleRatio420)
	for i := range m.Y {
		m.Y[i] = 235 // limited-range white
	}
	for i := range m.Cb {
		m.Cb[i], m.Cr[i] = 128, 128
	}
	f := FromVideoYCbCr(m)
	if f.Y[0] != 235 || f.FullRange || f.Matrix != 1 {
		t.Errorf("limited-range white became Y=%d (full range %v, matrix %d), want 235 unchanged", f.Y[0], f.FullRange, f.Matrix)
	}
	if g := FromImage(m); g.Y[0] >= 235 {
		t.Errorf("the same samples read as full range gave Y=%d; expected a darker value", g.Y[0])
	}
	if e := FromVideoYCbCr(image.NewYCbCr(image.Rect(0, 0, 0, 0), image.YCbCrSubsampleRatio420)); e.Width != 0 {
		t.Error("empty image")
	}
}

// Transparent and translucent pixels show the backdrop in every pixel
// layout FromImageOver has a path for, and a fully transparent pixel is
// exactly the colour Fit pads with.
func TestFromImageOver(t *testing.T) {
	bg := color.NRGBA{32, 64, 192, 255}
	by, bcb, bcr := colorTo709(bg)
	clear := color.NRGBA{255, 0, 0, 0}      // the red underneath must not show
	half := color.NRGBA{255, 255, 255, 128} // white at 128/255
	mix := func(c, b uint8) uint8 { return uint8((int(c)*128 + int(b)*127 + 127) / 255) }
	want := FromImage(solid(2, 2, color.NRGBA{mix(255, bg.R), mix(255, bg.G), mix(255, bg.B), 255}))
	opaque := color.NRGBA{200, 40, 90, 255}

	layouts := []struct {
		name string
		make func(c color.NRGBA) image.Image
	}{
		{"nrgba", func(c color.NRGBA) image.Image { return solid(6, 4, c) }},
		{"rgba", func(c color.NRGBA) image.Image {
			m := image.NewRGBA(image.Rect(0, 0, 6, 4))
			for i := 0; i < 24; i++ {
				m.Set(i%6, i/6, c)
			}
			return m
		}},
		{"paletted", func(c color.NRGBA) image.Image {
			m := image.NewPaletted(image.Rect(0, 0, 6, 4), color.Palette{color.Black, c})
			for i := range m.Pix {
				m.Pix[i] = 1
			}
			return m
		}},
		{"nrgba64", func(c color.NRGBA) image.Image {
			m := image.NewNRGBA64(image.Rect(0, 0, 6, 4))
			for i := 0; i < 24; i++ {
				m.Set(i%6, i/6, c)
			}
			return m
		}},
		{"nycbcra", func(c color.NRGBA) image.Image {
			m := image.NewNYCbCrA(image.Rect(0, 0, 6, 4), image.YCbCrSubsampleRatio420)
			yy, cb, cr := color.RGBToYCbCr(c.R, c.G, c.B)
			for i := range m.Y {
				m.Y[i], m.A[i] = yy, c.A
			}
			for i := range m.Cb {
				m.Cb[i], m.Cr[i] = cb, cr
			}
			return m
		}},
	}
	for _, l := range layouts {
		checkSolid(t, l.name+"/transparent", FromImageOver(l.make(clear), bg), by, bcb, bcr, 0)
		checkSolid(t, l.name+"/translucent", FromImageOver(l.make(half), bg), want.Y[0], want.Cb[0], want.Cr[0], 1)
		checkSolid(t, l.name+"/transparent, no backdrop", FromImageOver(l.make(clear), nil), 16, 128, 128, 0)
		if !sameFrame(FromImageOver(l.make(opaque), bg), FromImage(l.make(opaque))) {
			t.Errorf("%s: the backdrop changed an opaque image", l.name)
		}
		if !sameFrame(FromImageOver(l.make(half), color.Black), FromImage(l.make(half))) {
			t.Errorf("%s: a black backdrop differs from FromImage", l.name)
		}
		// The backdrop passes through the EXIF orientation fast path.
		checkSolid(t, l.name+"/oriented", FromImageOver(Orient(l.make(clear), 6), bg), by, bcb, bcr, 0)

		// Scaled and padded, a transparent image is indistinguishable from
		// the padding around it.
		for _, mode := range []FitMode{Contain, Cover, Stretch} {
			checkSolid(t, l.name+"/fit", Fit(FromImageOver(l.make(clear), bg), 40, 18, mode, bg), by, bcb, bcr, 0)
		}
	}

	// Per pixel: an opaque white column next to a transparent one, on an
	// odd width so the last chroma sample covers a single column.
	m := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			if x < 2 {
				m.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
			}
		}
	}
	f := FromImageOver(m, bg)
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			wantY := by
			if x < 2 {
				wantY = 235
			}
			if got := f.Y[y*f.YStride+x]; got != wantY {
				t.Errorf("Y(%d,%d)=%d, want %d", x, y, got, wantY)
			}
		}
		if cy := y / 2; f.Cb[cy*f.CStride] != 128 || f.Cb[cy*f.CStride+1] != bcb || f.Cb[cy*f.CStride+2] != bcb {
			t.Errorf("Cb row %d = %v, want 128 then %d", cy, f.Cb[cy*f.CStride:cy*f.CStride+3], bcb)
		}
	}
}

// Lossy WebP with transparency decodes to an image.NYCbCrA whose colour
// samples are limited range; they keep their levels and show the backdrop
// where the alpha plane says so.
func TestFromVideoNYCbCrA(t *testing.T) {
	bg := color.NRGBA{32, 64, 192, 255}
	by, bcb, bcr := colorTo709(bg)
	m := image.NewNYCbCrA(image.Rect(0, 0, 8, 6), image.YCbCrSubsampleRatio420)
	for i := range m.Y {
		m.Y[i] = 235 // limited-range white
		if i%8 < 4 {
			m.A[i] = 255
		}
	}
	for i := range m.Cb {
		m.Cb[i], m.Cr[i] = 128, 128
	}
	f := FromVideoNYCbCrA(m, bg)
	if f.Matrix != 1 || f.FullRange {
		t.Errorf("frame tagged matrix=%d full=%v, want 709 limited", f.Matrix, f.FullRange)
	}
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			wantY, wantCb, wantCr := by, bcb, bcr
			if x < 4 {
				wantY, wantCb, wantCr = 235, 128, 128
			}
			c := (y/2)*f.CStride + x/2
			if f.Y[y*f.YStride+x] != wantY || f.Cb[c] != wantCb || f.Cr[c] != wantCr {
				t.Fatalf("(%d,%d) = %d,%d,%d, want %d,%d,%d", x, y, f.Y[y*f.YStride+x], f.Cb[c], f.Cr[c], wantY, wantCb, wantCr)
			}
		}
	}
	// A sub-image keeps its own alpha offsets.
	sub := m.SubImage(image.Rect(2, 2, 8, 6)).(*image.NYCbCrA)
	g := FromVideoNYCbCrA(sub, bg)
	if g.Width != 6 || g.Height != 4 || g.Y[0] != 235 || g.Y[1] != 235 || g.Y[2] != by || g.Cb[0] != 128 || g.Cb[1] != bcb {
		t.Errorf("sub-image: %dx%d Y=%v Cb=%v", g.Width, g.Height, g.Y[:3], g.Cb[:2])
	}
	if e := FromVideoNYCbCrA(image.NewNYCbCrA(image.Rectangle{}, image.YCbCrSubsampleRatio420), bg); e.Width != 0 {
		t.Error("empty image")
	}
}
