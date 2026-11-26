package native

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/gomcpgo/govideo/internal/bits"
)

// The tests in this file need no oracle: they decode layer III streams that
// are written here, bit by bit, from the layout of the standard. Each
// channel of a granule holds a few spectral lines of ±1, coded with the
// second table of quadruples, whose codes are the four values inverted;
// that is little of the format, but enough to say where a line must come
// out, how loud, and in which channel.

// l3Channel is one channel of a granule of a built stream.
type l3Channel struct {
	gain  int         // global_gain
	short bool        // three short blocks
	lines map[int]int // +1 or -1, by position in coded order
	scfc  int         // scalefac_compress
	scf   [][2]int    // the scalefactors: value and size in bits, in coded order
}

// l3Stream describes a built stream.
type l3Stream struct {
	rate, channels, kbps int
	modeExt              int // joint stereo with this mode extension, if not 0
	maxBegin             int // how far back a frame's audio data may start
}

func (s l3Stream) mpeg1() bool { return s.rate >= 32000 }

// samples is the number of samples of a channel in a frame.
func (s l3Stream) samples() int {
	if s.mpeg1() {
		return 1152
	}
	return 576
}

// build writes the frames of a stream. frames holds, for each frame, its
// granules' channels in order (two granules in MPEG-1, one at the lower
// rates).
func (s l3Stream) build(t testing.TB, frames [][]l3Channel) [][]byte {
	t.Helper()
	var hdr [4]byte
	hdr[0] = 0xff
	rates, kbps, side := []int{44100, 48000, 32000}, []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}, 17
	div := 1
	switch {
	case s.mpeg1():
		hdr[1] = 0xfb
		if s.channels == 2 {
			side = 32
		}
	default:
		hdr[1], div = 0xf3, 2
		if s.rate < 16000 {
			hdr[1], div = 0xe3, 4
		}
		kbps = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
		if s.channels == 1 {
			side = 9
		}
	}
	ri, bi := -1, -1
	for i, r := range rates {
		if r/div == s.rate {
			ri = i
		}
	}
	for i, k := range kbps {
		if k == s.kbps {
			bi = i
		}
	}
	// No frame is padded: at 44.1 kHz and its fractions, where a frame is
	// not a whole number of bytes, the stream is a little under its rate.
	size := s.samples() / 8 * s.kbps * 1000 / s.rate
	if ri < 0 || bi < 0 {
		t.Fatalf("no frames at %d Hz and %d kbit/s", s.rate, s.kbps)
	}
	hdr[2] = byte(bi<<4 | ri<<2)
	switch {
	case s.channels == 1:
		hdr[3] = 3 << 6
	case s.modeExt != 0:
		hdr[3] = byte(1<<6 | s.modeExt<<4)
	}
	slot := size - 4 - side

	// The audio data of the frames, laid end to end in the slots that the
	// frames leave after their side information; a frame's data starts no
	// more than maxBegin bytes before its own slot, and ends within it.
	var (
		main   = make([]byte, len(frames)*slot)
		end    int
		out    [][]byte
		perGr  = len(frames[0])
		expect = s.channels
	)
	if s.mpeg1() {
		expect *= 2
	}
	if perGr != expect {
		t.Fatalf("a frame of %d channel granules, want %d", perGr, expect)
	}
	for n, fr := range frames {
		var w bits.Writer
		part23 := make([]int, len(fr))
		for i, c := range fr {
			from := w.Len(0)
			for _, v := range c.scf {
				w.U(uint32(v[0]), v[1])
			}
			last := -1
			for l := range c.lines {
				last = max(last, l)
			}
			for q := 0; q <= last/4 && last >= 0; q++ {
				var nib uint32
				for k := 0; k < 4; k++ {
					if c.lines[4*q+k] != 0 {
						nib |= 8 >> k
					}
				}
				w.U(^nib, 4)
				for k := 0; k < 4; k++ {
					if v := c.lines[4*q+k]; v != 0 {
						w.U(uint32(max(-v, 0)), 1)
					}
				}
			}
			part23[i] = w.Len(0) - from
		}
		w.Align()
		data := w.Bytes()
		start := max(end, n*slot-s.maxBegin)
		if start+len(data) > (n+1)*slot {
			t.Fatalf("frame %d: %d bytes of audio data do not fit", n, len(data))
		}
		copy(main[start:], data)
		end = start + len(data)

		var sw bits.Writer
		if s.mpeg1() {
			sw.U(uint32(n*slot-start), 9)
			sw.U(0, 7-2*s.channels) // private bits
			sw.U(0, 4*s.channels)   // scfsi
		} else {
			sw.U(uint32(n*slot-start), 8)
			sw.U(0, s.channels)
		}
		for i, c := range fr {
			sw.U(uint32(part23[i]), 12)
			sw.U(0, 9) // big_values
			sw.U(uint32(c.gain), 8)
			if s.mpeg1() {
				sw.U(uint32(c.scfc), 4)
			} else {
				sw.U(uint32(c.scfc), 9)
			}
			if c.short {
				sw.U(1, 1)  // window switching
				sw.U(2, 2)  // block_type
				sw.U(0, 1)  // mixed_block_flag
				sw.U(0, 10) // table_select
				sw.U(0, 9)  // subblock_gain
			} else {
				sw.U(0, 1)
				sw.U(0, 15) // table_select
				sw.U(0, 7)  // region counts
			}
			if s.mpeg1() {
				sw.U(0, 1) // preflag
			}
			sw.U(0, 1) // scalefac_scale
			sw.U(1, 1) // count1table_select
		}
		sw.Align()
		if len(sw.Bytes()) != side {
			t.Fatalf("side information of %d bytes, want %d", len(sw.Bytes()), side)
		}
		out = append(out, append(append(hdr[:4:4], sw.Bytes()...), make([]byte, slot)...))
	}
	for n := range out {
		copy(out[n][4+side:], main[n*slot:(n+1)*slot])
	}
	return out
}

