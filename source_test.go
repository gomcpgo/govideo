package govideo

import (
	"context"
	"math"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/videoprobe"
)

// TestSeekSync checks that a trim starts decoding at the keyframe before the
// cut, not at the start of the file, and that Matroska and MP4 agree on which
// keyframe that is. The pictures themselves are checked by TestDecodeOS.
func TestSeekSync(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	// Keyframes every half second, B-frames in between.
	enc := []string{"-f", "lavfi", "-i", "testsrc2=d=3:s=320x240:r=30",
		"-c:v", "libx264", "-preset", "veryfast", "-g", "15", "-bf", "3", "-pix_fmt", "yuv420p"}
	const cut = 1700 * time.Millisecond
	got := map[string]time.Duration{}
	for _, name := range []string{"gop.mp4", "gop.mkv", "gop.webm"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			args := append(append([]string(nil), enc...), path)
			if name == "gop.webm" {
				// What browsers write: no Cues, unknown sizes.
				args = []string{"-f", "lavfi", "-i", "testsrc2=d=3:s=320x240:r=30",
					"-c:v", "libvpx", "-g", "15", "-deadline", "realtime", "-live", "1", path}
			}
			if err := oracle.TryFFmpeg(args...); err != nil {
				oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
			}
			m, err := openMedia(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer m.close()
			rd, err := m.reader(context.Background(), m.stream(videoprobe.Video))
			if err != nil {
				t.Fatal(err)
			}
			rd.(syncSeeker).seekSync(cut)
			p, err := rd.next()
			if err != nil {
				t.Fatal(err)
			}
			if !p.sync || p.pts > cut || p.pts < time.Second {
				t.Errorf("reading starts at %v (sync %v), want the keyframe at 1.5s", p.pts, p.sync)
			}
			t.Logf("reading starts at %v", p.pts)
			got[name] = p.pts
		})
	}
	if a, b := got["gop.mp4"], got["gop.mkv"]; a != 0 && b != 0 && (a-b > time.Millisecond || b-a > time.Millisecond) {
		t.Errorf("MP4 seeks to %v, Matroska to %v", a, b)
	}
}

// TestVideoStreamSteadyState checks that showing frame after frame of an
// inter-frame stream allocates nothing of the size of a picture: decoded
// pictures stay in the decoder and are copied into frames the stream reuses.
func TestVideoStreamSteadyState(t *testing.T) {
	if !BuildInfo().OSBackend {
		t.Skip("no operating-system codec backend on this platform")
	}
	const w, h, fps, count = 1280, 720, 30, 60
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "card.mp4")
	tl := NewTimeline(Canvas{Width: w, Height: h, FrameRate: R(fps, 1)})
	tl.AddGenerated(count*time.Second/fps, GenerateOptions{Pattern: PatternTestCard})
	if err := tl.Export(ctx, path, ExportOptions{Profile: ProfileCompatible}); err != nil {
		t.Fatal(err)
	}
	m, err := openMedia(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	decs := &videoDecoders{}
	defer decs.close()
	before := oscodec.VideoDecoderStats().Pictures
	v, err := openVideo(ctx, m, m.stream(videoprobe.Video), 0, 0, DeinterlaceAuto, decs)
	if err != nil {
		t.Fatal(err)
	}
	defer v.close()
	show := func(from, to int) {
		for n := from; n < to; n++ {
			f, seq, err := v.frameAt(time.Duration(n) * time.Second / fps)
			if err != nil {
				t.Fatal(err)
			}
			if seq != int64(n)+1 || f.Width != w || f.Height != h {
				t.Fatalf("frame %d: sequence %d, %dx%d", n, seq, f.Width, f.Height)
			}
		}
	}
	show(0, 20) // the look-ahead fills and the frames are allocated
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	show(20, count)
	runtime.ReadMemStats(&m1)
	perFrame := (m1.TotalAlloc - m0.TotalAlloc) / (count - 20)
	t.Logf("%d bytes allocated per frame shown; a frame is %d bytes", perFrame, w*h*3/2)
	if perFrame > w*h*3/2/8 {
		t.Errorf("%d bytes allocated per frame shown: pictures are not being reused", perFrame)
	}
	// The decoder holds no more pictures than the stream is looking at.
	if out := oscodec.VideoDecoderStats().Pictures - before; out > 4 {
		t.Errorf("%d pictures held at the end of the stream", out)
	}
	v.close()
	if out := oscodec.VideoDecoderStats().Pictures - before; out != 0 {
		t.Errorf("%d pictures not released by close", out)
	}
	if len(decs.idle) != 1 || len(decs.frames) != 1 {
		t.Errorf("close left %d decoders and %d frames for the next clip, want 1 and 1", len(decs.idle), len(decs.frames))
	}
}

