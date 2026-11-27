package native

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	mathbits "math/bits"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gomcpgo/govideo/internal/bits"
	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/flac"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// The decoder is tested three ways. Frames written bit by bit here, from
// RFC 9639, cover what the module's encoder never writes: linear prediction,
// wasted bits, escaped partitions, 5-bit Rice parameters, every sample size
// and channel count, blocks of varying size. They are made from samples, so
// the samples are what must come back. What the module's encoder writes
// must come back too. And where the oracle is installed, files that ffmpeg
// encoded are decoded here and by ffmpeg, to the same samples.

// flacTestSub is one channel of a frame as the test writes it.
type flacTestSub struct {
	kind      int     // the type code of the subframe: 0, 1, 8 to 12, 32 to 63
	x         []int64 // the samples, as coded: wasted bits left in
	wasted    int
	coef      []int64 // of a linear predictor; the first goes with the sample before
	precision int
	shift     int
	rice5     bool // Rice parameters of 5 bits
	partOrder int
	escape    int // every escape-th partition is stored as it is; 0 for none
}

// order is the number of samples the predictor looks back on.
func (s *flacTestSub) order() int {
	switch {
	case s.kind >= 32:
		return s.kind - 31
	case s.kind >= 8:
		return s.kind - 8
	}
	return 0
}

// predict is the prediction for sample i of y (§9.2.5, §9.2.6).
func (s *flacTestSub) predict(y []int64, i int) int64 {
	switch s.kind {
	case 9:
		return y[i-1]
	case 10:
		return 2*y[i-1] - y[i-2]
	case 11:
		return 3*y[i-1] - 3*y[i-2] + y[i-3]
	case 12:
		return 4*y[i-1] - 6*y[i-2] + 4*y[i-3] - y[i-4]
	}
	var sum int64
	for j, c := range s.coef {
		sum += c * y[i-1-j]
	}
	return sum >> uint(s.shift)
}

// bitsFor is the number of bits that hold v in two's complement.
func bitsFor(v int64) int {
	if v < 0 {
		v = ^v
	}
	return mathbits.Len64(uint64(v)) + 1
}

// put writes v in n bits of two's complement, n up to 33.
func put(w *bits.Writer, v int64, n int) {
	if n > 32 {
		w.U(uint32(uint64(v)>>32), n-32)
		n = 32
	}
	w.U(uint32(v), n)
}

// write writes the subframe for samples of bps bits (§9.2). It reports
// false where the samples cannot be coded as asked: a residual that does
// not fit 32 bits, or one too large for the Rice code and an escape both.
func (s *flacTestSub) write(w *bits.Writer, bps int) bool {
	w.U(uint32(s.kind)<<1|uint32(min(s.wasted, 1)), 8)
	if s.wasted > 0 {
		w.Unary(uint32(s.wasted - 1))
	}
	bps -= s.wasted
	n := len(s.x)
	y := make([]int64, n)
	for i, v := range s.x {
		y[i] = v >> uint(s.wasted)
	}
	switch s.kind {
	case 0:
		put(w, y[0], bps)
		return true
	case 1:
		for _, v := range y {
			put(w, v, bps)
		}
		return true
	}
	order := s.order()
	for _, v := range y[:order] {
		put(w, v, bps)
	}
	if s.kind >= 32 {
		w.U(uint32(s.precision-1), 4)
		w.U(uint32(s.shift), 5)
		for _, c := range s.coef {
			put(w, c, s.precision)
		}
	}
	// The residual, in partitions (§9.2.7).
	res := make([]int64, n)
	for i := order; i < n; i++ {
		res[i] = y[i] - s.predict(y, i)
		if res[i] <= math.MinInt32 || res[i] > math.MaxInt32 {
			return false
		}
	}
	paramBits, maxParam := 4, 14
	if s.rice5 {
		paramBits, maxParam = 5, 30
		w.U(1, 2)
	} else {
		w.U(0, 2)
	}
	w.U(uint32(s.partOrder), 4)
	size := n >> uint(s.partOrder)
	for p := 0; p < 1<<uint(s.partOrder); p++ {
		part := res[p*size : (p+1)*size]
		if p == 0 {
			part = part[order:]
		}
		// The cheapest Rice parameter, of those about the size of the
		// residual.
		raw, best, k := 0, int64(-1), 0
		var sum uint64
		for _, r := range part {
			if r != 0 || raw != 0 {
				raw = max(raw, bitsFor(r))
			}
			sum += uint64(uint32(r<<1) ^ uint32(r>>63))
		}
		about := mathbits.Len64(sum / uint64(max(len(part), 1)))
		for try := max(0, min(about-2, maxParam)); try <= min(about+1, maxParam); try++ {
			var size int64
			for _, r := range part {
				size += int64(uint32(r<<1)^uint32(r>>63))>>uint(try) + 1 + int64(try)
			}
			if best < 0 || size < best {
				best, k = size, try
			}
		}
		escaped := s.escape > 0 && p%s.escape == 0 || best > int64(len(part))*40+64
		if escaped {
			if raw > 31 {
				return false
			}
			w.U(1<<uint(paramBits)-1, paramBits)
			w.U(uint32(raw), 5)
			for _, r := range part {
				put(w, r, raw)
			}
			continue
		}
		w.U(uint32(k), paramBits)
		for _, r := range part {
			u := uint32(r<<1) ^ uint32(r>>63)
			w.Unary(u >> uint(k))
			w.U(u, k)
		}
	}
	return true
}

// flacTestFrame is a frame as the test writes it.
type flacTestFrame struct {
	variable bool
	number   uint64
	rate     int
	rateHow  byte // how the header states the rate: 't' by its code, 'k' in kHz, 'h' in Hz, 'd' in tens of Hz, 0 not at all
	depth    int
	depthHow byte // 't' by its code, 0 not at all
	assign   int  // the channel code
	sizeHow  byte // 't' by its code, 'b' in a byte, 'w' in two
	subs     []flacTestSub
}

