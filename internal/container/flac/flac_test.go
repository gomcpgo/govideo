package flac

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/gomcpgo/govideo/codec/native"
	"github.com/gomcpgo/govideo/internal/codechdr"
)

// The files of these tests are written by the module's encoder, which makes
// frames of 4096 samples numbered from 0: the reader must hand back each of
// them, byte for byte, whatever stands around and between them.

// stream is a FLAC stream in its parts.
type stream struct {
	head   []byte // the marker and STREAMINFO
	frames [][]byte
}

// encode makes a stream of n sample frames from f, whose value is a sample.
func encode(t testing.TB, rate, channels, n int, f func(i, ch int) float64) stream {
	t.Helper()
	e, err := native.NewFLACEncoder(rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]float32, 0, n*channels)
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			pcm = append(pcm, float32(f(i, c)))
		}
	}
	var s stream
	keep := func(frames [][]byte, err error) {
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range frames {
			s.frames = append(s.frames, append([]byte(nil), fr...))
		}
	}
	keep(e.Encode(pcm))
	keep(e.Flush())
	s.head = e.Header()
	return s
}

// music is a sound with something in it for a predictor, and noise is one
// with nothing: its frames hold the samples as they are.
func music(i, ch int) float64 {
	t := float64(i) / 48000
	return 0.4*math.Sin(2*math.Pi*(220+40*t)*t+float64(ch)) + 0.1*math.Sin(2*math.Pi*1300*t)
}

func noise(seed uint64) func(i, ch int) float64 {
	rng := rand.New(rand.NewPCG(seed, 1))
	return func(int, int) float64 { return 2*rng.Float64() - 1 }
}

// file puts a stream together, with other metadata blocks after STREAMINFO.
func (s stream) file(blocks ...[]byte) []byte {
	out := append([]byte(nil), s.head...)
	if len(blocks) > 0 {
		out[4] &^= 0x80 // STREAMINFO is no longer the last block
	}
	for i, b := range blocks {
		b = append([]byte(nil), b...)
		if i == len(blocks)-1 {
			b[0] |= 0x80
		}
		out = append(out, b...)
	}
	for _, fr := range s.frames {
		out = append(out, fr...)
	}
	return out
}

// block is a metadata block of the given type.
func block(kind byte, body []byte) []byte {
	return append([]byte{kind, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
}

// comments is a Vorbis comment block with the given fields.
func comments(vendor string, fields ...string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	b = append(b, vendor...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(fields)))
	for _, f := range fields {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return block(4, b)
}

func id3v2(body int) []byte {
	return append([]byte{'I', 'D', '3', 4, 0, 0, byte(body >> 21 & 0x7f), byte(body >> 14 & 0x7f), byte(body >> 7 & 0x7f), byte(body & 0x7f)}, make([]byte, body)...)
}

func open(t testing.TB, data []byte) *File {
	t.Helper()
	f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// readAll returns every frame of a file.
func readAll(t testing.TB, f *File, data []byte) [][]byte {
	t.Helper()
	rd := f.NewReader(bytes.NewReader(data))
	var out [][]byte
	for {
		fr, err := rd.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), fr...))
	}
}

// same checks that the frames read are those written.
func same(t *testing.T, what string, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d frames read, %d written", what, len(got), len(want))
		return
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s: frame %d is %d bytes, was written as %d", what, i, len(got[i]), len(want[i]))
			return
		}
	}
}

