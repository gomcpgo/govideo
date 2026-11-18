package process

import (
	"encoding/binary"
	"image"
	"image/color"
	"io"

	"github.com/gomcpgo/govideo/codec"
)

// orientedImage is a lazy view of src with an EXIF orientation applied.
// FromImage recognises it and applies the transform on the converted frame
// instead of going through At.
type orientedImage struct {
	src image.Image
	o   int
}

func (m *orientedImage) ColorModel() color.Model { return m.src.ColorModel() }

func (m *orientedImage) Bounds() image.Rectangle {
	b := m.src.Bounds()
	if m.o >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}

func (m *orientedImage) At(x, y int) color.Color {
	b := m.src.Bounds()
	w, h := b.Dx(), b.Dy()
	var sx, sy int
	switch m.o {
	case 2:
		sx, sy = w-1-x, y
	case 3:
		sx, sy = w-1-x, h-1-y
	case 4:
		sx, sy = x, h-1-y
	case 5:
		sx, sy = y, x
	case 6:
		sx, sy = y, h-1-x
	case 7:
		sx, sy = w-1-y, h-1-x
	case 8:
		sx, sy = w-1-y, x
	default:
		sx, sy = x, y
	}
	return m.src.At(b.Min.X+sx, b.Min.Y+sy)
}

// Orient returns img with the EXIF orientation (1..8) applied, so that the
// result displays upright. Orientation 1 and unknown values return img
// unchanged. The result is a lazy view; FromImage converts it efficiently.
func Orient(img image.Image, exifOrientation int) image.Image {
	if exifOrientation < 2 || exifOrientation > 8 {
		return img
	}
	return &orientedImage{src: img, o: exifOrientation}
}

func applyOrientation(f *codec.VideoFrame, o int) *codec.VideoFrame {
	switch o {
	case 2:
		return flipH(f)
	case 3:
		return Rotate(f, 180)
	case 4:
		return flipH(Rotate(f, 180))
	case 5:
		return flipH(Rotate(f, 90))
	case 6:
		return Rotate(f, 90)
	case 7:
		return flipH(Rotate(f, 270))
	case 8:
		return Rotate(f, 270)
	}
	return f
}

// flipH mirrors a frame left-right in place and returns it.
func flipH(f *codec.VideoFrame) *codec.VideoFrame {
	flip := func(p []byte, stride, w, h int) {
		for y := 0; y < h; y++ {
			row := p[y*stride : y*stride+w]
			for i, j := 0, w-1; i < j; i, j = i+1, j-1 {
				row[i], row[j] = row[j], row[i]
			}
		}
	}
	cw, ch := f.ChromaSize()
	flip(f.Y, f.YStride, f.Width, f.Height)
	flip(f.Cb, f.CStride, cw, ch)
	flip(f.Cr, f.CStride, cw, ch)
	return f
}

// Rotate returns the frame rotated clockwise by 0, 90, 180 or 270 degrees
// (negative values and multiples of 360 are normalised). Other angles, and
// 0, return f unchanged.
func Rotate(f *codec.VideoFrame, degrees int) *codec.VideoFrame {
	d := ((degrees % 360) + 360) % 360
	if f == nil || (d != 90 && d != 180 && d != 270) {
		return f
	}
	w, h := f.Width, f.Height
	if d != 180 {
		w, h = h, w
	}
	out := codec.NewVideoFrame(w, h)
	out.Matrix, out.FullRange = f.Matrix, f.FullRange
	scw, sch := f.ChromaSize()
	rotatePlane(out.Y, out.YStride, f.Y, f.YStride, f.Width, f.Height, d)
	rotatePlane(out.Cb, out.CStride, f.Cb, f.CStride, scw, sch, d)
	rotatePlane(out.Cr, out.CStride, f.Cr, f.CStride, scw, sch, d)
	return out
}

// rotatePlane rotates a w×h source plane clockwise by d into dst.
func rotatePlane(dst []byte, dstride int, src []byte, sstride, w, h, d int) {
	switch d {
	case 90: // dst(x,y) = src(y, h-1-x); dst is h×w
		for y := 0; y < w; y++ {
			row := dst[y*dstride : y*dstride+h]
			for x := range row {
				row[x] = src[(h-1-x)*sstride+y]
			}
		}
	case 180:
		for y := 0; y < h; y++ {
			row := dst[y*dstride : y*dstride+w]
			s := src[(h-1-y)*sstride : (h-1-y)*sstride+w]
			for x := range row {
				row[x] = s[w-1-x]
			}
		}
	case 270: // dst(x,y) = src(w-1-y, x); dst is h×w
		for y := 0; y < w; y++ {
			row := dst[y*dstride : y*dstride+h]
			for x := range row {
				row[x] = src[x*sstride+(w-1-y)]
			}
		}
	}
}

// EXIFOrientation reads the EXIF orientation tag (0x0112) from a JPEG
// stream. It returns 1 (upright) when the tag is absent, the data is not a
// JPEG, or anything is malformed. It reads only the leading marker segments.
func EXIFOrientation(r io.Reader) int {
	if r == nil {
		return 1
	}
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for seg := 0; seg < 256; seg++ {
		if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 0xFF {
			return 1
		}
		m := b[1]
		for fill := 0; m == 0xFF; fill++ { // fill bytes before a marker
			if fill > 64 {
				return 1
			}
			if _, err := io.ReadFull(r, b[:1]); err != nil {
				return 1
			}
			m = b[0]
		}
		if m == 0xD9 || m == 0xDA || m == 0x00 {
			return 1 // EOI, start of scan, or garbage
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			continue // standalone markers
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 1
		}
		n := int(binary.BigEndian.Uint16(b[:])) - 2
		if n < 0 {
			return 1
		}
		if m != 0xE1 {
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return 1
			}
			continue
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return 1
		}
		if o := parseEXIFOrientation(data); o != 0 {
			return o
		}
	}
	return 1
}

// parseEXIFOrientation returns the orientation from an APP1 payload, or 0.
func parseEXIFOrientation(d []byte) int {
	if len(d) < 14 || string(d[:6]) != "Exif\x00\x00" {
		return 0
	}
	t := d[6:]
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:]) != 42 {
		return 0
	}
	off := int64(bo.Uint32(t[4:]))
	size := int64(len(t))
	if off < 8 || off+2 > size {
		return 0
	}
	n := int64(bo.Uint16(t[off:]))
	for i := int64(0); i < n; i++ {
		e := off + 2 + i*12
		if e+12 > size {
			return 0
		}
		if bo.Uint16(t[e:]) != 0x0112 {
			continue
		}
		if bo.Uint16(t[e+2:]) != 3 || bo.Uint32(t[e+4:]) != 1 {
			return 0
		}
		if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 0
	}
	return 0
}
