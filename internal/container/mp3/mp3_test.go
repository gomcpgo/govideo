package mp3

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gomcpgo/govideo/internal/codechdr"
)

// Frame headers: MPEG-1 layer III at 44.1 kHz in stereo, 128 and 64 kbit/s
// (417 and 208 bytes), and MPEG-2 layer III at 16 kHz in mono, 32 kbit/s
// (144 bytes, 576 samples).
var (
	h128 = []byte{0xff, 0xfb, 0x90, 0x00}
	h64  = []byte{0xff, 0xfb, 0x50, 0x00}
	hLow = []byte{0xff, 0xf3, 0x48, 0xc0}
)

// frame returns a whole frame with the given header and, at off, payload.
func frame(t testing.TB, hdr []byte, off int, payload ...byte) []byte {
	h, ok := codechdr.MPEGAudioFrameHeader(hdr)
	if !ok || h.Size == 0 {
		t.Fatalf("bad test header % x", hdr)
	}
	f := make([]byte, h.Size)
	copy(f, hdr)
	copy(f[off:], payload)
	return f
}

func frames(t testing.TB, hdr []byte, n int) []byte {
	var out []byte
	for i := 0; i < n; i++ {
		out = append(out, frame(t, hdr, 0)...)
	}
	return out
}

func id3v2(body int) []byte {
	return append([]byte{'I', 'D', '3', 4, 0, 0, byte(body >> 21 & 0x7f), byte(body >> 14 & 0x7f), byte(body >> 7 & 0x7f), byte(body & 0x7f)}, make([]byte, body)...)
}

// xing builds a Xing or Info header with a frame count and, if lame, LAME's
// extension carrying a delay and a padding.
func xing(tag string, count uint32, lame bool, delay, padding int) []byte {
	p := append([]byte(tag), 0, 0, 0, 3) // frames and bytes
	p = binary.BigEndian.AppendUint32(p, count)
	p = binary.BigEndian.AppendUint32(p, 0)
	if lame {
		p = append(p, "LAME3.100"...)
		p = append(p, make([]byte, 12)...)
		p = append(p, byte(delay>>4), byte(delay&15)<<4|byte(padding>>8), byte(padding))
	}
	return p
}

