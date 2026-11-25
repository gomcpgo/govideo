package govideo_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"golang.org/x/image/webp"
)

// Stills that carry an EXIF orientation, in each container that can: the
// APP1 segment of a JPEG, the eXIf chunk of a PNG and the EXIF chunk of a
// WebP file. They are put together here, byte by byte around a picture the
// standard library encodes (or, for WebP, one of the fixtures of
// webp_test.go and a few lines of lossless encoder), because no encoder on
// a test machine is sure to write the tag.

// exifOrientation returns EXIF data holding nothing but the orientation
// tag: a little-endian TIFF structure with one directory entry.
func exifOrientation(o int) []byte {
	t := []byte("II*\x00\x08\x00\x00\x00") // byte order, 42, the directory at 8
	t = binary.LittleEndian.AppendUint16(t, 1)
	t = binary.LittleEndian.AppendUint16(t, 0x0112) // Orientation
	t = binary.LittleEndian.AppendUint16(t, 3)      // SHORT
	t = binary.LittleEndian.AppendUint32(t, 1)
	t = binary.LittleEndian.AppendUint32(t, uint32(o))
	return binary.LittleEndian.AppendUint32(t, 0) // no further directory
}

// quadrants draws a w×h picture whose quarters are the four colours given
// for the top left, top right, bottom left and bottom right: no two of the
// eight orientations show it alike.
func quadrants(w, h int, c [4]color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			q := 0
			if x >= w/2 {
				q = 1
			}
			if y >= h/2 {
				q += 2
			}
			img.SetNRGBA(x, y, c[q])
		}
	}
	return img
}

// jpegStill encodes img as a JPEG with the orientation in an APP1 segment.
func jpegStill(t *testing.T, img image.Image, o int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), exifOrientation(o)...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out = append(out, payload...)
	return append(out, buf.Bytes()[2:]...)
}

// pngStill encodes img as a PNG with the orientation in an eXIf chunk:
// after the header, or after the picture when last is set.
func pngStill(t *testing.T, img image.Image, o int, last bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := buf.Bytes()
	at := 8 + 12 + 13 // the signature and IHDR
	if last {
		at = len(p) - 12 // before IEND
	}
	exif := exifOrientation(o)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(exif)))
	chunk = append(append(chunk, "eXIf"...), exif...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := append([]byte{}, p[:at]...)
	out = append(out, chunk...)
	return append(out, p[at:]...)
}

// riffChunk is one chunk of a WebP file.
type riffChunk struct {
	kind string
	data []byte
}

// webpChunks splits a WebP file into its chunks.
func webpChunks(t *testing.T, file []byte) []riffChunk {
	t.Helper()
	var chunks []riffChunk
	for p := 12; p+8 <= len(file); {
		n := int(binary.LittleEndian.Uint32(file[p+4:]))
		if p+8+n > len(file) {
			t.Fatal("WebP fixture: a chunk runs past the end")
		}
		chunks = append(chunks, riffChunk{string(file[p : p+4]), file[p+8 : p+8+n]})
		p += 8 + n + n&1
	}
	return chunks
}

// webpFile puts chunks together as a WebP file.
func webpFile(chunks []riffChunk) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = binary.LittleEndian.AppendUint32(append(body, c.kind...), uint32(len(c.data)))
		body = append(body, c.data...)
		if len(c.data)%2 == 1 {
			body = append(body, 0)
		}
	}
	return append(binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body))), body...)
}

// VP8X header flags (RFC 9649, extended file format).
const (
	vp8xEXIF  = 0x08
	vp8xAlpha = 0x10
)

// vp8x returns the header chunk of an extended-format WebP file.
func vp8x(flags byte, w, h int) riffChunk {
	d := []byte{flags, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)}
	return riffChunk{"VP8X", d}
}

