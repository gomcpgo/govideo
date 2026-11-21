package govideo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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
