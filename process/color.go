package process

import (
	"image"
	"image/color"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

const (
	kr709, kb709   = 0.2126, 0.0722
	kr601, kb601   = 0.299, 0.114
	kr2020, kb2020 = 0.2627, 0.0593
)

var rgbTo709 = kernels.NewRGBToYUV(kr709, kb709, false)

// lumaWeights resolves an H.273 matrix code to (Kr, Kb). Unspecified codes
// follow the usual convention: BT.601 below 720 lines, BT.709 otherwise.
func lumaWeights(matrix, height int) (kr, kb float64, is709 bool) {
	switch matrix {
	case 1:
		return kr709, kb709, true
	case 5, 6:
		return kr601, kb601, false
	case 9, 10:
		return kr2020, kb2020, false
	}
	if height < 720 {
		return kr601, kb601, false
	}
	return kr709, kb709, true
}

// ConvertTo709Limited converts a frame from its tagged matrix and range to
// BT.709 limited range. A frame already in that format is returned as is.
func ConvertTo709Limited(f *codec.VideoFrame) *codec.VideoFrame {
	if f == nil {
		return nil
	}
	kr, kb, is709 := lumaWeights(f.Matrix, f.Height)
	if is709 && !f.FullRange {
		if f.Matrix == 1 {
			return f
		}
		g := *f // same planes, explicit tag
		g.Matrix = 1
		return &g
	}
	c := kernels.NewYUVConv(kr, kb, f.FullRange, kr709, kb709, false)
	out := codec.NewVideoFrame(f.Width, f.Height)
	cw, ch := f.ChromaSize()
	if f.Width == 0 || f.Height == 0 {
		return out
	}
	for y := 0; y < f.Height; y++ {
		cy := y >> 1
		kernels.ConvertLumaRow(out.Y[y*out.YStride:], f.Y[y*f.YStride:],
			f.Cb[cy*f.CStride:], f.Cr[cy*f.CStride:], f.Width, &c)
	}
	for y := 0; y < ch; y++ {
		kernels.ConvertChromaRow(out.Cb[y*out.CStride:], out.Cr[y*out.CStride:],
			f.Cb[y*f.CStride:], f.Cr[y*f.CStride:], cw, &c)
	}
	return out
}

// ToRGBA converts a frame to an opaque RGBA image using the frame's matrix
// and range tags (nearest-neighbour chroma).
func ToRGBA(f *codec.VideoFrame) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	if f.Width == 0 || f.Height == 0 {
		return img
	}
	kr, kb, _ := lumaWeights(f.Matrix, f.Height)
	c := kernels.NewYUVToRGB(kr, kb, f.FullRange)
	for y := 0; y < f.Height; y++ {
		cy := y >> 1
		kernels.YUVToRGBARow(img.Pix[y*img.Stride:], f.Y[y*f.YStride:],
			f.Cb[cy*f.CStride:], f.Cr[cy*f.CStride:], f.Width, &c)
	}
	return img
}

// backdrop16 is a backdrop colour as the 16-bit premultiplied R'G'B' that
// color.Color reports. Its alpha is ignored, which amounts to the colour
// itself lying over black; nil is black. The top byte of each component is
// what colorTo709 converts, so a fully transparent pixel composited over the
// backdrop comes out exactly as the padding Fit draws in the same colour.
func backdrop16(bg color.Color) (c [3]uint32) {
	if bg != nil {
		c[0], c[1], c[2], _ = bg.RGBA()
	}
	return c
}

// colorTo709 converts a colour (alpha ignored, composited over black) to
// BT.709 limited-range Y'CbCr.
func colorTo709(c color.Color) (y, cb, cr byte) {
	if c == nil {
		return 16, 128, 128
	}
	bg := backdrop16(c)
	px := [4]byte{byte(bg[0] >> 8), byte(bg[1] >> 8), byte(bg[2] >> 8), 255}
	var yy, u, v [1]byte
	kernels.RGBAToYRow(yy[:], px[:], 1, &rgbTo709)
	kernels.RGBAToCbCrRow(u[:], v[:], px[:], px[:], 1, &rgbTo709)
	return yy[0], u[0], v[0]
}

// over16 composites one premultiplied 16-bit component over the backdrop
// component bg, given the pixel's 16-bit alpha, and returns the top byte.
func over16(c, a, bg uint32) byte {
	if a < 0xffff && bg != 0 {
		c = min(c+(bg*(0xffff-a)+0x7fff)/0xffff, 0xffff)
	}
	return byte(c >> 8)
}

// FromImage converts any image to a BT.709 limited-range 4:2:0 frame.
// Transparent pixels are composited over black; FromImageOver takes another
// backdrop. JPEG-style *image.YCbCr (BT.601 full range) is converted without
// an RGB round trip.
func FromImage(img image.Image) *codec.VideoFrame { return FromImageOver(img, nil) }

