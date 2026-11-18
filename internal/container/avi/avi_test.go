package avi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// --- tiny RIFF writer for hand-built fixtures --------------------------------

func u16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func chunk(id string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	out := append(append([]byte(id), u32(uint32(len(p)))...), p...)
	if len(p)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

func list(form string, items ...[]byte) []byte {
	return chunk("LIST", []byte(form), bytes.Join(items, nil))
}

func riff(form string, items ...[]byte) []byte {
	return chunk("RIFF", []byte(form), bytes.Join(items, nil))
}

func avih(usPerFrame, frames, streams, w, h uint32) []byte {
	return chunk("avih", u32(usPerFrame), u32(0), u32(0), u32(0x10), u32(frames), u32(0), u32(streams),
		u32(0), u32(w), u32(h), make([]byte, 16))
}

func strh(typ, handler string, scale, rate, start, length, sampleSize uint32) []byte {
	return chunk("strh", []byte(typ), []byte(handler), u32(0), u32(0), u32(0), u32(scale), u32(rate),
		u32(start), u32(length), u32(0), u32(0), u32(sampleSize), make([]byte, 8))
}

func bitmapInfo(w, h int32, bits uint16, compression string, extra ...byte) []byte {
	comp := []byte(compression)
	if compression == "" {
		comp = make([]byte, 4)
	}
	return chunk("strf", u32(40), u32(uint32(w)), u32(uint32(h)), u16(1), u16(bits), comp,
		u32(0), u32(0), u32(0), u32(0), u32(0), extra)
}

func waveFormat(tag, channels uint16, rate uint32, bits uint16, extra ...byte) []byte {
	align := channels * ((bits + 7) / 8)
	return chunk("strf", u16(tag), u16(channels), u32(rate), u32(rate*uint32(align)), u16(align), u16(bits),
		u16(uint16(len(extra))), extra)
}

var (
	jpegA = []byte{0xFF, 0xD8, 1, 2, 3, 0xFF, 0xD9} // odd length: exercises padding
	jpegB = []byte{0xFF, 0xD8, 9, 9, 0xFF, 0xD9}
)

// sample is a small but complete file: MJPEG video at 25 fps with one
// dropped frame, 16-bit mono PCM at 8 kHz, a "rec " list, junk and index
// chunks inside movi, a trailing idx1 and an OpenDML AVIX segment.
func sample() []byte {
	hdrl := list("hdrl",
		avih(40000, 3, 2, 16, 8),
		list("strl", strh("vids", "mjpg", 1, 25, 0, 3, 0), bitmapInfo(16, 8, 24, "MJPG"), chunk("strn", []byte("cam"))),
		list("strl", strh("auds", "\x00\x00\x00\x00", 1, 8000, 0, 0, 2), waveFormat(1, 1, 8000, 16)),
		chunk("JUNK", make([]byte, 5)),
	)
	movi := list("movi",
		chunk("00dc", jpegA),
		chunk("01wb", make([]byte, 640)),
		chunk("JUNK", make([]byte, 3)),
		list("rec ", chunk("00dc"), chunk("01wb", make([]byte, 320))),
		chunk("00pc", make([]byte, 8)),
		chunk("ix00", make([]byte, 24)),
		chunk("00dc", jpegB),
	)
	return bytes.Join([][]byte{
		riff("AVI ", hdrl, list("INFO", chunk("ISFT", []byte("test\x00"))), movi, chunk("idx1", make([]byte, 48))),
		riff("AVIX", list("movi", chunk("00db", jpegA), chunk("01wb", make([]byte, 640)))),
	}, nil)
}

func open(t *testing.T, data []byte) *File {
	t.Helper()
	f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func TestSniff(t *testing.T) {
	if !Sniff(sample()) {
		t.Error("sample not recognised")
	}
	for _, in := range []string{"", "RIFF\x00\x00\x00\x00AVI", "RIFF\x00\x00\x00\x00WAVEfmt ", "RIFX\x00\x00\x00\x00AVI "} {
		if Sniff([]byte(in)) {
			t.Errorf("Sniff(%q) = true", in)
		}
	}
}

func TestOpen(t *testing.T) {
	data := sample()
	f := open(t, data)
	if len(f.Streams) != 2 || f.Truncated || f.IndexCapped {
		t.Fatalf("streams %d truncated %v capped %v", len(f.Streams), f.Truncated, f.IndexCapped)
	}
	if f.MicroSecPerFrame != 40000 || f.HeaderFrames != 3 || f.Width != 16 || f.Height != 8 {
		t.Errorf("avih: %+v", f)
	}
	v, a := f.Video(), f.Audio()
	if v != f.Streams[0] || a != f.Streams[1] {
		t.Fatal("Video/Audio picked the wrong streams")
	}
	if v.Type != StreamVideo || v.Handler != "mjpg" || v.Compression != "MJPG" || v.FourCC() != "MJPG" ||
		v.Width != 16 || v.Height != 8 || v.TopDown || v.BitCount != 24 || v.Scale != 1 || v.Rate != 25 {
		t.Errorf("video header: %+v", v)
	}
	if num, den := v.FrameRate(); num != 25 || den != 1 {
		t.Errorf("frame rate %d/%d", num, den)
	}
	if a.Type != StreamAudio || a.Handler != "" || a.FormatTag != FormatPCM || a.Channels != 1 || a.SampleRate != 8000 ||
		a.BitsPerSample != 16 || a.BlockAlign != 2 || a.AvgBytesPerSec != 16000 || !a.IsPCM() || a.SampleSize != 2 {
		t.Errorf("audio header: %+v", a)
	}

	// Four frames: one in a rec list is dropped (zero length), one in AVIX.
	if f.Frames() != 4 || v.Frames() != 4 || len(v.Chunks) != 4 {
		t.Fatalf("video frames %d", v.Frames())
	}
	want := [][]byte{jpegA, nil, jpegB, jpegA}
	var buf []byte
	for i, c := range v.Chunks {
		got, err := f.ReadChunk(c, buf)
		if err != nil {
			t.Fatalf("ReadChunk %d: %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("frame %d = % x, want % x", i, got, want[i])
		}
		if ts := v.ChunkTime(i); ts != time.Duration(i)*40*time.Millisecond {
			t.Errorf("frame %d at %v", i, ts)
		}
		buf = got[:0]
	}
	if v.TotalBytes != int64(2*len(jpegA)+len(jpegB)) {
		t.Errorf("video bytes %d", v.TotalBytes)
	}
	if v.Duration() != 160*time.Millisecond {
		t.Errorf("video duration %v", v.Duration())
	}

	if len(a.Chunks) != 3 || a.TotalBytes != 1600 || a.Frames() != 800 {
		t.Fatalf("audio: %d chunks, %d bytes, %d frames", len(a.Chunks), a.TotalBytes, a.Frames())
	}
	for i, ms := range []int{0, 40, 60, 100} { // the last entry is the end time
		if ts := a.ChunkTime(i); ts != time.Duration(ms)*time.Millisecond {
			t.Errorf("audio chunk %d at %v, want %dms", i, ts, ms)
		}
	}
	if f.Duration != 160*time.Millisecond {
		t.Errorf("file duration %v", f.Duration)
	}
}

func TestStartAndFallbackRate(t *testing.T) {
	data := riff("AVI ",
		list("hdrl", avih(100000, 2, 2, 4, 4),
			// No strh rate: the avih frame period (10 fps) applies.
			list("strl", strh("vids", "\x00\x00\x00\x00", 0, 0, 5, 2, 0), bitmapInfo(4, -4, 24, "")),
			list("strl", strh("auds", "\x00\x00\x00\x00", 1, 4000, 2000, 0, 1), waveFormat(1, 1, 4000, 8)),
			// A stream without strf keeps its slot but cannot be interpreted.
			list("strl", strh("vids", "xxxx", 1, 30, 0, 0, 0))),
		list("movi", chunk("00db", make([]byte, 48)), chunk("01wb", make([]byte, 1000)),
			chunk("00db", make([]byte, 48)), chunk("02dc", []byte{1})))
	f := open(t, data)
	if len(f.Streams) != 3 || f.Streams[2].Type != StreamOther || len(f.Streams[2].Chunks) != 1 {
		t.Fatalf("streams: %d", len(f.Streams))
	}
	v, a := f.Video(), f.Audio()
	if v.Height != 4 || !v.TopDown || v.Compression != "" || v.FourCC() != "" {
		t.Errorf("video: %+v", v)
	}
	if num, den := v.FrameRate(); num != 1_000_000 || den != 100_000 {
		t.Errorf("fallback rate %d/%d", num, den)
	}
	if v.StartTime() != 500*time.Millisecond || v.ChunkTime(1) != 600*time.Millisecond || v.Duration() != 200*time.Millisecond {
		t.Errorf("video timing: start %v, frame 1 %v, duration %v", v.StartTime(), v.ChunkTime(1), v.Duration())
	}
	if a.StartTime() != 500*time.Millisecond || a.Duration() != 250*time.Millisecond {
		t.Errorf("audio timing: start %v, duration %v", a.StartTime(), a.Duration())
	}
	if f.Duration != 750*time.Millisecond {
		t.Errorf("file duration %v", f.Duration)
	}
}

func TestExtensibleAndCompressedAudio(t *testing.T) {
	guid := append(u16(3), make([]byte, 14)...) // IEEE float SubFormat
	ext := bytes.Join([][]byte{u16(32), u32(3), guid}, nil)
	data := riff("AVI ",
		list("hdrl", avih(0, 0, 2, 0, 0),
			list("strl", strh("auds", "\x00\x00\x00\x00", 1, 48000, 0, 0, 8), waveFormat(FormatExtensible, 2, 48000, 32, ext...)),
			// MP3-style stream: one variable-size frame per chunk, 1152 samples each.
			list("strl", strh("auds", "\x00\x00\x00\x00", 1152, 44100, 0, 0, 0), waveFormat(FormatMP3, 2, 44100, 0, 1, 2, 3))),
		list("movi", chunk("00wb", make([]byte, 4800*8)), chunk("01wb", make([]byte, 100)), chunk("01wb", make([]byte, 90))))
	f := open(t, data)
	a := f.Streams[0]
	if !a.Extensible || a.FormatTag != FormatIEEEFloat || a.ChannelMask != 3 || a.Channels != 2 || a.BitsPerSample != 32 || !a.IsPCM() {
		t.Errorf("extensible: %+v", a)
	}
	if a.Duration() != 100*time.Millisecond {
		t.Errorf("float duration %v", a.Duration())
	}
	m := f.Streams[1]
	if m.FormatTag != FormatMP3 || m.IsPCM() || !bytes.Equal(m.Extra, []byte{1, 2, 3}) || m.Frames() != 2 {
		t.Errorf("mp3: %+v", m)
	}
	if got, want := m.ChunkTime(1), time.Duration(1152*int64(time.Second)/44100); got < want-time.Microsecond || got > want+time.Microsecond {
		t.Errorf("mp3 chunk 1 at %v, want %v", got, want)
	}
}

// A recording that was cut short: the index ends at the last whole chunk,
// whatever the list sizes claim.
func TestTruncated(t *testing.T) {
	full := sample()
	whole := open(t, full)
	last := whole.Video().Chunks[2] // jpegB, the last frame of the first segment
	for _, cut := range []int64{last.Offset + 3, last.Offset - 2, last.Offset - 8} {
		data := full[:cut]
		f := open(t, data)
		if !f.Truncated {
			t.Errorf("cut at %d: not flagged", cut)
		}
		if n := len(f.Video().Chunks); n != 2 {
			t.Errorf("cut at %d: %d video chunks, want 2", cut, n)
		}
		for _, s := range f.Streams {
			for _, c := range s.Chunks {
				if _, err := f.ReadChunk(c, nil); err != nil {
					t.Errorf("cut at %d: %v", cut, err)
				}
			}
		}
	}

	// An unfinalised file: RIFF and movi sizes were never patched.
	hdrl := list("hdrl", avih(40000, 0, 1, 16, 8),
		list("strl", strh("vids", "mjpg", 1, 25, 0, 0, 0), bitmapInfo(16, 8, 24, "MJPG")))
	data := bytes.Join([][]byte{[]byte("RIFF\x00\x00\x00\x00AVI "), hdrl, []byte("LIST\x00\x00\x00\x00movi"),
		chunk("00dc", jpegB), chunk("00dc", jpegB), []byte("00dc\x10\x00\x00\x00\xff\xd8")}, nil)
	f := open(t, data)
	if n := f.Frames(); n != 2 || !f.Truncated {
		t.Errorf("unfinalised: %d frames, truncated %v", n, f.Truncated)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	for _, in := range [][]byte{nil, []byte("RIFF"), []byte("RIFF\x04\x00\x00\x00WAVE"), []byte("\x00\x00\x00\x18ftypisom")} {
		if _, err := Open(ctx, bytes.NewReader(in), int64(len(in))); !errors.Is(err, ErrNotAVI) {
			t.Errorf("Open(%q) = %v, want ErrNotAVI", in, err)
		}
	}
	noStreams := riff("AVI ", list("hdrl", avih(40000, 0, 0, 16, 8)), list("movi", chunk("00dc", jpegA)))
	if _, err := Open(ctx, bytes.NewReader(noStreams), int64(len(noStreams))); err == nil || errors.Is(err, ErrNotAVI) {
		t.Errorf("no streams: %v", err)
	}
	// A declared size larger than the data must not read past the end.
	data := sample()
	if f, err := Open(ctx, bytes.NewReader(data), int64(len(data))+1000); err != nil || f.Frames() != 4 {
		t.Errorf("oversized: %v", err)
	}

	// Cancellation is observed while scanning.
	var many [][]byte
	for i := 0; i < 5000; i++ {
		many = append(many, chunk("00dc", jpegB))
	}
	big := riff("AVI ", list("hdrl", avih(40000, 0, 1, 16, 8),
		list("strl", strh("vids", "mjpg", 1, 25, 0, 0, 0), bitmapInfo(16, 8, 24, "MJPG"))), list("movi", many...))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Open(cctx, bytes.NewReader(big), int64(len(big))); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Open = %v", err)
	}
	if f := open(t, big); f.Frames() != 5000 {
		t.Errorf("frames %d", f.Frames())
	}
}

func TestReadChunkBounds(t *testing.T) {
	data := sample()
	f := open(t, data)
	for _, c := range []Chunk{
		{Offset: -1, Size: 4},
		{Offset: int64(len(data)) - 2, Size: 4},
		{Offset: int64(len(data)) + 1, Size: 0},
		{Offset: 0, Size: MaxChunkSize + 1},
	} {
		if _, err := f.ReadChunk(c, nil); err == nil {
			t.Errorf("ReadChunk(%+v) succeeded", c)
		}
	}
	// A reader shorter than the size it was opened with fails cleanly.
	short, err := Open(context.Background(), bytes.NewReader(data), int64(len(data))+64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.ReadChunk(Chunk{Offset: int64(len(data)) - 2, Size: 8}, nil); err == nil {
		t.Error("read past the end of the data succeeded")
	}
}

func TestScaleDur(t *testing.T) {
	for _, c := range []struct {
		n, num, den uint64
		want        time.Duration
	}{
		{432, 20833, 500000, 17999712 * time.Microsecond},
		{1, 1, 3, 333333333},
		{2, 1, 3, 666666667},
		{5, 0, 3, 0},
		{5, 3, 0, 0},
		{1 << 40, 1 << 32, 1, 1<<63 - 1},
		{1 << 62, 1 << 62, 3, 1<<63 - 1},
	} {
		if got := scaleDur(c.n, c.num, c.den); got != c.want {
			t.Errorf("scaleDur(%d, %d, %d) = %d, want %d", c.n, c.num, c.den, got, c.want)
		}
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(sample())
	f.Add(riff("AVI ", list("hdrl", avih(40000, 1, 1, 16, 8),
		list("strl", strh("vids", "mjpg", 1, 25, 0, 1, 0), bitmapInfo(16, 8, 24, "MJPG"))),
		list("movi", chunk("00dc", jpegB))))
	f.Add([]byte("RIFF\x00\x00\x00\x00AVI LIST\x00\x00\x00\x00movi00dc\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		af, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		var buf []byte
		for _, s := range af.Streams {
			s.FrameRate()
			s.FourCC()
			if s.Duration() < 0 || s.StartTime() < 0 || s.Frames() < 0 {
				t.Fatalf("negative timing: %v %v %d", s.Duration(), s.StartTime(), s.Frames())
			}
			var sum int64
			for i, c := range s.Chunks {
				if i < 64 {
					b, err := af.ReadChunk(c, buf)
					if err != nil {
						t.Fatalf("ReadChunk of an indexed chunk: %v", err)
					}
					buf = b[:0]
				}
				if s.ChunkTime(i) < 0 {
					t.Fatalf("negative chunk time")
				}
				sum += int64(c.Size)
			}
			if sum != s.TotalBytes {
				t.Fatalf("TotalBytes %d, chunks sum to %d", s.TotalBytes, sum)
			}
		}
		if af.Duration < 0 {
			t.Fatalf("negative duration %v", af.Duration)
		}
	})
}