// bytes writes the frame (§9), or returns nil where a subframe cannot be
// coded as asked.
func (f *flacTestFrame) bytes(t testing.TB) []byte {
	t.Helper()
	n := len(f.subs[0].x)
	var w bits.Writer
	sync := uint32(0xfff8)
	if f.variable {
		sync = 0xfff9
	}
	w.U(sync, 16)
	sizeCode := map[int]uint32{192: 1, 576: 2, 1152: 3, 2304: 4, 4608: 5, 256: 8, 512: 9, 1024: 10,
		2048: 11, 4096: 12, 8192: 13, 16384: 14, 32768: 15}[n]
	switch f.sizeHow {
	case 'b':
		sizeCode = 6
	case 'w':
		sizeCode = 7
	}
	if sizeCode == 0 {
		t.Fatalf("no way to state a block of %d samples by code", n)
	}
	rateCode := map[int]uint32{88200: 1, 176400: 2, 192000: 3, 8000: 4, 16000: 5, 22050: 6, 24000: 7,
		32000: 8, 44100: 9, 48000: 10, 96000: 11}[f.rate]
	switch f.rateHow {
	case 0:
		rateCode = 0
	case 'k':
		rateCode = 12
	case 'h':
		rateCode = 13
	case 'd':
		rateCode = 14
	}
	depthCode := map[int]uint32{8: 1, 12: 2, 16: 4, 20: 5, 24: 6, 32: 7}[f.depth]
	if f.depthHow == 0 {
		depthCode = 0
	}
	w.U(sizeCode, 4)
	w.U(rateCode, 4)
	w.U(uint32(f.assign), 4)
	w.U(depthCode<<1, 4)
	flacWriteNumber(&w, f.number)
	switch sizeCode {
	case 6:
		w.U(uint32(n-1), 8)
	case 7:
		w.U(uint32(n-1), 16)
	}
	switch rateCode {
	case 12:
		w.U(uint32(f.rate/1000), 8)
	case 13:
		w.U(uint32(f.rate), 16)
	case 14:
		w.U(uint32(f.rate/10), 16)
	}
	w.U(refCRC(w.Bytes(), 8, 0x07), 8)
	side := flacSide(f.assign)
	for c := range f.subs {
		bps := f.depth
		if c == side {
			bps++
		}
		if !f.subs[c].write(&w, bps) {
			return nil
		}
	}
	w.Align()
	return binary.BigEndian.AppendUint16(w.Bytes(), uint16(refCRC(w.Bytes(), 16, 0x8005)))
}

// pcm is what the frame decodes to: a block of each channel, left and right
// restored (§4.2).
func (f *flacTestFrame) pcm() [][]int64 {
	out := make([][]int64, len(f.subs))
	for c := range out {
		out[c] = append([]int64(nil), f.subs[c].x...)
	}
	for i := range out[0] {
		switch f.assign {
		case flacLeftSide:
			out[1][i] = out[0][i] - out[1][i]
		case flacSideRight:
			out[0][i] += out[1][i]
		case flacMidSide:
			mid, side := out[0][i]<<1|out[1][i]&1, out[1][i]
			out[0][i], out[1][i] = (mid+side)>>1, (mid-side)>>1
		}
	}
	return out
}

// flacTestInfo is a STREAMINFO block for a stream of the given format.
func flacTestInfo(rate, channels, depth int) []byte {
	b := make([]byte, 34)
	binary.BigEndian.PutUint16(b, 16)
	binary.BigEndian.PutUint16(b[2:], 65535)
	binary.BigEndian.PutUint64(b[10:], uint64(rate)<<44|uint64(channels-1)<<41|uint64(depth-1)<<36)
	return b
}

// checkFrame decodes a frame the test wrote and compares every sample: as
// the decoder holds it, and as it gives it out.
func checkFrame(t testing.TB, f *flacTestFrame, data []byte, what string) {
	t.Helper()
	d, err := NewFLACDecoder(flacTestInfo(f.rate, len(f.subs), f.depth))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	// Bytes after the frame are not the decoder's concern.
	got, err := d.Decode(append(data[:len(data):len(data)], 0xff, 0xf8, 1, 2, 3))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	want := f.pcm()
	n := len(want[0])
	if got.SampleRate != f.rate || got.Channels != len(want) || len(got.Samples) != n*len(want) {
		t.Fatalf("%s: %d samples at %d Hz in %d channels, want %d at %d Hz in %d", what, len(got.Samples), got.SampleRate, got.Channels, n*len(want), f.rate, len(want))
	}
	scale := math.Ldexp(1, 1-f.depth)
	for c := range want {
		for i, v := range want[c] {
			if int64(d.ch[c][i]) != v {
				t.Fatalf("%s: sample %d of channel %d is %d, was %d", what, i, c, d.ch[c][i], v)
			}
			if g := got.Samples[i*len(want)+c]; g != float32(float64(v)*scale) {
				t.Fatalf("%s: sample %d of channel %d comes out as %v, want %v", what, i, c, g, float32(float64(v)*scale))
			}
		}
	}
}