// FromImageOver is FromImage with transparent and translucent pixels
// composited over bg (nil is black; bg's own alpha is ignored). The blend is
// done on the gamma-encoded values, as browsers and image editors do. A
// fully transparent pixel becomes exactly the colour Fit pads with, so a
// still with a transparent margin meets the letterbox bars without a seam.
func FromImageOver(img image.Image, bg color.Color) *codec.VideoFrame {
	if o, ok := img.(*orientedImage); ok {
		return applyOrientation(FromImageOver(o.src, bg), o.o)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return codec.NewVideoFrame(0, 0)
	}
	switch m := img.(type) {
	case *image.YCbCr:
		return fromYCbCr(m)
	case *image.NYCbCrA:
		return fromNYCbCrA(m, true, bg)
	case *image.Gray:
		return fromGray(m)
	}

	bg16 := backdrop16(bg)
	bg8 := [3]uint32{bg16[0] >> 8, bg16[1] >> 8, bg16[2] >> 8}
	black := bg8 == [3]uint32{}

	// Everything else goes through RGBX rows already composited over bg.
	var row func(y int, buf []byte) []byte
	switch m := img.(type) {
	case *image.RGBA:
		row = func(y int, buf []byte) []byte {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			src := m.Pix[o : o+w*4]
			if black {
				return src // premultiplied values are the picture over black
			}
			for i := 0; i < w*4; i += 4 {
				na := 255 - uint32(src[i+3])
				if na == 0 {
					buf[i], buf[i+1], buf[i+2] = src[i], src[i+1], src[i+2]
					continue
				}
				// min guards against invalid data (a component above its alpha).
				buf[i] = byte(min(uint32(src[i])+(bg8[0]*na+127)/255, 255))
				buf[i+1] = byte(min(uint32(src[i+1])+(bg8[1]*na+127)/255, 255))
				buf[i+2] = byte(min(uint32(src[i+2])+(bg8[2]*na+127)/255, 255))
			}
			return buf
		}
	case *image.NRGBA:
		row = func(y int, buf []byte) []byte {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			src := m.Pix[o : o+w*4]
			for i := 0; i < w*4; i += 4 {
				a := uint32(src[i+3])
				if a == 255 {
					buf[i], buf[i+1], buf[i+2] = src[i], src[i+1], src[i+2]
					continue
				}
				na := 255 - a
				buf[i] = byte((uint32(src[i])*a + bg8[0]*na + 127) / 255)
				buf[i+1] = byte((uint32(src[i+1])*a + bg8[1]*na + 127) / 255)
				buf[i+2] = byte((uint32(src[i+2])*a + bg8[2]*na + 127) / 255)
			}
			return buf
		}
	case *image.Paletted:
		pal := make([][3]byte, len(m.Palette))
		for i, c := range m.Palette {
			r, g, bb, a := c.RGBA()
			pal[i] = [3]byte{over16(r, a, bg16[0]), over16(g, a, bg16[1]), over16(bb, a, bg16[2])}
		}
		row = func(y int, buf []byte) []byte {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			for x, idx := range m.Pix[o : o+w] {
				var p [3]byte // an index past the palette is black, as image/png makes it
				if int(idx) < len(pal) {
					p = pal[idx]
				}
				buf[x*4], buf[x*4+1], buf[x*4+2] = p[0], p[1], p[2]
			}
			return buf
		}
	default:
		row = func(y int, buf []byte) []byte {
			for x := 0; x < w; x++ {
				r, g, bb, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				buf[x*4], buf[x*4+1], buf[x*4+2] = over16(r, a, bg16[0]), over16(g, a, bg16[1]), over16(bb, a, bg16[2])
			}
			return buf
		}
	}

	f := codec.NewVideoFrame(w, h)
	buf0, buf1 := make([]byte, w*4), make([]byte, w*4)
	for y := 0; y < h; y += 2 {
		r0 := row(y, buf0)
		r1 := r0
		kernels.RGBAToYRow(f.Y[y*f.YStride:], r0, w, &rgbTo709)
		if y+1 < h {
			r1 = row(y+1, buf1)
			kernels.RGBAToYRow(f.Y[(y+1)*f.YStride:], r1, w, &rgbTo709)
		}
		cy := y >> 1
		kernels.RGBAToCbCrRow(f.Cb[cy*f.CStride:], f.Cr[cy*f.CStride:], r0, r1, w, &rgbTo709)
	}
	return f
}

func fromGray(m *image.Gray) *codec.VideoFrame {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	var lut [256]byte
	for i := range lut {
		lut[i] = byte(16 + (i*219+127)/255)
	}
	f := codec.NewVideoFrame(w, h)
	for y := 0; y < h; y++ {
		o := m.PixOffset(b.Min.X, b.Min.Y+y)
		dst := f.Y[y*f.YStride : y*f.YStride+w]
		for x, v := range m.Pix[o : o+w] {
			dst[x] = lut[v]
		}
	}
	return f
}

// fromYCbCr handles Go's JPEG-style Y'CbCr (BT.601, full range) at any
// chroma subsampling by resampling chroma to 4:2:0 and converting the matrix.
func fromYCbCr(m *image.YCbCr) *codec.VideoFrame { return convertYCbCr(m, true) }

