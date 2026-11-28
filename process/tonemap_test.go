package process

import (
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/gomcpgo/govideo/codec"
)

// The transfer functions against the levels the standards print: Rec. ITU-R
// BT.2100 for the ends and the join of each curve, and the reference levels
// of Report ITU-R BT.2408, which it gives for PQ and for HLG on a 1000 cd/m²
// display.
func TestHDRTransferFunctions(t *testing.T) {
	for _, c := range []struct{ signal, nits float64 }{
		{0, 0}, {1, 10000},
		{0.38, 26},  // an 18% grey card
		{0.58, 203}, // HDR reference white
		{0.7518, 1000},
		{0.5081, 100},
	} {
		// The report rounds its signal levels to a percent.
		if got := pqEOTF(c.signal); math.Abs(got-c.nits) > 0.04*c.nits+1e-9 {
			t.Errorf("PQ signal %.4f is %.2f cd/m², want %.0f", c.signal, got, c.nits)
		}
	}
	for _, c := range []struct{ signal, scene float64 }{{0, 0}, {0.5, 1.0 / 12}, {1, 1}} {
		if got := hlgInverseOETF(c.signal); math.Abs(got-c.scene) > 1e-6 {
			t.Errorf("HLG signal %.2f is scene light %.6f, want %.6f", c.signal, got, c.scene)
		}
	}
	for i := 0; i <= 1000; i++ {
		e := float64(i) / 1000
		if back := hlgOETF(hlgInverseOETF(e)); math.Abs(back-e) > 1e-6 {
			t.Fatalf("HLG signal %.3f comes back as %.6f", e, back)
		}
	}
	// HLG on a 1000 cd/m² display: grey of scene light s shows 1000*s^1.2.
	for _, c := range []struct{ signal, nits float64 }{{0.38, 26}, {0.75, 203}, {1, 1000}} {
		if got := hlgPeak * math.Pow(hlgInverseOETF(c.signal), hlgGamma); math.Abs(got-c.nits) > 0.04*c.nits {
			t.Errorf("HLG signal %.2f shows %.1f cd/m² on a 1000 cd/m² display, want %.0f", c.signal, got, c.nits)
		}
	}
}

// The conversions between primaries, derived from the chromaticities, against
// the matrices as they are published (Rec. ITU-R BT.2087 for BT.2020 to
// BT.709; the usual figures for P3-D65).
func TestPrimariesMatrices(t *testing.T) {
	want := map[int][9]float64{
		9:  {1.6605, -0.5876, -0.0728, -0.1246, 1.1329, -0.0083, -0.0182, -0.1006, 1.1187},
		12: {1.22494, -0.22494, 0, -0.04206, 1.04206, 0, -0.01964, -0.07864, 1.09827},
		1:  {1, 0, 0, 0, 1, 0, 0, 0, 1},
	}
	for code, w := range want {
		m, err := NewToneMapper(HDR{Transfer: TransferPQ, Primaries: code})
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range m.t.M {
			if math.Abs(float64(v)-w[i]) > 2e-4 {
				t.Errorf("primaries %d: matrix entry %d is %.5f, want %.4f", code, i, v, w[i])
			}
		}
	}
	// The luminance of BT.2020's primaries is the luma weights of its matrix.
	y := primaries(9).rgbToXYZ()[1]
	if math.Abs(y[0]-kr2020) > 1e-4 || math.Abs(y[2]-kb2020) > 1e-4 {
		t.Errorf("BT.2020 luminance weights %.4f, %.4f, %.4f", y[0], y[1], y[2])
	}
}

// refSource is a kind of source frame for refToneMap.
type refSource struct {
	hdr       HDR
	matrix    int
	fullRange bool
	depth     int
}

// refToneMap is the tone mapping of one pixel written straight from the
// formulas, in float64 and with no tables: the reference the kernel is held
// to. It takes the samples as a DeepFrame holds them and returns R'G'B' of
// the standard picture, 0..1.
func refToneMap(s refSource, y, cb, cr uint16) [3]float64 {
	return refCurve(refLight(s, y, cb, cr))
}