// decodeL3 decodes frames with a new decoder and returns the samples of
// each channel.
func decodeL3(t testing.TB, s l3Stream, frames [][]byte) [][]float32 {
	t.Helper()
	dec := NewMP3Decoder()
	ch := make([][]float32, s.channels)
	for n, f := range frames {
		fr, err := dec.Decode(f)
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		if fr.SampleRate != s.rate || fr.Channels != s.channels || fr.Frames() != s.samples() {
			t.Fatalf("frame %d: %d samples at %d Hz in %d channels, want %d, %d, %d",
				n, fr.Frames(), fr.SampleRate, fr.Channels, s.samples(), s.rate, s.channels)
		}
		for i, v := range fr.Samples {
			ch[i%s.channels] = append(ch[i%s.channels], v)
		}
	}
	return ch
}

// spectrum returns the power of x, under a raised-cosine window, at the
// centre of each of n bands that share out the frequencies up to half the
// sample rate.
func spectrum(x []float32, n int) []float64 {
	p := make([]float64, n)
	for k := range p {
		w := math.Pi * (float64(k) + 0.5) / float64(n)
		var re, im float64
		for i, v := range x {
			v := float64(v) * (0.5 - 0.5*math.Cos(2*math.Pi*(float64(i)+0.5)/float64(len(x))))
			re += v * math.Cos(w*float64(i))
			im += v * math.Sin(w*float64(i))
		}
		p[k] = re*re + im*im
	}
	return p
}

func peak(p []float64) (at int) {
	for i, v := range p {
		if v > p[at] {
			at = i
		}
	}
	return at
}

