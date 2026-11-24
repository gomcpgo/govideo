package process

import (
	"math"

	"github.com/gomcpgo/govideo/internal/kernels"
)

const (
	resampleZeroCrossings = 32   // sinc lobes on each side of the centre
	resampleRolloff       = 0.95 // cutoff as a fraction of the lower Nyquist
	resampleKaiserBeta    = 9.0  // about 90 dB stop-band attenuation
	resampleCacheLimit    = 1 << 20
)

// Resampler is a streaming windowed-sinc sample-rate converter for
// interleaved float32 audio. Output is time-aligned with the input (no
// delay is added), and the output depends only on the input samples, not on
// how they are split across Process calls.
type Resampler struct {
	channels int
	l, m     int64 // reduced ratio: out/in = l/m
	pass     bool
	half     int     // filter taps on each side
	cutoff   float64 // relative to the input rate's Nyquist
	phases   [][]float32
	scratch  []float32
	tmp      []float64

	buf      []float32 // input frames starting at absolute index bufStart
	bufStart int64
	totalIn  int64
	n        int64 // next output frame index
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// NewResampler returns a resampler from inRate to outRate. Invalid rates
// are treated as equal (pass-through); channels below 1 are treated as 1.
func NewResampler(inRate, outRate, channels int) *Resampler {
	if channels < 1 {
		channels = 1
	}
	r := &Resampler{channels: channels}
	if inRate <= 0 || outRate <= 0 || inRate == outRate {
		r.pass = true
		return r
	}
	g := gcd(int64(inRate), int64(outRate))
	r.l, r.m = int64(outRate)/g, int64(inRate)/g
	r.cutoff = resampleRolloff * math.Min(1, float64(outRate)/float64(inRate))
	r.half = int(math.Ceil(resampleZeroCrossings / r.cutoff))
	if r.l*int64(2*r.half) <= resampleCacheLimit {
		r.phases = make([][]float32, r.l)
	} else {
		r.scratch = make([]float32, 2*r.half)
	}
	r.tmp = make([]float64, 2*r.half)
	r.reset()
	return r
}

func (r *Resampler) reset() {
	// Input before the start is silence: seed half-1 zero frames.
	r.buf = make([]float32, (r.half-1)*r.channels)
	r.bufStart = -int64(r.half - 1)
	r.totalIn, r.n = 0, 0
}

// StartAt declares that the input which follows begins at frame in of the
// signal, not at its start, and returns the index of the first output frame
// that will be produced. The output frames keep the positions they have when
// the signal is converted from its start, so what follows is what that
// conversion gives from the returned frame on, but for the first few frames:
// the input before frame in counts as silence. It must be called before any
// input is processed.
func (r *Resampler) StartAt(in int64) (out int64) {
	if in <= 0 {
		return 0
	}
	if r.pass {
		return in
	}
	r.bufStart = in - int64(r.half-1)
	r.totalIn = in
	r.n = (in*r.l + r.m - 1) / r.m
	return r.n
}

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	h := x / 2
	for k := 1; k < 64; k++ {
		term *= (h / float64(k)) * (h / float64(k))
		sum += term
		if term < sum*1e-17 {
			break
		}
	}
	return sum
}

// weights returns the 2*half filter taps for the fractional position
// num/l. Tap k applies to input frame ipos-half+1+k.
func (r *Resampler) weights(num int64) []float32 {
	var w []float32
	if r.phases != nil {
		if w = r.phases[num]; w != nil {
			return w
		}
		w = make([]float32, 2*r.half)
		r.phases[num] = w
	} else {
		w = r.scratch
	}
	frac := float64(num) / float64(r.l)
	i0b := besselI0(resampleKaiserBeta)
	hf := float64(r.half)
	var sum float64
	tmp := r.tmp
	for k := range tmp {
		tmp[k] = 0
		t := float64(k-r.half+1) - frac
		u := t / hf
		if u <= -1 || u >= 1 {
			continue
		}
		x := math.Pi * r.cutoff * t
		s := 1.0
		if x != 0 {
			s = math.Sin(x) / x
		}
		v := s * besselI0(resampleKaiserBeta*math.Sqrt(1-u*u)) / i0b
		tmp[k] = v
		sum += v
	}
	for k, v := range tmp {
		w[k] = float32(v / sum) // unity DC gain for every phase
	}
	return w
}

// generate emits output frames while their filter window is fully buffered
// and the output index is below limit (limit < 0 means no limit).
func (r *Resampler) generate(limit int64) []float32 {
	ch := r.channels
	end := r.bufStart + int64(len(r.buf)/ch) // exclusive absolute frame index
	var out []float32
	if avail := (end - int64(r.half)) * r.l / r.m; avail > r.n {
		out = make([]float32, 0, int(avail-r.n+1)*ch)
	}
	for limit < 0 || r.n < limit {
		pos := r.n * r.m
		ipos := pos / r.l
		if ipos+int64(r.half) >= end {
			break
		}
		w := r.weights(pos % r.l)
		base := int(ipos-int64(r.half)+1-r.bufStart) * ch
		win := r.buf[base : base+2*r.half*ch]
		for c := 0; c < ch; c++ {
			out = append(out, kernels.DotInterleaved(win, w, ch, c))
		}
		r.n++
	}
	// Drop input the next output no longer needs.
	keep := r.n*r.m/r.l - int64(r.half) + 1
	if drop := keep - r.bufStart; drop > 0 {
		if have := int64(len(r.buf) / ch); drop > have {
			drop = have
		}
		n := copy(r.buf, r.buf[int(drop)*ch:])
		r.buf = r.buf[:n]
		r.bufStart += drop
	}
	return out
}

// Process consumes interleaved input and returns the output frames that
// can be produced so far. A trailing partial frame is ignored.
func (r *Resampler) Process(in []float32) []float32 {
	in = in[:len(in)/r.channels*r.channels]
	if r.pass {
		return append([]float32(nil), in...)
	}
	r.buf = append(r.buf, in...)
	r.totalIn += int64(len(in) / r.channels)
	return r.generate(-1)
}

// Flush returns the remaining output so that the total output length is
// round(totalInput * outRate / inRate) frames, then resets the resampler
// for a new stream.
func (r *Resampler) Flush() []float32 {
	if r.pass {
		return nil
	}
	target := (r.totalIn*r.l + r.m/2) / r.m
	r.buf = append(r.buf, make([]float32, (r.half+1)*r.channels)...)
	out := r.generate(target)
	r.reset()
	return out
}
