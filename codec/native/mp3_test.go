package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gomcpgo/govideo/internal/container/mp3"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// mp3Fixture has ffmpeg's libmp3lame encode a test sound (musicLike: notes
// with sharp starts, which make an encoder switch to short blocks, and two
// channels that differ) and returns the file.
func mp3Fixture(t testing.TB, dir, name string, rate, channels int, seconds float64, args ...string) string {
	t.Helper()
	oracle.Require(t)
	pcm := render(int(seconds*float64(rate)), channels, musicLike(rate, 11))
	raw := make([]byte, 0, 2*len(pcm))
	for _, v := range pcm {
		raw = binary.LittleEndian.AppendUint16(raw, uint16(v))
	}
	in := filepath.Join(dir, name+".s16")
	if err := os.WriteFile(in, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	cmd := append([]string{"-f", "s16le", "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels), "-i", in, "-c:a", "libmp3lame"}, args...)
	if err := oracle.TryFFmpeg(append(cmd, out)...); err != nil {
		oracle.Missing(t, "ffmpeg cannot make %s: %v", name, err)
	}
	return out
}

// decodeMP3File decodes every frame of an MP3 file, from frame start on.
func decodeMP3File(t testing.TB, path string, start int64) (pcm []float32, f *mp3.File) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	st, _ := file.Stat()
	if f, err = mp3.Open(context.Background(), file, st.Size()); err != nil {
		t.Fatal(err)
	}
	rd := f.NewReader(file)
	if got := rd.Skip(start); got != start {
		t.Fatalf("%s: skipped %d frames of %d", path, got, start)
	}
	dec := NewMP3Decoder()
	for {
		frame, err := rd.Next()
		if err == io.EOF {
			return pcm, f
		}
		if err != nil {
			t.Fatal(err)
		}
		fr, err := dec.Decode(frame)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if fr.SampleRate != f.SampleRate || fr.Channels != f.Channels || fr.Frames() != f.FrameSamples {
			t.Fatalf("%s: a frame of %d samples, %d Hz, %d channels in a file of %d, %d, %d",
				path, fr.Frames(), fr.SampleRate, fr.Channels, f.FrameSamples, f.SampleRate, f.Channels)
		}
		pcm = append(pcm, fr.Samples...)
	}
}

// ffmpegPCM is ffmpeg's decode of a file, at its own rate and channels.
func ffmpegPCM(t testing.TB, path string) []float32 {
	t.Helper()
	raw := oracle.FFmpegOutput(t, "-i", path, "-c:a", "pcm_f32le", "-f", "f32le", "-")
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return out
}

// TestMP3AgainstFFmpeg decodes files of every sample rate and channel mode
// and compares the samples with ffmpeg's decode of them. ffmpeg starts a
// file that states its encoder's delay where the audio does, after that
// delay and the decoder's, and ends it before the padding; a file that
// states nothing it decodes from end to end.
func TestMP3AgainstFFmpeg(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name           string
		rate, channels int
		args           []string
	}{
		{"44k_joint_128k.mp3", 44100, 2, []string{"-b:a", "128k"}},
		{"44k_stereo_192k.mp3", 44100, 2, []string{"-b:a", "192k", "-joint_stereo", "0"}},
		{"48k_vbr.mp3", 48000, 2, []string{"-q:a", "4"}},
		{"48k_320k.mp3", 48000, 2, []string{"-b:a", "320k"}},
		{"32k_joint_64k.mp3", 32000, 2, []string{"-b:a", "64k"}},
		{"44k_mono_32k.mp3", 44100, 1, []string{"-b:a", "32k"}},
		{"44k_mono_noheader.mp3", 44100, 1, []string{"-b:a", "96k", "-write_xing", "0", "-id3v2_version", "0"}},
		{"22k_joint_48k.mp3", 22050, 2, []string{"-b:a", "48k"}},
		{"24k_mono_vbr.mp3", 24000, 1, []string{"-q:a", "6"}},
		{"16k_mono_24k.mp3", 16000, 1, []string{"-b:a", "24k"}},
		{"16k_stereo_16k.mp3", 16000, 2, []string{"-b:a", "16k"}},
		{"12k_mono_16k.mp3", 12000, 1, []string{"-b:a", "16k"}},
		{"11k_stereo_32k.mp3", 11025, 2, []string{"-b:a", "32k"}},
		{"8k_mono_8k.mp3", 8000, 1, []string{"-b:a", "8k"}},
		{"8k_stereo_vbr_noheader.mp3", 8000, 2, []string{"-q:a", "7", "-write_xing", "0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := mp3Fixture(t, dir, c.name, c.rate, c.channels, 4, c.args...)
			got, f := decodeMP3File(t, path, 0)
			want := ffmpegPCM(t, path)
			got = got[min(f.Lead()*f.Channels, len(got)):]
			if int64(len(want)) != f.Samples()*int64(f.Channels) || len(got) < len(want) {
				t.Fatalf("ffmpeg decoded %d samples and the decoder %d (after the delay); the file holds %d",
					len(want), len(got), f.Samples()*int64(f.Channels))
			}
			var worst, sum, power float64
			for i, w := range want {
				d := math.Abs(float64(got[i] - w))
				worst = max(worst, d)
				sum += d * d
				power += float64(w) * float64(w)
			}
			rms := math.Sqrt(sum / float64(len(want)))
			t.Logf("%d samples: largest difference %.2e, rms %.2e (signal rms %.2e)", len(want), worst, rms, math.Sqrt(power/float64(len(want))))
			if worst > 1e-5 || power == 0 {
				t.Errorf("differs from ffmpeg's decode by up to %.2e (rms %.2e)", worst, rms)
			}
		})
	}
}

// TestMP3Files compares the decoder with ffmpeg on MP3 files of the
// machine it runs on: GOVIDEO_MP3_FILES names a directory, searched for
// .mp3 files, or a text file that lists one path to a line. Files made by
// other encoders than the one the fixtures come from hold what those do not
// (intensity stereo, mixed blocks, checksums, damage).
//
//	GOVIDEO_MP3_FILES=~/Music go test ./codec/native -run TestMP3Files -v
//
// Two kinds of file are reported and not failed, because the difference is
// ffmpeg's choice and not an error (SPEC §14.1):
//
//   - Intensity stereo at the lower sample rates. ffmpeg takes every stereo
//     position for one; the standard, this decoder and the macOS system
//     decoder leave a band alone whose position is the largest its field
//     can hold. lame's decoder does as ffmpeg.
//   - A stream cut out of a longer one, whose first frames need audio data
//     that is not in the file. They are silent here; ffmpeg decodes them
//     with what it has. The first four frames are not compared.
func TestMP3Files(t *testing.T) {
	src := os.Getenv("GOVIDEO_MP3_FILES")
	if src == "" {
		t.Skip("GOVIDEO_MP3_FILES is not set")
	}
	oracle.Require(t)
	var files []string
	if st, err := os.Stat(src); err != nil {
		t.Fatal(err)
	} else if st.IsDir() {
		filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".mp3") {
				files = append(files, p)
			}
			return nil
		})
	} else {
		f, err := os.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		for sc := bufio.NewScanner(f); sc.Scan(); {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				files = append(files, line)
			}
		}
		f.Close()
	}

	type result struct {
		path, note string
		worst      float64
		frames     int64
		skipped    bool
		intensity  bool // intensity stereo at a low rate
	}
	// begin is a frame's main_data_begin: how far back its audio data starts.
	begin := func(frame []byte) int {
		p := 4
		if frame[1]&1 == 0 {
			p += 2
		}
		switch {
		case len(frame) < p+2:
			return 0
		case frame[1]&8 != 0: // MPEG-1
			return int(frame[p])<<1 | int(frame[p+1])>>7
		}
		return int(frame[p])
	}
	check := func(path string) (r result) {
		r.path = path
		file, err := os.Open(path)
		if err != nil {
			r.note, r.skipped = err.Error(), true
			return r
		}
		defer file.Close()
		st, _ := file.Stat()
		f, err := mp3.Open(context.Background(), file, st.Size())
		if err != nil || f.Codec != "mp3" {
			r.note, r.skipped = "not read as an MP3 file", true
			return r
		}
		rd := f.NewReader(file)
		dec := NewMP3Decoder()
		var got []float32
		from := 0
		for {
			frame, err := rd.Next()
			if err != nil {
				break
			}
			if r.frames == 0 && begin(frame) > 0 {
				from = (4*f.FrameSamples - f.Lead()) * f.Channels
			}
			if f.FrameSamples == 576 && frame[3]>>6 == 1 && frame[3]&0x10 != 0 {
				r.intensity = true
			}
			fr, err := dec.Decode(frame)
			if err != nil {
				r.note = "frame " + strconv.FormatInt(r.frames, 10) + ": " + err.Error()
				return r
			}
			if fr.Channels != f.Channels {
				r.note, r.skipped = "the number of channels changes", true
				return r
			}
			r.frames++
			got = append(got, fr.Samples...)
		}
		raw, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", path,
			"-map", "0:a:0", "-c:a", "pcm_f32le", "-f", "f32le", "-").Output()
		if err != nil {
			r.note, r.skipped = "ffmpeg cannot decode it", true
			return r
		}
		got = got[min(f.Lead()*f.Channels, len(got)):]
		n := min(len(raw)/4, len(got))
		if d := len(raw)/4 - int(f.Samples())*f.Channels; d != 0 {
			r.note = "ffmpeg decodes " + strconv.Itoa(d/f.Channels) + " more samples than the file is taken to hold"
		}
		for i := max(from, 0); i < n; i++ {
			w := math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
			r.worst = max(r.worst, math.Abs(float64(got[i]-w)))
		}
		return r
	}

	results := make([]result, len(files))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = check(files[i])
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()

	var same, differ, known, skipped int
	var frames int64
	for _, r := range results {
		frames += r.frames
		switch {
		case r.skipped:
			skipped++
			t.Logf("left out: %s: %s", r.path, r.note)
		case r.worst > 1e-5 && r.intensity:
			known++
			t.Logf("%s: intensity stereo at a low rate: differs from ffmpeg's decode by up to %.2e", r.path, r.worst)
		case r.worst > 1e-5 || strings.HasPrefix(r.note, "frame"):
			differ++
			t.Errorf("%s: differs from ffmpeg's decode by up to %.2e %s", r.path, r.worst, r.note)
		default:
			same++
			if r.note != "" {
				t.Logf("%s: %s", r.path, r.note)
			}
		}
	}
	t.Logf("%d files, %d frames: %d as ffmpeg decodes them, %d not, %d with intensity stereo, %d left out",
		len(files), frames, same, differ, known, skipped)
}

// BenchmarkMP3Decode decodes ten seconds of a stereo file at 44.1 kHz and
// 128 kbit/s.
func BenchmarkMP3Decode(b *testing.B) {
	path := mp3Fixture(b, b.TempDir(), "bench.mp3", 44100, 2, 10, "-b:a", "128k")
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	f, err := mp3.Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rd, dec := f.NewReader(bytes.NewReader(data)), NewMP3Decoder()
		for {
			frame, err := rd.Next()
			if err != nil {
				break
			}
			if _, err := dec.Decode(frame); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(10*float64(b.N)/b.Elapsed().Seconds(), "x-realtime")
}
