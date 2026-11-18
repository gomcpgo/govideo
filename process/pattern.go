package process

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Generated pictures are drawn as RGBA images and converted to the working
// format with FromImage, so they share its colour conversion.

// Solid returns a w by h image of one colour.
func Solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	return img
}

// barColors are 75% colour bars in the usual order of descending luminance.
var barColors = [7]color.RGBA{
	{191, 191, 191, 255}, {191, 191, 0, 255}, {0, 191, 191, 255}, {0, 191, 0, 255},
	{191, 0, 191, 255}, {191, 0, 0, 255}, {0, 0, 191, 255},
}

// BarColor returns the colour of bar i (0..6) of the Bars pattern.
func BarColor(i int) color.RGBA { return barColors[i] }

// Bars returns colour bars: seven 75% bars over the top three quarters, and
// a black-to-white ramp across the bottom quarter.
func Bars(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	split := h - h/4
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			c := barColors[x*7/w]
			if y >= split {
				v := uint8(0)
				if w > 1 {
					v = uint8((x*255 + (w-1)/2) / (w - 1))
				}
				c = color.RGBA{v, v, v, 255}
			}
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = c.R, c.G, c.B, 255
		}
	}
	return img
}

// Gradient returns a linear blend from a (left, or top when vertical) to b.
func Gradient(w, h int, a, b color.Color, vertical bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	ca, cb := color.RGBAModel.Convert(a).(color.RGBA), color.RGBAModel.Convert(b).(color.RGBA)
	mix := func(p, q uint8, i, n int) uint8 {
		if n <= 1 {
			return p
		}
		return uint8((int(p)*(n-1-i) + int(q)*i + (n-1)/2) / (n - 1))
	}
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			i, n := x, w
			if vertical {
				i, n = y, h
			}
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = mix(ca.R, cb.R, i, n), mix(ca.G, cb.G, i, n), mix(ca.B, cb.B, i, n), 255
		}
	}
	return img
}

// Checker returns a checkerboard of a and b with square cells of the given
// size in pixels.
func Checker(w, h, cell int, a, b color.Color) *image.RGBA {
	cell = max(cell, 1)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	ca, cb := color.RGBAModel.Convert(a).(color.RGBA), color.RGBAModel.Convert(b).(color.RGBA)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			c := ca
			if (x/cell+y/cell)%2 == 1 {
				c = cb
			}
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = c.R, c.G, c.B, 255
		}
	}
	return img
}

var (
	fontOnce sync.Once
	fontData *opentype.Font
	fontErr  error

	faceMu sync.Mutex
	faces  = map[int]font.Face{} // by size in 1/4 pixels
)

// face returns the built-in typeface (Go Bold, Latin only) at a pixel size.
func face(size float64) (font.Face, error) {
	fontOnce.Do(func() { fontData, fontErr = opentype.Parse(gobold.TTF) })
	if fontErr != nil {
		return nil, fontErr
	}
	key := int(size*4 + 0.5)
	faceMu.Lock()
	defer faceMu.Unlock()
	if f := faces[key]; f != nil {
		return f, nil
	}
	f, err := opentype.NewFace(fontData, &opentype.FaceOptions{Size: float64(key) / 4, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	if len(faces) > 64 {
		clear(faces)
	}
	faces[key] = f
	return f, nil
}

// DrawText draws text centred on (cx, cy) in the given colour. Lines are
// separated by "\n". size is the type size in pixels; it is reduced when
// needed so the widest line fits within maxWidth (0 means the image width).
// The built-in typeface covers Latin scripts only.
func DrawText(img *image.RGBA, text string, cx, cy int, size float64, maxWidth int, c color.Color) error {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if text == "" || size < 1 {
		return nil
	}
	if maxWidth <= 0 {
		maxWidth = img.Bounds().Dx()
	}
	size = min(size, 2048)
	var f font.Face
	for {
		var err error
		if f, err = face(size); err != nil {
			return fmt.Errorf("process: text: %w", err)
		}
		widest := 0
		for _, l := range lines {
			widest = max(widest, font.MeasureString(f, l).Ceil())
		}
		if widest <= maxWidth || size <= 6 {
			break
		}
		size = max(6, min(size*float64(maxWidth)/float64(widest), size-1))
	}
	m := f.Metrics()
	lineH := (m.Ascent + m.Descent).Ceil() * 6 / 5
	top := cy - lineH*len(lines)/2
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f}
	for i, l := range lines {
		w := font.MeasureString(f, l)
		base := top + i*lineH + (lineH-(m.Ascent+m.Descent).Ceil())/2 + m.Ascent.Ceil()
		d.Dot = fixed.Point26_6{X: fixed.I(cx) - w/2, Y: fixed.I(base)}
		d.DrawString(l)
	}
	return nil
}

// CounterBits is the number of cells in the frame counter of the test card.
const CounterBits = 16

// CounterCell returns the rectangle of bit i (0 = most significant) of the
// test card's frame counter in a w by h picture.
func CounterCell(w, h, i int) image.Rectangle {
	cw := w / CounterBits
	return image.Rect(i*cw, h-h/8, (i+1)*cw, h)
}

// TestCard draws frame n of an animated test pattern over base, which must
// be a Bars image and is not modified. The card adds a white bar that sweeps
// from left to right once per second, a timecode, and the frame number as a
// row of 16 black or white cells along the bottom edge (most significant bit
// first), which lets a test read back exactly which frame it is looking at.
func TestCard(base *image.RGBA, n int64, fpsNum, fpsDen int64) *image.RGBA {
	b := base.Bounds()
	w, h := b.Dx(), b.Dy()
	img := image.NewRGBA(b)
	copy(img.Pix, base.Pix)
	fill := func(r image.Rectangle, c color.Color) {
		draw.Draw(img, r.Intersect(b), image.NewUniform(c), image.Point{}, draw.Src)
	}
	// Sweep: position within the current second.
	perSec := max(fpsNum, 1)
	phase := (n * max(fpsDen, 1)) % perSec
	bar := max(w/64, 2)
	x := int(phase * int64(w-bar) / perSec)
	fill(image.Rect(x, h/2, x+bar, h/2+h/8), color.White)

	for i := 0; i < CounterBits; i++ {
		c := color.RGBA{0, 0, 0, 255}
		if n>>(CounterBits-1-i)&1 == 1 {
			c = color.RGBA{255, 255, 255, 255}
		}
		fill(CounterCell(w, h, i), c)
	}

	t := time.Duration(float64(n) * float64(max(fpsDen, 1)) / float64(perSec) * float64(time.Second))
	label := fmt.Sprintf("%02d:%02d:%02d.%03d  #%d", int(t.Hours()), int(t.Minutes())%60, int(t.Seconds())%60, t.Milliseconds()%1000, n)
	box := image.Rect(w/4, h/5, w-w/4, h/5+h/7)
	fill(box, color.RGBA{0, 0, 0, 255})
	DrawText(img, label, w/2, box.Min.Y+box.Dy()/2, float64(h)/14, box.Dx()-box.Dx()/10, color.White)
	return img
}