func rms(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

// repeat makes n frames that each hold the same channel granules.
func repeat(n int, gr ...l3Channel) [][]l3Channel {
	out := make([][]l3Channel, n)
	for i := range out {
		out[i] = gr
	}
	return out
}

// TestMP3Line puts one spectral line in every granule, with a sign that
// changes at random, at every sample rate, and looks for it in the output:
// at the frequency of the line, twice as loud for four steps of gain, and as
// long as the frames say.
func TestMP3Line(t *testing.T) {
	for _, s := range []l3Stream{
		{rate: 48000, channels: 1, kbps: 128},
		{rate: 44100, channels: 2, kbps: 160},
		{rate: 32000, channels: 2, kbps: 64},
		{rate: 24000, channels: 1, kbps: 64},
		{rate: 22050, channels: 2, kbps: 80},
		{rate: 16000, channels: 1, kbps: 32},
		{rate: 12000, channels: 2, kbps: 32},
		{rate: 11025, channels: 1, kbps: 16},
		{rate: 8000, channels: 1, kbps: 16},
	} {
		for _, line := range []int{5, 40, 201} {
			const frames = 48
			rng := rand.New(rand.NewPCG(uint64(s.rate), uint64(line)))
			a, b := make([][]l3Channel, frames), make([][]l3Channel, frames)
			for f := range a {
				for i := 0; i < s.channels*s.samples()/576; i++ {
					lines := map[int]int{line: 1 - 2*rng.IntN(2)}
					a[f] = append(a[f], l3Channel{gain: 186, lines: lines})
					b[f] = append(b[f], l3Channel{gain: 190, lines: lines})
				}
			}
			x := decodeL3(t, s, s.build(t, a))
			y := decodeL3(t, s, s.build(t, b))
			for c := range x {
				if len(x[c]) != frames*s.samples() {
					t.Fatalf("%d Hz: %d samples of %d frames", s.rate, len(x[c]), frames)
				}
				mid := x[c][2*1152 : 2*1152+16*576]
				if got := peak(spectrum(mid, 576)); got < line-1 || got > line+1 {
					t.Errorf("%d Hz, channel %d: line %d comes out at %d", s.rate, c, line, got)
				}
				// A line of ±1 at gain 186 is 2^((186-210)/4) = 1/64.
				if level := rms(mid); level < 0.005 || level > 0.02 {
					t.Errorf("%d Hz, channel %d: a line at gain 186 has rms %.3g", s.rate, c, level)
				}
				for i, v := range x[c] {
					if d := math.Abs(float64(y[c][i] - 2*v)); d > 1e-6 {
						t.Fatalf("%d Hz, channel %d, sample %d: %g at gain 190 and %g at 186, want twice", s.rate, c, i, y[c][i], v)
					}
				}
			}
		}
	}
}

// TestMP3ShortBlocks puts a line in one window of a granule of short blocks:
// it comes out at its frequency, which is three times as coarse, and at the
// time of its window, not spread over the granule and the next as a long
// block is.
func TestMP3ShortBlocks(t *testing.T) {
	s := l3Stream{rate: 48000, channels: 1, kbps: 128}
	silent := l3Channel{gain: 150}
	for _, c := range []struct {
		pos, window, line int // coded position: at 48 kHz the bands are 4 lines wide up to line 16, then 6, in each window
	}{
		{0, 0, 0}, {5, 1, 1}, {10, 2, 2}, {12 + 8 + 1, 2, 4 + 1}, {36 + 8, 2, 12}, {48 + 6 + 2, 1, 16 + 2},
	} {
		frames := repeat(12, silent, silent)
		frames[6] = []l3Channel{{gain: 190, short: true, lines: map[int]int{c.pos: 1}}, silent}
		x := decodeL3(t, s, s.build(t, frames))[0]
		// The transform of a granule spans two granules; the short windows
		// are 384 samples long and centred 384, 576 and 768 samples into
		// that span, and the synthesis filter, 512 samples long, puts its
		// output some 240 samples later.
		centre := 12*576 + 240 + 384 + 192*c.window
		burst := x[centre-480 : centre+480]
		if outside := rms(x[:centre-480]) + rms(x[centre+480:]); outside > 0.05*rms(burst) {
			t.Errorf("window %d: sound away from the window (rms %.3g, and %.3g in it)", c.window, outside, rms(burst))
		}
		var at, total float64
		for i, v := range burst {
			at += float64(i) * float64(v) * float64(v)
			total += float64(v) * float64(v)
		}
		if total == 0 {
			t.Fatalf("window %d: silence", c.window)
		}
		// (The windows are 192 samples apart.)
		if math.Abs(at/total-480) > 90 {
			t.Errorf("window %d: the sound is centred %.0f samples from where the window is", c.window, at/total-480)
		}
		if got := peak(spectrum(burst, 192)); got < c.line-1 || got > c.line+1 {
			t.Errorf("window %d: line %d comes out at %d", c.window, c.line, got)
		}
	}
}

// channelLevels decodes a stereo stream whose every granule holds gr and
// returns the level of each channel and whether they are in phase.
func channelLevels(t *testing.T, s l3Stream, gr ...l3Channel) (left, right float64, inPhase bool) {
	t.Helper()
	x := decodeL3(t, s, s.build(t, repeat(16, gr...)))
	a, b := x[0][2304:6912], x[1][2304:6912]
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return rms(a), rms(b), dot > 0
}

// TestMP3JointStereo checks the two ways two channels are coded as one and
// a bit more. Sum and difference: a line in the first channel alone is in
// both outputs, in the second alone it is in both with opposite signs.
// Intensity: a band the second channel leaves empty is the first channel's,
// shared out by the position that stands for the second's scalefactor; the
// shares are those of the standard (ISO/IEC 11172-3, and 13818-3 for the
// lower rates), and a band whose position is the one marked illegal is left
// as it is.
func TestMP3JointStereo(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-4*math.Max(want, 1e-3) }
	line := map[int]int{9: 1}
	on, off := l3Channel{gain: 160, lines: line}, l3Channel{gain: 160}

	ms := l3Stream{rate: 44100, channels: 2, kbps: 160, modeExt: 2}
	plain := l3Stream{rate: 44100, channels: 2, kbps: 160}
	ref, none, _ := channelLevels(t, plain, on, off, on, off)
	if ref == 0 || none != 0 {
		t.Fatalf("two separate channels: levels %g and %g", ref, none)
	}
	// The sum and difference are each 1/√2 of the channel they make.
	if l, r, same := channelLevels(t, ms, on, off, on, off); !near(l, ref/math.Sqrt2) || !near(r, l) || !same {
		t.Errorf("a line in the sum: levels %g and %g (in phase: %v), want both %g", l, r, same, ref/math.Sqrt2)
	}
	if l, r, same := channelLevels(t, ms, off, on, off, on); !near(l, ref/math.Sqrt2) || !near(r, l) || same {
		t.Errorf("a line in the difference: levels %g and %g (in phase: %v), want both %g, opposed", l, r, same, ref/math.Sqrt2)
	}

	// MPEG-1 intensity stereo. scalefac_compress 4 gives the scalefactors of
	// the first 11 bands 3 bits and the rest none; line 9 is in band 2.
	is := l3Stream{rate: 44100, channels: 2, kbps: 160, modeExt: 1}
	for pos := 0; pos <= 7; pos++ {
		var scf [][2]int
		for i := 0; i < 11; i++ {
			scf = append(scf, [2]int{pos, 3})
		}
		right := l3Channel{gain: 160, scfc: 4, scf: scf}
		l, r, _ := channelLevels(t, is, on, right, on, right)
		ratio := math.Tan(float64(pos) * math.Pi / 12)
		wl, wr := ref*ratio/(1+ratio), ref/(1+ratio)
		if pos == 6 {
			wl, wr = ref, 0
		}
		if pos == 7 { // not a position: the band is two channels
			wl, wr = ref, 0
		}
		if !near(l, wl) || !near(r, wr) {
			t.Errorf("MPEG-1 intensity position %d: levels %g and %g, want %g and %g", pos, l, r, wl, wr)
		}
	}

	// At the lower rates one channel keeps the level and the other is
	// lowered by a step for each two positions: 2^(-1/4), or 2^(-1/2) when
	// the low bit of the second channel's scalefac_compress is set. With
	// the rest of it 108, the first 7 bands have positions of 3 bits, of
	// which 7 marks a band that has none; the other bands have position 0.
	low := l3Stream{rate: 22050, channels: 2, kbps: 80, modeExt: 1}
	lref, _, _ := channelLevels(t, l3Stream{rate: 22050, channels: 2, kbps: 80}, on, off)
	for _, step := range []int{0, 1} {
		for pos := 0; pos <= 7; pos++ {
			var scf [][2]int
			for i := 0; i < 7; i++ {
				scf = append(scf, [2]int{pos, 3})
			}
			l, r, _ := channelLevels(t, low, on, l3Channel{gain: 160, scfc: 108<<1 | step, scf: scf})
			i0 := math.Pow(2, -0.25*float64(1+step))
			wl, wr := lref, lref
			switch {
			case pos == 7:
				wr = 0
			case pos%2 == 1:
				wl *= math.Pow(i0, float64(pos+1)/2)
			default:
				wr *= math.Pow(i0, float64(pos)/2)
			}
			if !near(l, wl) || !near(r, wr) {
				t.Errorf("low-rate intensity position %d, step %d: levels %g and %g, want %g and %g", pos, step, l, r, wl, wr)
			}
		}
	}
}

