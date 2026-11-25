package govideo_test

import (
	"context"
	"errors"
	"image"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
)

// rgb24 packs the picture Frame returned as the oracle packs its frames.
func rgb24(t *testing.T, img image.Image) []byte {
	t.Helper()
	m, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("Frame returned a %T, want *image.RGBA", img)
	}
	w, h := m.Rect.Dx(), m.Rect.Dy()
	out := make([]byte, 0, w*h*3)
	for y := 0; y < h; y++ {
		row := m.Pix[y*m.Stride:]
		for x := 0; x < w; x++ {
			out = append(out, row[x*4], row[x*4+1], row[x*4+2])
		}
	}
	return out
}

// frameAt returns the picture of path at a time, through the retry that
// tests of the system decoder take on a virtual machine.
func frameAt(t *testing.T, path string, o govideo.FrameOptions) image.Image {
	t.Helper()
	var img image.Image
	if err := retryOnVM(t, func() (err error) {
		img, err = govideo.Frame(context.Background(), path, o)
		return err
	}); err != nil {
		t.Fatalf("Frame at %v: %v", o.At, err)
	}
	return img
}

// checkFrames asks a test card file of n frames for pictures at times in
// and between its frames, and reads from each the number of the frame it
// shows.
func checkFrames(t *testing.T, path string, n int) {
	t.Helper()
	frame := time.Second / cardFPS
	for _, c := range []struct {
		at   time.Duration
		want int
	}{
		{0, 0},
		{frame - time.Millisecond, 0}, // still the first frame
		{frame + time.Millisecond, 1},
		{time.Second, 30},
		{47*frame + frame/2, 47}, // the middle of a frame, far from a sync sample
		{time.Duration(n-1)*frame + time.Millisecond, n - 1},
		{time.Hour, n - 1}, // past the end: the last frame
		{10 * frame, 10},   // and back: every call starts afresh
	} {
		img := frameAt(t, path, govideo.FrameOptions{At: c.at})
		if b := img.Bounds(); b.Dx() != cardW || b.Dy() != cardH {
			t.Fatalf("at %v: %dx%d, want %dx%d", c.at, b.Dx(), b.Dy(), cardW, cardH)
		}
		pix := rgb24(t, img)
		if got := counter(pix); got != c.want {
			t.Errorf("at %v: frame %d, want %d", c.at, got, c.want)
		}
		for i := 0; i < 7; i++ {
			want := process.BarColor(i)
			r, g, b := rgb(pix, (2*i+1)*cardW/14, cardH/10)
			if !near(r, int(want.R), 12) || !near(g, int(want.G), 12) || !near(b, int(want.B), 12) {
				t.Errorf("at %v: bar %d is rgb(%d,%d,%d), want rgb(%d,%d,%d)", c.at, i, r, g, b, want.R, want.G, want.B)
			}
		}
	}
	// Sizes: one side given, both given (the picture fits inside), larger.
	for _, c := range []struct{ w, h, wantW, wantH int }{
		{320, 0, 320, 180}, {0, 90, 160, 90}, {200, 200, 200, 113}, {400, 100, 178, 100}, {1280, 0, 1280, 720}, {1, 1, 1, 1},
	} {
		img := frameAt(t, path, govideo.FrameOptions{At: time.Second, Width: c.w, Height: c.h})
		if b := img.Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("asked for %dx%d: got %dx%d, want %dx%d", c.w, c.h, b.Dx(), b.Dy(), c.wantW, c.wantH)
		}
	}
	// A reduced picture is the same picture: the top left bar is still grey
	// and the bottom right cell of the counter reads as it did.
	small := rgb24(t, frameAt(t, path, govideo.FrameOptions{At: time.Second, Width: 320}))
	want := process.BarColor(0)
	if r, g, b := int(small[(18*320+22)*3]), int(small[(18*320+22)*3+1]), int(small[(18*320+22)*3+2]); !near(r, int(want.R), 12) || !near(g, int(want.G), 12) || !near(b, int(want.B), 12) {
		t.Errorf("reduced: first bar is rgb(%d,%d,%d), want rgb(%d,%d,%d)", r, g, b, want.R, want.G, want.B)
	}
}

// TestFrame takes single pictures from a Motion JPEG file, which every
// platform decodes.
func TestFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "card.mov")
	exportCard(t, path, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	checkFrames(t, path, 90)
}

// TestFrameH264 does the same through the system decoder, where a picture
// in the middle of a group is reached from the sync sample before it, with
// and without reordered frames, and no decoded picture may be left behind.
func TestFrameH264(t *testing.T) {
	requireOS(t)
	dir := t.TempDir()
	before := oscodec.VideoDecoderStats()
	for _, reorder := range []bool{false, true} {
		t.Run("reorder="+strconv.FormatBool(reorder), func(t *testing.T) {
			defer govideo.HookH264(func(c *oscodec.EncodeConfig) { c.Reorder = reorder; c.KeyInterval = 40 }, nil)()
			path := filepath.Join(dir, "card"+strconv.FormatBool(reorder)+".mp4")
			exportCard(t, path, 90, govideo.ExportOptions{Profile: govideo.ProfileCompatible})
			checkFrames(t, path, 90)
		})
	}
	if after := oscodec.VideoDecoderStats(); after.Pictures != before.Pictures {
		t.Errorf("%d decoded pictures were not released", after.Pictures-before.Pictures)
	}
}

