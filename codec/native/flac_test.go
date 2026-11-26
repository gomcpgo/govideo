package native

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/gomcpgo/govideo/internal/bits"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// The encoder is tested against a decoder written here from RFC 9639, which
// shares nothing with it: its own checksums, the predictors as the standard
// writes them, and more of the format than the encoder uses (linear
// prediction, wasted bits, escaped partitions). FLAC is lossless, so the
// test is that every sample comes back, on any platform; where the oracle
// is installed, ffmpeg must read the same samples too.

// flacDecoded is what the test decoder found in a FLAC file.
type flacDecoded struct {
	rate, channels, bps  int
	blockMin, blockMax   int
	frameMin, frameMax   int
	total                int64
	md5                  [16]byte
	samples              []int32 // interleaved
	frames               []int   // the size in bytes of each frame
	blocks               []int   // the sample frames in each
	assign               []int   // the channel assignment of each
	kinds                map[string]int
	inSubset, rateInHead bool
}

// refCRC is a checksum of width bits with the polynomial poly, one bit at a
// time.
func refCRC(b []byte, width uint, poly uint32) uint32 {
	var c uint32
	for _, v := range b {
		c ^= uint32(v) << (width - 8)
		for range 8 {
			if c&(1<<(width-1)) != 0 {
				c = c<<1 ^ poly
			} else {
				c <<= 1
			}
		}
	}
	return c & (1<<width - 1)
}

// signed reads n bits of two's complement.
func signed(r *bits.Reader, n int) int32 {
	if n == 0 {
		return 0
	}
	return int32(r.U(n)<<uint(32-n)) >> uint(32-n)
}

