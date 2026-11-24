package process

import (
	"math"
	"math/rand"
	"testing"
)

func sine(freq float64, rate, frames, channels int, amp float64) []float32 {
	s := make([]float32, frames*channels)
	for i := 0; i < frames; i++ {
		v := float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			s[i*channels+c] = v
		}
	}
	return s
}

func resampleAll(inRate, outRate, ch int, in []float32, chunkFrames int) []float32 {
	r := NewResampler(inRate, outRate, ch)
	var out []float32
	for len(in) > 0 {
		n := chunkFrames * ch
		if n > len(in) {
			n = len(in)
		}
		out = append(out, r.Process(in[:n])...)
		in = in[n:]
	}
	return append(out, r.Flush()...)
}

func TestResamplerSine(t *testing.T) {
	cases := []struct {
		in, out int
		freq    float64
	}{
		{44100, 48000, 1000},
		{44100, 48000, 15000},
		{48000, 44100, 1000},
		{96000, 48000, 5000},
		{8000, 48000, 440},
		{22050, 48000, 3000},
		{32000, 48000, 10000},
		{47999, 48000, 1000}, // huge phase count: uncached path
	}
	for _, c := range cases {
		const amp = 0.5
		in := sine(c.freq, c.in, c.in, 2, amp) // one second
		out := resampleAll(c.in, c.out, 2, in, 1000)
		if got := len(out) / 2; got != c.out {
			t.Errorf("%d→%d: %d output frames, want %d", c.in, c.out, got, c.out)
			continue
		}
		// Output is time-aligned, so compare against the ideal sine away
		// from the edges (where the implicit silence causes ringing).
		var maxErr, peak float64
		for i := c.out / 10; i < c.out*9/10; i++ {
			want := amp * math.Sin(2*math.Pi*c.freq*float64(i)/float64(c.out))
			for ch := 0; ch < 2; ch++ {
				v := float64(out[i*2+ch])
				maxErr = math.Max(maxErr, math.Abs(v-want))
				peak = math.Max(peak, math.Abs(v))
			}
		}
		if maxErr > 2e-3 {
			t.Errorf("%d→%d %gHz: max error %g", c.in, c.out, c.freq, maxErr)
		}
		if math.Abs(peak-amp) > 5e-3 {
			t.Errorf("%d→%d %gHz: peak %g, want %g", c.in, c.out, c.freq, peak, amp)
		}
	}
}

func TestResamplerRejectsAliases(t *testing.T) {
	// 30 kHz at 96 kHz is above the 24 kHz output Nyquist: must vanish.
	out := resampleAll(96000, 48000, 1, sine(30000, 96000, 96000, 1, 0.5), 4096)
	var peak float64
	for _, v := range out[4800 : len(out)-4800] {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if peak > 1e-3 {
		t.Errorf("alias peak %g", peak)
	}
	// DC is preserved exactly enough.
	dc := make([]float32, 44100)
	for i := range dc {
		dc[i] = 0.25
	}
	out = resampleAll(44100, 48000, 1, dc, 512)
	for _, v := range out[4800 : len(out)-4800] {
		if math.Abs(float64(v)-0.25) > 1e-5 {
			t.Fatalf("DC became %g", v)
		}
	}
}

func TestResamplerPassthroughAndLengths(t *testing.T) {
	in := sine(440, 48000, 1234, 2, 0.3)
	out := resampleAll(48000, 48000, 2, in, 100)
	if len(out) != len(in) {
		t.Fatalf("passthrough length %d want %d", len(out), len(in))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatal("passthrough changed samples")
		}
	}
	r := NewResampler(48000, 48000, 2)
	p := r.Process(in)
	p[0] = 99
	if in[0] == 99 {
		t.Error("passthrough must not alias its input")
	}
	// Exact long-run ratio for awkward lengths, including empty input.
	for _, n := range []int{0, 1, 2, 7, 147, 1000, 44101} {
		got := len(resampleAll(44100, 48000, 1, make([]float32, n), 333))
		want := int(math.Round(float64(n) * 48000 / 44100))
		if got != want {
			t.Errorf("%d in → %d out, want %d", n, got, want)
		}
		got = len(resampleAll(48000, 8000, 3, make([]float32, n*3), 97)) / 3
		want = int(math.Round(float64(n) / 6))
		if d := got - want; d < -1 || d > 1 {
			t.Errorf("48k→8k: %d in → %d out, want %d", n, got, want)
		}
	}
	// Invalid arguments degrade to pass-through rather than panicking.
	bad := NewResampler(0, -1, 0)
	if got := bad.Process([]float32{1, 2, 3}); len(got) != 3 || bad.Flush() != nil {
		t.Error("invalid resampler should pass through")
	}
}

func TestResamplerChunkIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	in := make([]float32, 20000*2)
	for i := range in {
		in[i] = rng.Float32()*2 - 1
	}
	for _, rates := range [][2]int{{44100, 48000}, {48000, 32000}, {11025, 48000}} {
		ref := resampleAll(rates[0], rates[1], 2, in, len(in))
		for _, chunk := range []int{1, 7, 480, 4096} {
			got := resampleAll(rates[0], rates[1], 2, in, chunk)
			if len(got) != len(ref) {
				t.Fatalf("%v chunk %d: length %d vs %d", rates, chunk, len(got), len(ref))
			}
			for i := range ref {
				if got[i] != ref[i] {
					t.Fatalf("%v chunk %d: sample %d differs", rates, chunk, i)
				}
			}
		}
	}
	// A resampler is reusable after Flush and stays bounded in memory.
	r := NewResampler(44100, 48000, 2)
	a := append(r.Process(in), r.Flush()...)
	for i := 0; i < 50; i++ {
		r.Process(in)
		if len(r.buf) > len(in)+4*r.half*2 {
			t.Fatalf("resampler buffer grew to %d", len(r.buf))
		}
	}
	r.Flush()
	b := append(r.Process(in), r.Flush()...)
	if len(a) != len(b) || a[len(a)/2] != b[len(b)/2] {
		t.Error("resampler not reusable after Flush")
	}
}

func TestToStereo(t *testing.T) {
	if got := ToStereo([]float32{0.1, 0.2}, 1); len(got) != 4 || got[0] != 0.1 || got[1] != 0.1 || got[3] != 0.2 {
		t.Errorf("mono: %v", got)
	}
	in := []float32{0.1, 0.2, 0.3, 0.4}
	got := ToStereo(in, 2)
	if len(got) != 4 || got[2] != 0.3 {
		t.Errorf("stereo: %v", got)
	}
	got[0] = 9
	if in[0] == 9 {
		t.Error("stereo copy aliases input")
	}
	// 5.1: L R C LFE Ls Rs. Centre goes to both sides equally, LFE is
	// dropped, left surround only to the left.
	c := ToStereo([]float32{0, 0, 1, 0, 0, 0}, 6)
	if c[0] <= 0 || c[0] != c[1] {
		t.Errorf("centre: %v", c)
	}
	if lfe := ToStereo([]float32{0, 0, 0, 1, 0, 0}, 6); lfe[0] != 0 || lfe[1] != 0 {
		t.Errorf("LFE leaked: %v", lfe)
	}
	ls := ToStereo([]float32{0, 0, 0, 0, 1, 0}, 6)
	l := ToStereo([]float32{1, 0, 0, 0, 0, 0}, 6)
	if ls[0] <= 0 || ls[1] != 0 || l[1] != 0 {
		t.Errorf("surround/left routing: %v %v", ls, l)
	}
	if r := float64(ls[0] / l[0]); math.Abs(r-math.Sqrt2/2) > 1e-5 {
		t.Errorf("surround is %g of front, want -3 dB", r)
	}
	if r := float64(c[0] / l[0]); math.Abs(r-math.Sqrt2/2) > 1e-5 {
		t.Errorf("centre is %g of front, want -3 dB", r)
	}
	// The matrix is not normalised: the front channels pass at unity, as in
	// ITU-R BS.775, whatever the channel count.
	for _, ch := range []int{3, 4, 5, 6, 8} {
		in := make([]float32, ch)
		in[0] = 0.5
		if out := ToStereo(in, ch); out[0] != 0.5 || out[1] != 0 {
			t.Errorf("%d channels: front left 0.5 came out as %v, want unity gain", ch, out)
		}
	}
	// 5.1 at full scale on every channel sums to 1 + 2/sqrt(2) a side; the
	// mixer's soft clipper, not the downmix, deals with it.
	full := ToStereo([]float32{1, 1, 1, 1, 1, 1}, 6)
	if want := 1 + math.Sqrt2; math.Abs(float64(full[0])-want) > 1e-5 || full[0] != full[1] {
		t.Errorf("full-scale 5.1: %v, want %g a side", full, want)
	}
	// No channel count may panic, and none may come out silent.
	for ch := 1; ch <= 12; ch++ {
		in := make([]float32, ch*3+1) // with a trailing partial frame
		for i := range in {
			in[i] = 1
		}
		out := ToStereo(in, ch)
		if len(out) != (len(in)/ch)*2 {
			t.Fatalf("%d channels: %d output samples", ch, len(out))
		}
		for _, v := range out {
			if v <= 0 {
				t.Fatalf("%d channels: sample %g", ch, v)
			}
		}
	}
	if len(ToStereo([]float32{1, 2}, 0)) != 4 {
		t.Error("channels<1 should be treated as mono")
	}
}