// randomFrame makes up a frame: its header, and for each channel a signal
// and a way to code it. seen counts what was made.
func randomFrame(rng *rand.Rand, seen map[string]int) *flacTestFrame {
	f := &flacTestFrame{rateHow: 't', depthHow: 't', sizeHow: 't'}
	// The header.
	switch rng.IntN(6) {
	case 0:
		f.rate, f.rateHow = 1000*(1+rng.IntN(255)), 'k'
	case 1:
		f.rate, f.rateHow = 1+rng.IntN(65535), 'h'
	case 2:
		f.rate, f.rateHow = 10*(1+rng.IntN(65535)), 'd'
	case 3:
		f.rate, f.rateHow = 1+rng.IntN(1<<20-1), 0
	default:
		f.rate = []int{88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rng.IntN(11)]
	}
	seen["rate "+string(rune(max(f.rateHow, '-')))]++
	if f.depth = []int{8, 12, 16, 20, 24, 32}[rng.IntN(6)]; rng.IntN(5) == 0 {
		f.depth, f.depthHow = 4+rng.IntN(29), 0
	}
	seen[fmt.Sprintf("depth %d", f.depth)]++
	n := []int{192, 576, 1152, 2304, 4608, 256, 512, 1024, 2048, 4096}[rng.IntN(10)]
	// Mostly small blocks: the test's own checksums go bit by bit.
	switch k := rng.IntN(60); {
	case k < 20:
		n, f.sizeHow = 1+rng.IntN(256), 'b'
	case k < 32:
		n, f.sizeHow = 1+rng.IntN(3000), 'w'
	case k < 34:
		n = []int{8192, 16384, 32768}[rng.IntN(3)]
	case k < 35:
		n, f.sizeHow = 65535-rng.IntN(2), 'w'
	}
	seen["size "+string(rune(f.sizeHow))]++
	if f.variable = rng.IntN(3) == 0; f.variable {
		f.number = rng.Uint64() >> uint(28+rng.IntN(36))
		seen["variable"]++
	} else {
		f.number = rng.Uint64() >> uint(33+rng.IntN(31))
	}
	channels := 1 + rng.IntN(8)
	f.assign = channels - 1
	if rng.IntN(2) == 0 {
		channels, f.assign = 2, flacLeftSide+rng.IntN(3)
	}
	seen[fmt.Sprintf("channels %d", f.assign)]++

	// signal fills y with a sound of the given size: quiet or loud, smooth
	// or not, or at the ends of the range.
	signal := func(y []int64, bps int) {
		top := int64(1)<<uint(bps-1) - 1
		level := float64(top) * math.Pow(rng.Float64(), 3)
		freq, phase := 0.2*rng.Float64()*rng.Float64(), 7*rng.Float64()
		noise := level * []float64{0, 0.001, 0.05, 1}[rng.IntN(4)]
		kind := rng.IntN(10)
		for i := range y {
			v := level*math.Sin(phase+freq*float64(i)) + noise*(2*rng.Float64()-1)
			switch kind {
			case 0:
				v = float64(top) * float64(1-2*rng.IntN(2)) // the two ends
			case 1:
				v = float64(top) * (2*rng.Float64() - 1) // anything
			}
			y[i] = max(-top-1, min(top, int64(math.Round(v))))
		}
		switch kind {
		case 0:
			y[rng.IntN(len(y))] = -top - 1
		case 2:
			for i := range y {
				y[i] = y[0]
			}
		}
	}
	// plan decides how a channel of samples of bps bits is to be coded.
	plan := func(s *flacTestSub, n, bps int) {
		switch k := rng.IntN(10); {
		case k == 0:
			s.kind = 1
		case k < 5 || n < 2:
			s.kind = 8 + rng.IntN(min(5, n))
		default:
			order := 1 + rng.IntN(min(32, n-1))
			if rng.IntN(2) == 0 {
				order = 1 + rng.IntN(min(8, n-1))
			}
			s.kind = 31 + order
			// Coefficients whose sum is about one, after the shift.
			s.shift = rng.IntN(16)
			gain := 0.3 + 0.65*rng.Float64()
			weight := make([]float64, order)
			var total float64
			for j := range weight {
				weight[j] = (2*rng.Float64() - 1) / float64(j+1)
				total += math.Abs(weight[j])
			}
			s.coef = make([]int64, order)
			s.precision = 1
			for j := range weight {
				c := int64(math.Round(weight[j] / total * gain * math.Ldexp(1, s.shift)))
				s.coef[j] = max(-1<<14, min(1<<14-1, c))
				s.precision = max(s.precision, bitsFor(s.coef[j]))
			}
			s.precision += rng.IntN(16 - s.precision)
		}
		order := s.order()
		for s.partOrder = rng.IntN(16); n&(1<<uint(s.partOrder)-1) != 0 || n>>uint(s.partOrder) <= order; {
			s.partOrder--
		}
		s.rice5 = rng.IntN(3) == 0
		if rng.IntN(4) == 0 {
			s.escape = 1 + rng.IntN(3)
		}
	}
	// drive makes the samples of a channel from a residual, through the
	// predictor planned for it, so that the residual is small whatever the
	// predictor: that is what a linear predictor of large samples needs.
	drive := func(s *flacTestSub, bps int) {
		top := int64(1)<<uint(bps-1) - 1
		spread := float64(top) * 0.02 * math.Pow(rng.Float64(), 4)
		order := s.order()
		for i := range s.x {
			if i < order {
				s.x[i] = int64(float64(top) * 0.5 * (2*rng.Float64() - 1))
				continue
			}
			v := s.predict(s.x, i) + int64(math.Round(spread*rng.NormFloat64()))
			s.x[i] = max(-top-1, min(top, v))
		}
	}

	// channel makes the samples of a channel and plans its coding. Now and
	// then the low bits of every sample are zero, and are left out of the
	// coding. A channel that is a difference cannot be the lowest value its
	// bits hold: no left and right give it.
	channel := func(s *flacTestSub, bps int, difference bool) {
		if rng.IntN(5) == 0 {
			s.wasted = 1 + rng.IntN(min(bps-1, 12))
		}
		coded := bps - s.wasted
		plan(s, n, coded)
		if s.kind >= 32 && rng.IntN(3) > 0 {
			drive(s, coded)
		} else {
			signal(s.x, coded)
		}
		for i := range s.x {
			if lowest := int64(-1) << uint(coded-1); difference && s.x[i] == lowest {
				s.x[i]++
			}
			s.x[i] <<= uint(s.wasted)
		}
	}
	f.subs = make([]flacTestSub, channels)
	for c := range f.subs {
		f.subs[c].x = make([]int64, n)
	}
	side := flacSide(f.assign)
	top := int64(1)<<uint(f.depth-1) - 1
	if side >= 0 {
		// The difference of the channels first, then a left and a right
		// that have it.
		s, other := &f.subs[side], &f.subs[1-side]
		channel(s, f.depth+1, true)
		signal(other.x, f.depth)
		for i, d := range s.x {
			// Left goes where there is a right for it: right is left less
			// the difference.
			l := other.x[i]/2 + d/2
			l = max(l, -top-1, -top-1+d)
			l = min(l, top, top+d)
			r := l - d
			switch f.assign {
			case flacLeftSide:
				other.x[i] = l
			case flacSideRight:
				other.x[i] = r
			default:
				other.x[i] = (l + r) >> 1
			}
		}
		plan(other, n, f.depth)
	} else {
		for c := range f.subs {
			channel(&f.subs[c], f.depth, false)
		}
	}
	for c := range f.subs {
		s := &f.subs[c]
		constant := true
		for _, v := range s.x {
			constant = constant && v == s.x[0]
		}
		if constant && rng.IntN(2) == 0 {
			s.kind = 0
		}
	}
	return f
}