// decodeFLAC decodes a whole FLAC file, failing the test on anything the
// standard does not allow.
func decodeFLAC(t testing.TB, file []byte) flacDecoded {
	t.Helper()
	var d flacDecoded
	d.kinds = map[string]int{}
	if len(file) < 42 || string(file[:4]) != "fLaC" {
		t.Fatalf("no fLaC marker in a file of %d bytes", len(file))
	}
	// The metadata blocks: STREAMINFO first, and the last one flagged.
	pos := 4
	for first := true; ; first = false {
		if pos+4 > len(file) {
			t.Fatal("metadata runs past the end of the file")
		}
		last, kind := file[pos]&0x80 != 0, file[pos]&0x7f
		size := int(file[pos+1])<<16 | int(file[pos+2])<<8 | int(file[pos+3])
		pos += 4
		if pos+size > len(file) {
			t.Fatalf("metadata block of %d bytes runs past the end of the file", size)
		}
		if first {
			if kind != 0 || size != 34 {
				t.Fatalf("the first metadata block has type %d and %d bytes, want STREAMINFO", kind, size)
			}
			b := file[pos:]
			d.blockMin, d.blockMax = int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:]))
			d.frameMin = int(b[4])<<16 | int(b[5])<<8 | int(b[6])
			d.frameMax = int(b[7])<<16 | int(b[8])<<8 | int(b[9])
			v := binary.BigEndian.Uint64(b[10:])
			d.rate, d.channels, d.bps = int(v>>44), int(v>>41&7)+1, int(v>>36&31)+1
			d.total = int64(v & (1<<36 - 1))
			copy(d.md5[:], b[18:34])
		}
		pos += size
		if last {
			break
		}
	}
	if d.rate == 0 || d.blockMin < 16 || d.blockMax < d.blockMin {
		t.Fatalf("STREAMINFO: %d Hz, blocks of %d to %d", d.rate, d.blockMin, d.blockMax)
	}
	d.inSubset, d.rateInHead = true, true

	for number := 0; pos < len(file); number++ {
		r := bits.NewReader(file[pos:])
		if sync := r.U(16); sync != 0xfff8 {
			t.Fatalf("frame %d at byte %d starts with %#x, want the sync code of a fixed block size", number, pos, sync)
		}
		sizeCode, rateCode, assign, bpsCode := int(r.U(4)), int(r.U(4)), int(r.U(4)), int(r.U(3))
		if r.U(1) != 0 {
			t.Fatalf("frame %d: reserved bit set", number)
		}
		// The frame number.
		lead := r.U(8)
		n := 0
		for lead&(0x80>>uint(n)) != 0 {
			n++
		}
		if n == 1 || n > 7 {
			t.Fatalf("frame %d: number starts with byte %#x", number, lead)
		}
		num := uint64(lead) & (0xff >> uint(n+1))
		if n == 0 {
			num = uint64(lead)
		}
		for i := 1; i < n; i++ {
			c := r.U(8)
			if c&0xc0 != 0x80 {
				t.Fatalf("frame %d: continuation byte %#x in its number", number, c)
			}
			num = num<<6 | uint64(c&0x3f)
		}
		if num != uint64(number) {
			t.Fatalf("frame %d is numbered %d", number, num)
		}
		var block int
		switch {
		case sizeCode == 0:
			t.Fatalf("frame %d: reserved block size code", number)
		case sizeCode == 1:
			block = 192
		case sizeCode <= 5:
			block = 576 << uint(sizeCode-2)
		case sizeCode == 6:
			block = int(r.U(8)) + 1
		case sizeCode == 7:
			block = int(r.U(16)) + 1
		default:
			block = 256 << uint(sizeCode-8)
		}
		rate := 0
		switch rateCode {
		case 0:
			rate, d.inSubset, d.rateInHead = d.rate, false, false
		case 12:
			rate = int(r.U(8)) * 1000
		case 13:
			rate = int(r.U(16))
		case 14:
			rate = int(r.U(16)) * 10
		case 15:
			t.Fatalf("frame %d: forbidden sample rate code", number)
		default:
			rate = []int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rateCode]
		}
		if rate != d.rate {
			t.Fatalf("frame %d is at %d Hz in a stream of %d Hz", number, rate, d.rate)
		}
		bps := []int{0, 8, 12, 0, 16, 20, 24, 32}[bpsCode]
		if bps != d.bps {
			t.Fatalf("frame %d has samples of %d bits (code %d) in a stream of %d", number, bps, bpsCode, d.bps)
		}
		channels := assign + 1
		if assign >= 8 {
			channels = 2
			if assign > 10 {
				t.Fatalf("frame %d: reserved channel assignment %d", number, assign)
			}
		}
		if channels != d.channels {
			t.Fatalf("frame %d has %d channels in a stream of %d", number, channels, d.channels)
		}
		head := r.Pos() / 8
		if got, want := r.U(8), refCRC(file[pos:pos+head], 8, 0x07); got != want {
			t.Fatalf("frame %d: header CRC %#x, want %#x", number, got, want)
		}
		if block > d.blockMax || (block > 4608 && d.rate <= 48000) || block > 16384 {
			d.inSubset = d.inSubset && block <= d.blockMax && block <= 16384 && (block <= 4608 || d.rate > 48000)
			if block > d.blockMax {
				t.Fatalf("frame %d has %d samples; STREAMINFO says at most %d", number, block, d.blockMax)
			}
		}

		ch := make([][]int32, channels)
		for c := range ch {
			size := bps
			if assign == 8 && c == 1 || assign == 9 && c == 0 || assign == 10 && c == 1 {
				size++ // a side channel
			}
			ch[c] = decodeSubframe(t, r, &d, block, size, number)
		}
		r.AlignByte()
		end := r.Pos() / 8
		if got, want := r.U(16), refCRC(file[pos:pos+end], 16, 0x8005); r.Err() != nil || got != want {
			t.Fatalf("frame %d: CRC %#x, want %#x (%v)", number, got, want, r.Err())
		}
		for i := 0; i < block; i++ {
			switch assign {
			case 8: // left, side
				ch[1][i] = ch[0][i] - ch[1][i]
			case 9: // side, right
				ch[0][i] += ch[1][i]
			case 10: // mid, side
				mid, side := ch[0][i], ch[1][i]
				mid = mid<<1 | side&1
				ch[0][i], ch[1][i] = (mid+side)>>1, (mid-side)>>1
			}
			for c := range ch {
				d.samples = append(d.samples, ch[c][i])
			}
		}
		size := end + 2
		d.frames, d.blocks, d.assign = append(d.frames, size), append(d.blocks, block), append(d.assign, assign)
		pos += size
	}
	return d
}