// FromVideoYCbCr converts a Y'CbCr image whose samples are BT.601 limited
// range (16..235), as lossy WebP stores them. Go's image.YCbCr type has no
// way to say so, and FromImage assumes the full range JPEG uses. For such
// an image with an alpha plane see FromVideoNYCbCrA.
func FromVideoYCbCr(m *image.YCbCr) *codec.VideoFrame { return convertYCbCr(m, false) }

// FromVideoNYCbCrA is FromVideoYCbCr for an image with an alpha plane, as
// lossy WebP with transparency decodes: the colour samples are BT.601
// limited range, and the picture is composited over bg (nil is black) as
// FromImageOver does.
func FromVideoNYCbCrA(m *image.NYCbCrA, bg color.Color) *codec.VideoFrame {
	return fromNYCbCrA(m, false, bg)
}

// fromNYCbCrA converts the colour planes like convertYCbCr and blends the
// result over bg by the alpha plane. The samples are not premultiplied.
// Blending Y'CbCr is the same as blending the R'G'B' it stands for, because
// the conversion between them is affine and the two weights sum to one.
func fromNYCbCrA(m *image.NYCbCrA, fullRange bool, bg color.Color) *codec.VideoFrame {
	f := convertYCbCr(&m.YCbCr, fullRange) // always a new frame, so it can be blended in place
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return f
	}
	by, bcb, bcr := colorTo709(bg)
	for y := 0; y < h; y++ {
		o := m.AOffset(b.Min.X, b.Min.Y+y)
		row := f.Y[y*f.YStride : y*f.YStride+w]
		for x, a := range m.A[o : o+w] {
			if a != 255 {
				row[x] = byte((uint32(row[x])*uint32(a) + uint32(by)*uint32(255-a) + 127) / 255)
			}
		}
	}
	// A chroma sample covers a 2x2 block, so it takes the block's mean alpha.
	cw, ch := f.ChromaSize()
	for cy := 0; cy < ch; cy++ {
		o0 := m.AOffset(b.Min.X, b.Min.Y+cy*2)
		o1 := o0
		if cy*2+1 < h {
			o1 = m.AOffset(b.Min.X, b.Min.Y+cy*2+1)
		}
		a0, a1 := m.A[o0:o0+w], m.A[o1:o1+w]
		dcb := f.Cb[cy*f.CStride : cy*f.CStride+cw]
		dcr := f.Cr[cy*f.CStride : cy*f.CStride+cw]
		for cx := range dcb {
			x0 := cx * 2
			x1 := min(x0+1, w-1)
			a := uint32(a0[x0]) + uint32(a0[x1]) + uint32(a1[x0]) + uint32(a1[x1])
			if a != 4*255 {
				na := 4*255 - a
				dcb[cx] = byte((uint32(dcb[cx])*a + uint32(bcb)*na + 2*255) / (4 * 255))
				dcr[cx] = byte((uint32(dcr[cx])*a + uint32(bcr)*na + 2*255) / (4 * 255))
			}
		}
	}
	return f
}

func convertYCbCr(m *image.YCbCr, fullRange bool) *codec.VideoFrame {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	f := codec.NewVideoFrame(w, h)
	if w <= 0 || h <= 0 {
		return f
	}
	f.Matrix, f.FullRange = 6, fullRange
	for y := 0; y < h; y++ {
		o := m.YOffset(b.Min.X, b.Min.Y+y)
		copy(f.Y[y*f.YStride:y*f.YStride+w], m.Y[o:o+w])
	}
	cw, ch := f.ChromaSize()
	fast := m.SubsampleRatio == image.YCbCrSubsampleRatio420 && b.Min.X&1 == 0 && b.Min.Y&1 == 0
	for cy := 0; cy < ch; cy++ {
		y0 := b.Min.Y + cy*2
		y1 := y0 + 1
		if y1 >= b.Max.Y {
			y1 = y0
		}
		dcb := f.Cb[cy*f.CStride : cy*f.CStride+cw]
		dcr := f.Cr[cy*f.CStride : cy*f.CStride+cw]
		if fast {
			o := m.COffset(b.Min.X, y0)
			copy(dcb, m.Cb[o:o+cw])
			copy(dcr, m.Cr[o:o+cw])
			continue
		}
		for cx := 0; cx < cw; cx++ {
			x0 := b.Min.X + cx*2
			x1 := x0 + 1
			if x1 >= b.Max.X {
				x1 = x0
			}
			o00, o01 := m.COffset(x0, y0), m.COffset(x1, y0)
			o10, o11 := m.COffset(x0, y1), m.COffset(x1, y1)
			dcb[cx] = byte((int(m.Cb[o00]) + int(m.Cb[o01]) + int(m.Cb[o10]) + int(m.Cb[o11]) + 2) / 4)
			dcr[cx] = byte((int(m.Cr[o00]) + int(m.Cr[o01]) + int(m.Cr[o10]) + int(m.Cr[o11]) + 2) / 4)
		}
	}
	return ConvertTo709Limited(f)
}