// refLight is the first half of refToneMap: the light of a pixel on a 1000
// cd/m² display, as a fraction of it, in BT.709 primaries. A colour outside
// them has a component below zero.
func refLight(s refSource, y, cb, cr uint16) [3]float64 {
	kr, kb := 0.2627, 0.0593
	switch s.matrix {
	case 1:
		kr, kb = 0.2126, 0.0722
	case 6:
		kr, kb = 0.299, 0.114
	}
	yn := (float64(y) - 4096) / 56064
	pb, pr := (float64(cb)-32768)/57344, (float64(cr)-32768)/57344
	if s.fullRange {
		top := float64(uint32(1<<s.depth-1) << (16 - s.depth))
		yn, pb, pr = float64(y)/top, (float64(cb)-32768)/top, (float64(cr)-32768)/top
	}
	clamp := func(v float64) float64 { return math.Min(math.Max(v, 0), 1) }
	sig := [3]float64{yn + 2*(1-kr)*pr, 0, yn + 2*(1-kb)*pb}
	sig[1] = (yn - kr*sig[0] - kb*sig[2]) / (1 - kr - kb)

	// Display light on a 1000 cd/m² display, as a fraction of it, in the
	// source's primaries.
	var light [3]float64
	if s.hdr.Transfer == TransferPQ {
		const m1, m2, c1, c2, c3 = 0.1593017578125, 78.84375, 0.8359375, 18.8515625, 18.6875
		for i, e := range sig {
			p := math.Pow(clamp(e), 1/m2)
			light[i] = 10 * math.Pow(math.Max(p-c1, 0)/(c2-c3*p), 1/m1)
		}
	} else {
		const a, b, c = 0.17883277, 0.28466892, 0.55991073
		for i, e := range sig {
			if e = clamp(e); e <= 0.5 {
				light[i] = e * e / 3
			} else {
				light[i] = (math.Exp((e-c)/a) + b) / 12
			}
		}
		w := [3]float64{0.2627, 0.6780, 0.0593}
		switch s.hdr.Primaries {
		case 1:
			w = [3]float64{0.2126, 0.7152, 0.0722}
		case 12:
			w = [3]float64{0.2290, 0.6917, 0.0793}
		}
		ys := w[0]*light[0] + w[1]*light[1] + w[2]*light[2]
		f := 0.0
		if ys > 0 {
			f = math.Pow(ys, 0.2)
		}
		light = [3]float64{light[0] * f, light[1] * f, light[2] * f}
	}

	m := [9]float64{1.660491, -0.587641, -0.072850, -0.124550, 1.132900, -0.008349, -0.018151, -0.100579, 1.118730}
	switch s.hdr.Primaries {
	case 1:
		m = [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}
	case 12:
		m = [9]float64{1.224940, -0.224940, 0, -0.042057, 1.042057, 0, -0.019638, -0.078636, 1.098274}
	}
	return [3]float64{
		m[0]*light[0] + m[1]*light[1] + m[2]*light[2],
		m[3]*light[0] + m[4]*light[1] + m[5]*light[2],
		m[6]*light[0] + m[7]*light[1] + m[8]*light[2],
	}
}

// refCurve is the second half of refToneMap: from the light refLight gives
// to R'G'B' of the standard picture.
func refCurve(rgb [3]float64) [3]float64 {
	clamp := func(v float64) float64 { return math.Min(math.Max(v, 0), 1) }
	lum := math.Max(0.2126*rgb[0]+0.7152*rgb[1]+0.0722*rgb[2], 0)
	if lo := math.Min(rgb[0], math.Min(rgb[1], rgb[2])); lo < 0 {
		k := lum / (lum - lo)
		for i := range rgb {
			rgb[i] = lum + k*(rgb[i]-lum)
		}
	}
	// The curve, for the largest component: its HLG signal as a grey, shown
	// on a display of gamma 2.4.
	top := math.Max(rgb[0], math.Max(rgb[1], rgb[2]))
	var out [3]float64
	if top <= 0 {
		return out
	}
	scene := math.Pow(math.Min(top, 1), 1/1.2)
	signal := math.Sqrt(3 * scene)
	if scene > 1.0/12 {
		signal = 0.17883277*math.Log(12*scene-0.28466892) + 0.55991073
	}
	gain := math.Pow(signal, 2.4) / top
	for i := range rgb {
		out[i] = math.Pow(clamp(rgb[i]*gain), 1/2.4)
	}
	return out
}

