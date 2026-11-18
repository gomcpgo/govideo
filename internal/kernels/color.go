package kernels

import "math"

// ColorShift is the number of fractional bits in the fixed-point colour
// coefficients.
const ColorShift = 14

const colorRound = 1 << (ColorShift - 1)

func clip8(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func fix(v float64) int32 { return int32(math.Round(v * (1 << ColorShift))) }

// ranges returns the luma offset, luma scale and chroma scale of 8-bit YUV.
func ranges(full bool) (yoff, yscale, cscale float64) {
	if full {
		return 0, 255, 255
	}
	return 16, 219, 224
}

// RGBToYUV holds fixed-point coefficients converting 8-bit R'G'B' to 8-bit
// Y'CbCr.
type RGBToYUV struct {
	YOff          int32
	YR, YG, YB    int32
	CbR, CbG, CbB int32
	CrR, CrG, CrB int32
}

// NewRGBToYUV builds coefficients for the luma weights kr, kb and the given
// range.
func NewRGBToYUV(kr, kb float64, full bool) RGBToYUV {
	yoff, ys, cs := ranges(full)
	c := RGBToYUV{YOff: int32(yoff)}
	c.YR = fix(kr * ys / 255)
	c.YB = fix(kb * ys / 255)
	c.YG = fix(ys/255) - c.YR - c.YB // keep the row sum exact so white maps to peak
	c.CbR = fix(-kr / (2 * (1 - kb)) * cs / 255)
	c.CbB = fix(0.5 * cs / 255)
	c.CbG = -c.CbR - c.CbB // rows sum to zero so greys stay neutral
	c.CrR = fix(0.5 * cs / 255)
	c.CrB = fix(-kb / (2 * (1 - kr)) * cs / 255)
	c.CrG = -c.CrR - c.CrB
	return c
}

// RGBAToYRow converts n RGBX pixels (4 bytes each, alpha ignored) to luma.
func RGBAToYRow(dst []byte, rgba []byte, n int, c *RGBToYUV) {
	rgba = rgba[:n*4]
	dst = dst[:n]
	for i := range dst {
		r, g, b := int32(rgba[i*4]), int32(rgba[i*4+1]), int32(rgba[i*4+2])
		dst[i] = clip8(c.YOff + (c.YR*r+c.YG*g+c.YB*b+colorRound)>>ColorShift)
	}
}

// RGBAToCbCrRow converts two RGBX rows of w pixels to one 4:2:0 chroma row
// of (w+1)/2 samples by averaging each 2x2 block. row1 may alias row0.
func RGBAToCbCrRow(dstCb, dstCr []byte, row0, row1 []byte, w int, c *RGBToYUV) {
	cw := (w + 1) / 2
	const sh = ColorShift + 2
	const rnd = 1 << (sh - 1)
	for i := 0; i < cw; i++ {
		x0 := i * 2
		x1 := x0 + 1
		if x1 >= w {
			x1 = x0
		}
		a, b2 := x0*4, x1*4
		r := int32(row0[a]) + int32(row0[b2]) + int32(row1[a]) + int32(row1[b2])
		g := int32(row0[a+1]) + int32(row0[b2+1]) + int32(row1[a+1]) + int32(row1[b2+1])
		b := int32(row0[a+2]) + int32(row0[b2+2]) + int32(row1[a+2]) + int32(row1[b2+2])
		dstCb[i] = clip8(128 + (c.CbR*r+c.CbG*g+c.CbB*b+rnd)>>sh)
		dstCr[i] = clip8(128 + (c.CrR*r+c.CrG*g+c.CrB*b+rnd)>>sh)
	}
}

// YUVToRGB holds fixed-point coefficients converting 8-bit Y'CbCr to R'G'B'.
type YUVToRGB struct {
	YOff, YMul         int32
	RCr, GCb, GCr, BCb int32
}

// NewYUVToRGB builds coefficients for the luma weights kr, kb and range.
func NewYUVToRGB(kr, kb float64, full bool) YUVToRGB {
	kg := 1 - kr - kb
	yoff, ys, cs := ranges(full)
	return YUVToRGB{
		YOff: int32(yoff),
		YMul: fix(255 / ys),
		RCr:  fix(2 * (1 - kr) * 255 / cs),
		BCb:  fix(2 * (1 - kb) * 255 / cs),
		GCb:  fix(-kb * 2 * (1 - kb) / kg * 255 / cs),
		GCr:  fix(-kr * 2 * (1 - kr) / kg * 255 / cs),
	}
}

// YUVToRGBARow converts n luma samples with 4:2:0 chroma (nearest sample)
// to opaque RGBA.
func YUVToRGBARow(dst []byte, y, cb, cr []byte, n int, c *YUVToRGB) {
	for i := 0; i < n; i++ {
		l := (int32(y[i])-c.YOff)*c.YMul + colorRound
		u := int32(cb[i>>1]) - 128
		v := int32(cr[i>>1]) - 128
		dst[i*4] = clip8((l + c.RCr*v) >> ColorShift)
		dst[i*4+1] = clip8((l + c.GCb*u + c.GCr*v) >> ColorShift)
		dst[i*4+2] = clip8((l + c.BCb*u) >> ColorShift)
		dst[i*4+3] = 255
	}
}

// YUVConv holds a fixed-point Y'CbCr to Y'CbCr conversion (matrix and/or
// range change). Chroma never depends on luma, so that column is omitted.
type YUVConv struct {
	SrcYOff, DstYOff int32
	A00, A01, A02    int32
	A11, A12         int32
	A21, A22         int32
}

// NewYUVConv builds the conversion from (srcKr, srcKb, srcFull) to
// (dstKr, dstKb, dstFull).
func NewYUVConv(srcKr, srcKb float64, srcFull bool, dstKr, dstKb float64, dstFull bool) YUVConv {
	syoff, sys, scs := ranges(srcFull)
	dyoff, dys, dcs := ranges(dstFull)
	srcKg := 1 - srcKr - srcKb
	dstKg := 1 - dstKr - dstKb
	// f maps source deltas (Y-yoff, Cb-128, Cr-128) to destination deltas.
	f := func(dy, dcb, dcr float64) (float64, float64, float64) {
		yn, pb, pr := dy/sys, dcb/scs, dcr/scs
		r := yn + 2*(1-srcKr)*pr
		b := yn + 2*(1-srcKb)*pb
		g := (yn - srcKr*r - srcKb*b) / srcKg
		y2 := dstKr*r + dstKg*g + dstKb*b
		return y2 * dys, (b - y2) / (2 * (1 - dstKb)) * dcs, (r - y2) / (2 * (1 - dstKr)) * dcs
	}
	var c YUVConv
	c.SrcYOff, c.DstYOff = int32(syoff), int32(dyoff)
	y0, _, _ := f(1, 0, 0)
	y1, cb1, cr1 := f(0, 1, 0)
	y2, cb2, cr2 := f(0, 0, 1)
	c.A00, c.A01, c.A02 = fix(y0), fix(y1), fix(y2)
	c.A11, c.A12 = fix(cb1), fix(cb2)
	c.A21, c.A22 = fix(cr1), fix(cr2)
	return c
}

// ConvertLumaRow converts n luma samples; cb and cr are the co-sited 4:2:0
// chroma rows of the source.
func ConvertLumaRow(dst, y, cb, cr []byte, n int, c *YUVConv) {
	for i := 0; i < n; i++ {
		u := int32(cb[i>>1]) - 128
		v := int32(cr[i>>1]) - 128
		dst[i] = clip8(c.DstYOff + (c.A00*(int32(y[i])-c.SrcYOff)+c.A01*u+c.A02*v+colorRound)>>ColorShift)
	}
}

// ConvertChromaRow converts n chroma sample pairs.
func ConvertChromaRow(dstCb, dstCr, cb, cr []byte, n int, c *YUVConv) {
	for i := 0; i < n; i++ {
		u := int32(cb[i]) - 128
		v := int32(cr[i]) - 128
		dstCb[i] = clip8(128 + (c.A11*u+c.A12*v+colorRound)>>ColorShift)
		dstCr[i] = clip8(128 + (c.A21*u+c.A22*v+colorRound)>>ColorShift)
	}
}