// reservoirStream is a stream whose frames keep their audio data in the
// frames before them, as far back as the format lets them, and whose lines
// change from granule to granule.
func reservoirStream(t testing.TB, s l3Stream, n int) [][]byte {
	rng := rand.New(rand.NewPCG(5, uint64(s.rate)))
	frames := make([][]l3Channel, n)
	for i := range frames {
		for g := 0; g < s.channels*s.samples()/576; g++ {
			c := l3Channel{gain: 140 + rng.IntN(20), lines: map[int]int{}, short: rng.IntN(5) == 0}
			for k := 0; k < 12; k++ {
				c.lines[rng.IntN(120)] = 1 - 2*rng.IntN(2)
			}
			frames[i] = append(frames[i], c)
		}
	}
	return s.build(t, frames)
}

// TestMP3Reservoir starts decoding inside streams whose frames reach back
// for their audio data. The frames that lack it are silent, each the full
// count of samples; once the data is there and the frame before was whole
// (two before, at the lower rates, where a frame is one granule), the
// samples are those of a decoder that read the stream from its start.
func TestMP3Reservoir(t *testing.T) {
	for _, s := range []l3Stream{
		{rate: 44100, channels: 2, kbps: 160, maxBegin: 511},
		{rate: 48000, channels: 1, kbps: 32, maxBegin: 511},
		{rate: 24000, channels: 2, kbps: 32, maxBegin: 255},
		{rate: 8000, channels: 1, kbps: 8, maxBegin: 255},
	} {
		frames := reservoirStream(t, s, 80)
		whole := decodeL3(t, s, frames)
		for _, start := range []int{1, 7, 30} {
			part := decodeL3(t, s, frames[start:])
			at := func(frame int) (a, b []float32) {
				n := s.samples()
				return whole[0][(start+frame)*n:][:n], part[0][frame*n:][:n]
			}
			if _, b := at(0); rms(b) != 0 {
				t.Errorf("%d Hz, from frame %d: the first frame has sound without its audio data", s.rate, start)
			}
			// When does the data of every later frame lie within what was
			// read? The run-in of the seek in package govideo covers it.
			settled := -1
			for f := 0; f < len(frames)-start; f++ {
				a, b := at(f)
				same := true
				for i := range a {
					if a[i] != b[i] {
						same = false
						break
					}
				}
				if !same {
					settled = -1
				} else if settled < 0 {
					settled = f
				}
			}
			if settled < 0 || settled > 40 {
				t.Errorf("%d Hz, from frame %d: the samples are those of a decode from the start from frame %d on", s.rate, start, settled)
			}
		}
	}
}

