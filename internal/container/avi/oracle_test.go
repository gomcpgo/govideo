package avi_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gomcpgo/govideo/internal/container/avi"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// These tests use the locally installed ffmpeg/ffprobe binaries as black-box
// oracles (SPEC §11). Fixtures are generated at test time, never committed.

func TestOracle(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	video := []string{"-f", "lavfi", "-i", "testsrc2=d=2:s=320x240:r=25"}
	cases := []struct {
		name     string
		args     []string
		tag      uint16
		bits     int
		channels int
		rate     int
	}{
		{"mjpeg_s16.avi", []string{"-f", "lavfi", "-i", "sine=f=440:d=2:r=48000", "-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_s16le", "-ac", "2"}, avi.FormatPCM, 16, 2, 48000},
		{"mjpeg_u8.avi", []string{"-f", "lavfi", "-i", "sine=f=440:d=2:r=11025", "-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_u8", "-ac", "1"}, avi.FormatPCM, 8, 1, 11025},
		{"mjpeg_ntsc_f32.avi", []string{"-f", "lavfi", "-i", "sine=f=440:d=2:r=44100", "-r", "30000/1001", "-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_f32le"}, avi.FormatIEEEFloat, 32, 1, 44100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			oracle.FFmpeg(t, append(append(append([]string{}, video...), c.args...), path)...)
			want := oracle.FFprobe(t, path, "-count_packets")
			fh, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer fh.Close()
			st, _ := fh.Stat()
			f, err := avi.Open(context.Background(), fh, st.Size())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if f.Truncated || f.IndexCapped {
				t.Errorf("truncated %v, capped %v", f.Truncated, f.IndexCapped)
			}
			if len(f.Streams) != 2 || len(want.Streams) != 2 {
				t.Fatalf("%d streams, ffprobe %d", len(f.Streams), len(want.Streams))
			}
			v, a := f.Video(), f.Audio()
			if v == nil || a == nil || v.Index != 0 || a.Index != 1 {
				t.Fatal("expected video then audio")
			}
			wv, wa := want.Streams[0], want.Streams[1]

			if v.FourCC() != wv.CodecTag {
				t.Errorf("fourcc %q, ffprobe %q", v.FourCC(), wv.CodecTag)
			}
			if v.Width != wv.Width || v.Height != wv.Height {
				t.Errorf("size %dx%d, ffprobe %dx%d", v.Width, v.Height, wv.Width, wv.Height)
			}
			n, _ := strconv.ParseInt(wv.NbReadPackets, 10, 64)
			if v.Frames() != n || f.Frames() != n || n == 0 {
				t.Errorf("frames %d, ffprobe %d", v.Frames(), n)
			}
			num, den := v.FrameRate()
			if rate := oracle.Ratio(wv.RFrameRate); den == 0 || math.Abs(float64(num)/float64(den)-rate) > rate*1e-4 {
				t.Errorf("frame rate %d/%d, ffprobe %s", num, den, wv.RFrameRate)
			}
			var buf []byte
			var total int64
			for i, c := range v.Chunks {
				b, err := f.ReadChunk(c, buf)
				if err != nil {
					t.Fatalf("ReadChunk %d: %v", i, err)
				}
				if !bytes.HasPrefix(b, []byte{0xFF, 0xD8}) || !bytes.HasSuffix(b, []byte{0xFF, 0xD9}) {
					t.Fatalf("frame %d (%d bytes) is not a whole JPEG", i, len(b))
				}
				total += int64(len(b))
				buf = b[:0]
			}
			if total != v.TotalBytes {
				t.Errorf("video bytes %d, TotalBytes %d", total, v.TotalBytes)
			}

			rate, _ := strconv.Atoi(wa.SampleRate)
			if a.FormatTag != c.tag || a.BitsPerSample != c.bits || a.Channels != c.channels || a.SampleRate != c.rate ||
				a.SampleRate != rate || a.Channels != wa.Channels || a.BitsPerSample != wa.BitsPerSample || !a.IsPCM() {
				t.Errorf("audio %+v, ffprobe rate %s channels %d bits %d", a, wa.SampleRate, wa.Channels, wa.BitsPerSample)
			}
			if a.BlockAlign != c.channels*c.bits/8 || a.AvgBytesPerSec != a.BlockAlign*c.rate {
				t.Errorf("block align %d, bytes/s %d", a.BlockAlign, a.AvgBytesPerSec)
			}
			// Two seconds of audio, to within one packet of muxer slack.
			if got := float64(a.Frames()) / float64(c.rate); math.Abs(got-2) > 0.05 {
				t.Errorf("audio length %.4fs, want 2s", got)
			}
			if frames, err := strconv.ParseInt(wa.NbFrames, 10, 64); err == nil && frames != a.Frames() {
				t.Errorf("audio sample frames %d, ffprobe %d", a.Frames(), frames)
			}
			if pk, _ := strconv.Atoi(wa.NbReadPackets); pk != len(a.Chunks) {
				t.Errorf("audio chunks %d, ffprobe packets %d", len(a.Chunks), pk)
			}

			d, ok := oracle.Seconds(want.Format.Duration)
			if !ok || math.Abs(f.Duration.Seconds()-d) > 0.04 {
				t.Errorf("duration %v, ffprobe %s", f.Duration, want.Format.Duration)
			}
			if d, ok := oracle.Seconds(wv.Duration); ok && math.Abs(v.Duration().Seconds()-d) > 1e-3 {
				t.Errorf("video duration %v, ffprobe %s", v.Duration(), wv.Duration)
			}
		})
	}
}