// decodeSubframe decodes one channel of a frame: block samples of bps bits.
func decodeSubframe(t testing.TB, r *bits.Reader, d *flacDecoded, block, bps, number int) []int32 {
	t.Helper()
	if r.U(1) != 0 {
		t.Fatalf("frame %d: subframe padding bit set", number)
	}
	kind := int(r.U(6))
	wasted := 0
	if r.U(1) == 1 {
		for wasted = 1; r.U(1) == 0 && r.Err() == nil; wasted++ {
		}
		bps -= wasted
	}
	x := make([]int32, block)
	residual := func(order int) {
		method := int(r.U(2))
		if method > 1 {
			t.Fatalf("frame %d: reserved residual coding method", number)
		}
		partOrder := int(r.U(4))
		if partOrder > 8 {
			d.inSubset = false
		}
		if block&(1<<uint(partOrder)-1) != 0 || block>>uint(partOrder) <= order && !(partOrder == 0 && block == order) {
			t.Fatalf("frame %d: partition order %d for a block of %d with predictor order %d", number, partOrder, block, order)
		}
		i := order
		for p := 0; p < 1<<uint(partOrder); p++ {
			end := (p + 1) * (block >> uint(partOrder))
			k := int(r.U(4 + method))
			if k == 15+16*method {
				for raw := int(r.U(5)); i < end; i++ {
					x[i] = signed(r, raw)
				}
				continue
			}
			for ; i < end && r.Err() == nil; i++ {
				q := uint32(0)
				for r.U(1) == 0 && r.Err() == nil {
					q++
				}
				u := q<<uint(k) | r.U(k)
				x[i] = int32(u>>1) ^ -int32(u&1)
			}
		}
	}
	switch {
	case kind == 0:
		d.kinds["constant"]++
		v := signed(r, bps)
		for i := range x {
			x[i] = v
		}
	case kind == 1:
		d.kinds["verbatim"]++
		for i := range x {
			x[i] = signed(r, bps)
		}
	case kind >= 8 && kind <= 12:
		order := kind - 8
		d.kinds["fixed "+strconv.Itoa(order)]++
		if order > block {
			t.Fatalf("frame %d: predictor order %d in a block of %d", number, order, block)
		}
		for i := 0; i < order; i++ {
			x[i] = signed(r, bps)
		}
		residual(order)
		for i := order; i < block; i++ {
			switch order {
			case 1:
				x[i] += x[i-1]
			case 2:
				x[i] += 2*x[i-1] - x[i-2]
			case 3:
				x[i] += 3*x[i-1] - 3*x[i-2] + x[i-3]
			case 4:
				x[i] += 4*x[i-1] - 6*x[i-2] + 4*x[i-3] - x[i-4]
			}
		}
	case kind >= 32:
		order := kind - 31
		d.kinds["lpc"]++
		if order > block {
			t.Fatalf("frame %d: predictor order %d in a block of %d", number, order, block)
		}
		if order > 12 && d.rate <= 48000 {
			d.inSubset = false
		}
		for i := 0; i < order; i++ {
			x[i] = signed(r, bps)
		}
		precision := int(r.U(4)) + 1
		shift := signed(r, 5)
		if precision == 16 || shift < 0 {
			t.Fatalf("frame %d: coefficient precision code 15 or shift %d", number, shift)
		}
		coef := make([]int64, order)
		for i := range coef {
			coef[i] = int64(signed(r, precision))
		}
		residual(order)
		for i := order; i < block; i++ {
			var sum int64
			for j, c := range coef {
				sum += c * int64(x[i-1-j])
			}
			x[i] += int32(sum >> uint(shift))
		}
	default:
		t.Fatalf("frame %d: reserved subframe type %d", number, kind)
	}
	if r.Err() != nil {
		t.Fatalf("frame %d: %v", number, r.Err())
	}
	for i := range x {
		x[i] <<= uint(wasted)
	}
	return x
}

// encodeFLAC encodes interleaved samples, chunk sample frames to a call, and
// returns the file: the header written first and again at the end, as an
// export does.
func encodeFLAC(t testing.TB, rate, channels int, pcm []int16, chunk int) []byte {
	t.Helper()
	e, err := NewFLACEncoder(rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	file := e.Header()
	if len(file) != FLACHeaderSize {
		t.Fatalf("header of %d bytes, want %d", len(file), FLACHeaderSize)
	}
	put := func(frames [][]byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			file = append(file, f...)
		}
	}
	in := make([]float32, 0, chunk*channels)
	for len(pcm) > 0 {
		n := min(chunk*channels, len(pcm))
		in = in[:0]
		for _, v := range pcm[:n] {
			in = append(in, float32(v)/32767)
		}
		put(e.Encode(in))
		pcm = pcm[n:]
	}
	put(e.Flush())
	copy(file, e.Header())
	return file
}

// checkFLAC decodes file and compares everything in it with the sound that
// went in.
func checkFLAC(t testing.TB, file []byte, rate, channels int, pcm []int16) flacDecoded {
	t.Helper()
	d := decodeFLAC(t, file)
	if d.rate != rate || d.channels != channels || d.bps != 16 || d.total != int64(len(pcm)/channels) {
		t.Fatalf("STREAMINFO: %d samples of %d bits at %d Hz in %d channels, want %d of 16 bits at %d Hz in %d",
			d.total, d.bps, d.rate, d.channels, len(pcm)/channels, rate, channels)
	}
	if len(d.samples) != len(pcm) {
		t.Fatalf("%d samples decoded, %d encoded", len(d.samples), len(pcm))
	}
	for i, v := range pcm {
		if d.samples[i] != int32(v) {
			t.Fatalf("sample %d (frame %d, channel %d) is %d, was %d", i, i/channels, i%channels, d.samples[i], v)
		}
	}
	raw := make([]byte, 0, 2*len(pcm))
	for _, v := range pcm {
		raw = binary.LittleEndian.AppendUint16(raw, uint16(v))
	}
	if want := md5.Sum(raw); d.md5 != want {
		t.Errorf("STREAMINFO MD5 %x, the sound's is %x", d.md5, want)
	}
	// Every block is full but the last, and the frame sizes are the ones
	// stated.
	least, most := 0, 0
	for i, size := range d.frames {
		if i < len(d.frames)-1 && d.blocks[i] != flacBlockSize {
			t.Fatalf("frame %d of %d holds %d samples", i, len(d.frames), d.blocks[i])
		}
		if i == 0 || size < least {
			least = size
		}
		most = max(most, size)
	}
	if d.blockMin != flacBlockSize || d.blockMax != flacBlockSize || d.frameMin != least || d.frameMax != most {
		t.Errorf("STREAMINFO: blocks of %d to %d, frames of %d to %d bytes; the frames are %d to %d bytes",
			d.blockMin, d.blockMax, d.frameMin, d.frameMax, least, most)
	}
	return d
}