// TestMP3Packets covers what a packet may be: frames end to end, a frame
// cut short, and what is not a frame at all.
func TestMP3Packets(t *testing.T) {
	s := l3Stream{rate: 44100, channels: 2, kbps: 160}
	frames := reservoirStream(t, s, 6)
	whole := decodeL3(t, s, frames)

	// Three frames in one packet, and trailing bytes that are not a frame.
	dec := NewMP3Decoder()
	var got []float32
	for i := 0; i < len(frames); i += 3 {
		packet := append(append(append([]byte{}, frames[i]...), frames[i+1]...), frames[i+2]...)
		fr, err := dec.Decode(append(packet, 1, 2, 3, 4, 5))
		if err != nil || fr.Frames() != 3*1152 {
			t.Fatalf("three frames in a packet: %v, %d samples", err, fr.Frames())
		}
		got = append(got, fr.Samples...)
	}
	for i, v := range got {
		if v != whole[i%2][i/2] {
			t.Fatalf("three frames in a packet: sample %d differs from the frames one by one", i)
		}
	}

	// A frame cut short is silence of its length, and decoding goes on.
	dec = NewMP3Decoder()
	for i, f := range frames {
		if i == 2 {
			f = f[:20]
		}
		fr, err := dec.Decode(f)
		if err != nil || fr.Frames() != 1152 {
			t.Fatalf("frame %d of a stream with a cut frame: %v", i, err)
		}
		if i == 2 && rms(fr.Samples) != 0 {
			t.Errorf("a frame cut short has sound")
		}
		if i == 5 && rms(fr.Samples) == 0 {
			t.Errorf("silence three frames after a frame cut short")
		}
	}

	// Side information that cannot be: more than 288 pairs of big values.
	bad := append([]byte{}, frames[1]...)
	bad[4+2], bad[4+3], bad[4+4] = 0xff, 0xff, 0xff
	if fr, err := NewMP3Decoder().Decode(bad); err != nil || fr.Frames() != 1152 || rms(fr.Samples) != 0 {
		t.Errorf("a frame with impossible side information: %v, want silence", err)
	}

	for name, p := range map[string][]byte{
		"nothing":     nil,
		"text":        []byte("this is not an MPEG audio frame"),
		"layer II":    {0xff, 0xfd, 0x90, 0x00, 0, 0, 0, 0},
		"free format": {0xff, 0xfb, 0x00, 0x00, 0, 0, 0, 0},
	} {
		if fr, err := NewMP3Decoder().Decode(p); err == nil {
			t.Errorf("%s: decoded to %d samples", name, fr.Frames())
		} else if name != "free format" && !errors.Is(err, ErrNotMP3) {
			t.Errorf("%s: %v, want ErrNotMP3", name, err)
		}
	}
}