func TestGainFadeMix(t *testing.T) {
	s := []float32{1, -1, 0.5}
	ApplyGain(s, 0.5)
	if s[0] != 0.5 || s[1] != -0.5 || s[2] != 0.25 {
		t.Errorf("gain: %v", s)
	}
	dst := []float32{1, 1, 1}
	MixInto(dst, []float32{1, 2})
	if dst[0] != 2 || dst[1] != 3 || dst[2] != 1 {
		t.Errorf("mix: %v", dst)
	}
	MixInto(dst[:1], []float32{1, 2, 3})
	if dst[0] != 3 || dst[1] != 3 {
		t.Errorf("mix (long src): %v", dst)
	}

	// 100-frame stereo track, 10-frame fade in, 20-frame fade out.
	const total = 100
	whole := make([]float32, total*2)
	for i := range whole {
		whole[i] = 1
	}
	ApplyFade(whole, 2, 0, 10, 20, total)
	wantAt := func(pos int) float32 {
		g := float32(1)
		if pos < 10 {
			g = float32(pos) / 10
		}
		if rem := total - pos; rem < 20 {
			g *= float32(rem) / 20
		}
		return g
	}
	for i := 0; i < total; i++ {
		if whole[i*2] != wantAt(i) || whole[i*2+1] != wantAt(i) {
			t.Fatalf("fade at %d: %g, want %g", i, whole[i*2], wantAt(i))
		}
	}
	if whole[0] != 0 || whole[2*50] != 1 || whole[2*99] != 0.05 {
		t.Errorf("fade envelope endpoints wrong: %g %g %g", whole[0], whole[100], whole[198])
	}
	// Chunked application matches.
	chunked := make([]float32, total*2)
	for i := range chunked {
		chunked[i] = 1
	}
	for start := 0; start < total; start += 7 {
		end := min(start+7, total)
		ApplyFade(chunked[start*2:end*2], 2, int64(start), 10, 20, total)
	}
	for i := range whole {
		if whole[i] != chunked[i] {
			t.Fatalf("chunked fade differs at %d", i)
		}
	}
	// No fades: untouched. Positions past the end are silent.
	plain := []float32{1, 1}
	ApplyFade(plain, 1, 0, 0, 0, 10)
	ApplyFade(plain, 0, 0, 5, 5, 10)
	if plain[0] != 1 || plain[1] != 1 {
		t.Error("no-op fade changed samples")
	}
	past := []float32{1, 1}
	ApplyFade(past, 1, 10, 0, 5, 10)
	if past[0] != 0 || past[1] != 0 {
		t.Errorf("past the end: %v", past)
	}
}