// TestFLACDecodeFrames decodes frames of every kind the format has.
func TestFLACDecodeFrames(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32))
	seen := map[string]int{}
	const frames = 2500
	for i := 0; i < frames; i++ {
		f := randomFrame(rng, seen)
		data := f.bytes(t)
		for c := range f.subs {
			if data != nil {
				break
			}
			// A channel that cannot be coded as planned (the residual
			// does not fit) is stored as it is.
			seen["fallback"]++
			f.subs[c].kind = 1
			data = f.bytes(t)
		}
		if data == nil {
			t.Fatalf("frame %d cannot be written", i)
		}
		what := fmt.Sprintf("frame %d (%d samples of %d bits, channels %d", i, len(f.subs[0].x), f.depth, f.assign)
		for c, s := range f.subs {
			name := fmt.Sprintf("kind %d", min(s.kind, 32))
			if s.kind >= 32 && (f.depth == 32 || c == flacSide(f.assign) && f.depth >= 24) {
				name = "wide lpc"
			}
			if c == flacSide(f.assign) && f.depth == 32 {
				seen[fmt.Sprintf("33 bits, kind %d", min(s.kind, 32))]++
			}
			seen[name]++
			if s.kind >= 8 {
				seen[fmt.Sprintf("rice5 %v", s.rice5)]++
				seen[fmt.Sprintf("escape %v", s.escape > 0)]++
			}
			if s.wasted > 0 {
				seen["wasted"]++
			}
			what += fmt.Sprintf("; kind %d wasted %d", s.kind, s.wasted)
		}
		checkFrame(t, f, data, what+")")
	}
	// The frames made must be of every kind, or the test is not what it
	// says.
	for _, want := range []string{"rate t", "rate k", "rate h", "rate d", "rate -", "size t", "size b", "size w", "variable",
		"depth 4", "depth 8", "depth 12", "depth 16", "depth 20", "depth 24", "depth 31", "depth 32",
		"channels 0", "channels 1", "channels 2", "channels 3", "channels 4", "channels 5", "channels 6", "channels 7",
		"channels 8", "channels 9", "channels 10", "kind 0", "kind 1", "kind 8", "kind 9", "kind 10", "kind 11", "kind 12",
		"kind 32", "wide lpc", "33 bits, kind 1", "33 bits, kind 8", "33 bits, kind 12", "33 bits, kind 32",
		"rice5 true", "rice5 false", "escape true", "escape false", "wasted"} {
		if seen[want] < 5 {
			t.Errorf("%d frames with %q among the %d made", seen[want], want, frames)
		}
	}
	if seen["fallback"] > frames/4 {
		t.Errorf("%d of %d frames could not be coded as planned", seen["fallback"], frames)
	}
	t.Logf("%v", seen)
}

// TestFLACDecodeDirected holds a few cases that chance does not make: the
// two ends of every field.
func TestFLACDecodeDirected(t *testing.T) {
	ramp := func(n int, f func(i int) int64) []int64 {
		x := make([]int64, n)
		for i := range x {
			x[i] = f(i)
		}
		return x
	}
	for name, f := range map[string]*flacTestFrame{
		// A linear predictor of the highest order and the largest
		// coefficients there are, on the lowest samples: the first sixteen
		// products come to 2^49 before the other sixteen take them back
		// (§A.3).
		"the largest products": func() *flacTestFrame {
			s := flacTestSub{kind: 63, precision: 15, shift: 15, rice5: true, x: ramp(34, func(int) int64 { return math.MinInt32 }),
				coef: ramp(32, func(j int) int64 {
					if j < 16 {
						return 1<<14 - 1
					}
					return -1 << 14
				})}
			for i := 32; i < len(s.x); i++ {
				s.x[i] = max(math.MinInt32, min(math.MaxInt32, s.predict(s.x, i)+5))
			}
			return &flacTestFrame{rate: 48000, rateHow: 't', depth: 32, depthHow: 't', sizeHow: 'b', subs: []flacTestSub{s}}
		}(),
		"a shift of 0": {rate: 8000, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 'b', subs: []flacTestSub{{kind: 32,
			x: ramp(100, func(i int) int64 { return int64(i%7) - 3 }), coef: []int64{-1}, precision: 1}}},
		"a sample number of 36 bits": {variable: true, number: 1<<36 - 1, rate: 44100, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 't',
			subs: []flacTestSub{{kind: 1, x: ramp(192, func(i int) int64 { return int64(i * 100) })}}},
		"a frame number of 31 bits": {number: 1<<31 - 1, rate: 44100, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 't',
			subs: []flacTestSub{{kind: 12, x: ramp(192, func(i int) int64 { return int64(i * i) })}}},
		"one sample": {rate: 44100, rateHow: 't', depth: 24, depthHow: 't', sizeHow: 'b',
			subs: []flacTestSub{{kind: 8, x: []int64{-8388608}}, {kind: 1, x: []int64{8388607}}}, assign: 1},
		"all but one bit wasted": {rate: 44100, rateHow: 't', depth: 32, depthHow: 't', sizeHow: 'b', subs: []flacTestSub{
			{kind: 9, x: ramp(64, func(i int) int64 { return -int64(i%2) << 31 }), wasted: 31}}},
		// Left is 0 or the lowest sample, and so is right: their
		// difference has two bits above the 31 that are zero.
		"a difference of 33 bits, 31 of them wasted": {rate: 44100, rateHow: 't', depth: 32, depthHow: 't', sizeHow: 'b', assign: flacLeftSide, subs: []flacTestSub{
			{kind: 1, x: ramp(64, func(i int) int64 { return -int64(i%2) << 31 }), wasted: 31},
			{kind: 9, x: ramp(64, func(i int) int64 { return -int64(i%2)<<31 + int64(i/2%2)<<31 }), wasted: 31}}},
		"the largest partition order": {rate: 44100, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 't',
			subs: []flacTestSub{{kind: 8, x: ramp(32768, func(i int) int64 { return int64(i%251) - 125 }), partOrder: 15}}},
		"every partition escaped, some empty": {rate: 44100, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 't',
			subs: []flacTestSub{{kind: 9, x: ramp(4096, func(i int) int64 { return int64(i / 2048 * (i % 5)) }), partOrder: 4, escape: 1}}},
		"a residual of 31 bits, escaped": {rate: 44100, rateHow: 't', depth: 32, depthHow: 't', sizeHow: 'b',
			subs: []flacTestSub{{kind: 8, x: ramp(16, func(i int) int64 { return (1<<30 - 1) * int64(1-2*(i%2)) }), escape: 1}}},
		"the largest residual": {rate: 44100, rateHow: 't', depth: 32, depthHow: 't', sizeHow: 'b',
			subs: []flacTestSub{{kind: 8, x: ramp(16, func(i int) int64 { return math.MaxInt32 * int64(1-2*(i%2)) }), rice5: true}}},
	} {
		data := f.bytes(t)
		if data == nil {
			t.Errorf("%s: the frame cannot be written", name)
			continue
		}
		checkFrame(t, f, data, name)
	}
}