// Test signals, as interleaved 16-bit samples.

func sine(rate int, freq, level float64) func(i, ch int) float64 {
	return func(i, ch int) float64 { return level * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)) }
}

// render fills n sample frames of the given channels from f, whose value is
// in full-scale units.
func render(n, channels int, f func(i, ch int) float64) []int16 {
	out := make([]int16, 0, n*channels)
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			out = append(out, int16(math.Round(32767*max(-1, min(1, f(i, c))))))
		}
	}
	return out
}

// musicLike is a few decaying partials and a little noise, different in the
// two channels: something for every predictor order.
func musicLike(rate int, seed uint64) func(i, ch int) float64 {
	rng := rand.New(rand.NewPCG(seed, 7))
	noise := make([]float64, 1<<16)
	for i := range noise {
		noise[i] = rng.NormFloat64()
	}
	return func(i, ch int) float64 {
		t := float64(i) / float64(rate)
		note := math.Mod(t, 0.5)
		env := math.Exp(-6 * note)
		f := 220 * math.Pow(2, float64(int(t*2)%8)/12)
		v := 0.5*math.Sin(2*math.Pi*f*t) + 0.2*math.Sin(2*math.Pi*2*f*t+float64(ch)) + 0.1*math.Sin(2*math.Pi*3.01*f*t)
		pan := 0.6 + 0.4*float64(ch)*math.Sin(t)
		return env*v*pan + 0.002*noise[(i*2+ch)&(len(noise)-1)]
	}
}

func TestFLACRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	whiteNoise := func(level float64) func(i, ch int) float64 {
		return func(i, ch int) float64 { return level * (2*rng.Float64() - 1) }
	}
	tone := sine(48000, 440, 0.5)
	hiss := whiteNoise(0.01)
	cases := []struct {
		name           string
		rate, channels int
		frames         int
		f              func(i, ch int) float64
		kind           string // a kind of subframe that must be used
		assign         int    // a channel assignment that must be used; 0 for any
		maxRatio       float64
	}{
		{"silence", 48000, 2, 10000, func(i, ch int) float64 { return 0 }, "constant", flacLeftRight, 0.01},
		{"a constant level", 44100, 1, 9000, func(i, ch int) float64 { return -0.25 }, "constant", 0, 0.01},
		{"a tone", 16000, 1, 16000, sine(16000, 440, 0.5), "fixed 4", 0, 0.5},
		{"a low tone", 48000, 1, 48000, sine(48000, 50, 0.9), "fixed 2", 0, 0.25},
		{"a tone between", 48000, 1, 48000, sine(48000, 150, 0.3), "fixed 3", 0, 0.25},
		{"full-scale noise", 48000, 1, 20000, whiteNoise(1), "verbatim", 0, 1.01},
		{"quiet noise", 48000, 2, 20000, whiteNoise(0.002), "fixed 0", 0, 0.5},
		{"the same in both channels", 48000, 2, 20000, func(i, ch int) float64 { return tone(i, 0) }, "constant", flacLeftSide, 0.3},
		{"opposite channels", 48000, 2, 20000, func(i, ch int) float64 { return tone(i, 0) * float64(1-2*ch) }, "constant", flacMidSide, 0.3},
		{"a tone, with hiss on the left", 48000, 2, 20000, func(i, ch int) float64 {
			if ch == 0 {
				return tone(i, 0) + hiss(i, 0)
			}
			return tone(i, 0)
		}, "", flacSideRight, 0.6},
		{"a tone, with hiss on the right", 48000, 2, 20000, func(i, ch int) float64 {
			if ch == 1 {
				return tone(i, 0) + hiss(i, 0)
			}
			return tone(i, 0)
		}, "", flacLeftSide, 0.6},
		// The largest side sample there is: 32767 - -32768 needs 17 bits.
		{"opposite extremes", 44100, 2, 9000, func(i, ch int) float64 {
			if (i/3+ch)%2 == 0 {
				return 1
			}
			return -1.1
		}, "", 0, 1.1},
		{"music", 44100, 2, 44100 * 3, musicLike(44100, 3), "fixed 2", flacMidSide, 0.7},
		{"music in mono at a low rate", 16000, 1, 16000 * 3, musicLike(16000, 4), "fixed 2", 0, 0.8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pcm := render(c.frames, c.channels, c.f)
			file := encodeFLAC(t, c.rate, c.channels, pcm, 4800)
			d := checkFLAC(t, file, c.rate, c.channels, pcm)
			if c.kind != "" && d.kinds[c.kind] == 0 {
				t.Errorf("no %s subframe: %v", c.kind, d.kinds)
			}
			if c.assign != 0 {
				found := false
				for _, a := range d.assign {
					found = found || a == c.assign
				}
				if !found {
					t.Errorf("no frame with channel assignment %d: %v", c.assign, d.assign)
				}
			}
			if !d.inSubset {
				t.Error("the stream is outside the streamable subset")
			}
			ratio := float64(len(file)) / float64(2*len(pcm))
			if ratio > c.maxRatio {
				t.Errorf("%d bytes for %d of PCM: %.3f, want at most %.3f", len(file), 2*len(pcm), ratio, c.maxRatio)
			}
			t.Logf("%.3f of PCM; subframes %v", ratio, d.kinds)
		})
	}
}