// refYCbCr returns the BT.709 limited-range samples of R'G'B', unrounded.
func refYCbCr(c [3]float64) (y, cb, cr float64) {
	l := 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	return 16 + 219*l, 128 + 224*(c[2]-l)/1.8556, 128 + 224*(c[0]-l)/1.5748
}

// blocks returns a deep frame of w by h in which every 2x2 block of pixels
// has one colour, drawn by sample, so that the chroma of a block is the
// chroma of each of its pixels.
func blocks(w, h int, s refSource, sample func() (y, cb, cr uint16)) *codec.DeepFrame {
	f := (*codec.DeepFrame)(nil).Reuse(w, h)
	f.Depth, f.Matrix, f.FullRange = s.depth, s.matrix, s.fullRange
	cw, ch := (w+1)/2, (h+1)/2
	for by := range ch {
		for bx := range cw {
			y, cb, cr := sample()
			f.Cb[by*cw+bx], f.Cr[by*cw+bx] = cb, cr
			for _, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				if x, yy := 2*bx+p[0], 2*by+p[1]; x < w && yy < h {
					f.Y[yy*w+x] = y
				}
			}
		}
	}
	return f
}

// TestToneMapAgainstReference holds the kernel, with its tables and float32
// arithmetic, to the formulas: every kind of source, over colours that fill
// the signal range, to one step of the 8-bit output.
func TestToneMapAgainstReference(t *testing.T) {
	sources := []refSource{
		{HDR{TransferHLG, 9}, 9, false, 10},
		{HDR{TransferPQ, 9}, 9, false, 10},
		{HDR{TransferPQ, 9}, 9, true, 10},
		{HDR{TransferHLG, 9}, 9, true, 12},
		{HDR{TransferPQ, 12}, 1, false, 10}, // P3 in a BT.709 matrix
		{HDR{TransferHLG, 1}, 1, false, 10},
		{HDR{TransferHLG, 0}, 2, false, 10}, // nothing stated: BT.2020
		{HDR{TransferPQ, 12}, 6, false, 10},
	}
	for _, s := range sources {
		rng := rand.New(rand.NewPCG(1, uint64(s.hdr.Transfer)<<8|uint64(s.matrix)))
		// Samples of the whole range a decoder can give, most of them
		// colours no signal has: nothing may go wrong for those either.
		// Half are kept near grey, where real pictures are.
		sample := func() (uint16, uint16, uint16) {
			y := uint16(rng.IntN(1024)) << 6
			if rng.IntN(2) == 0 {
				return y, uint16(512+rng.IntN(161)-80) << 6, uint16(512+rng.IntN(161)-80) << 6
			}
			return y, uint16(rng.IntN(1024)) << 6, uint16(rng.IntN(1024)) << 6
		}
		const w, h = 258, 130
		src := blocks(w, h, s, sample)
		m, err := NewToneMapper(s.hdr)
		if err != nil {
			t.Fatal(err)
		}
		dst := m.Map(nil, src)
		if dst.Width != w || dst.Height != h || dst.Matrix != 1 || dst.FullRange {
			t.Fatalf("%+v: output is %dx%d, matrix %d, full range %t", s, dst.Width, dst.Height, dst.Matrix, dst.FullRange)
		}
		var worst float64
		for by := range h / 2 {
			for bx := range w / 2 {
				ci := by*src.CStride + bx
				y, cb, cr := refYCbCr(refToneMap(s, src.Y[2*by*w+2*bx], src.Cb[ci], src.Cr[ci]))
				for _, d := range []float64{
					float64(dst.Y[2*by*w+2*bx]) - y, float64(dst.Y[(2*by+1)*w+2*bx+1]) - y,
					float64(dst.Cb[by*dst.CStride+bx]) - cb, float64(dst.Cr[by*dst.CStride+bx]) - cr,
				} {
					worst = math.Max(worst, math.Abs(d))
				}
			}
		}
		// Half a step is rounding; the rest is the tables and float32.
		if worst > 0.75 {
			t.Errorf("%+v: output is up to %.2f steps from the reference", s, worst)
		} else {
			t.Logf("%+v: within %.2f steps of the reference", s, worst)
		}
	}
}