// TestFLACDecodeRoundTrip decodes what the module's encoder writes.
func TestFLACDecodeRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name           string
		rate, channels int
		frames         int
		f              func(i, ch int) float64
	}{
		{"music", 44100, 2, 44100, musicLike(44100, 3)},
		{"music in mono", 16000, 1, 16000, musicLike(16000, 4)},
		{"silence", 48000, 2, 10000, func(i, ch int) float64 { return 0 }},
		{"opposite channels", 48000, 2, 20000, func(i, ch int) float64 { return sine(48000, 440, 0.5)(i, 0) * float64(1-2*ch) }},
		{"opposite extremes", 44100, 2, 9000, func(i, ch int) float64 { return float64(1 - 2*((i/3+ch)%2)) }},
		{"a rate outside the header", 70001, 2, 9000, musicLike(70001, 5)},
		{"five samples", 48000, 2, 5, musicLike(48000, 6)},
	} {
		t.Run(c.name, func(t *testing.T) {
			pcm := render(c.frames, c.channels, c.f)
			file := encodeFLAC(t, c.rate, c.channels, pcm, 4800)
			sizes := decodeFLAC(t, file).frames
			d, err := NewFLACDecoder(file[:FLACHeaderSize])
			if err != nil {
				t.Fatal(err)
			}
			var got []float32
			for at := FLACHeaderSize; len(sizes) > 0; at, sizes = at+sizes[0], sizes[1:] {
				fr, err := d.Decode(file[at : at+sizes[0]])
				if err != nil {
					t.Fatal(err)
				}
				if fr.SampleRate != c.rate || fr.Channels != c.channels {
					t.Fatalf("%d Hz in %d channels, want %d Hz in %d", fr.SampleRate, fr.Channels, c.rate, c.channels)
				}
				got = append(got, fr.Samples...)
			}
			if len(got) != len(pcm) {
				t.Fatalf("%d samples decoded, %d encoded", len(got), len(pcm))
			}
			// As a 16-bit WAVE file of the same samples is read.
			for i, v := range pcm {
				if got[i] != float32(v)/32768 {
					t.Fatalf("sample %d is %v, was %d (%v)", i, got[i], v, float32(v)/32768)
				}
			}
		})
	}
}