// TestFLACLengths encodes streams that end at every kind of place: shorter
// than a predictor's order, on a block, one sample past it, and at lengths
// that have a block size code of their own or none.
func TestFLACLengths(t *testing.T) {
	for _, channels := range []int{1, 2} {
		f := musicLike(48000, uint64(channels))
		for _, n := range []int{1, 2, 3, 4, 5, 6, 15, 16, 17, 191, 192, 255, 256, 257, 576, 1000, 1024, 4095, 4096, 4097, 4608, 8192, 8193, 10000} {
			pcm := render(n, channels, f)
			// In calls of a size that divides nothing.
			d := checkFLAC(t, encodeFLAC(t, 48000, channels, pcm, 777), 48000, channels, pcm)
			if want := (n + flacBlockSize - 1) / flacBlockSize; len(d.frames) != want {
				t.Errorf("%d samples in %d channels: %d frames, want %d", n, channels, len(d.frames), want)
			}
			// One call for all of it gives the same file.
			if whole := encodeFLAC(t, 48000, channels, pcm, n); !bytes.Equal(whole, encodeFLAC(t, 48000, channels, pcm, 1)) {
				t.Errorf("%d samples in %d channels: the file depends on how the samples were handed over", n, channels)
			}
		}
	}
}

// TestFLACSampleRates covers each way a frame header states the rate: by a
// code, in kHz, in Hz, in tens of Hz, and not at all.
func TestFLACSampleRates(t *testing.T) {
	for rate, inHeader := range map[int]bool{
		8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true,
		88200: true, 96000: true, 176400: true, 192000: true,
		12000: true, 37000: true, 255000: true, // kHz
		1: true, 11025: true, 65535: true, // Hz
		256000: true, 384000: true, 655350: true, // tens of Hz
		70001: false, 655360: false, 1<<20 - 1: false, // STREAMINFO only
	} {
		pcm := render(5000, 2, musicLike(48000, 9))
		d := checkFLAC(t, encodeFLAC(t, rate, 2, pcm, 5000), rate, 2, pcm)
		if d.rateInHead != inHeader {
			t.Errorf("%d Hz: rate in the frame header: %v, want %v", rate, d.rateInHead, inHeader)
		}
	}
	for _, rate := range []int{0, -1, 1 << 20} {
		if _, err := NewFLACEncoder(rate, 2); err == nil {
			t.Errorf("no error for a sample rate of %d Hz", rate)
		}
	}
	for _, channels := range []int{0, 3, 8} {
		if _, err := NewFLACEncoder(48000, channels); err == nil {
			t.Errorf("no error for %d channels", channels)
		}
	}
}

// TestFLACManyFrames runs the frame number into its third byte.
func TestFLACManyFrames(t *testing.T) {
	const frames = 2100
	pcm := make([]int16, frames*flacBlockSize)
	for i := range pcm {
		pcm[i] = int16(i / flacBlockSize) // constant through each block
	}
	file := encodeFLAC(t, 8000, 1, pcm, 48000)
	if d := checkFLAC(t, file, 8000, 1, pcm); len(d.frames) != frames || d.kinds["constant"] != frames {
		t.Errorf("%d frames, %v, want %d constant ones", len(d.frames), d.kinds, frames)
	}
}