func TestOpenAndRead(t *testing.T) {
	s := encode(t, 44100, 2, 30000, music)
	if len(s.frames) != 8 {
		t.Fatalf("%d frames, want 8", len(s.frames))
	}
	var audio int64
	for _, fr := range s.frames {
		audio += int64(len(fr))
	}
	tags := comments("a vendor", "TITLE=A title", "Artist=One", "ARTIST=Two", "no equals sign", "=no name", "bad\x01name=x", "comment=caf\xc3\xa9")
	padding := block(1, make([]byte, 1000))
	seek := block(3, make([]byte, 18*3))
	for _, c := range []struct {
		name   string
		data   []byte
		offset int64
		tags   int
	}{
		{"nothing but the stream", s.file(), 42, 0},
		{"other blocks", s.file(padding, tags, seek), 42 + int64(len(padding)+len(tags)+len(seek)), 3},
		{"an ID3v2 tag in front", append(id3v2(700), s.file(tags)...), 710 + 42 + int64(len(tags)), 3},
		{"an ID3v1 tag behind", append(s.file(), append([]byte("TAG"), make([]byte, 125)...)...), 42, 0},
		{"junk before the first frame", append(s.file(padding), nil...), 42 + int64(len(padding)), 0},
	} {
		f := open(t, c.data)
		if f.SampleRate != 44100 || f.Channels != 2 || f.BitDepth != 16 || f.BlockSize != 4096 || f.Samples != 30000 || f.Measured ||
			f.DataOffset != c.offset || f.DataSize != audio || len(f.StreamInfo) != 34 || len(f.Tags) != c.tags {
			t.Errorf("%s: %+v", c.name, *f)
		}
		if c.tags > 0 && (f.Tags["title"] != "A title" || f.Tags["artist"] != "One;Two" || f.Tags["comment"] != "café") {
			t.Errorf("%s: tags %q", c.name, f.Tags)
		}
		same(t, c.name, readAll(t, f, c.data), s.frames)
		if a, err := codechdr.ParseFLACStreamInfo(f.StreamInfo); err != nil || a.SampleRate != 44100 || a.FrameSize != 4096 {
			t.Errorf("%s: STREAMINFO %+v, %v", c.name, a, err)
		}
	}

	// Bytes that are no frame, before the first and between two: stepped
	// over.
	junk := bytes.Repeat([]byte{0x55, 0xff, 0x00}, 100)
	data := append(append([]byte(nil), s.head...), junk...)
	for i, fr := range s.frames {
		if data = append(data, fr...); i == 3 {
			data = append(data, junk...)
		}
	}
	f := open(t, data)
	if f.DataOffset != 42+300 {
		t.Errorf("junk before the first frame: the audio starts at byte %d, want %d", f.DataOffset, 42+300)
	}
	got := readAll(t, f, data)
	// The frame before the junk has no checksum where it is looked for, at
	// the next header: it is given out with the junk behind it, which the
	// decoder does not look at.
	if len(got) == 8 {
		got[3] = got[3][:min(len(got[3]), len(s.frames[3]))]
	}
	same(t, "junk between frames", got, s.frames)

	// A stream with no frames: no sound, if it states none, and else not
	// the file it says it is.
	empty := encode(t, 44100, 2, 0, music).head
	if f := open(t, empty); f.Samples != 0 || f.DataSize != 0 || f.BlockSize != 0 || len(readAll(t, f, empty)) != 0 {
		t.Errorf("a file with no frames: %+v", *f)
	}
	if f, err := Open(context.Background(), bytes.NewReader(s.head), int64(len(s.head))); err == nil {
		t.Errorf("a file that states %d samples and has no frames: opened", f.Samples)
	}
	// Frames of another format than the header states are not its frames.
	other := encode(t, 44100, 1, 30000, music)
	other.head = s.head
	if _, err := Open(context.Background(), bytes.NewReader(other.file()), int64(len(other.file()))); err == nil {
		t.Error("a stereo header on mono frames: opened")
	}
}