// TestFrameOrientation checks against the oracle that the picture comes out
// upright from a file that is stored turned, and with square pixels from
// one whose pixels are not.
func TestFrameOrientation(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=2:s=320x240:r=25", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "12", base)
	for _, deg := range []int{0, 90, 180, 270} {
		path := filepath.Join(dir, "rot"+strconv.Itoa(deg)+".mp4")
		oracle.FFmpeg(t, "-display_rotation", strconv.Itoa(deg), "-i", base, "-c", "copy", path)
		w, h := 320, 240
		if deg%180 != 0 {
			w, h = h, w
		}
		// The oracle turns the frames as a player does.
		want := oracle.Frames(t, path, w, h)
		for _, n := range []int{0, 17, 49} {
			img := frameAt(t, path, govideo.FrameOptions{At: time.Duration(n)*time.Second/25 + 5*time.Millisecond})
			if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
				t.Fatalf("rotation %d: %dx%d, want %dx%d", deg, b.Dx(), b.Dy(), w, h)
			}
			got := rgb24(t, img)
			var sum float64
			for i := range got {
				d := float64(got[i]) - float64(want[n][i])
				sum += d * d
			}
			if psnr := 10 * math.Log10(255*255/(sum/float64(len(got)))); psnr < 30 {
				t.Errorf("rotation %d, frame %d: %.1f dB against the oracle's frame, want 30 or more", deg, n, psnr)
			}
			// The frames either side are a different picture: the right one was chosen.
			for _, other := range []int{n - 1, n + 1} {
				if other < 0 || other >= len(want) {
					continue
				}
				var osum float64
				for i := range got {
					d := float64(got[i]) - float64(want[other][i])
					osum += d * d
				}
				if osum <= sum {
					t.Errorf("rotation %d, frame %d: the picture is nearer the oracle's frame %d", deg, n, other)
				}
			}
		}
	}
	// 320x240 stored, pixels 4:3 wide: shown at 427x240.
	wide := filepath.Join(dir, "wide.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=320x240:r=25", "-vf", "setsar=4/3", "-c:v", "libx264", "-pix_fmt", "yuv420p", wide)
	for _, c := range []struct{ w, h, wantW, wantH int }{{0, 0, 427, 240}, {0, 120, 213, 120}, {100, 0, 100, 56}} {
		img := frameAt(t, wide, govideo.FrameOptions{Width: c.w, Height: c.h})
		if b := img.Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("wide pixels, asked for %dx%d: got %dx%d, want %dx%d", c.w, c.h, b.Dx(), b.Dy(), c.wantW, c.wantH)
		}
	}
}

// TestFrameErrors checks what Frame refuses, and how.
func TestFrameErrors(t *testing.T) {
	dir := t.TempDir()
	card := filepath.Join(dir, "card.mov")
	exportCard(t, card, 5, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	ctx := context.Background()
	for name, o := range map[string]govideo.FrameOptions{
		"a negative time": {At: -time.Second},
		"a negative size": {Width: -1},
		"too large":       {Height: 1 << 20},
	} {
		if _, err := govideo.Frame(ctx, card, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := govideo.Frame(ctx, filepath.Join(dir, "missing.mp4"), govideo.FrameOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := govideo.Frame(cancelled, card, govideo.FrameOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// A file with sound and no picture: 100 samples of 16-bit mono PCM.
	wav := filepath.Join(dir, "a.wav")
	hdr := "RIFF\xec\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\xbb\x00\x00\x00\x77\x01\x00\x02\x00\x10\x00data\xc8\x00\x00\x00"
	if err := os.WriteFile(wav, append([]byte(hdr), make([]byte, 200)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := govideo.Frame(ctx, wav, govideo.FrameOptions{}); err == nil || !contains(err.Error(), "no video stream") {
		t.Errorf("audio file: %v, want an error that says there is no video stream", err)
	}
	// Not a media file at all.
	txt := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(txt, []byte("not a video, whatever the name says"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := govideo.Frame(ctx, txt, govideo.FrameOptions{}); err == nil {
		t.Error("text file: no error")
	}
}

// TestFrameCodecUnavailable checks that a video this host has no decoder
// for is refused as a missing codec.
func TestFrameCodecUnavailable(t *testing.T) {
	if govideo.BuildInfo().OSBackend {
		t.Skip("this platform decodes H.264")
	}
	oracle.Require(t)
	path := filepath.Join(t.TempDir(), "h264.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=320x240:r=25", "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	_, err := govideo.Frame(context.Background(), path, govideo.FrameOptions{})
	var cu *govideo.ErrCodecUnavailable
	if !errors.As(err, &cu) || cu.Codec != "h264" || cu.Op != govideo.Decode {
		t.Errorf("got %v, want ErrCodecUnavailable for h264 decode", err)
	}
}