// lossyWebPStill returns the logo fixture (200x200: a white square in a
// transparent margin that hides red, a strip of half-transparent white
// along the top) with an orientation. With opaque set its alpha plane is
// left out, so the margin is red and the strip plain white: a lossy WebP
// picture of the other kind, which decodes to an *image.YCbCr.
func lossyWebPStill(t *testing.T, o int, opaque bool) []byte {
	t.Helper()
	logo, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(logoWebP), ""))
	if err != nil {
		t.Fatal(err)
	}
	var out []riffChunk
	for _, c := range webpChunks(t, logo) {
		switch {
		case c.kind == "VP8X":
			flags := c.data[0] | vp8xEXIF
			if opaque {
				flags &^= vp8xAlpha
			}
			c.data = append([]byte{flags}, c.data[1:]...)
		case c.kind == "ALPH" && opaque:
			continue
		}
		out = append(out, c)
	}
	return webpFile(append(out, riffChunk{"EXIF", exifOrientation(o)}))
}

// losslessWebPStill encodes a w×h picture of two colours, green where the
// function says so and black elsewhere, as a lossless WebP file with an
// orientation. Go has no WebP encoder, but two colours need little of one:
// every prefix code is of the "simple" kind (RFC 9649, 3.7.2.1.1), a pixel
// is one bit, and there are no transforms, colour cache or back references.
func losslessWebPStill(w, h, o int, green func(x, y int) bool) []byte {
	var out []byte
	var acc uint64
	var n uint
	put := func(v uint32, bits uint) { // least significant bit first
		acc |= uint64(v) << n
		for n += bits; n >= 8; n -= 8 {
			out = append(out, byte(acc))
			acc >>= 8
		}
	}
	put(0x2f, 8) // signature
	put(uint32(w-1), 14)
	put(uint32(h-1), 14)
	put(0, 1) // no alpha
	put(0, 3) // version
	put(0, 1) // no transform
	put(0, 1) // no colour cache
	put(0, 1) // one group of prefix codes
	// A simple code: 1, the number of symbols less one, whether the first
	// symbol takes 8 bits or 1, then the symbols.
	put(1, 1) // green: 0 and 255, a bit each
	put(1, 1)
	put(1, 1)
	put(0, 8)
	put(255, 8)
	for i := 0; i < 2; i++ { // red, blue: always 0, no bits
		put(1, 1)
		put(0, 1)
		put(0, 1)
		put(0, 1)
	}
	put(1, 1) // alpha: always 255
	put(0, 1)
	put(1, 1)
	put(255, 8)
	put(1, 1) // distance: never used
	put(0, 1)
	put(0, 1)
	put(0, 1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if green(x, y) {
				put(1, 1)
			} else {
				put(0, 1)
			}
		}
	}
	put(0, 7) // the last byte
	return webpFile([]riffChunk{vp8x(vp8xEXIF, w, h), {"VP8L", out}, {"EXIF", exifOrientation(o)}})
}

// shown gives where a point of a stored picture is displayed under each
// EXIF orientation, both as fractions of the width and height: the meaning
// of the tag (which stored edge is the top, which the left), written out
// here and not taken from the code under test.
func shown(o int, u, v float64) (float64, float64) {
	switch o {
	case 2: // mirrored left to right
		return 1 - u, v
	case 3: // turned half round
		return 1 - u, 1 - v
	case 4: // mirrored top to bottom
		return u, 1 - v
	case 5: // mirrored about the falling diagonal
		return v, u
	case 6: // turned a quarter clockwise
		return 1 - v, u
	case 7: // mirrored about the rising diagonal
		return 1 - v, 1 - u
	case 8: // turned a quarter anticlockwise
		return v, 1 - u
	}
	return u, v
}

// mark is a point of a stored picture, as fractions of its width and
// height, and the colour an export over stillBack shows for it.
type mark struct {
	u, v float64
	c    color.NRGBA
}

// orientedStill is a picture that can be written out with any EXIF
// orientation.
type orientedStill struct {
	name  string // the file extension, then what is particular about it
	w, h  int    // as stored
	make  func(o int) []byte
	marks []mark
	tol   int                             // how far a mark's colour may be off, per component
	check func(t *testing.T, data []byte) // that the fixture is the kind of picture it is meant to be
}

// stillBack is the canvas background the oriented stills are exported over.
var stillBack = color.NRGBA{32, 64, 192, 255}