// TestFalseSync puts a whole frame header, checksum and all, among the
// samples of a frame that stores them as they are. The reader must not end
// the frame there: the checksum of the frame does not agree.
func TestFalseSync(t *testing.T) {
	s := encode(t, 48000, 1, 20000, noise(1))
	hdr, ok := codechdr.FLACFrameHeader(s.frames[1])
	if !ok {
		t.Fatal("no header on the frame the encoder wrote")
	}
	// The same sound, with the bytes of the next frame's header where
	// samples 1000 to 1002 of the second frame are.
	fake := s.frames[2][:hdr.Size]
	if len(fake)%2 != 0 {
		t.Fatalf("a header of %d bytes does not fit whole samples", len(fake))
	}
	base := noise(1)
	planted := encode(t, 48000, 1, 20000, func(i, ch int) float64 {
		v := base(i, ch)
		if k := i - (4096 + 1000); k >= 0 && k < len(fake)/2 {
			return float64(int16(binary.BigEndian.Uint16(fake[2*k:]))) / 32767
		}
		return v
	})
	if !bytes.Contains(planted.frames[1], fake) {
		t.Fatal("the frame does not hold the header planted in it: it is not stored verbatim")
	}
	data := planted.file()
	f := open(t, data)
	same(t, "a frame with a frame header in it", readAll(t, f, data), planted.frames)

	// With the frame's checksum broken, nothing says the planted header is
	// not the next frame: the damaged frame is cut there, and what follows
	// is given out as it comes. All of the file is still handed over.
	bad := append([]byte(nil), data...)
	at := 42 + len(planted.frames[0]) + len(planted.frames[1]) - 1
	bad[at] ^= 0xff
	var total int
	for _, fr := range readAll(t, open(t, bad), bad) {
		total += len(fr)
	}
	if total != len(bad)-42 {
		t.Errorf("%d bytes of frames read from %d of audio", total, len(bad)-42)
	}
}

// TestDamage breaks frames and cuts the file short: the frames around the
// damage are read as they were written.
func TestDamage(t *testing.T) {
	s := encode(t, 48000, 2, 50000, music)
	data := s.file()
	starts := []int{42}
	for _, fr := range s.frames {
		starts = append(starts, starts[len(starts)-1]+len(fr))
	}
	// A bit flipped inside a frame: the frame keeps its place and length.
	bad := append([]byte(nil), data...)
	bad[starts[5]+100] ^= 0x10
	got := readAll(t, open(t, bad), bad)
	want := append([][]byte(nil), s.frames...)
	want[5] = bad[starts[5]:starts[6]]
	same(t, "a flipped bit", got, want)

	// A frame header destroyed: its frame goes with the one before.
	bad = append([]byte(nil), data...)
	bad[starts[5]] = 0
	got = readAll(t, open(t, bad), bad)
	if len(got) != len(s.frames)-1 || !bytes.Equal(got[6], s.frames[7]) || !bytes.Equal(got[3], s.frames[3]) {
		t.Errorf("a destroyed header: %d frames read of %d", len(got), len(s.frames))
	}

	// The file cut inside its last frame, and inside a frame header.
	for _, cut := range []int{len(data) - 10, starts[len(starts)-2] + 3, starts[4] + 1} {
		short := data[:cut]
		got := readAll(t, open(t, short), short)
		whole := 0
		for whole+1 < len(starts) && starts[whole+1] <= cut {
			whole++
		}
		if len(got) < whole || len(got) > whole+1 {
			t.Errorf("cut at byte %d: %d frames read, %d are whole", cut, len(got), whole)
			continue
		}
		// The bytes of a header cut short stay on the frame before them,
		// where the decoder does not look.
		if whole > 0 && len(got[whole-1]) < len(s.frames[whole-1])+codechdr.FLACMaxHeader {
			got[whole-1] = got[whole-1][:min(len(got[whole-1]), len(s.frames[whole-1]))]
		}
		same(t, "a file cut short", got[:whole], s.frames[:whole])
	}

	// Many damaged frames: every one is still handed over, at its place.
	long := encode(t, 8000, 1, 4096*60, music)
	bad = long.file()
	at := 42
	for i, fr := range long.frames {
		if i%2 == 1 {
			bad[at+len(fr)/2] ^= 1
		}
		at += len(fr)
	}
	got = readAll(t, open(t, bad), bad)
	if len(got) != len(long.frames) {
		t.Errorf("every second frame damaged: %d frames read of %d", len(got), len(long.frames))
	}
	for i := range got {
		if len(got[i]) != len(long.frames[i]) {
			t.Errorf("every second frame damaged: frame %d is %d bytes, was %d", i, len(got[i]), len(long.frames[i]))
			break
		}
	}
}

// counting counts what is read through it.
type counting struct {
	r     io.ReaderAt
	bytes int64
}

func (c *counting) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

