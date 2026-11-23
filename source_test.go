package govideo

import (
	"context"
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
		path := filepath.Join(dir, name)
		args := append(append([]string(nil), enc...), path)
		if name == "gop.webm" {
			// What browsers write: no Cues, unknown sizes.
			args = []string{"-f", "lavfi", "-i", "testsrc2=d=3:s=320x240:r=30",
				"-c:v", "libvpx", "-g", "15", "-deadline", "realtime", "-live", "1", path}
		}
		if err := oracle.TryFFmpeg(args...); err != nil {
			t.Logf("%s: the oracle cannot make this fixture: %v", name, err)
			continue
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
			t.Fatalf("%s: %v", name, err)
		}
		if !p.sync || p.pts > cut || p.pts < time.Second {
			t.Errorf("%s: reading starts at %v (sync %v), want the keyframe at 1.5s", name, p.pts, p.sync)
		}
		t.Logf("%s: reading starts at %v", name, p.pts)
		got[name] = p.pts
	}
	if len(got) == 0 {
		t.Skip("the oracle could make none of the fixtures")
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