// orientedStills returns a still for each way the orientation is stored
// and each way such a picture is decoded.
func orientedStills(t *testing.T) []orientedStill {
	var (
		red   = color.NRGBA{255, 0, 0, 255}
		green = color.NRGBA{0, 255, 0, 255}
		blue  = color.NRGBA{0, 0, 255, 255}
		white = color.NRGBA{255, 255, 255, 255}
		black = color.NRGBA{0, 0, 0, 255}
		back  = stillBack
		// Half-transparent white over the background.
		veiled = color.NRGBA{uint8((255*128 + 32*127) / 255), uint8((255*128 + 64*127) / 255), uint8((255*128 + 192*127) / 255), 255}
	)
	four := func(c [4]color.NRGBA) []mark {
		return []mark{{.25, .25, c[0]}, {.75, .25, c[1]}, {.25, .75, c[2]}, {.75, .75, c[3]}}
	}
	// 120x80, so that a quarter turn also changes the shape.
	const w, h = 120, 80
	colours := [4]color.NRGBA{red, green, blue, white}
	picture := quadrants(w, h, colours)
	corner := [4]color.NRGBA{green, black, black, black}
	// The logo: its strip is along the top of the stored picture. It looks
	// the same mirrored left to right, so it tells four ways up apart, not
	// eight; the pictures above tell all eight.
	logo := func(margin, strip color.NRGBA) []mark {
		return []mark{{.5, .5, white}, {.5, .075, strip}, {.5, .925, margin}, {.075, .5, margin}, {.925, .5, margin}}
	}
	return []orientedStill{
		{"jpg", w, h, func(o int) []byte { return jpegStill(t, picture, o) }, four(colours), 12, nil},
		{"png", w, h, func(o int) []byte { return pngStill(t, picture, o, false) }, four(colours), 12, nil},
		{"png, tag after the picture", w, h, func(o int) []byte { return pngStill(t, picture, o, true) }, four(colours), 12, nil},
		{"webp, lossless", w, h, func(o int) []byte {
			return losslessWebPStill(w, h, o, func(x, y int) bool { return x < w/2 && y < h/2 })
		}, four(corner), 12, func(t *testing.T, data []byte) {
			img, err := webp.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("the lossless fixture does not decode: %v", err)
			}
			if _, ok := img.(*image.NRGBA); !ok || img.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatalf("the lossless fixture decodes to %T %v, want a %dx%d *image.NRGBA", img, img.Bounds(), w, h)
			}
			for i, m := range four(corner) {
				if got := img.At(int(m.u*w), int(m.v*h)); got != color.Color(m.c) {
					t.Fatalf("the lossless fixture's quarter %d is %v, want %v", i, got, m.c)
				}
			}
		}},
		{"webp, lossy with alpha", 200, 200, func(o int) []byte { return lossyWebPStill(t, o, false) }, logo(back, veiled), 12,
			func(t *testing.T, data []byte) {
				if img, err := webp.Decode(bytes.NewReader(data)); err != nil {
					t.Fatalf("the lossy fixture does not decode: %v", err)
				} else if _, ok := img.(*image.NYCbCrA); !ok {
					t.Fatalf("the lossy fixture decodes to %T, want *image.NYCbCrA", img)
				}
			}},
		// Red this saturated does not come through two lossy codecs and
		// 4:2:0 exactly.
		{"webp, lossy", 200, 200, func(o int) []byte { return lossyWebPStill(t, o, true) }, logo(red, white), 24,
			func(t *testing.T, data []byte) {
				if img, err := webp.Decode(bytes.NewReader(data)); err != nil {
					t.Fatalf("the opaque lossy fixture does not decode: %v", err)
				} else if _, ok := img.(*image.YCbCr); !ok {
					t.Fatalf("the opaque lossy fixture decodes to %T, want *image.YCbCr", img)
				}
			}},
	}
}

// ext returns the file extension of a still.
func (s orientedStill) ext() string { return s.name[:strings.IndexAny(s.name+",", ",")] }

