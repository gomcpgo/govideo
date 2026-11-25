package videoprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func TestUnknownFormat(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("plain text, not media at all........")} {
		if _, err := Probe(context.Background(), bytes.NewReader(in), int64(len(in))); err == nil {
			t.Errorf("Probe(%q) succeeded", in)
		}
	}
}

// riffChunk builds a RIFF chunk whose payload is the concatenation of parts:
// strings and byte slices verbatim, ints as 32-bit and uint16s as 16-bit
// little-endian fields.
func riffChunk(id string, parts ...any) []byte {
	var p []byte
	for _, part := range parts {
		switch v := part.(type) {
		case string:
			p = append(p, v...)
		case []byte:
			p = append(p, v...)
		case int:
			p = binary.LittleEndian.AppendUint32(p, uint32(v))
		case uint16:
			p = binary.LittleEndian.AppendUint16(p, v)
		}
	}
	out := append(binary.LittleEndian.AppendUint32([]byte(id), uint32(len(p))), p...)
	if len(p)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

// aviSeed is a minimal AVI: 16x8 MJPEG at 25 fps (two frames, the second
// dropped) and 8-bit mono PCM at 8 kHz.
func aviSeed() []byte {
	pad := func(n int) []byte { return make([]byte, n) }
	hdrl := riffChunk("LIST", "hdrl",
		riffChunk("avih", 40000, pad(12), 2, 0, 2, 0, 16, 8, pad(16)),
		riffChunk("LIST", "strl",
			riffChunk("strh", "vids", "mjpg", pad(12), 1, 25, 0, 2, pad(20)),
			riffChunk("strf", 40, 16, 8, uint16(1), uint16(24), "MJPG", pad(20))),
		riffChunk("LIST", "strl",
			riffChunk("strh", "auds", pad(16), 1, 8000, 0, 640, 0, 0, 1, pad(8)),
			riffChunk("strf", uint16(1), uint16(1), 8000, 8000, uint16(1), uint16(8), uint16(0))))
	movi := riffChunk("LIST", "movi",
		riffChunk("00dc", []byte{0xFF, 0xD8, 9, 9, 0xFF, 0xD9}),
		riffChunk("01wb", pad(640)),
		riffChunk("00dc"))
	return riffChunk("RIFF", "AVI ", hdrl, movi)
}

func TestAVI(t *testing.T) {
	data := aviSeed()
	info, err := Probe(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "avi" || len(info.Streams) != 2 || info.Duration != 80*time.Millisecond {
		t.Fatalf("format %q, %d streams, duration %v", info.Format, len(info.Streams), info.Duration)
	}
	v, a := info.Streams[0], info.Streams[1]
	if v.Kind != Video || v.Codec != "mjpeg" || v.CodecTag != "MJPG" || v.Frames != 2 || v.Duration != 80*time.Millisecond ||
		v.Video.Width != 16 || v.Video.Height != 8 || v.Video.FrameRate != (Rational{25, 1}) || v.Video.BitDepth != 8 ||
		v.TimeBase != (Rational{1, 25}) {
		t.Errorf("video: %+v %+v", v, v.Video)
	}
	if a.Kind != Audio || a.Codec != "pcm_u8" || a.Frames != 640 || a.Duration != 80*time.Millisecond ||
		a.Audio.SampleRate != 8000 || a.Audio.Channels != 1 || a.Audio.BitDepth != 8 || a.Audio.ChannelLayout != "mono" ||
		a.BitRate != 64000 || a.TimeBase != (Rational{1, 8000}) {
		t.Errorf("audio: %+v %+v", a, a.Audio)
	}
	if _, err := info.JSON(); err != nil {
		t.Errorf("JSON: %v", err)
	}
}

// mp3Seed is a minimal MP3: an ID3v2 tag, an Info header that counts the
// frames and gives LAME's delay and padding (576 and 1728 samples), and four
// frames of MPEG-1 layer III at 44.1 kHz, 128 kbit/s (417 bytes each).
func mp3Seed() []byte {
	hdr := []byte{0xff, 0xfb, 0x90, 0x00}
	frame := func(payload []byte) []byte {
		f := make([]byte, 417)
		copy(f, hdr)
		copy(f[36:], payload)
		return f
	}
	info := append([]byte("Info\x00\x00\x00\x01\x00\x00\x00\x04LAME3.100"), make([]byte, 12)...)
	out := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x04\x00\x00\x00\x00"), frame(append(info, 0x24, 0x06, 0xc0))...)
	for i := 0; i < 4; i++ {
		out = append(out, frame(nil)...)
	}
	return out
}

func TestMP3(t *testing.T) {
	data := mp3Seed()
	info, err := Probe(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	// 4 frames of 1152 samples, less 576 + 1728: two frames' worth.
	want := 52244898 * time.Nanosecond // 2304 samples at 44.1 kHz
	if info.Format != "mp3" || len(info.Streams) != 1 || info.Duration != want || info.StartTime != 0 {
		t.Fatalf("format %q, %d streams, duration %v (want %v), start %v", info.Format, len(info.Streams), info.Duration, want, info.StartTime)
	}
	a := info.Streams[0]
	if a.Kind != Audio || a.Codec != "mp3" || a.Frames != 4 || a.Duration != want || a.BitRate != 128000 ||
		a.Audio.SampleRate != 44100 || a.Audio.Channels != 2 || a.Audio.ChannelLayout != "stereo" || a.TimeBase != (Rational{1, 44100}) {
		t.Errorf("audio: %+v %+v", a, a.Audio)
	}
	if _, err := info.JSON(); err != nil {
		t.Errorf("JSON: %v", err)
	}
}

// FuzzProbe feeds arbitrary bytes through container sniffing, parsing and
// JSON rendering. Seeds for each container come from the corpus directory
// and the container packages' own fuzz targets.
func FuzzProbe(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2\x00\x00\x00\x08moov"))
	f.Add([]byte("\x1a\x45\xdf\xa3\x84\x42\x82\x84webm"))
	f.Add([]byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x80\xbb\x00\x00\x00\xee\x02\x00\x04\x00\x10\x00data\x00\x00\x00\x00"))
	f.Add(aviSeed())
	f.Add(mp3Seed())
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := Probe(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if _, err := info.JSON(); err != nil {
			t.Fatalf("JSON: %v", err)
		}
	})
}
