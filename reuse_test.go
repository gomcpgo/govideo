package govideo_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// cardClip writes n frames of the test card as H.264, w×h, with a keyframe
// every ten frames so that cuts can start in different groups of pictures.
func cardClip(t *testing.T, path string, w, h, n int) {
	t.Helper()
	defer govideo.HookH264(func(c *oscodec.EncodeConfig) { c.KeyInterval = 10 }, nil)()
	tl := govideo.NewTimeline(govideo.Canvas{Width: w, Height: h, FrameRate: govideo.R(cardFPS, 1)})
	tl.AddGenerated(time.Duration(n)*time.Second/cardFPS, govideo.GenerateOptions{Pattern: govideo.PatternTestCard})
	if err := tl.Export(context.Background(), path, govideo.ExportOptions{Profile: govideo.ProfileCompatible}); err != nil {
		t.Fatalf("Export %s: %v", filepath.Base(path), err)
	}
}

// cut is a run of frames [from, to) of a test card file.
type cut struct {
	path     string
	from, to int
}

// exportCuts joins the cuts and checks that the output shows exactly their
// frames. It returns how many decoder sessions the export opened and how
// many clips went on with the session of an earlier clip.
func exportCuts(t *testing.T, cuts []cut) (sessions, reused int64) {
	t.Helper()
	at := func(frame int) time.Duration { return time.Duration(frame) * time.Second / cardFPS }
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	var want []int
	for _, c := range cuts {
		tl.AddClip(c.path, govideo.ClipOptions{Trim: govideo.Range(at(c.from), at(c.to))})
		for n := c.from; n < c.to; n++ {
			want = append(want, n)
		}
	}
	out := filepath.Join(t.TempDir(), "out.mov")
	var before oscodec.DecoderStats
	if err := retryOnVM(t, func() error {
		before = oscodec.VideoDecoderStats()
		return tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := oscodec.VideoDecoderStats()
	if after.Pictures != before.Pictures {
		t.Errorf("%d decoded pictures were not released", after.Pictures-before.Pictures)
	}
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != len(want) {
		t.Fatalf("%d frames, want %d", len(frames), len(want))
	}
	bad := 0
	for i, f := range frames {
		if got := counter(f); got != want[i] && bad < 5 {
			t.Errorf("output frame %d shows source frame %d, want %d", i, got, want[i])
			bad++
		}
	}
	return after.Sessions - before.Sessions, after.Reused - before.Reused
}

// TestDecoderReuse joins many cuts and checks the pictures and how many
// system decoder sessions it took. Clips of one format share a session; a
// session that cannot take a clip's format is set aside, not thrown away;
// and a clip entered at a picture that leaves the decoder's references in
// place gets a session of its own.
func TestDecoderReuse(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4"), filepath.Join(dir, "c.mp4")
	cardClip(t, a, cardW, cardH, 90)
	cardClip(t, b, cardW/2, cardH/2, 90)
	cardClip(t, c, 2*cardW, 2*cardH, 90)

	t.Run("one file", func(t *testing.T) {
		// Out of order, starting on keyframes and between them.
		cuts := []cut{{a, 60, 75}, {a, 13, 25}, {a, 40, 50}, {a, 0, 10}, {a, 85, 90}, {a, 13, 25}}
		sessions, reused := exportCuts(t, cuts)
		if sessions != 1 || reused != int64(len(cuts))-1 {
			t.Errorf("%d sessions and %d reuses for %d cuts of one file, want 1 and %d", sessions, reused, len(cuts), len(cuts)-1)
		}
	})
	t.Run("two formats in turn", func(t *testing.T) {
		cuts := []cut{{a, 0, 10}, {b, 23, 33}, {a, 30, 42}, {b, 50, 60}, {a, 5, 12}, {b, 0, 7}}
		sessions, reused := exportCuts(t, cuts)
		// One session if the system decoder changes picture size in place,
		// else one per format; no clip after the first of its kind opens one.
		if sessions > 2 || sessions+reused != int64(len(cuts)) {
			t.Errorf("%d sessions and %d reuses for %d cuts of two files", sessions, reused, len(cuts))
		}
	})
	t.Run("three formats in turn", func(t *testing.T) {
		// More formats than idle decoders are kept: correct, if not thrifty.
		cuts := []cut{{a, 0, 8}, {b, 23, 31}, {c, 44, 52}, {a, 61, 69}, {b, 3, 11}, {c, 15, 23}, {c, 70, 78}}
		sessions, reused := exportCuts(t, cuts)
		t.Logf("%d sessions and %d reuses for %d cuts of three files", sessions, reused, len(cuts))
		if reused < 1 {
			t.Error("two cuts of one file in a row did not share a session")
		}
	})
	t.Run("open GOP", func(t *testing.T) {
		// Keyframes that are not IDR pictures: after the first, each one
		// leaves the pictures before it in the decoder.
		src, in := filepath.Join(dir, "card.mov"), filepath.Join(dir, "opengop.mp4")
		exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
		if err := oracle.TryFFmpeg("-i", src,
			"-vf", "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt709",
			"-c:v", "libx264", "-preset", "medium", "-crf", "16", "-pix_fmt", "yuv420p",
			"-bf", "3", "-refs", "4", "-g", "20", "-x264-params", "open-gop=1:b-pyramid=strict:b-adapt=0:scenecut=0",
			"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv", "-an", in); err != nil {
			oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
		}
		cuts := []cut{{in, 51, 60}, {in, 5, 15}, {in, 70, 80}, {in, 45, 52}}
		sessions, reused := exportCuts(t, cuts)
		// Only the cut that starts in the first group of pictures begins on
		// an IDR picture.
		if sessions != 3 || reused != 1 {
			t.Errorf("%d sessions and %d reuses, want 3 and 1: a session went on from a keyframe that is not an IDR picture, or was not reused from one that is", sessions, reused)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		// An export that stops midway gives every picture back.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
		tl.AddClip(a, govideo.ClipOptions{})
		tl.AddClip(b, govideo.ClipOptions{})
		before := oscodec.VideoDecoderStats()
		err := retryOnVM(t, func() error {
			return tl.Export(ctx, filepath.Join(t.TempDir(), "out.mov"), govideo.ExportOptions{
				Profile: govideo.ProfileIntermediate,
				Progress: func(p govideo.Progress) {
					if p.Frame == 100 {
						cancel()
					}
				}})
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Export: %v, want a cancellation", err)
		}
		if out := oscodec.VideoDecoderStats().Pictures - before.Pictures; out != 0 {
			t.Errorf("%d decoded pictures were not released", out)
		}
	})
}