// TestFLACDecodeDamage checks what the decoder makes of frames that are not
// valid: silence of the frame's length where the header holds, an error
// where it does not, and never a sample of the damaged sound.
func TestFLACDecodeDamage(t *testing.T) {
	tone := func(n int) []int64 {
		x := make([]int64, n)
		for i := range x {
			x[i] = int64(20000 * math.Sin(float64(i)/9))
		}
		return x
	}
	frame := func(mod func(f *flacTestFrame)) []byte {
		f := &flacTestFrame{rate: 44100, rateHow: 't', depth: 16, depthHow: 't', sizeHow: 't', assign: 1,
			subs: []flacTestSub{{kind: 12, x: tone(1024), partOrder: 3}, {kind: 35, x: tone(1024), coef: []int64{3, -2, 1, 1}, precision: 4, shift: 1}}}
		if mod != nil {
			mod(f)
		}
		data := f.bytes(t)
		if data == nil {
			t.Fatal("the frame cannot be written")
		}
		return data
	}
	// seal puts the right checksum on a frame whose content was changed.
	seal := func(b []byte) []byte {
		binary.BigEndian.PutUint16(b[len(b)-2:], uint16(refCRC(b[:len(b)-2], 16, 0x8005)))
		return b
	}
	good := frame(nil)
	hdr, ok := codechdr.FLACFrameHeader(good)
	if !ok || hdr.BlockSize != 1024 {
		t.Fatalf("header of the frame: %+v, %v", hdr, ok)
	}
	// set changes bits of the byte at off, counted from the header's end.
	set := func(off int, and, or byte) []byte {
		b := append([]byte(nil), good...)
		b[hdr.Size+off] = b[hdr.Size+off]&and | or
		return seal(b)
	}
	silent := map[string][]byte{
		"a flipped bit":                 func() []byte { b := append([]byte(nil), good...); b[len(b)/2] ^= 4; return b }(),
		"a wrong checksum":              func() []byte { b := append([]byte(nil), good...); b[len(b)-1]++; return b }(),
		"cut short":                     good[:len(good)/2],
		"cut before the checksum":       good[:len(good)-2],
		"cut after the header":          good[:hdr.Size],
		"a subframe that starts with 1": set(0, 0xff, 0x80),
		"a reserved subframe type":      set(0, 0x81, 2<<1),
		"another reserved type":         set(0, 0x81, 13<<1),
		"a reserved residual coding":    set(1+4*2, 0x3f, 0x80),
		"coefficients of 16 bits":       frame(func(f *flacTestFrame) { f.subs[1].precision = 16 }),
		"a negative shift":              frame(func(f *flacTestFrame) { f.subs[1].shift = 31 }),
		"more wasted bits than a sample has": frame(func(f *flacTestFrame) {
			f.subs[0] = flacTestSub{kind: 1, x: make([]int64, 1024), wasted: 16}
		}),
	}
	// A partition order that does not divide the block, and a predictor
	// longer than the block, cannot be made by the writer: they are written
	// for a block that suits and the block size in the header is changed.
	odd := frame(func(f *flacTestFrame) { f.sizeHow, f.subs[0].partOrder = 'w', 3 })
	binary.BigEndian.PutUint16(odd[5:], 1021-1)
	odd[7] = byte(refCRC(odd[:7], 8, 0x07))
	silent["a partition order that does not divide the block"] = seal(odd)
	short := frame(func(f *flacTestFrame) { f.sizeHow, f.subs[0].partOrder = 'w', 0 })
	binary.BigEndian.PutUint16(short[5:], 3-1)
	short[7] = byte(refCRC(short[:7], 8, 0x07))
	silent["a predictor longer than the block"] = seal(short)

	for name, data := range silent {
		d, _ := NewFLACDecoder(nil)
		fr, err := d.Decode(data)
		if err != nil {
			t.Errorf("%s: %v, want silence", name, err)
			continue
		}
		h, _ := codechdr.FLACFrameHeader(data)
		if fr.SampleRate != 44100 || fr.Channels != 2 || len(fr.Samples) != 2*h.BlockSize {
			t.Errorf("%s: %d samples at %d Hz in %d channels, want %d at 44100 Hz in 2", name, len(fr.Samples), fr.SampleRate, fr.Channels, 2*h.BlockSize)
		}
		for i, v := range fr.Samples {
			if v != 0 {
				t.Errorf("%s: sample %d is %v, want silence", name, i, v)
				break
			}
		}
	}
	// The frame is good as it was written.
	d, _ := NewFLACDecoder(nil)
	if fr, err := d.Decode(good); err != nil || fr.Samples[2*500] == 0 {
		t.Fatalf("the frame as written: %v", err)
	}

	// What is no frame at all.
	for name, data := range map[string][]byte{
		"nothing":                 nil,
		"four bytes":              good[:4],
		"no sync code":            append([]byte{0xff, 0xf0}, good[2:]...),
		"a wrong header checksum": func() []byte { b := append([]byte(nil), good...); b[hdr.Size-1] ^= 1; return b }(),
		"an MP3 frame":            {0xff, 0xfb, 0x90, 0x00, 0, 0, 0, 0, 0, 0},
		"a header cut short":      good[:hdr.Size-1],
		"a reserved channel code": func() []byte {
			b := append([]byte(nil), good...)
			b[3] |= 0xf0
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
		"a reserved sample size": func() []byte {
			b := append([]byte(nil), good...)
			b[3] = b[3]&0xf0 | 3<<1
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
		"the reserved bit": func() []byte {
			b := append([]byte(nil), good...)
			b[3] |= 1
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
		"a reserved block size": func() []byte {
			b := append([]byte(nil), good...)
			b[2] &= 0x0f
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
		"the forbidden rate code": func() []byte {
			b := append([]byte(nil), good...)
			b[2] |= 0x0f
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
		"a number that is no code": func() []byte {
			b := append([]byte(nil), good...)
			b[4] = 0x80
			b[5] = byte(refCRC(b[:5], 8, 7))
			return b
		}(),
	} {
		d, _ := NewFLACDecoder(nil)
		if _, err := d.Decode(data); !errors.Is(err, ErrNotFLAC) {
			t.Errorf("%s: %v, want ErrNotFLAC", name, err)
		}
	}

	// A frame that leaves its format to a header the decoder was not
	// given, and one of another format than the stream's.
	bare := (&flacTestFrame{rate: 44100, depth: 16, sizeHow: 't', subs: []flacTestSub{{kind: 1, x: tone(192)}}}).bytes(t)
	d, _ = NewFLACDecoder(nil)
	if _, err := d.Decode(bare); err == nil || !strings.Contains(err.Error(), "STREAMINFO") {
		t.Errorf("a frame that states no format, without STREAMINFO: %v", err)
	}
	d, _ = NewFLACDecoder(flacTestInfo(44100, 1, 16))
	if fr, err := d.Decode(bare); err != nil || fr.SampleRate != 44100 || fr.Samples[100] == 0 {
		t.Errorf("a frame that states no format, with STREAMINFO: %v", err)
	}
	for name, info := range map[string][]byte{"rate": flacTestInfo(48000, 2, 16), "channels": flacTestInfo(44100, 3, 16), "sample size": flacTestInfo(44100, 2, 24)} {
		d, _ := NewFLACDecoder(info)
		if _, err := d.Decode(good); err == nil || errors.Is(err, ErrNotFLAC) {
			t.Errorf("a frame with another %s than the stream's: %v", name, err)
		}
	}
	d, _ = NewFLACDecoder(nil)
	d.Decode(good)
	if _, err := d.Decode(bare); err == nil {
		t.Error("a mono frame after a stereo one, without STREAMINFO: no error")
	}
	if _, err := NewFLACDecoder(flacTestInfo(44100, 2, 3)); err == nil {
		t.Error("no error for a stream of 3-bit samples")
	}
	if _, err := NewFLACDecoder([]byte("fLaC")); err == nil {
		t.Error("no error for a configuration cut short")
	}
}

// TestFLACDecodeAgainstFFmpeg decodes files that ffmpeg's encoder wrote,
// which uses linear prediction and everything else the format has for the
// sample sizes it takes, and compares with ffmpeg's decode of them: the
// same samples. The frames are cut out of the file by the test's own
// decoder, which must agree too.
func TestFLACDecodeAgainstFFmpeg(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	music := `aevalsrc=0.5*sin(2*PI*(220+30*t)*t)+0.2*sin(2*PI*1760*t)*lt(mod(t\,0.3)\,0.1)|0.4*sin(2*PI*(331-20*t)*t)+0.1*sin(2*PI*5000*t):d=3:s=`
	noise := "anoisesrc=d=2:c=pink:a=0.5:r="
	for _, c := range []struct {
		name   string
		source string
		args   []string
		depth  int
	}{
		{"s16_44k", music + "44100", []string{"-sample_fmt", "s16"}, 16},
		{"s16_level0", music + "48000", []string{"-sample_fmt", "s16", "-compression_level", "0"}, 16},
		{"s16_level12", music + "48000", []string{"-sample_fmt", "s16", "-compression_level", "12"}, 16},
		{"s16_cholesky", music + "48000", []string{"-sample_fmt", "s16", "-lpc_type", "cholesky", "-compression_level", "8"}, 16},
		{"s16_mono_8k", music + "8000", []string{"-sample_fmt", "s16", "-ac", "1"}, 16},
		{"s16_small_blocks", music + "22050", []string{"-sample_fmt", "s16", "-frame_size", "500"}, 16},
		{"s16_large_blocks", music + "96000", []string{"-sample_fmt", "s16", "-frame_size", "16384"}, 16},
		{"s16_independent", music + "44100", []string{"-sample_fmt", "s16", "-ch_mode", "indep"}, 16},
		{"s16_mid_side", music + "44100", []string{"-sample_fmt", "s16", "-ch_mode", "mid_side"}, 16},
		{"s16_left_side", music + "44100", []string{"-sample_fmt", "s16", "-ch_mode", "left_side"}, 16},
		{"s16_right_side", music + "44100", []string{"-sample_fmt", "s16", "-ch_mode", "right_side"}, 16},
		{"s16_noise", noise + "44100", []string{"-sample_fmt", "s16", "-ac", "2"}, 16},
		{"s24_48k", music + "48000", []string{"-sample_fmt", "s32"}, 24},
		{"s24_level12", music + "96000", []string{"-sample_fmt", "s32", "-compression_level", "12"}, 24},
		{"s24_noise", noise + "48000", []string{"-sample_fmt", "s32", "-ac", "2"}, 24},
		{"s24_loud_noise", "anoisesrc=d=2:c=white:a=1:r=48000", []string{"-sample_fmt", "s32", "-ac", "2"}, 24},
		{"s24_5.1", music + "48000", []string{"-sample_fmt", "s32", "-af", "pan=5.1|c0=c0|c1=c1|c2=0.5*c0+0.5*c1|c3=0.1*c0|c4=0.3*c1|c5=0.3*c0-0.2*c1"}, 24},
		{"s16_7.1", music + "44100", []string{"-sample_fmt", "s16", "-af", "pan=7.1|c0=c0|c1=c1|c2=0.5*c0|c3=0.1*c1|c4=0.3*c1|c5=0.3*c0|c6=c0-c1|c7=0.7*c1"}, 16},
		{"s16_3.0", music + "32000", []string{"-sample_fmt", "s16", "-af", "pan=3.0|c0=c0|c1=c1|c2=0.5*c0+0.5*c1"}, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".flac")
			args := append([]string{"-f", "lavfi", "-i", c.source, "-c:a", "flac"}, c.args...)
			if err := oracle.TryFFmpeg(append(args, path)...); err != nil {
				oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
			}
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ref := decodeFLAC(t, file)
			if ref.bps != c.depth {
				t.Fatalf("the file holds samples of %d bits, want %d", ref.bps, c.depth)
			}
			// ffmpeg's decode, as 32-bit samples: the file's own, shifted up.
			raw := oracle.FFmpegOutput(t, "-i", path, "-c:a", "pcm_s32le", "-f", "s32le", "-")
			if len(raw) != 4*len(ref.samples) {
				t.Fatalf("ffmpeg decodes %d samples; the file has %d", len(raw)/4, len(ref.samples))
			}
			meta := len(file)
			for _, size := range ref.frames {
				meta -= size
			}
			d, err := NewFLACDecoder(file[:42])
			if err != nil {
				t.Fatal(err)
			}
			scale := math.Ldexp(1, 1-c.depth)
			i := 0
			for at, sizes := meta, ref.frames; len(sizes) > 0; at, sizes = at+sizes[0], sizes[1:] {
				fr, err := d.Decode(file[at : at+sizes[0]])
				if err != nil {
					t.Fatalf("frame at byte %d: %v", at, err)
				}
				if fr.SampleRate != ref.rate || fr.Channels != ref.channels {
					t.Fatalf("%d Hz in %d channels, want %d Hz in %d", fr.SampleRate, fr.Channels, ref.rate, ref.channels)
				}
				for j, g := range fr.Samples {
					if i >= len(ref.samples) {
						t.Fatalf("more than the %d samples of the file", len(ref.samples))
					}
					want := int32(binary.LittleEndian.Uint32(raw[4*i:])) >> uint(32-c.depth)
					if got := d.ch[j%fr.Channels][j/fr.Channels]; got != want || ref.samples[i] != want {
						t.Fatalf("sample %d: %d here, %d in the test's decoder, %d from ffmpeg", i, got, ref.samples[i], want)
					}
					if g != float32(float64(want)*scale) {
						t.Fatalf("sample %d comes out as %v, want %v", i, g, float32(float64(want)*scale))
					}
					i++
				}
			}
			if i != len(ref.samples) {
				t.Fatalf("%d samples decoded of %d", i, len(ref.samples))
			}
			t.Logf("%d frames, %d channels, subframes %v", len(ref.frames), ref.channels, ref.kinds)
		})
	}
}

// The three example files of RFC 9639 (appendix D), which the standard
// decodes bit by bit: a frame of one sample with wasted bits; a file with a
// seek table, a comment and padding, whose frames use a fixed predictor in
// side and right, and a last block of three samples; and a linear predictor
// of 8-bit samples with a residual in two partitions.
var flacRFCExamples = []struct {
	name                  string
	file                  string
	rate, channels, depth int
	samples               int64
	title                 string
	first                 []int32 // the first sample frame
}{
	{"example 1", "664c6143800000221000100000000f00000f0ac442f0000000013e84b41807dc690307586a3dad1a2e0f" +
		"fff869180000bf0358fd03128baa9a", 44100, 2, 16, 1, "", []int32{25588, 10416}},
	{"example 2", "664c614300000022001000100000170000440ac442f000000013d5b0564975e98b8d8b930422757b8103" +
		"03000012000000000000000000000000000000000010" +
		"0400003a200000007265666572656e6365206c6962464c414320312e332e33203230313930383034010000000e000000" +
		"5449544c453dd7a9d79cd795d79d" + "81000006000000000000" +
		"fff86998000f9912086701623d1442998f5df70d6fe00c17caeb21000ee7a77a24a1590c1217b603097b784faa9a33d285e070ad5b1b4851b4010d99d2cd1a68f1e6b810" +
		"fff869180102a402c382c40bc14a03ee48dd03b67c1330", 44100, 2, 16, 19, "שלום", []int32{10372, 6070}},
	{"example 3", "664c6143800000221000100000001f00001f07d0007000000018f8f9e396f5cbcfc6dc807f9977906b32" +
		"fff868020017e944004f6f313d1047d227cb6d090831452bdc2822228057a3", 32000, 1, 8, 24, "", []int32{0}},
}

// TestFLACDecodeRFCExamples reads and decodes the standard's own example
// files: the sound must have the checksum each file states, which the
// standard checks too.
func TestFLACDecodeRFCExamples(t *testing.T) {
	for _, c := range flacRFCExamples {
		file, err := hex.DecodeString(c.file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := flac.Open(context.Background(), bytes.NewReader(file), int64(len(file)))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if f.SampleRate != c.rate || f.Channels != c.channels || f.BitDepth != c.depth || f.Samples != c.samples || f.Tags["title"] != c.title {
			t.Errorf("%s: %d samples at %d Hz in %d channels of %d bits, title %q", c.name, f.Samples, f.SampleRate, f.Channels, f.BitDepth, f.Tags["title"])
		}
		d, err := NewFLACDecoder(f.StreamInfo)
		if err != nil {
			t.Fatal(err)
		}
		sum, width := md5.New(), (f.BitDepth+7)/8
		var total int64
		for rd := f.NewReader(bytes.NewReader(file)); ; {
			frame, err := rd.Next()
			if err != nil {
				break
			}
			fr, err := d.Decode(frame)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			for i := 0; i < fr.Frames(); i++ {
				for ch := 0; ch < fr.Channels; ch++ {
					v := d.ch[ch][i]
					if total == 0 && i == 0 && v != c.first[ch] {
						t.Errorf("%s: the first sample of channel %d is %d, want %d", c.name, ch, v, c.first[ch])
					}
					if want := float32(v) / float32(int(1)<<uint(c.depth-1)); fr.Samples[i*fr.Channels+ch] != want {
						t.Errorf("%s: sample %d comes out as %v, want %v", c.name, i, fr.Samples[i*fr.Channels+ch], want)
					}
					sum.Write([]byte{byte(v), byte(v >> 8)}[:width])
				}
			}
			total += int64(fr.Frames())
		}
		if total != c.samples {
			t.Errorf("%s: %d samples decoded, want %d", c.name, total, c.samples)
		}
		if got := sum.Sum(nil); !bytes.Equal(got, f.StreamInfo[18:34]) {
			t.Errorf("%s: the sound decoded has checksum %x; the file states %x", c.name, got, f.StreamInfo[18:34])
		}
	}
}

// TestFLACFiles decodes the FLAC files of a directory, or those listed in a
// file, one path to a line, and holds each to two things: the checksum of
// the sound that its encoder put in its header, which needs no oracle, and
// ffmpeg's decode of it, sample for sample. It is for real files, and runs
// when it is told where they are:
//
//	GOVIDEO_FLAC_FILES=~/Music go test ./codec/native -run TestFLACFiles -v
func TestFLACFiles(t *testing.T) {
	src := os.Getenv("GOVIDEO_FLAC_FILES")
	if src == "" {
		t.Skip("GOVIDEO_FLAC_FILES is not set")
	}
	var files []string
	if st, err := os.Stat(src); err != nil {
		t.Fatal(err)
	} else if st.IsDir() {
		filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".flac") {
				files = append(files, p)
			}
			return nil
		})
	} else {
		list, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(list), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				files = append(files, line)
			}
		}
	}
	var seconds float64
	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		st, _ := file.Stat()
		f, err := flac.Open(context.Background(), file, st.Size())
		if err != nil {
			t.Errorf("%s: %v", path, err)
			file.Close()
			continue
		}
		d, err := NewFLACDecoder(f.StreamInfo)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			file.Close()
			continue
		}
		// The checksum is of the samples as little-endian PCM, each in the
		// whole bytes its size takes (RFC 9639 §8.2).
		sum, width := md5.New(), (f.BitDepth+7)/8
		var pcm []byte
		var all []int32
		var total, silent int64
		for rd := f.NewReader(file); ; {
			frame, err := rd.Next()
			if err != nil {
				break
			}
			fr, err := d.Decode(frame)
			if err != nil {
				t.Errorf("%s: sample %d: %v", path, total, err)
				break
			}
			h, _ := codechdr.FLACFrameHeader(frame)
			if !d.frame(h, f.BitDepth, frame) {
				silent++
			}
			pcm = pcm[:0]
			for i := 0; i < fr.Frames(); i++ {
				for c := 0; c < fr.Channels; c++ {
					v := d.ch[c][i]
					pcm = append(pcm, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
					pcm = pcm[:len(pcm)-4+width]
					all = append(all, v)
				}
			}
			sum.Write(pcm)
			total += int64(fr.Frames())
		}
		file.Close()
		seconds += float64(total) / float64(f.SampleRate)
		what := fmt.Sprintf("%s (%d Hz, %d channels, %d bits, %.1f s)", filepath.Base(path), f.SampleRate, f.Channels, f.BitDepth, float64(total)/float64(f.SampleRate))
		if silent > 0 {
			t.Errorf("%s: %d frames that could not be decoded", what, silent)
		}
		if total != f.Samples {
			t.Errorf("%s: %d samples decoded; the file has %d", what, total, f.Samples)
		}
		stated := f.StreamInfo[18:34]
		switch {
		case bytes.Equal(stated, make([]byte, 16)):
			t.Logf("%s: no checksum in the file", what)
		case !bytes.Equal(sum.Sum(nil), stated):
			t.Errorf("%s: the sound decoded has checksum %x; the file states %x", what, sum.Sum(nil), stated)
		}
		// ffmpeg, where it is installed and decodes the file at all: it
		// gives up on some that are not valid.
		raw, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", path, "-map", "0:a:0", "-c:a", "pcm_s32le", "-f", "s32le", "-").Output()
		if _, missing := exec.LookPath("ffmpeg"); missing != nil {
			continue
		}
		if err != nil {
			t.Logf("%s: ffmpeg does not decode it (%v); %d samples here", what, err, total)
			continue
		}
		{
			if len(raw) != 4*len(all) {
				t.Errorf("%s: ffmpeg decodes %d samples, %d here", what, len(raw)/4, len(all))
				continue
			}
			for i, v := range all {
				if want := int32(binary.LittleEndian.Uint32(raw[4*i:])) >> uint(32-f.BitDepth); v != want {
					t.Errorf("%s: sample %d is %d, %d in ffmpeg's decode", what, i, v, want)
					break
				}
			}
		}
	}
	t.Logf("%d files, %.0f s of sound", len(files), seconds)
}

