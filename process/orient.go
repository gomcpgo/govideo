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

// FlipH mirrors a frame left to right, in place, and returns it.
func FlipH(f *codec.VideoFrame) *codec.VideoFrame {
	if f == nil {
		return nil
	}
	return flipH(f)
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
	return RotateInto(nil, f, degrees)
}

// RotateInto is Rotate writing the result to the planes of into when they
// are large enough (see codec.VideoFrame.Reuse), so that rotating picture
// after picture does not allocate; into may be nil but not f. When the
// angle is no quarter, half or three-quarter turn, f itself is returned and
// into is left alone.
func RotateInto(into, f *codec.VideoFrame, degrees int) *codec.VideoFrame {
	d := ((degrees % 360) + 360) % 360
	if f == nil || (d != 90 && d != 180 && d != 270) {
		return f
	}
	w, h := f.Width, f.Height
	if d != 180 {
		w, h = h, w
	}
	out := into.Reuse(w, h)
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

// OrientFrame applies an EXIF orientation (1..8) to a frame, as Orient does
// to an image: for pictures that are converted by something other than
// FromImage, such as lossy WebP (FromVideoYCbCr). Orientation 1 and unknown
// values return f unchanged; a mirrored frame is changed in place, and a
// turned one is a new frame.
func OrientFrame(f *codec.VideoFrame, exifOrientation int) *codec.VideoFrame {
	if f == nil {
		return nil
	}
	return applyOrientation(f, exifOrientation)
}

const (
	// maxEXIF bounds how much of a PNG or WebP EXIF chunk is read. The
	// orientation is in the first directory, which writers put at the
	// start; what follows it (thumbnails, maker notes) is not needed.
	maxEXIF = 1 << 20
	// maxPNGChunks and maxWebPChunks bound the walk to the EXIF chunk,
	// which may follow the picture. A PNG's picture is cut into chunks of
	// a few kilobytes each, so a large one has many.
	maxPNGChunks  = 1 << 20
	maxWebPChunks = 1 << 16
)

// EXIFOrientation reads the EXIF orientation tag (0x0112) of a still image:
// from the APP1 segment of a JPEG, the eXIf chunk of a PNG (PNG third
// edition) or the EXIF chunk of a WebP file in the extended format (RFC
// 9649). It returns 1 (upright) when the tag is absent, the data is none of
// these, or anything is malformed. Of a JPEG it reads only the leading
// marker segments; of the others, the chunk headers up to the EXIF chunk,
// which may come after the picture (seeking past the rest when r is an
// io.Seeker).
func EXIFOrientation(r io.Reader) int {
	if r == nil {
		return 1
	}
	var b [12]byte
	if _, err := io.ReadFull(r, b[:2]); err != nil {
		return 1
	}
	if b[0] == 0xFF && b[1] == 0xD8 {
		return jpegOrientation(r)
	}
	if _, err := io.ReadFull(r, b[2:8]); err != nil {
		return 1
	}
	if string(b[:8]) == "\x89PNG\r\n\x1a\n" {
		return pngOrientation(r)
	}
	if _, err := io.ReadFull(r, b[8:]); err != nil {
		return 1
	}
	if string(b[:4]) == "RIFF" && string(b[8:]) == "WEBP" {
		// The RIFF size counts everything after itself: the form type
		// just read, then the chunks.
		return webpOrientation(r, int64(binary.LittleEndian.Uint32(b[4:]))-4)
	}
	return 1
}

// skip moves r forward by n bytes, by seeking when it can. Seeking past the
// end is not an error: the read that follows fails instead.
func skip(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	if s, ok := r.(io.Seeker); ok {
		_, err := s.Seek(n, io.SeekCurrent)
		return err
	}
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

// readEXIF reads the start of an EXIF chunk of n bytes, as much as maxEXIF
// allows, growing the buffer only as data arrives so that a chunk size
// that the file does not back allocates nothing.
func readEXIF(r io.Reader, n int64) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, min(n, maxEXIF)))
	return data
}

// pngOrientation reads the orientation from a PNG stream that is past its
// signature. The eXIf chunk holds a TIFF structure and may come before or
// after the picture (IDAT); chunk checksums are not verified.
func pngOrientation(r io.Reader) int {
	var h [8]byte
	for chunk := 0; chunk < maxPNGChunks; chunk++ {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return 1
		}
		n := int64(binary.BigEndian.Uint32(h[:4]))
		if n > 1<<31-1 {
			return 1 // PNG: a chunk length is at most 2^31-1
		}
		switch string(h[4:]) {
		case "IEND":
			return 1
		case "eXIf":
			if o := tiffOrientation(readEXIF(r, n)); o != 0 {
				return o
			}
			return 1 // a PNG has at most one
		}
		if skip(r, n+4) != nil { // the data and its CRC
			return 1
		}
	}
	return 1
}

// webpOrientation reads the orientation from a WebP stream that is past
// its 12-byte RIFF header and has left bytes of chunks to come, by the RIFF
// size. The EXIF chunk follows the picture. Its payload is a TIFF
// structure; some writers put the "Exif\0\0" of a JPEG's APP1 segment in
// front, which is accepted. The chunk is read whether or not the VP8X
// header's EXIF flag announces it.
func webpOrientation(r io.Reader, left int64) int {
	var h [8]byte
	for chunk := 0; chunk < maxWebPChunks && left >= 8; chunk++ {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return 1
		}
		n := int64(binary.LittleEndian.Uint32(h[4:]))
		left -= 8 + n + n&1 // chunks are padded to an even length
		if string(h[:4]) == "EXIF" {
			data := readEXIF(r, n)
			if o := parseEXIFOrientation(data); o != 0 {
				return o
			}
			if o := tiffOrientation(data); o != 0 {
				return o
			}
			return 1
		}
		if skip(r, n+n&1) != nil {
			return 1
		}
	}
	return 1
}

// jpegOrientation reads the orientation from a JPEG stream that is past its
// SOI marker.
func jpegOrientation(r io.Reader) int {
	var b [2]byte
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
	if len(d) < 6 || string(d[:6]) != "Exif\x00\x00" {
		return 0
	}
	return tiffOrientation(d[6:])
}

// tiffOrientation returns the orientation tag of the first image file
// directory of a TIFF structure, which is what EXIF data is, or 0.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
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