func TestFLACEncoderUse(t *testing.T) {
	e, err := NewFLACEncoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Encode(make([]float32, 3)); err == nil {
		t.Error("no error for half a sample frame")
	}
	if e.FrameSize() != flacBlockSize || e.Priming() != 0 {
		t.Errorf("frame size %d, priming %d", e.FrameSize(), e.Priming())
	}
	// Samples past full scale are clamped, as in a WAVE file.
	frames, _ := e.Encode([]float32{2, -2, float32(math.NaN()), 0.5})
	if len(frames) != 0 {
		t.Fatalf("%d frames from two samples", len(frames))
	}
	frames, err = e.Flush()
	if err != nil || len(frames) != 1 {
		t.Fatalf("Flush: %d frames, %v", len(frames), err)
	}
	d := decodeFLAC(t, append(e.Header(), frames[0]...))
	if want := []int32{32767, -32768, 0, 16384}; fmt.Sprint(d.samples) != fmt.Sprint(want) {
		t.Errorf("decoded %v, want %v", d.samples, want)
	}
	if again, err := e.Flush(); err != nil || len(again) != 0 {
		t.Errorf("a second Flush: %d frames, %v", len(again), err)
	}
	if _, err := e.Encode(make([]float32, 2)); err == nil {
		t.Error("no error for samples after Flush")
	}
	// Every 16-bit value survives the float32 the tests pass it through.
	for v := -32768; v <= 32767; v++ {
		if got := s16(float32(v) / 32767); int(got) != v {
			t.Fatalf("%d comes back as %d", v, got)
		}
	}
}

// TestFLACChecksums checks the two CRCs against the published check values
// of their parameters (the CRC of "123456789") and against a bit-by-bit
// computation.
func TestFLACChecksums(t *testing.T) {
	check := []byte("123456789")
	if got := flacCRC8(check); got != 0xf4 {
		t.Errorf("CRC-8 of %q is %#x, want 0xf4", check, got)
	}
	if got := flacCRC16(check); got != 0xfee8 {
		t.Errorf("CRC-16 of %q is %#x, want 0xfee8", check, got)
	}
	rng := rand.New(rand.NewPCG(5, 6))
	b := make([]byte, 1000)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	for _, n := range []int{0, 1, 2, 17, 1000} {
		if got, want := uint32(flacCRC8(b[:n])), refCRC(b[:n], 8, 0x07); got != want {
			t.Errorf("CRC-8 of %d bytes is %#x, want %#x", n, got, want)
		}
		if got, want := uint32(flacCRC16(b[:n])), refCRC(b[:n], 16, 0x8005); got != want {
			t.Errorf("CRC-16 of %d bytes is %#x, want %#x", n, got, want)
		}
	}
}

// TestFLACFrameNumber checks the variable-length number of the frame header
// at every length: against UTF-8 where UTF-8 goes, and the byte patterns of
// the longer forms.
func TestFLACFrameNumber(t *testing.T) {
	code := func(v uint64) []byte {
		var w bits.Writer
		flacWriteNumber(&w, v)
		return w.Bytes()
	}
	for _, v := range []rune{0, 1, 0x7f, 0x80, 0x7ff, 0x800, 0xd7ff, 0xe000, 0xffff, 0x10000, 0x10ffff} {
		if got, want := code(uint64(v)), utf8.AppendRune(nil, v); !bytes.Equal(got, want) {
			t.Errorf("%#x is coded % x, want % x", v, got, want)
		}
	}
	for v, want := range map[uint64]string{
		0x1fffff:    "f7 bf bf bf",
		0x200000:    "f8 88 80 80 80",
		0x3ffffff:   "fb bf bf bf bf",
		0x4000000:   "fc 84 80 80 80 80",
		0x7fffffff:  "fd bf bf bf bf bf",
		0x80000000:  "fe 82 80 80 80 80 80",
		0xfffffffff: "fe bf bf bf bf bf bf",
	} {
		if got := fmt.Sprintf("% x", code(v)); got != want {
			t.Errorf("%#x is coded %s, want %s", v, got, want)
		}
	}
}

// TestFLACRice writes Rice codes and reads them back, among them quotients
// longer than the writer takes in one piece.
func TestFLACRice(t *testing.T) {
	type code struct {
		r int32
		k uint
	}
	codes := []code{{0, 0}, {-1, 0}, {1, 0}, {0, 14}, {-1, 5}, {32767, 14}, {-32768, 3}, {1 << 20, 14}, {-1 << 20, 0}, {40, 0}, {31, 0}, {33, 1}, {5000, 2}}
	rng := rand.New(rand.NewPCG(8, 9))
	for range 2000 {
		k := uint(rng.IntN(15))
		codes = append(codes, code{int32(rng.NormFloat64() * float64(uint(1)<<k) * 2), k})
	}
	var w bits.Writer
	want := 0
	for _, c := range codes {
		u := flacFold(c.r)
		flacWriteRice(&w, u, c.k)
		want += int(u>>c.k) + 1 + int(c.k)
	}
	if w.Len(0) != want {
		t.Fatalf("%d bits written, want %d", w.Len(0), want)
	}
	w.Align()
	r := bits.NewReader(w.Bytes())
	for i, c := range codes {
		q := uint32(0)
		for r.U(1) == 0 && r.Err() == nil {
			q++
		}
		u := q<<c.k | r.U(int(c.k))
		if got := int32(u>>1) ^ -int32(u&1); got != c.r || r.Err() != nil {
			t.Fatalf("code %d: %d with parameter %d reads back as %d (%v)", i, c.r, c.k, got, r.Err())
		}
	}
}