// grey returns the 10-bit limited-range luma sample of a signal level.
func grey(signal float64) uint16 { return uint16(math.Round(64+876*signal)) << 6 }

// A grey HLG signal comes out as the same signal: the tone curve is HLG's
// own picture on a standard display. A grey PQ signal comes out as the HLG
// signal of the same light, so the reference levels of BT.2408 land where
// HLG has them: an 18% grey card (26 cd/m²) at 38%, a white card (203) at
// 75%. And greys stay grey.
func TestToneMapGreys(t *testing.T) {
	check := func(name string, hdr HDR, signal, want float64) {
		t.Helper()
		m, _ := NewToneMapper(hdr)
		src := blocks(8, 8, refSource{hdr, 9, false, 10}, func() (uint16, uint16, uint16) { return grey(signal), 32768, 32768 })
		dst := m.Map(nil, src)
		if got := (float64(dst.Y[9]) - 16) / 219; math.Abs(got-want) > 1.5/219 {
			t.Errorf("%s grey at %.3f comes out at %.3f, want %.3f", name, signal, got, want)
		}
		if dst.Cb[3] != 128 || dst.Cr[3] != 128 {
			t.Errorf("%s grey at %.3f comes out coloured: Cb %d, Cr %d", name, signal, dst.Cb[3], dst.Cr[3])
		}
	}
	for i := 0; i <= 20; i++ {
		s := float64(i) / 20
		check("HLG", HDR{TransferHLG, 9}, s, s)
	}
	for _, c := range []struct{ pq, hlg float64 }{{0, 0}, {0.2, 0.13}, {0.38, 0.38}, {0.58, 0.75}, {0.7518, 1}, {0.9, 1}, {1, 1}} {
		// The level HLG has for that light; the pairs above are the
		// report's, which rounds them to a percent.
		pq := float64(grey(c.pq)>>6-64) / 876
		want := hlgOETF(math.Pow(math.Min(pqEOTF(pq)/hlgPeak, 1), 1/hlgGamma))
		if math.Abs(want-c.hlg) > 0.012 {
			t.Errorf("PQ signal %.3f is HLG signal %.3f, want about %.2f", c.pq, want, c.hlg)
		}
		check("PQ", HDR{TransferPQ, 9}, c.pq, want)
	}
}