func open(t testing.TB, data []byte) *File {
	t.Helper()
	f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestCounted(t *testing.T) {
	// Constant rate, nothing but frames.
	f := open(t, frames(t, h128, 10))
	if f.Codec != "mp3" || f.SampleRate != 44100 || f.Channels != 2 || f.FrameSamples != 1152 ||
		f.Frames != 10 || !f.Counted || f.VBR || f.BitRate != 128000 || f.DataOffset != 0 || f.DataSize != 4170 || f.Samples() != 11520 {
		t.Errorf("constant rate: %+v", *f)
	}
	// Two rates, between an ID3v2 tag (with a footer) and an ID3v1 tag.
	tag := id3v2(300)
	tag[5] = 0x10
	tag = append(tag, make([]byte, 10)...)
	v1 := append([]byte("TAG"), make([]byte, 125)...)
	f = open(t, cat(tag, tag, frames(t, h128, 3), frames(t, h64, 4), v1))
	if f.Frames != 7 || !f.VBR || f.DataOffset != 640 || f.DataSize != 3*417+4*208 {
		t.Errorf("two rates with tags: %+v", *f)
	}
	// A frame cut short by the end of the file is not one.
	whole := frames(t, h128, 5)
	if f = open(t, whole[:len(whole)-1]); f.Frames != 4 {
		t.Errorf("cut short: %d frames, want 4", f.Frames)
	}
	// Other data in the stream (an APE tag, damage) is stepped over, and so
	// is what follows the last frame.
	junk := bytes.Repeat([]byte{0xff, 0xfb, 0x00, 0x01, 0x02}, 40) // sync words that start no frame
	if f = open(t, cat(frames(t, h128, 3), junk, frames(t, h128, 2), junk)); f.Frames != 5 {
		t.Errorf("with junk: %d frames, want 5", f.Frames)
	}
	// Frames of another stream are not counted.
	if f = open(t, cat(frames(t, h128, 3), frames(t, hLow, 3))); f.Frames != 3 {
		t.Errorf("a second stream: %d frames, want 3", f.Frames)
	}
	// A single frame that fills the file.
	if f = open(t, frames(t, h64, 1)); f.Frames != 1 {
		t.Errorf("one frame: %d frames", f.Frames)
	}
}

func TestHeaders(t *testing.T) {
	// Xing: the count is taken, and the header frame is not audio.
	data := cat(id3v2(20), frame(t, h128, 36, xing("Xing", 6, false, 0, 0)...), frames(t, h64, 6))
	f := open(t, data)
	if f.Frames != 6 || f.Counted || !f.VBR || f.BitRate != 64000 || f.DataOffset != 30+417 || f.DataSize != 6*208 {
		t.Errorf("Xing: %+v", *f)
	}
	// Info with LAME's delay and padding.
	f = open(t, cat(frame(t, h128, 36, xing("Info", 6, true, 576, 1080)...), frames(t, h128, 6)))
	if f.Frames != 6 || f.VBR || f.Delay != 576 || f.Padding != 1080 || f.Samples() != 6*1152-576-1080 {
		t.Errorf("Info: %+v samples %d", *f, f.Samples())
	}
	// The low rates in mono keep the header nine bytes after the frame header.
	f = open(t, cat(frame(t, hLow, 13, xing("Info", 4, true, 576, 100)...), frames(t, hLow, 4)))
	if f.Frames != 4 || f.SampleRate != 16000 || f.Channels != 1 || f.FrameSamples != 576 || f.Delay != 576 || f.Padding != 100 {
		t.Errorf("low rate: %+v", *f)
	}
	// VBRI.
	vbri := append([]byte("VBRI"), make([]byte, 14)...)
	binary.BigEndian.PutUint32(vbri[14:], 5)
	f = open(t, cat(frame(t, h128, 36, vbri...), frames(t, h128, 5)))
	if f.Frames != 5 || f.Counted || !f.VBR {
		t.Errorf("VBRI: %+v", *f)
	}
	// A count the file could not hold is not believed: the frames are counted.
	f = open(t, cat(frame(t, h128, 36, xing("Xing", 1<<30, false, 0, 0)...), frames(t, h128, 3)))
	if f.Frames != 3 || !f.Counted {
		t.Errorf("impossible count: %+v", *f)
	}
	// A header without a count.
	noCount := frame(t, h128, 36, []byte("Info\x00\x00\x00\x00")...)
	if f = open(t, cat(noCount, frames(t, h128, 3))); f.Frames != 3 || !f.Counted {
		t.Errorf("no count: %+v", *f)
	}
}

func TestLayerII(t *testing.T) {
	// MPEG-1 layer II, 48 kHz, 128 kbit/s: 384 bytes, 1152 samples.
	f := open(t, frames(t, []byte{0xff, 0xfd, 0x84, 0x00}, 4))
	if f.Codec != "mp2" || f.SampleRate != 48000 || f.Frames != 4 || f.FrameSamples != 1152 || f.DataSize != 4*384 {
		t.Errorf("%+v", *f)
	}
}

func TestNotMP3(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":            nil,
		"a tag alone":      id3v2(100),
		"a header alone":   append(append([]byte{}, h128...), make([]byte, 64)...),
		"free format":      bytes.Repeat([]byte{0xff, 0xfb, 0x00, 0x00}, 400),
		"text":             []byte("ID3 is how this sentence starts, and that is all"),
		"a tag past EOF":   id3v2(100)[:50],
		"sync words alone": bytes.Repeat([]byte{0xff, 0xfb}, 1000),
	} {
		if _, err := Open(context.Background(), bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrNotMP3) {
			t.Errorf("%s: %v, want ErrNotMP3", name, err)
		}
	}
	if !Sniff(h128) || !Sniff([]byte("ID3\x04")) || Sniff([]byte{0xff, 0xf1, 0x50, 0x80}) || Sniff([]byte("RIFF")) || Sniff(nil) {
		t.Error("Sniff")
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := frames(t, h128, 10)
	if _, err := Open(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(frames(f, h128, 3))
	f.Add(cat(id3v2(12), frame(f, h128, 36, xing("Xing", 2, true, 576, 600)...), frames(f, h64, 2), []byte("TAG"), make([]byte, 125)))
	f.Add(cat(frame(f, hLow, 13, xing("Info", 2, true, 576, 100)...), frames(f, hLow, 2)))
	vbri := append([]byte("VBRI"), make([]byte, 14)...)
	vbri[17] = 2
	f.Add(cat(frame(f, h128, 36, vbri...), frames(f, h128, 2)))
	f.Add(frames(f, []byte{0xff, 0xfd, 0x84, 0x00}, 2))
	f.Add(cat(frames(f, h128, 2), []byte{0xff, 0xfb, 0, 1, 0xff, 0xff, 0xff}, frames(f, h128, 1)))
	f.Fuzz(func(t *testing.T, data []byte) {
		size := int64(len(data))
		m, err := Open(context.Background(), bytes.NewReader(data), size)
		if err != nil {
			return
		}
		if m.Frames < 1 || m.Frames > size/minFrame+1 || m.SampleRate < 8000 || m.FrameSamples < 384 ||
			m.Channels < 1 || m.Channels > 2 || m.BitRate <= 0 || m.Samples() < 0 ||
			m.DataOffset < 0 || m.DataSize < 0 || m.DataOffset+m.DataSize > size {
			t.Fatalf("%+v", *m)
		}
	})
}