func TestSoftClip(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	s := []float32{0, 0.5, -0.5, 0.9, -0.9, 0.95, 1, 1.5, -1.5, 10, -10, 1e30, nan, inf, -inf}
	in := append([]float32(nil), s...)
	SoftClip(s)
	for i, v := range s {
		if v != v || v < -1 || v > 1 {
			t.Fatalf("sample %d (%g) → %g out of bounds", i, in[i], v)
		}
	}
	for i := 0; i < 5; i++ {
		if s[i] != in[i] {
			t.Errorf("sample %g below threshold changed to %g", in[i], s[i])
		}
	}
	if s[12] != 0 {
		t.Errorf("NaN → %g", s[12])
	}
	if s[13] != 1 || s[14] != -1 {
		t.Errorf("Inf → %g, %g", s[13], s[14])
	}
	if s[7] != -s[8] || s[9] != -s[10] {
		t.Error("not symmetric")
	}
	// Monotonic and continuous through the knee.
	prev := float32(-2)
	for x := float32(0.8); x < 3; x += 0.001 {
		v := []float32{x}
		SoftClip(v)
		if v[0] < prev {
			t.Fatalf("not monotonic at %g", x)
		}
		if prev > -2 && v[0]-prev > 0.0011 {
			t.Fatalf("jump at %g", x)
		}
		prev = v[0]
	}
}

func TestCrossfade(t *testing.T) {
	const total = 1000
	ones := func(n int, v float32) []float32 {
		s := make([]float32, n)
		for i := range s {
			s[i] = v
		}
		return s
	}
	// The outgoing track alone gives its gain, the incoming alone its own.
	out, in := ones(2*total, 1), make([]float32, 2*total)
	Crossfade(out, in, 2, 0, total)
	gin := ones(2*total, 0)
	Crossfade(gin, ones(2*total, 1), 2, 0, total)
	for i := 0; i < total; i++ {
		o, n := float64(out[2*i]), float64(gin[2*i])
		if out[2*i+1] != out[2*i] || gin[2*i+1] != gin[2*i] {
			t.Fatalf("frame %d: the channels differ", i)
		}
		if math.Abs(o*o+n*n-1) > 1e-6 {
			t.Fatalf("frame %d: gains %v and %v, squares sum to %v", i, o, n, o*o+n*n)
		}
		// The curves are mirror images.
		if m := float64(gin[2*(total-1-i)]); math.Abs(o-m) > 1e-6 {
			t.Fatalf("frame %d: outgoing gain %v, incoming gain at the mirror position %v", i, o, m)
		}
		if i > 0 && (out[2*i] > out[2*i-2] || gin[2*i] < gin[2*i-2]) {
			t.Fatalf("frame %d: the gains do not run one way", i)
		}
	}
	if out[0] < 0.999 || out[2*total-2] > 0.002 || gin[0] > 0.002 || gin[2*total-2] < 0.999 {
		t.Errorf("ends: outgoing %v to %v, incoming %v to %v", out[0], out[2*total-2], gin[0], gin[2*total-2])
	}
	if mid := float64(out[total]); math.Abs(mid-math.Sqrt2/2) > 0.002 {
		t.Errorf("middle: gain %v, want -3 dB", mid)
	}
	// Any chunking gives the same samples.
	whole := sine(440, 48000, total, 2, 0.5)
	other := sine(1000, 48000, total, 2, 0.5)
	want := append([]float32(nil), whole...)
	Crossfade(want, other, 2, 0, total)
	got := append([]float32(nil), whole...)
	for pos := 0; pos < total; {
		n := min(1+pos%97, total-pos)
		Crossfade(got[2*pos:2*(pos+n)], other[2*pos:2*(pos+n)], 2, int64(pos), total)
		pos += n
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: %v in chunks, %v whole", i, got[i], want[i])
		}
	}
	// Positions outside the crossfade hold the end gains; no length does nothing.
	o := []float32{1, 1}
	Crossfade(o, []float32{0.5, 0.5}, 2, total+5, total)
	if o[0] != 0.5 {
		t.Errorf("past the end: %v, want the incoming track alone", o[0])
	}
	o = []float32{1, 1}
	Crossfade(o, []float32{0.5, 0.5}, 2, 0, 0)
	if o[0] != 1 {
		t.Errorf("no length: %v, want the outgoing track untouched", o[0])
	}
}