// bruteRice is the size of the cheapest partitioned Rice coding of
// res[order:], found by pricing every parameter of every partition of every
// partition order, one sample at a time.
func bruteRice(res []int32, order int) int64 {
	n := len(res)
	best := int64(-1)
	for p := 0; p <= flacMaxPartOrder && n%(1<<uint(p)) == 0 && n>>uint(p) > order; p++ {
		var total int64
		for j := 0; j < 1<<uint(p); j++ {
			from, to := j*(n>>uint(p)), (j+1)*(n>>uint(p))
			if j == 0 {
				from = order
			}
			least := int64(-1)
			for k := uint(0); k <= flacMaxRice; k++ {
				var b int64
				for _, r := range res[from:to] {
					b += int64(flacFold(r)>>k) + 1 + int64(k)
				}
				if least < 0 || b < least {
					least = b
				}
			}
			total += 4 + least
		}
		if best < 0 || total < best {
			best = total
		}
	}
	return best + 6
}

// TestFLACRiceSearch holds the search to two things. The size it states is
// the size written: the partition order and parameters it returns, priced
// one sample at a time, come to it exactly. And it is the least there is:
// the search prices only the parameters about the level of each partition,
// and on residuals of every kind that must lose nothing against trying them
// all.
func TestFLACRiceSearch(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	laplace := func(scale float64) func(i int) int32 {
		return func(int) int32 { return int32(math.Round(rng.ExpFloat64() * scale * float64(1-2*rng.IntN(2)))) }
	}
	kinds := map[string]func(i int) int32{
		"zeros":       func(int) int32 { return 0 },
		"small":       laplace(0.7),
		"medium":      laplace(40),
		"large":       laplace(6000),
		"huge":        func(int) int32 { return int32(rng.IntN(1<<21) - 1<<20) },
		"a burst":     func(i int) int32 { return laplace(3 + 4000*float64(i/1024%2))(i) },
		"a click":     func(i int) int32 { return laplace(2)(i) + int32(100000*(i/2000%2)*((i+1)/2000%2)) },
		"rising":      func(i int) int32 { return laplace(1 + float64(i)/2)(i) },
		"one outlier": func(i int) int32 { return laplace(1)(i) + int32(1<<20*(i/4000)) },
	}
	var s riceSearch
	for name, f := range kinds {
		for _, n := range []int{4096, 4000, 1024, 777, 48, 16, 5} {
			for _, order := range []int{0, 1, 4} {
				res := make([]int32, n)
				for i := range res {
					res[i] = f(i)
				}
				size, partOrder, params := s.cost(res, order)
				if len(params) != 1<<uint(partOrder) || n%len(params) != 0 || n/len(params) <= order {
					t.Fatalf("%s, %d samples, order %d: partition order %d with %d parameters", name, n, order, partOrder, len(params))
				}
				written := int64(6)
				for j, k := range params {
					from, to := j*(n/len(params)), (j+1)*(n/len(params))
					if j == 0 {
						from = order
					}
					written += 4
					for _, r := range res[from:to] {
						written += int64(flacFold(r)>>k) + 1 + int64(k)
					}
				}
				if size != written {
					t.Errorf("%s, %d samples, order %d: the search says %d bits; its coding takes %d", name, n, order, size, written)
				}
				if least := bruteRice(res, order); size != least {
					t.Errorf("%s, %d samples, order %d: %d bits, and the least possible is %d", name, n, order, size, least)
				}
			}
		}
	}
}

// TestFLACPlanIsWhatIsWritten checks that a subframe takes the bits its
// plan says: the choice between predictors, and between the stereo codings,
// rests on those figures.
func TestFLACPlanIsWhatIsWritten(t *testing.T) {
	e, err := NewFLACEncoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(13, 14))
	signals := map[string]func(i, ch int) float64{
		"silence": func(i, ch int) float64 { return 0 },
		"noise":   func(i, ch int) float64 { return 2*rng.Float64() - 1 },
		"music":   musicLike(48000, 5),
		"a tone":  sine(48000, 100, 0.8),
	}
	for name, f := range signals {
		for _, n := range []int{4096, 1000, 7, 1} {
			for _, bps := range []int{16, 17} {
				x := make([]int32, n)
				for i, v := range render(n, 1, f) {
					x[i] = int32(v) << uint(bps-16)
				}
				var sf flacSubframe
				e.planSubframe(x, bps, &sf)
				var w bits.Writer
				e.writeSubframe(&w, x, bps, &sf)
				if w.Len(0) != sf.bits {
					t.Errorf("%s, %d samples of %d bits: planned %d bits, wrote %d (kind %d, order %d)", name, n, bps, sf.bits, w.Len(0), sf.kind, sf.order)
				}
			}
		}
	}
}