// The same light gives the same picture whichever of the two signals states
// it: colours of a 1000 cd/m² display, written as PQ and as HLG.
func TestToneMapPQAndHLGAgree(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 3))
	const n = 64
	var pq, hlg [n * n][3]uint16
	for i := range pq {
		// Display light in BT.2020 primaries, weighted towards the dark end.
		// A bright saturated colour of a 1000 cd/m² display is more scene
		// light than HLG has a signal for; those are left out.
		var light [3]float64
		var ys float64
		for {
			for c := range light {
				light[c] = math.Pow(rng.Float64(), 3)
			}
			ys = 0.2627*light[0] + 0.6780*light[1] + 0.0593*light[2]
			if max(light[0], light[1], light[2])*math.Pow(ys, 1/1.2-1) <= 1 {
				break
			}
		}
		var ep, eh [3]float64
		for c, v := range light {
			y := math.Pow(v*1000/10000, 0.1593017578125)
			ep[c] = math.Pow((0.8359375+18.8515625*y)/(1+18.6875*y), 78.84375)
			// HLG's OOTF backwards: scene light is display light by
			// Yd^(1/1.2-1), Yd the display's luminance.
			eh[c] = hlgOETF(math.Min(v*math.Pow(ys, 1/1.2-1), 1))
		}
		toYCbCr := func(e [3]float64) [3]uint16 {
			y := 0.2627*e[0] + 0.6780*e[1] + 0.0593*e[2]
			// All 16 bits of each sample are used: rounded to 10, the dark
			// end of HLG is coarse enough to hide a real difference.
			q := func(v float64) uint16 { return uint16(math.Round(v * 64)) }
			return [3]uint16{q(64 + 876*y), q(512 + 896*(e[2]-y)/1.8814), q(512 + 896*(e[0]-y)/1.4746)}
		}
		pq[i], hlg[i] = toYCbCr(ep), toYCbCr(eh)
	}
	run := func(hdr HDR, px *[n * n][3]uint16) *codec.VideoFrame {
		i := 0
		src := blocks(2*n, 2*n, refSource{hdr, 9, false, 16}, func() (uint16, uint16, uint16) {
			p := px[i]
			i++
			return p[0], p[1], p[2]
		})
		m, _ := NewToneMapper(hdr)
		return m.Map(nil, src)
	}
	a, b := run(HDR{TransferPQ, 9}, &pq), run(HDR{TransferHLG, 9}, &hlg)
	worst := 0
	diff := func(x, y []byte) {
		for i := range x {
			worst = max(worst, int(x[i])-int(y[i]), int(y[i])-int(x[i]))
		}
	}
	diff(a.Y, b.Y)
	diff(a.Cb, b.Cb)
	diff(a.Cr, b.Cr)
	if worst > 1 {
		t.Errorf("PQ and HLG of the same light differ by up to %d steps", worst)
	}
}

// What the curve promises of colours: a saturated one keeps its hue and no
// component leaves the range, and one that BT.709 cannot show comes out at
// the edge of BT.709, with its luminance.
func TestToneMapColours(t *testing.T) {
	m, _ := NewToneMapper(HDR{TransferPQ, 9})
	one := func(light [3]float64) [3]float64 { // BT.2020 display light, cd/m², to R'G'B' out
		var e [3]float64
		for c, v := range light {
			y := math.Pow(v/10000, 0.1593017578125)
			e[c] = math.Pow((0.8359375+18.8515625*y)/(1+18.6875*y), 78.84375)
		}
		yy := 0.2627*e[0] + 0.6780*e[1] + 0.0593*e[2]
		q := func(v float64) uint16 { return uint16(math.Round(v * 64)) }
		src := blocks(2, 2, refSource{HDR{TransferPQ, 9}, 9, false, 10}, func() (uint16, uint16, uint16) {
			return q(64 + 876*yy), q(512 + 896*(e[2]-yy)/1.8814), q(512 + 896*(e[0]-yy)/1.4746)
		})
		// Sixteen bits of signal, so that the test is of the mapping and
		// not of rounding to 10.
		src.Depth = 16
		rgba := ToRGBA(m.Map(nil, src)).Pix
		return [3]float64{float64(rgba[0]) / 255, float64(rgba[1]) / 255, float64(rgba[2]) / 255}
	}
	// BT.709 red, written in BT.2020 primaries, at four levels from the
	// straight part of the curve to past its top: always pure red, brighter
	// each time until the top.
	to2020 := matrix3{}
	for r, row := range m.t.M[:9:9] {
		to2020[r/3][r%3] = float64(row)
	}
	to2020 = to2020.inverse()
	red := [3]float64{to2020[0][0], to2020[1][0], to2020[2][0]} // BT.709 red in BT.2020 RGB
	last := 0.0
	for _, nits := range []float64{20, 100, 400, 1000, 4000} {
		out := one([3]float64{red[0] * nits / red[0], red[1] * nits / red[0], red[2] * nits / red[0]})
		if out[1] > 0.05 || out[2] > 0.05 {
			t.Errorf("BT.709 red at %v cd/m² comes out as %.3f: not pure red", nits, out)
		}
		if out[0] < last-0.004 || (nits >= 1000 && out[0] < 0.98) {
			t.Errorf("BT.709 red at %v cd/m² comes out at %.3f (the level before: %.3f)", nits, out[0], last)
		}
		last = out[0]
	}
	// BT.2020 green is outside BT.709: it comes out on the edge of BT.709
	// (no red in it) with the luminance it had, which setting what is below
	// zero to zero would not keep.
	out := one([3]float64{0, 40, 0})
	lum := 0.2126*math.Pow(out[0], 2.4) + 0.7152*math.Pow(out[1], 2.4) + 0.0722*math.Pow(out[2], 2.4)
	want := 0.6780 * 40 / 1000 * math.Pow(3, 1.2) // its luminance, on the straight part of the curve
	if math.Abs(lum-want)/want > 0.06 || out[0] > 0.03 || out[1] <= out[2] {
		t.Errorf("BT.2020 green comes out as %.3f, luminance %.4f, want green at luminance %.4f", out, lum, want)
	}
}