// FuzzFLACDecode decodes whatever it is given as the subframes of a frame
// whose header the fuzzer chooses too: the checksums, which a fuzzer cannot
// get past, are left out. Nothing may panic or read outside its arrays.
func FuzzFLACDecode(f *testing.F) {
	rng := rand.New(rand.NewPCG(41, 42))
	for range 40 {
		fr := randomFrame(rng, map[string]int{})
		if len(fr.subs[0].x) > 600 {
			continue
		}
		if data := fr.bytes(f); data != nil {
			h, _ := codechdr.FLACFrameHeader(data)
			f.Add(data[h.Size:], uint16(h.BlockSize), uint8(fr.assign), uint8(fr.depth))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, block uint16, assign, depth uint8) {
		h := codechdr.FLACFrame{BlockSize: max(int(block), 1), Assignment: int(assign % 11), Channels: int(assign%11) + 1}
		if h.Assignment >= flacLeftSide {
			h.Channels = 2
		}
		d, _ := NewFLACDecoder(nil)
		bps := 4 + int(depth%29)
		if end, ok := d.subframes(h, bps, data); ok {
			if end > len(data) {
				t.Fatalf("the subframes end at byte %d of %d", end, len(data))
			}
			d.stereo(h, bps)
		}
		// And as a packet, header and all.
		d.Decode(data)
	})
}

func BenchmarkFLACDecode(b *testing.B) {
	for _, c := range []struct {
		name           string
		rate, channels int
	}{{"48k_stereo", 48000, 2}, {"16k_mono", 16000, 1}} {
		b.Run(c.name, func(b *testing.B) {
			pcm := render(c.rate*10, c.channels, musicLike(c.rate, 1))
			file := encodeFLAC(b, c.rate, c.channels, pcm, 4800)
			sizes := decodeFLAC(b, file).frames
			d, _ := NewFLACDecoder(file[:FLACHeaderSize])
			b.SetBytes(int64(2 * len(pcm)))
			b.ReportAllocs()
			for b.Loop() {
				at := FLACHeaderSize
				for _, size := range sizes {
					if _, err := d.Decode(file[at : at+size]); err != nil {
						b.Fatal(err)
					}
					at += size
				}
			}
		})
	}
}
