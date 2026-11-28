package process

import (
	"fmt"
	"math"
	"runtime"
	"sync"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

// The transfer functions of high dynamic range video, by their H.273 codes.
const (
	TransferPQ  = 16 // SMPTE ST 2084, the curve of HDR10 and Dolby Vision
	TransferHLG = 18 // ARIB STD-B67, hybrid log-gamma
)

const (
	hlgPeak  = 1000.0 // cd/m²: the display at which a PQ picture and an HLG one are the same
	hlgGamma = 1.2    // the system gamma of HLG on that display (BT.2100)
	sdrGamma = 2.4    // the display gamma of standard video (BT.1886)

	toneBands = 16 // chroma rows a worker takes at a time, at least
)

// HDR describes the signal of a high dynamic range picture: what its samples
// mean, beyond what the frame itself says of its matrix and range.
type HDR struct {
	// Transfer is the H.273 code of the transfer function: TransferPQ or
	// TransferHLG.
	Transfer int
	// Primaries is the H.273 code of the colour primaries: 9 for BT.2020, 1
	// for BT.709, 12 for P3 with a D65 white. Any other is taken for
	// BT.2020, which is what high dynamic range video is made in.
	Primaries int
}

// ToneMapper brings high dynamic range pictures to the working format: HLG
// or PQ Y'CbCr of more than 8 bits to 8-bit BT.709 limited range, with the
// highlights compressed into the standard range and the colours brought
// into the BT.709 gamut (SPEC §7.1). One ToneMapper serves the pictures of
// one stream; it is not for use by several goroutines at once.
//
// The tone curve is HLG's own. An HLG signal is a standard signal over the
// lower half of its range and bends over above that to hold the highlights,
// so that a standard display shown it has a usable picture (Rec. ITU-R
// BT.2100; Report ITU-R BT.2390): mid-tones about where a standard camera
// puts them, highlights rolled off, nothing clipped. For a grey, the picture
// made here is that one. Per pixel:
//
//   - The signal is made the light of a 1000 cd/m² display, the level at
//     which HLG and PQ are defined to agree: PQ by its EOTF, HLG by its
//     inverse OETF and its OOTF for that display. The two kinds of source
//     are alike from here on.
//   - The colour is taken to BT.709 primaries. One that BT.709 cannot show
//     is moved towards grey at its own luminance, just far enough.
//   - The curve gives the light of a standard display for a grey of that
//     display light: what the HLG signal of the grey shows on a display of
//     gamma 2.4 (Rec. ITU-R BT.1886). It is straight up to about 50 cd/m²,
//     where it takes 268 cd/m² for white, and is HLG's logarithm above, to
//     white at 1000 cd/m². Brighter light is white.
//   - A colour is scaled as a whole, by the curve's factor for the largest
//     of its three components. Its hue and saturation are kept, which a
//     curve applied to each component would wash out in the highlights, and
//     no component can pass the top of the range: a bright saturated light
//     stays saturated and gets darker, as a standard camera shows it.
//   - The result is encoded with the gamma of 2.4.
//
// The curve is fixed: it does not look at the picture, so the same source
// frame always gives the same output, wherever a clip is cut. What that
// costs, and the curves it was chosen over, are in SPEC §14.1.
type ToneMapper struct {
	hdr HDR
	t   kernels.ToneMap

	// What of the source frames the coefficients in t were made for.
	ready     bool
	matrix    int
	depth     int
	fullRange bool

	rows [][]float32 // each worker's rows of colour differences
}

// NewToneMapper returns a tone mapper for pictures of the signal hdr.
func NewToneMapper(hdr HDR) (*ToneMapper, error) {
	if hdr.Transfer != TransferPQ && hdr.Transfer != TransferHLG {
		return nil, fmt.Errorf("process: no tone mapping for transfer function %d", hdr.Transfer)
	}
	m := &ToneMapper{hdr: hdr}
	t := &m.t

	t.HLG = hdr.Transfer == TransferHLG
	for i := range t.Linear {
		e := math.Min(float64(i)/kernels.ToneLinearSize, 1)
		if t.HLG {
			t.Linear[i] = float32(hlgInverseOETF(e))
		} else {
			t.Linear[i] = float32(pqEOTF(e) / hlgPeak)
		}
	}
	for i := range t.Gamma {
		u := math.Min(float64(i)/kernels.ToneCurveSize, 1)
		// OOTF and Gain: entry i is for the light u^4 (see kernels.ToneMap).
		m := u * u * u * u
		t.OOTF[i] = float32(math.Pow(m, hlgGamma-1))
		t.Gain[i] = float32(math.Pow(3, sdrGamma/2)) // the slope of the curve at black
		if m > 0 {
			t.Gain[i] = float32(compat(m) / m)
		}
		t.Gamma[i] = float32(math.Pow(u, 2/sdrGamma))
	}

	src := primaries(hdr.Primaries)
	dst := primaries(1)
	toXYZ, fromXYZ := src.rgbToXYZ(), dst.rgbToXYZ().inverse()
	for r := range 3 {
		for c := range 3 {
			t.M[3*r+c] = float32(fromXYZ[r][0]*toXYZ[0][c] + fromXYZ[r][1]*toXYZ[1][c] + fromXYZ[r][2]*toXYZ[2][c])
		}
	}
	t.WR, t.WG, t.WB = kr709, 1-kr709-kb709, kb709
	t.YR, t.YG, t.YB = 219*kr709, 219*(1-kr709-kb709), 219*kb709
	t.PbMul, t.PrMul = 224/(2*(1-kb709)), 224/(2*(1-kr709))
	return m, nil
}

// source sets the coefficients that depend on how the source frames hold
// their samples.
func (m *ToneMapper) source(f *codec.DeepFrame) {
	if m.ready && m.matrix == f.Matrix && m.depth == f.Depth && m.fullRange == f.FullRange {
		return
	}
	m.ready, m.matrix, m.depth, m.fullRange = true, f.Matrix, f.Depth, f.FullRange
	// A matrix that is not stated is the one that goes with the primaries.
	kr, kb := kr2020, kb2020
	switch f.Matrix {
	case 1:
		kr, kb = kr709, kb709
	case 5, 6:
		kr, kb = kr601, kb601
	case 9, 10:
	default:
		if m.hdr.Primaries == 1 {
			kr, kb = kr709, kb709
		}
	}
	kg := 1 - kr - kb
	// Samples fill 16 bits from the top, so the limited range is the same
	// numbers at every depth (codec.DeepFrame).
	yoff, yscale, cscale := 4096.0, 56064.0, 57344.0
	if f.FullRange {
		depth := min(max(f.Depth, 8), 16)
		top := float64(uint32(1<<depth-1) << (16 - depth))
		yoff, yscale, cscale = 0, top, top
	}
	t := &m.t
	t.YOff, t.YMul = float32(yoff), float32(1/yscale)
	t.RCr, t.BCb = float32(2*(1-kr)/cscale), float32(2*(1-kb)/cscale)
	t.GCb, t.GCr = float32(-kb*2*(1-kb)/kg/cscale), float32(-kr*2*(1-kr)/kg/cscale)
}

// Map tone-maps src into a frame of the working format and returns it. The
// planes of dst are reused when they are large enough (it may be nil), as by
// codec.VideoFrame.Reuse.
//
// The rows are shared out among the processors: the work is the same for
// every pixel and none depends on another, so the result does not depend on
// how many there are.
func (m *ToneMapper) Map(dst *codec.VideoFrame, src *codec.DeepFrame) *codec.VideoFrame {
	w, h := src.Width, src.Height
	dst = dst.Reuse(w, h)
	if w <= 0 || h <= 0 {
		return dst
	}
	m.source(src)
	ch := (h + 1) / 2
	workers := min(runtime.GOMAXPROCS(0), (ch+toneBands-1)/toneBands)
	for len(m.rows) < workers {
		m.rows = append(m.rows, nil)
	}
	var wg sync.WaitGroup
	for i := range workers {
		if len(m.rows[i]) < 4*w {
			m.rows[i] = make([]float32, 4*w)
		}
		c0, c1 := ch*i/workers, ch*(i+1)/workers
		if i == workers-1 {
			m.band(dst, src, c0, c1, m.rows[i]) // this goroutine takes a share too
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.band(dst, src, c0, c1, m.rows[i])
		}()
	}
	wg.Wait()
	return dst
}

// band tone-maps the chroma rows c0 to c1-1 of src and the luma rows that go
// with them. buf holds four rows of colour differences.
func (m *ToneMapper) band(dst *codec.VideoFrame, src *codec.DeepFrame, c0, c1 int, buf []float32) {
	w, h := src.Width, src.Height
	pb0, pr0, pb1, pr1 := buf[:w], buf[w:2*w], buf[2*w:3*w], buf[3*w:4*w]
	for cy := c0; cy < c1; cy++ {
		cb, cr := src.Cb[cy*src.CStride:], src.Cr[cy*src.CStride:]
		y := 2 * cy
		kernels.ToneMapRow(dst.Y[y*dst.YStride:], pb0, pr0, src.Y[y*src.YStride:], cb, cr, w, &m.t)
		if y+1 < h {
			kernels.ToneMapRow(dst.Y[(y+1)*dst.YStride:], pb1, pr1, src.Y[(y+1)*src.YStride:], cb, cr, w, &m.t)
			kernels.ToneMapChromaRow(dst.Cb[cy*dst.CStride:], dst.Cr[cy*dst.CStride:], pb0, pr0, pb1, pr1, w)
		} else {
			kernels.ToneMapChromaRow(dst.Cb[cy*dst.CStride:], dst.Cr[cy*dst.CStride:], pb0, pr0, pb0, pr0, w)
		}
	}
}

// The constants of the PQ curve (SMPTE ST 2084; Rec. ITU-R BT.2100 table 4).
const (
	pqM1 = 2610.0 / 16384
	pqM2 = 2523.0 / 4096 * 128
	pqC1 = 3424.0 / 4096
	pqC2 = 2413.0 / 4096 * 32
	pqC3 = 2392.0 / 4096 * 32
)

// pqEOTF returns the luminance, in cd/m², of the PQ signal e in 0..1.
func pqEOTF(e float64) float64 {
	p := math.Pow(math.Max(e, 0), 1/pqM2)
	return 10000 * math.Pow(math.Max(p-pqC1, 0)/(pqC2-pqC3*p), 1/pqM1)
}

// The constants of the HLG curve (ARIB STD-B67; Rec. ITU-R BT.2100 table 5).
const (
	hlgA = 0.17883277
	hlgB = 1 - 4*hlgA
)

var hlgC = 0.5 - hlgA*math.Log(4*hlgA)

// hlgInverseOETF returns the scene light, in 0..1, of the HLG signal e in
// 0..1.
func hlgInverseOETF(e float64) float64 {
	if e <= 0.5 {
		return e * e / 3
	}
	return (math.Exp((e-hlgC)/hlgA) + hlgB) / 12
}

// compat returns the light of a standard display, in 0..1, for grey of the
// display light m, a fraction of hlgPeak.
func compat(m float64) float64 {
	return math.Pow(hlgOETF(math.Pow(math.Min(m, 1), 1/hlgGamma)), sdrGamma)
}

// hlgOETF returns the HLG signal of scene light in 0..1.
func hlgOETF(s float64) float64 {
	if s <= 1.0/12 {
		return math.Sqrt(3 * s)
	}
	return hlgA*math.Log(12*s-hlgB) + hlgC
}

// chromaticities are the CIE xy coordinates of three primaries and a white.
type chromaticities struct{ rx, ry, gx, gy, bx, by, wx, wy float64 }

// primaries returns the chromaticities of an H.273 colour primaries code;
// BT.2020's for one that is not known here.
func primaries(code int) chromaticities {
	const wx, wy = 0.3127, 0.3290 // D65
	switch code {
	case 1:
		return chromaticities{0.640, 0.330, 0.300, 0.600, 0.150, 0.060, wx, wy}
	case 12:
		return chromaticities{0.680, 0.320, 0.265, 0.690, 0.150, 0.060, wx, wy}
	}
	return chromaticities{0.708, 0.292, 0.170, 0.797, 0.131, 0.046, wx, wy}
}

type matrix3 [3][3]float64

// rgbToXYZ returns the matrix from linear RGB of the primaries to CIE XYZ,
// in which their white has a luminance of 1 (SMPTE RP 177).
func (c chromaticities) rgbToXYZ() matrix3 {
	xyz := func(x, y float64) [3]float64 { return [3]float64{x / y, 1, (1 - x - y) / y} }
	r, g, b, w := xyz(c.rx, c.ry), xyz(c.gx, c.gy), xyz(c.bx, c.by), xyz(c.wx, c.wy)
	p := matrix3{{r[0], g[0], b[0]}, {r[1], g[1], b[1]}, {r[2], g[2], b[2]}}
	inv := p.inverse()
	var m matrix3
	for col := range 3 {
		s := inv[col][0]*w[0] + inv[col][1]*w[1] + inv[col][2]*w[2]
		for row := range 3 {
			m[row][col] = p[row][col] * s
		}
	}
	return m
}

// inverse returns the inverse of a matrix that has one.
func (m matrix3) inverse() matrix3 {
	cof := func(r0, c0, r1, c1 int) float64 { return m[r0][c0]*m[r1][c1] - m[r0][c1]*m[r1][c0] }
	var a matrix3
	a[0][0], a[0][1], a[0][2] = cof(1, 1, 2, 2), -cof(0, 1, 2, 2), cof(0, 1, 1, 2)
	a[1][0], a[1][1], a[1][2] = -cof(1, 0, 2, 2), cof(0, 0, 2, 2), -cof(0, 0, 1, 2)
	a[2][0], a[2][1], a[2][2] = cof(1, 0, 2, 1), -cof(0, 0, 2, 1), cof(0, 0, 1, 1)
	det := m[0][0]*a[0][0] + m[0][1]*a[1][0] + m[0][2]*a[2][0]
	for r := range 3 {
		for c := range 3 {
			a[r][c] /= det
		}
	}
	return a
}
