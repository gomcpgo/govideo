package kernels

import "math"

const (
	// ToneLinearSize is the number of intervals of ToneMap.Linear.
	ToneLinearSize = 4096
	// ToneCurveSize is the number of intervals of the curves of a ToneMap
	// that are read between their entries.
	ToneCurveSize = 1024
)

// ToneCurve is a function on 0..1 as ToneCurveSize+1 values at even steps,
// read with straight lines between them. (The entry after the last is there
// so that reading at 1 needs no test.)
type ToneCurve [ToneCurveSize + 2]float32

// at reads the curve at u, which must be in 0..1.
func (c *ToneCurve) at(u float32) float32 {
	x := u * ToneCurveSize
	i := int(x)
	a := c[i]
	return a + (c[i+1]-a)*(x-float32(i))
}

// ToneMap holds what ToneMapRow needs to bring a high dynamic range picture
// to a standard one: the coefficients and tables of one kind of source. The
// process package builds it, and says there what each step is for (SPEC
// §7.1).
type ToneMap struct {
	// Source Y'CbCr, samples filling 16 bits from the top, to R'G'B' in
	// 0..1: R' = (Y - YOff)*YMul + RCr*(Cr - 32768), and so on.
	YOff, YMul         float32
	RCr, GCb, GCr, BCb float32
	// Linear undoes the source's transfer function: entry i is the light of
	// the signal i/ToneLinearSize, and it is read with straight lines
	// between the entries, as a ToneCurve is. For PQ that is the light of
	// the display, as a fraction of the 1000 cd/m² the tone curve is laid
	// out on, which it can pass. For HLG it is the light of the scene.
	//
	// It has to be read that finely. A saturated colour has a component
	// near zero that is the difference of large ones once it is taken to
	// other primaries, and the gamma of the output is steepest at zero: an
	// error of a thousandth in the light of one component, which reading
	// the nearest entry gave, was four steps of the output there.
	Linear [ToneLinearSize + 2]float32
	// M takes linear RGB of the source's primaries to linear RGB of
	// BT.709's, row by row. WR, WG and WB are the luminance of the latter.
	M          [9]float32
	WR, WG, WB float32
	// OOTF, when HLG is set, turns scene light into display light, a
	// fraction of the same 1000 cd/m²: the factor for a pixel of luminance
	// Y is read at Y^(1/4).
	HLG  bool
	OOTF ToneCurve
	// Gain is the tone curve: for a pixel whose largest component of
	// display light is m, the factor that takes its components to the
	// light of a standard display, 0..1, read at m^(1/4).
	Gain ToneCurve
	// Gamma encodes for the display: entry s is (s*s)^(1/2.4).
	Gamma ToneCurve
	// R'G'B' of the result to BT.709 Y'CbCr: Y' = 16 + YR*R' + YG*G' +
	// YB*B', and the two colour differences from the unscaled luma by PbMul
	// and PrMul.
	YR, YG, YB   float32
	PbMul, PrMul float32
}

// root4 returns the fourth root of x. A curve that spans many decades of
// light is stored against it, which spreads its entries about as evenly as
// the eye weighs them and costs two square roots, not a logarithm.
func root4(x float32) float32 {
	return float32(math.Sqrt(float64(float32(math.Sqrt(float64(x))))))
}

func sqrt32(x float32) float32 { return float32(math.Sqrt(float64(x))) }

// ToneMapRow tone-maps n pixels of a row: y, with the 4:2:0 chroma rows cb
// and cr (the nearest sample is used), to BT.709 limited-range luma in dstY.
// The two colour differences of each pixel go to pb and pr, already scaled
// to chroma steps, for ToneMapChromaRow to average.
//
// For each pixel: to R'G'B', to linear light, to BT.709 primaries; a colour
// outside those is brought to their edge at its own luminance; HLG scene
// light is made display light; every component is scaled by the tone curve's
// factor for the largest of them, so that the colour keeps its hue and
// saturation and none can pass the top of the range; and the result is
// encoded for a display of gamma 2.4.
func ToneMapRow(dstY []byte, pb, pr []float32, y, cb, cr []uint16, n int, t *ToneMap) {
	dstY, pb, pr, y = dstY[:n], pb[:n], pr[:n], y[:n]
	lin := func(v float32) float32 {
		// The comparisons are written so that a NaN, which bad input cannot
		// make here but a future caller might, falls to zero.
		if !(v > 0) {
			return t.Linear[0]
		}
		if v >= 1 {
			return t.Linear[ToneLinearSize]
		}
		x := v * ToneLinearSize
		i := int(x)
		a := t.Linear[i]
		return a + (t.Linear[i+1]-a)*(x-float32(i))
	}
	for i := range dstY {
		c := i >> 1
		u, v := float32(cb[c])-32768, float32(cr[c])-32768
		l := (float32(y[i]) - t.YOff) * t.YMul
		sr, sg, sb := lin(l+t.RCr*v), lin(l+t.GCb*u+t.GCr*v), lin(l+t.BCb*u)

		r := t.M[0]*sr + t.M[1]*sg + t.M[2]*sb
		g := t.M[3]*sr + t.M[4]*sg + t.M[5]*sb
		b := t.M[6]*sr + t.M[7]*sg + t.M[8]*sb
		lum := max(t.WR*r+t.WG*g+t.WB*b, 0)
		if lo := min(r, g, b); lo < 0 {
			// Outside BT.709: towards the grey of the same luminance until
			// no component is negative.
			k := lum / (lum - lo)
			r, g, b = lum+k*(r-lum), lum+k*(g-lum), lum+k*(b-lum)
		}
		if t.HLG {
			f := t.OOTF.at(root4(min(lum, 1)))
			r, g, b = r*f, g*f, b*f
		}
		var gain float32
		if m := max(r, g, b, 0); m >= 1 {
			gain = 1 / m // brighter than the curve goes: its top, in the pixel's colour
		} else {
			gain = t.Gain.at(root4(m))
		}
		r, g, b = r*gain, g*gain, b*gain
		// Rounding can leave a component a hair outside 0..1.
		r = t.Gamma.at(sqrt32(min(max(r, 0), 1)))
		g = t.Gamma.at(sqrt32(min(max(g, 0), 1)))
		b = t.Gamma.at(sqrt32(min(max(b, 0), 1)))

		luma := t.YR*r + t.YG*g + t.YB*b
		dstY[i] = byte(16.5 + luma)
		luma *= 1.0 / 219
		pb[i], pr[i] = (b-luma)*t.PbMul, (r-luma)*t.PrMul
	}
}

// ToneMapChromaRow makes one 4:2:0 chroma row of (w+1)/2 samples from the
// colour differences ToneMapRow gave for two rows of w pixels, by averaging
// each 2x2 block. The second pair of rows may be the first.
func ToneMapChromaRow(dstCb, dstCr []byte, pb0, pr0, pb1, pr1 []float32, w int) {
	for i := range (w + 1) / 2 {
		x0 := 2 * i
		x1 := min(x0+1, w-1)
		dstCb[i] = clip8(int32(128.5 + 0.25*(pb0[x0]+pb0[x1]+pb1[x0]+pb1[x1])))
		dstCr[i] = clip8(int32(128.5 + 0.25*(pr0[x0]+pr0[x1]+pr1[x0]+pr1[x1])))
	}
}