// TestSkip goes to frames by their number, and must get there without
// reading the file up to them.
func TestSkip(t *testing.T) {
	for _, c := range []struct {
		name string
		s    stream
	}{
		{"noise", encode(t, 48000, 2, 4096*400+100, noise(2))},
		{"music", encode(t, 48000, 2, 4096*400, music)},
		{"silence", encode(t, 8000, 1, 4096*3000, func(int, int) float64 { return 0 })},
		{"three frames", encode(t, 48000, 1, 4096*3, music)},
	} {
		data := c.s.file(block(1, make([]byte, 5000)))
		f := open(t, data)
		last := int64(len(c.s.frames) - 1)
		for _, n := range []int64{-1, 0, 1, 2, 3, last / 3, last / 2, last - 1, last, last + 1, last + 1000} {
			src := &counting{r: bytes.NewReader(data)}
			rd := f.NewReader(src)
			passed := rd.Skip(n)
			if want := max(0, min(n, last)); passed != want {
				t.Errorf("%s: Skip(%d) passed %d frames, want %d", c.name, n, passed, want)
				continue
			}
			fr, err := rd.Next()
			if err != nil || !bytes.Equal(fr, c.s.frames[passed]) {
				t.Errorf("%s: after Skip(%d), the next frame is not frame %d (%v)", c.name, n, passed, err)
			}
			if f.FirstSample(fr) != passed*4096 {
				t.Errorf("%s: frame %d starts at sample %d", c.name, passed, f.FirstSample(fr))
			}
			// A handful of places are read, not the file.
			if limit := int64(3 << 20); len(data) > 8<<20 && src.bytes > limit {
				t.Errorf("%s: Skip(%d) read %d bytes of a file of %d", c.name, n, src.bytes, len(data))
			}
		}
		t.Logf("%s: %d frames in %d bytes", c.name, len(c.s.frames), len(data))
	}
}

// renumber rewrites the header of a frame: as one of a stream of blocks of
// varying size, numbered by its first sample, or with another frame number.
func renumber(t testing.TB, frame []byte, variable bool, number uint64) []byte {
	t.Helper()
	h, ok := codechdr.FLACFrameHeader(frame)
	if !ok || h.Number >= 0x80 {
		t.Fatalf("frame header: %+v, %v", h, ok)
	}
	out := append([]byte(nil), frame[:4]...)
	if variable {
		out[1] |= 1
	}
	// The number, as UTF-8 codes one and carried on to 36 bits.
	switch n := 1 + (max(64-leadingZeros(number), 7)-2)/5; {
	case number < 0x80:
		out = append(out, byte(number))
	default:
		out = append(out, byte(0xff<<uint(8-n))|byte(number>>uint(6*(n-1))))
		for i := n - 2; i >= 0; i-- {
			out = append(out, 0x80|byte(number>>uint(6*i))&0x3f)
		}
	}
	out = append(out, frame[5:h.Size-1]...)
	out = append(out, codechdr.FLACCRC8(out))
	out = append(out, frame[h.Size:len(frame)-2]...)
	return binary.BigEndian.AppendUint16(out, codechdr.FLACCRC16(0, out))
}

func leadingZeros(v uint64) int {
	n := 0
	for ; n < 64 && v&(1<<uint(63-n)) == 0; n++ {
	}
	return n
}