// TestAudioSeek checks that a stream opened partway through its source
// gives the samples a stream read from the start gives there, and gets them
// without decoding what comes before. The signal is a rising tone in each
// channel, so a shift of a sample shows; the Matroska file's timestamps are
// whole milliseconds, which no packet of 1024 samples starts on.
func TestAudioSeek(t *testing.T) {
	oracle.Require(t)
	ctx := context.Background()
	dir := t.TempDir()
	const length = 20
	tone := []string{"-f", "lavfi", "-i", "aevalsrc=0.4*sin(2*PI*(200+60*t)*t)|0.4*sin(2*PI*(300+45*t)*t):d=20:s=44100"}
	picture := []string{"-f", "lavfi", "-i", "testsrc2=d=20:s=160x120:r=10"}
	// Noise substitution is left out of the AAC: the noise a decoder makes
	// up depends on everything it has decoded.
	aac := []string{"-c:a", "aac", "-aac_pns", "0", "-b:a", "128k"}
	for _, c := range []struct {
		name string
		args []string
		os   bool // needs the system decoder
	}{
		{"lc.m4a", append(tone, aac...), true},
		{"lc.mkv", append(tone, aac...), true},
		{"lc48.mp4", append(append(picture, tone...), append(aac, "-ar", "48000", "-c:v", "mjpeg")...), true},
		{"lc22.m4a", append(tone, append(aac, "-ar", "22050", "-b:a", "64k")...), true},
		{"he.m4a", append(tone, "-c:a", "aac_at", "-profile:a", "4", "-b:a", "64k"), true},
		{"he.mkv", append(tone, "-c:a", "aac_at", "-profile:a", "4", "-b:a", "64k"), true},
		{"pcm.wav", tone, false},
		{"pcm.mov", append(tone, "-c:a", "pcm_s16le"), false},
		{"pcm48.mov", append(tone, "-c:a", "pcm_s24be", "-ar", "48000"), false},
		{"pcm.avi", append(append(picture, tone...), "-c:v", "mjpeg", "-c:a", "pcm_s16le"), false},
		// MP3 frames keep their audio data in the frames before them, the
		// more the lower the bit rate: the run-in has to reach that far.
		{"cbr.mp3", append(tone, "-c:a", "libmp3lame", "-b:a", "128k"), false},
		{"vbr.mp3", append(tone, "-c:a", "libmp3lame", "-q:a", "6"), false},
		{"noheader.mp3", append(tone, "-c:a", "libmp3lame", "-b:a", "48k", "-ac", "1", "-write_xing", "0"), false},
		{"mono24.mp3", append(tone, "-c:a", "libmp3lame", "-b:a", "16k", "-ac", "1", "-ar", "24000"), false},
		{"stereo22.mp3", append(tone, "-c:a", "libmp3lame", "-b:a", "32k", "-ar", "22050"), false},
		{"mp3.mp4", append(tone, "-c:a", "libmp3lame", "-b:a", "64k"), false},
		{"mp3.mkv", append(tone, "-c:a", "libmp3lame", "-b:a", "64k"), false},
		{"mp3.mov", append(append(picture, tone...), "-c:v", "mjpeg", "-c:a", "libmp3lame", "-b:a", "96k"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.os && !BuildInfo().OSBackend {
				t.Skip("no operating-system codec backend on this platform")
			}
			path := filepath.Join(dir, c.name)
			if err := oracle.TryFFmpeg(append(append([]string(nil), c.args...), path)...); err != nil {
				oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
			}
			m, err := openMedia(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer m.close()
			as := m.stream(videoprobe.Audio)
			// read returns n sample frames from the place at into the stream,
			// reached by cutting there or by reading up to it.
			read := func(cut, at time.Duration, n int) ([]float32, *audioStream) {
				a, err := openAudio(ctx, m, as, cut)
				if err != nil {
					t.Fatal(err)
				}
				defer a.close()
				buf := make([]float32, 2*(int(durToFrames(at-cut))+n))
				got, err := a.read(buf)
				if err != nil {
					t.Fatal(err)
				}
				return buf[2*(got-min(got, n)) : 2*got], a
			}
			const n = outRate / 2
			for _, cut := range []time.Duration{50 * time.Millisecond, 3210 * time.Millisecond, 11 * time.Second, 17537100 * time.Microsecond} {
				whole, all := read(0, cut, n)
				part, a := read(cut, cut, n)
				if len(whole) != 2*n || len(part) != 2*n {
					t.Fatalf("cut at %v: %d samples read from the start, %d from the cut, want %d", cut, len(whole)/2, len(part)/2, n)
				}
				worst, at := 0.0, 0
				for i := range whole {
					if d := math.Abs(float64(whole[i] - part[i])); d > worst {
						worst, at = d, i/2
					}
				}
				if worst > 1e-4 {
					t.Errorf("cut at %v: sample %d differs by %.5f from the stream read from the start", cut, at, worst)
				}
				// A cut well into the file must not cost what the file before
				// it does. (Matroska audio starts at a Cluster, up to five
				// seconds back in what the oracle writes.)
				if cut >= 10*time.Second {
					if !a.seeked || a.decoded*3 > all.decoded {
						t.Errorf("cut at %v: %d packets decoded (seeked: %v); %d when read from the start", cut, a.decoded, a.seeked, all.decoded)
					}
				}
				t.Logf("cut at %v: %d packets decoded, %d from the start; largest difference %.1e", cut, a.decoded, all.decoded, worst)
			}
		})
	}
}