// TestStillOrientation checks that a still is shown the way its EXIF
// orientation says, whichever of JPEG, PNG and WebP it is, for all eight
// orientations: each still is exported and the picture looked at where
// marked points of the stored image should have gone. It needs no oracle:
// the export is Motion JPEG, read back with Frame.
func TestStillOrientation(t *testing.T) {
	back := stillBack
	dir := t.TempDir()
	const cw, ch = 640, 360
	for i, s := range orientedStills(t) {
		for o := 1; o <= 8; o++ {
			t.Run(fmt.Sprintf("%s/%d", s.name, o), func(t *testing.T) {
				data := s.make(o)
				if s.check != nil {
					s.check(t, data)
				}
				in := filepath.Join(dir, fmt.Sprintf("%d-%d.%s", i, o, s.ext()))
				if err := os.WriteFile(in, data, 0o644); err != nil {
					t.Fatal(err)
				}
				tl := govideo.NewTimeline(govideo.Canvas{Width: cw, Height: ch, FrameRate: govideo.R(30, 1), Background: back})
				tl.AddImage(in, time.Second/10, govideo.ImageOptions{})
				out := in + ".mov"
				if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
					t.Fatal(err)
				}
				img, err := govideo.Frame(context.Background(), out, govideo.FrameOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if img.Bounds() != image.Rect(0, 0, cw, ch) {
					t.Fatalf("the export is %v, want %dx%d", img.Bounds(), cw, ch)
				}
				// The still as displayed: its sides change places from
				// orientation 5 on. It is fitted inside the canvas and
				// centred, with the background around it.
				dw, dh := float64(s.w), float64(s.h)
				if o >= 5 {
					dw, dh = dh, dw
				}
				scale := math.Min(cw/dw, ch/dh)
				x0, y0 := (cw-dw*scale)/2, (ch-dh*scale)/2
				at := func(x, y int, want color.NRGBA, what string) {
					t.Helper()
					r, g, b, _ := img.At(x, y).RGBA()
					if !near(int(r>>8), int(want.R), s.tol) || !near(int(g>>8), int(want.G), s.tol) || !near(int(b>>8), int(want.B), s.tol) {
						t.Errorf("%s, at (%d,%d) of the export: rgb(%d,%d,%d), want rgb(%d,%d,%d)", what, x, y, r>>8, g>>8, b>>8, want.R, want.G, want.B)
					}
				}
				for _, m := range s.marks {
					u, v := shown(o, m.u, m.v)
					at(int(x0+u*dw*scale), int(y0+v*dh*scale), m.c, fmt.Sprintf("the stored picture's (%.0f%%, %.0f%%)", m.u*100, m.v*100))
				}
				// Beside the still the canvas shows: left and right of an
				// upright shape, or of a square one.
				if x0 >= 16 {
					at(int(x0/2), ch/2, back, "the bar on the left")
					at(cw-int(x0/2), ch/2, back, "the bar on the right")
				}
			})
		}
	}
}

// TestStillOrientationOracle compares the same stills with the oracle's
// reading of them, in every orientation, for the containers whose tag the
// oracle applies: ffmpeg 9 does for all three; an older one may not.
func TestStillOrientationOracle(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	for i, s := range orientedStills(t) {
		t.Run(s.name, func(t *testing.T) {
			file := func(o int) string {
				path := filepath.Join(dir, fmt.Sprintf("%d-%d.%s", i, o, s.ext()))
				if err := os.WriteFile(path, s.make(o), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			}
			// An oracle that reads a quarter turn as it reads none does
			// not look at the tag: there is nothing to compare with.
			decode := func(o int) []byte {
				return oracle.FFmpegOutput(t, "-i", file(o), "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
			}
			if bytes.Equal(decode(1), decode(6)) {
				t.Skipf("this ffmpeg does not apply the EXIF orientation of a %s file", s.ext())
			}
			for o := 1; o <= 8; o++ {
				if p := stillPSNR(t, file(o), filepath.Join(dir, "out.mov")); p < 32 {
					t.Errorf("orientation %d: picture differs from the oracle's (PSNR %.1f dB)", o, p)
				}
			}
		})
	}
}