// TestMeasured reads the length of files that do not state it, and of
// streams numbered by sample.
func TestMeasured(t *testing.T) {
	unstated := func(s stream) stream {
		s.head = append([]byte(nil), s.head...)
		// The 36 bits of the sample count.
		s.head[8+13] &= 0xf0
		copy(s.head[8+14:8+18], []byte{0, 0, 0, 0})
		return s
	}
	for _, n := range []int{1, 100, 4096, 4097, 4096 * 40, 4096*300 + 17} {
		s := unstated(encode(t, 48000, 2, n, music))
		data := s.file()
		src := &counting{r: bytes.NewReader(data)}
		f, err := Open(context.Background(), src, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if f.Samples != int64(n) || !f.Measured || f.BlockSize != 4096 {
			t.Errorf("%d samples, not stated: measured %d (%v), blocks of %d", n, f.Samples, f.Measured, f.BlockSize)
		}
		if len(data) > 2<<20 && src.bytes > 1<<20 {
			t.Errorf("%d samples: %d bytes read of %d to find the length", n, src.bytes, len(data))
		}
		same(t, "length not stated", readAll(t, f, data), s.frames)
	}

	// Blocks of varying size: each frame says where its samples start.
	s := encode(t, 48000, 2, 4096*50+1000, music)
	var v stream
	v.head = append([]byte(nil), s.head...)
	binary.BigEndian.PutUint16(v.head[8:], 16) // the smallest block
	for i, fr := range s.frames {
		v.frames = append(v.frames, renumber(t, fr, true, uint64(i)*4096))
	}
	// The same range of sizes on frames numbered as frames: a stream of
	// varying blocks from before frames said which they are. Its length is
	// the header's, and a frame is not gone to by its number.
	old := stream{head: v.head, frames: s.frames}
	if f := open(t, old.file()); f.BlockSize != 0 || f.Samples != 4096*50+1000 {
		t.Errorf("a range of block sizes, frames numbered: %d samples, block size %d", f.Samples, f.BlockSize)
	} else if passed := f.NewReader(bytes.NewReader(old.file())).Skip(20); passed != 0 {
		t.Errorf("a range of block sizes, frames numbered: Skip passed %d frames", passed)
	}
	for _, st := range []stream{v, unstated(v)} {
		data := st.file()
		f := open(t, data)
		if f.BlockSize != 0 || f.Samples != 4096*50+1000 {
			t.Errorf("blocks of varying size: %d samples, block size %d", f.Samples, f.BlockSize)
		}
		same(t, "blocks of varying size", readAll(t, f, data), st.frames)
		rd := f.NewReader(bytes.NewReader(data))
		if passed := rd.Skip(20); passed != 0 {
			t.Errorf("blocks of varying size: Skip passed %d frames", passed)
		}
		if fr, err := rd.Next(); err != nil || f.FirstSample(fr) != 0 {
			t.Errorf("blocks of varying size: the first frame after Skip: %v", err)
		}
		fr, _ := rd.Next()
		if f.FirstSample(fr) != 4096 {
			t.Errorf("blocks of varying size: the second frame starts at sample %d", f.FirstSample(fr))
		}
	}

	// A stream that does not start at frame 0, as when it was cut out of a
	// longer one: frames and samples count from its first, and its length is
	// its own.
	var late stream
	late.head = s.head
	for i, fr := range s.frames {
		late.frames = append(late.frames, renumber(t, fr, false, uint64(i)+5000))
	}
	for _, st := range []stream{late, unstated(late)} {
		data := st.file()
		f := open(t, data)
		if f.Samples != 4096*50+1000 {
			t.Errorf("a stream that starts at frame 5000: %d samples", f.Samples)
		}
		rd := f.NewReader(bytes.NewReader(data))
		if fr, _ := rd.Next(); f.FirstSample(fr) != 0 {
			t.Errorf("a stream that starts at frame 5000: its first frame starts at sample %d", f.FirstSample(fr))
		}
		rd = f.NewReader(bytes.NewReader(data))
		if passed := rd.Skip(30); passed != 30 {
			t.Errorf("a stream that starts at frame 5000: Skip(30) passed %d", passed)
		}
		if fr, _ := rd.Next(); !bytes.Equal(fr, st.frames[30]) || f.FirstSample(fr) != 30*4096 {
			t.Error("a stream that starts at frame 5000: Skip(30) is not at its frame 30")
		}
	}

	// Frame numbers that do not rise are not gone by: the reader stays
	// before the place where they stop making sense.
	var odd stream
	odd.head = s.head
	for i, fr := range s.frames {
		n := uint64(i)
		if i >= 10 {
			n = uint64(i % 3)
		}
		odd.frames = append(odd.frames, renumber(t, fr, false, n))
	}
	data := odd.file()
	f := open(t, data)
	rd := f.NewReader(bytes.NewReader(data))
	if passed := rd.Skip(40); passed > 9 {
		t.Errorf("frame numbers that fall back: Skip(40) passed %d frames", passed)
	} else if fr, _ := rd.Next(); !bytes.Equal(fr, odd.frames[passed]) {
		t.Errorf("frame numbers that fall back: after passing %d frames the reader is not at frame %d", passed, passed)
	}
}

func TestNotFLAC(t *testing.T) {
	s := encode(t, 48000, 2, 5000, music)
	good := s.file()
	mod := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}
	for name, data := range map[string][]byte{
		"nothing":                   nil,
		"the marker alone":          []byte("fLaC"),
		"another marker":            mod(func(b []byte) { b[0] = 'F' }),
		"cut inside STREAMINFO":     good[:30],
		"another first block":       mod(func(b []byte) { b[4] = 0x81 }),
		"STREAMINFO of 35 bytes":    mod(func(b []byte) { b[7] = 35 }),
		"no sample rate":            mod(func(b []byte) { b[18], b[19], b[20] = 0, 0, b[20]&0x0f }),
		"samples of 3 bits":         mod(func(b []byte) { b[20], b[21] = b[20]&0xfe, b[21]&0x0f|0x20 }),
		"a block past the end":      append(mod(func(b []byte) { b[4] = 0 })[:42], 1, 0xff, 0xff, 0xff, 0),
		"no last block":             append(mod(func(b []byte) { b[4] = 0 })[:42], 1, 0, 0, 0),
		"the forbidden block type":  append(mod(func(b []byte) { b[4] = 0 })[:42], 0xff, 0, 0, 0),
		"an ID3 tag and no marker":  append(id3v2(20), "not FLAC at all, whatever the tag in front may say"...),
		"an ID3 tag past the end":   append(id3v2(20)[:10], good...),
		"the marker after a little": append([]byte{0}, good...),
	} {
		if f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
			t.Errorf("%s: opened, as %+v", name, *f)
		}
	}
	// A comment block that is not valid costs the tags, not the file.
	for _, tags := range [][]byte{
		block(4, []byte{0xff, 0xff, 0xff, 0xff}),
		block(4, []byte{1, 0, 0, 0, 'v', 0xff, 0xff, 0xff, 0x7f}),
		block(4, []byte{0, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 'a', '=', 'b', 9, 0, 0, 0}),
		block(4, nil),
	} {
		data := s.file(tags)
		f := open(t, data)
		same(t, "a broken comment block", readAll(t, f, data), s.frames)
	}
}

