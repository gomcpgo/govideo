package govideo_test

import (
	"context"
	"errors"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// These tests pin down behaviour that real camera and phone files exposed.

// TestMirroredDisplayMatrix covers clips whose display matrix reflects the
// picture (front cameras write them).
func TestMirroredDisplayMatrix(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 30, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	in := filepath.Join(dir, "flipped.mov")
	if err := oracle.TryFFmpeg("-display_hflip", "-i", src, "-c", "copy", in); err != nil {
		t.Skipf("the oracle cannot write a reflecting display matrix: %v", err)
	}
	info, err := videoprobe.ProbeFile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if v := info.Streams[0].Video; !v.Mirrored || v.Rotation != 0 {
		t.Fatalf("probe: rotation %d mirrored %v, want 0 true", v.Rotation, v.Mirrored)
	}
	out := filepath.Join(dir, "out.mov")
	transcode(t, in, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 30 {
		t.Fatalf("%d frames, want 30", len(frames))
	}
	// The bars now run right to left.
	for i := 0; i < 7; i++ {
		want := process.BarColor(6 - i)
		r, g, b := rgb(frames[0], (2*i+1)*cardW/14, cardH/10)
		if !near(r, int(want.R), 14) || !near(g, int(want.G), 14) || !near(b, int(want.B), 14) {
			t.Errorf("bar %d is rgb(%d,%d,%d), want the mirrored rgb(%d,%d,%d)", i, r, g, b, want.R, want.G, want.B)
		}
	}
}

// TestTruncatedFile covers a file cut short (an interrupted copy or
// download): what is there plays, and the export does not fail.
func TestTruncatedFile(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 60, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mov")
	if err := os.WriteFile(cut, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mov")
	transcode(t, cut, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 60 {
		t.Fatalf("%d frames, want 60 (the index still describes the whole clip)", len(frames))
	}
	lastGood := -1
	for i, f := range frames {
		n := counter(f)
		switch {
		case n == i && lastGood == i-1:
			lastGood = i
		case n != lastGood:
			t.Fatalf("output frame %d shows source frame %d; want it to hold frame %d", i, n, lastGood)
		}
	}
	if lastGood < 15 || lastGood > 45 {
		t.Errorf("playback held from frame %d; about half of 60 frames survive the cut", lastGood)
	}
}

// TestMultichannelAACOrder checks that 5.1 AAC, which codes the centre
// channel first, is downmixed with left on the left.
func TestMultichannelAACOrder(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	for _, c := range []struct {
		channel     string
		left, right float64 // expected share of the tone in each output channel
	}{{"FL", 1, 0}, {"FR", 0, 1}, {"FC", 1, 1}, {"BL", 1, 0}, {"BR", 0, 1}, {"LFE", 0, 0}} {
		t.Run(c.channel, func(t *testing.T) {
			in := filepath.Join(dir, c.channel+".m4a")
			if err := oracle.TryFFmpeg("-f", "lavfi", "-i", "sine=f=440:d=2:r=48000", "-af", "pan=5.1|"+c.channel+"=c0",
				"-c:a", "aac", "-b:a", "256k", in); err != nil {
				t.Skipf("the oracle cannot make 5.1 AAC: %v", err)
			}
			tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180, FrameRate: govideo.R(30, 1)})
			tl.AddGenerated(2*time.Second, govideo.GenerateOptions{})
			tl.AddAudio(in, govideo.AudioOptions{})
			out := filepath.Join(dir, c.channel+".mov")
			if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
				t.Fatal(err)
			}
			l := oracle.Audio(t, out, "0:a:0", "-af", "pan=mono|c0=c0")
			r := oracle.Audio(t, out, "0:a:0", "-af", "pan=mono|c0=c1")
			level := func(s []float32) float64 {
				var sum float64
				for _, v := range s[9600:86400] {
					sum += float64(v) * float64(v)
				}
				return math.Sqrt(sum / 76800)
			}
			gl, gr := level(l), level(r)
			if (gl > 0.005) != (c.left > 0) || (gr > 0.005) != (c.right > 0) {
				t.Errorf("tone in %s came out at left %.4f, right %.4f", c.channel, gl, gr)
			}
		})
	}
}

// TestWebPRange checks that lossy WebP, which stores limited-range samples,
// keeps its black and white.
func TestWebPRange(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	png := filepath.Join(dir, "in.png")
	writePNG(t, png, 64, 64, color.White)
	webp := filepath.Join(dir, "white.webp")
	if err := oracle.TryFFmpeg("-i", png, "-c:v", "libwebp", "-lossless", "0", "-quality", "95", webp); err != nil {
		t.Skipf("the oracle has no WebP encoder: %v", err)
	}
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	tl.AddImage(webp, time.Second/2, govideo.ImageOptions{})
	out := filepath.Join(dir, "out.mov")
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatal(err)
	}
	r, g, b := rgb(oracle.Frames(t, out, cardW, cardH)[0], cardW/2, cardH/2)
	if r < 247 || g < 247 || b < 247 {
		t.Errorf("white WebP came out rgb(%d,%d,%d)", r, g, b)
	}
}

// TestUnsupportedInputErrors checks that inputs govideo cannot read are
// refused with an error that says what they are.
func TestUnsupportedInputErrors(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	export := func(add func(*govideo.Timeline)) error {
		tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180})
		add(tl)
		return tl.Export(context.Background(), filepath.Join(dir, "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	}
	var cu *govideo.ErrCodecUnavailable
	var uf *videoprobe.UnsupportedFormatError

	flac := filepath.Join(dir, "a.flac")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "sine=f=440:d=1", "-c:a", "flac", flac)
	if err := export(func(tl *govideo.Timeline) { tl.AddClip(flac, govideo.ClipOptions{}) }); !errors.As(err, &cu) || cu.Codec != "flac" {
		t.Errorf("FLAC file: %v, want ErrCodecUnavailable for flac", err)
	}
	ts := filepath.Join(dir, "a.ts")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=160x120:r=25", "-c:v", "mpeg2video", "-f", "mpegts", ts)
	if err := export(func(tl *govideo.Timeline) { tl.AddClip(ts, govideo.ClipOptions{}) }); !errors.As(err, &uf) || uf.Format != "mpegts" {
		t.Errorf("transport stream: %v, want UnsupportedFormatError for mpegts", err)
	}
	heic := filepath.Join(dir, "a.heic")
	if err := os.WriteFile(heic, []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic\x00\x00\x00\x08free"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := export(func(tl *govideo.Timeline) { tl.AddImage(heic, time.Second, govideo.ImageOptions{}) }); !errors.As(err, &cu) || cu.Codec != "heic" {
		t.Errorf("HEIC still: %v, want ErrCodecUnavailable for heic", err)
	}
}