// The picture does not depend on how the rows are shared out, and frames of
// odd sizes, down to a single pixel, are whole.
func TestToneMapSizesAndWorkers(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 8))
	s := refSource{HDR{TransferHLG, 9}, 9, false, 10}
	for _, size := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {3, 3}, {7, 5}, {33, 131}, {640, 359}, {0, 0}} {
		w, h := size[0], size[1]
		src := (*codec.DeepFrame)(nil).Reuse(w, h)
		for i := range src.Y {
			src.Y[i] = uint16(rng.IntN(1024)) << 6
		}
		for i := range src.Cb {
			src.Cb[i], src.Cr[i] = uint16(rng.IntN(1024))<<6, uint16(rng.IntN(1024))<<6
		}
		m, _ := NewToneMapper(s.hdr)
		old := runtime.GOMAXPROCS(1)
		a := m.Map(nil, src)
		runtime.GOMAXPROCS(7)
		// Into a frame that held something else, of another size.
		b := m.Map(codec.NewVideoFrame(w+9, h+3), src)
		runtime.GOMAXPROCS(old)
		if b.Width != w || b.Height != h || string(a.Y) != string(b.Y) || string(a.Cb) != string(b.Cb) || string(a.Cr) != string(b.Cr) {
			t.Errorf("%dx%d: the picture depends on the number of processors", w, h)
		}
		// A pixel's luma depends on its own samples only; check the corners
		// against the reference, where a row or column off would show.
		for _, p := range [][2]int{{0, 0}, {w - 1, 0}, {0, h - 1}, {w - 1, h - 1}} {
			if w == 0 {
				break
			}
			x, y := p[0], p[1]
			ci := y/2*src.CStride + x/2
			c := refToneMap(s, src.Y[y*w+x], src.Cb[ci], src.Cr[ci])
			if want, _, _ := refYCbCr(c); math.Abs(float64(a.Y[y*w+x])-want) > 1 {
				t.Errorf("%dx%d: luma at %d,%d is %d, want %.1f", w, h, x, y, a.Y[y*w+x], want)
			}
		}
	}
	if _, err := NewToneMapper(HDR{Transfer: 1}); err == nil {
		t.Error("a tone mapper was made for a standard transfer function")
	}
}

// BenchmarkToneMap is the cost of a 1080p picture: with the processors the
// machine has, and with one.
func BenchmarkToneMap(b *testing.B) {
	rng := rand.New(rand.NewPCG(1, 1))
	src := (*codec.DeepFrame)(nil).Reuse(1920, 1080)
	for i := range src.Y {
		src.Y[i] = uint16(64+rng.IntN(877)) << 6
	}
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = uint16(448+rng.IntN(128))<<6, uint16(448+rng.IntN(128))<<6
	}
	for _, c := range []struct {
		name  string
		hdr   HDR
		procs int
	}{{"HLG", HDR{TransferHLG, 9}, 0}, {"PQ", HDR{TransferPQ, 9}, 0}, {"HLG/1", HDR{TransferHLG, 9}, 1}} {
		b.Run(c.name, func(b *testing.B) {
			if c.procs > 0 {
				defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(c.procs))
			}
			m, _ := NewToneMapper(c.hdr)
			var dst *codec.VideoFrame
			b.ReportAllocs()
			for b.Loop() {
				dst = m.Map(dst, src)
			}
		})
	}
}