// TestFLACAgainstFFmpeg has ffmpeg decode what the encoder wrote: the same
// samples must come out, and the file must be what ffprobe takes it for.
func TestFLACAgainstFFmpeg(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(21, 22))
	for _, c := range []struct {
		name           string
		rate, channels int
		frames         int
		f              func(i, ch int) float64
	}{
		{"music_44k_stereo", 44100, 2, 44100 * 2, musicLike(44100, 3)},
		{"music_16k_mono", 16000, 1, 16000 * 2, musicLike(16000, 4)},
		{"noise_48k_stereo", 48000, 2, 10000, func(i, ch int) float64 { return 2*rng.Float64() - 1 }},
		{"extremes_48k_stereo", 48000, 2, 9000, func(i, ch int) float64 { return float64(1 - 2*((i/3+ch)%2)) }},
		{"silence_8k_mono", 8000, 1, 9000, func(i, ch int) float64 { return 0 }},
		{"five_samples", 48000, 2, 5, musicLike(48000, 6)},
		{"music_11025_mono", 11025, 1, 11025, musicLike(11025, 7)},
		{"music_70001_stereo", 70001, 2, 20000, musicLike(70001, 8)},
		{"music_384k_mono", 384000, 1, 20000, musicLike(384000, 9)},
	} {
		t.Run(c.name, func(t *testing.T) {
			pcm := render(c.frames, c.channels, c.f)
			path := filepath.Join(dir, c.name+".flac")
			if err := os.WriteFile(path, encodeFLAC(t, c.rate, c.channels, pcm, 4800), 0o644); err != nil {
				t.Fatal(err)
			}
			got := oracle.FFmpegOutput(t, "-i", path, "-c:a", "pcm_s16le", "-f", "s16le", "-")
			if len(got) != 2*len(pcm) {
				t.Fatalf("ffmpeg decoded %d samples, %d were encoded", len(got)/2, len(pcm))
			}
			for i, v := range pcm {
				if g := int16(binary.LittleEndian.Uint16(got[2*i:])); g != v {
					t.Fatalf("sample %d is %d in ffmpeg's decode, was %d", i, g, v)
				}
			}
			p := oracle.FFprobe(t, path)
			if len(p.Streams) != 1 {
				t.Fatalf("ffprobe: %d streams", len(p.Streams))
			}
			s := p.Streams[0]
			dur, _ := oracle.Seconds(s.Duration)
			if want := float64(c.frames) / float64(c.rate); p.Format.FormatName != "flac" || s.CodecName != "flac" || s.SampleRate != strconv.Itoa(c.rate) ||
				s.Channels != c.channels || math.Abs(dur-want) > 1e-5 {
				t.Errorf("ffprobe: %s, %s at %s Hz in %d channels, %s s; want flac at %d Hz in %d channels, %.6f s",
					p.Format.FormatName, s.CodecName, s.SampleRate, s.Channels, s.Duration, c.rate, c.channels, want)
			}
		})
	}
}

// FuzzFLACEncode encodes whatever samples it is given and requires them
// back from the decoder.
func FuzzFLACEncode(f *testing.F) {
	f.Add([]byte{0, 0, 1, 0, 0xff, 0x7f, 0, 0x80}, false, uint16(3))
	f.Add(bytes.Repeat([]byte{0x12, 0x34, 0xfe, 0xff}, 3000), true, uint16(1000))
	f.Fuzz(func(t *testing.T, data []byte, stereo bool, chunk uint16) {
		channels := 1
		if stereo {
			channels = 2
		}
		if len(data) > 1<<17 {
			data = data[:1<<17]
		}
		pcm := make([]int16, len(data)/2/channels*channels)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
		}
		checkFLAC(t, encodeFLAC(t, 44100, channels, pcm, int(chunk)+1), 44100, channels, pcm)
	})
}

func BenchmarkFLACEncode(b *testing.B) {
	for _, c := range []struct {
		name           string
		rate, channels int
	}{{"48k_stereo", 48000, 2}, {"16k_mono", 16000, 1}} {
		b.Run(c.name, func(b *testing.B) {
			pcm := render(c.rate*10, c.channels, musicLike(c.rate, 1))
			in := make([]float32, len(pcm))
			for i, v := range pcm {
				in[i] = float32(v) / 32767
			}
			// Ten seconds of sound: the bytes per second are those of
			// the PCM taken in.
			b.SetBytes(int64(2 * len(pcm)))
			b.ReportAllocs()
			size := 0
			for b.Loop() {
				e, _ := NewFLACEncoder(c.rate, c.channels)
				size = 0
				for at := 0; at < len(in); at += 4800 * c.channels {
					frames, err := e.Encode(in[at:min(at+4800*c.channels, len(in))])
					if err != nil {
						b.Fatal(err)
					}
					for _, f := range frames {
						size += len(f)
					}
				}
				frames, _ := e.Flush()
				for _, f := range frames {
					size += len(f)
				}
			}
			b.ReportMetric(float64(size)/float64(2*len(pcm)), "of-PCM")
		})
	}
}