// FuzzOpen opens whatever it is given, reads its frames and goes to one by
// number: nothing may panic, hang or hand out more than the file holds.
func FuzzOpen(f *testing.F) {
	s := encode(f, 8000, 1, 9000, music)
	f.Add(s.file(), int64(1))
	f.Add(s.file(comments("v", "a=b"), block(1, make([]byte, 10))), int64(2))
	f.Add(append(id3v2(5), s.file()...), int64(0))
	var v stream
	v.head = s.head
	for i, fr := range s.frames {
		v.frames = append(v.frames, renumber(f, fr, true, uint64(i)*4096))
	}
	f.Add(v.file(), int64(1))
	f.Fuzz(func(t *testing.T, data []byte, n int64) {
		file, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if file.SampleRate < 1 || file.Channels < 1 || file.Channels > 8 || file.BitDepth < 4 || file.BitDepth > 32 ||
			file.Samples < 0 || file.DataOffset < 0 || file.DataSize < 0 || file.DataOffset+file.DataSize > int64(len(data)) {
			t.Fatalf("%+v from %d bytes", *file, len(data))
		}
		rd := file.NewReader(bytes.NewReader(data))
		if passed := rd.Skip(n); passed < 0 || passed > max(n, 0) {
			t.Fatalf("Skip(%d) passed %d", n, passed)
		}
		total := 0
		for {
			fr, err := rd.Next()
			if err != nil {
				break
			}
			if len(fr) == 0 {
				t.Fatal("an empty frame")
			}
			file.FirstSample(fr)
			if total += len(fr); total > len(data) {
				t.Fatalf("%d bytes of frames from a file of %d", total, len(data))
			}
		}
	})
}