// mp3Noise makes n frames with valid headers of every kind and noise for
// the rest, some of it thinned so that the side information is often
// plausible.
func mp3Noise(seed uint64, n int) [][]byte {
	rng := rand.New(rand.NewPCG(seed, 99))
	var out [][]byte
	for len(out) < n {
		h := [4]byte{0xff, [...]byte{0xfb, 0xfa, 0xf3, 0xf2, 0xe3, 0xe2}[rng.IntN(6)], byte(1+rng.IntN(14))<<4 | byte(rng.IntN(3))<<2 | byte(rng.IntN(2))<<1, byte(rng.IntN(256))}
		size := 1441
		f := make([]byte, 4, size)
		copy(f, h[:])
		mask := byte(0xff)
		if rng.IntN(2) == 0 {
			mask = byte(rng.IntN(256)) & byte(rng.IntN(256))
		}
		for len(f) < size {
			f = append(f, byte(rng.IntN(256))&mask)
		}
		// A run of frames of one kind, so that the reservoir is used.
		for k := 0; k < 1+rng.IntN(4); k++ {
			g := append([]byte{}, f...)
			for i := 4; i < len(g); i += 1 + rng.IntN(40) {
				g[i] = byte(rng.IntN(256))
			}
			out = append(out, g)
		}
	}
	return out
}

// TestMP3Noise feeds the decoder frames of noise: nothing in a frame may
// take it outside its tables, and what comes out is finite.
func TestMP3Noise(t *testing.T) {
	dec := NewMP3Decoder()
	sound := 0
	for i, f := range mp3Noise(1, 6000) {
		fr, err := dec.Decode(f)
		if err != nil {
			continue // the header asks for a size the frame table does not have
		}
		for _, v := range fr.Samples {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("frame %d: a sample is %v", i, v)
			}
		}
		if rms(fr.Samples) != 0 {
			sound++
		}
	}
	if sound < 500 {
		t.Errorf("only %d frames of noise were decoded: the test reaches little of the decoder", sound)
	}
}

func FuzzMP3Decode(f *testing.F) {
	s := l3Stream{rate: 44100, channels: 2, kbps: 160, modeExt: 3, maxBegin: 511}
	low := l3Stream{rate: 16000, channels: 1, kbps: 16, maxBegin: 255}
	for _, frames := range [][][]byte{reservoirStream(f, s, 3), reservoirStream(f, low, 4), mp3Noise(2, 3), mp3Noise(3, 3)} {
		var packet []byte
		for _, fr := range frames {
			packet = append(packet, fr...)
		}
		f.Add(packet)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		dec := NewMP3Decoder()
		// The data as one packet of frames, then again in pieces, the
		// decoder keeping what the first pass left.
		for pass := 0; pass < 2; pass++ {
			for p := data; len(p) > 0; {
				n := len(p)
				if pass == 1 {
					n = min(n, 400)
				}
				fr, err := dec.Decode(p[:n])
				if err == nil && (fr.Channels < 1 || fr.Channels > 2 || fr.SampleRate < 8000 || len(fr.Samples)%(576*fr.Channels) != 0 || len(fr.Samples) == 0) {
					t.Fatalf("%d samples at %d Hz in %d channels", len(fr.Samples), fr.SampleRate, fr.Channels)
				}
				p = p[n:]
			}
		}
	})
}
