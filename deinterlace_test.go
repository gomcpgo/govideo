package govideo_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// The deinterlacing tests interlace the test card: sixty card frames at 50
// per second become the sixty fields of thirty frames at 25 per second, so
// every field carries its own number and an export at the field rate must
// show them all, in order.

const cardFields = 60

// toVideoRange converts the intermediate profile's full-range BT.601
// pictures to what an H.264 camera stream carries.
const toVideoRange = "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt709,"

var videoColour = []string{"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv"}

// fieldCard writes the 50 fps test card the fixtures are interlaced from.
func fieldCard(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "card50.mov")
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(50, 1)})
	tl.AddGenerated(cardFields*time.Second/50, govideo.GenerateOptions{Pattern: govideo.PatternTestCard})
	if err := tl.Export(context.Background(), path, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatalf("Export %s: %v", filepath.Base(path), err)
	}
	return path
}

// exportClip exports one clip at the card's size and the given frame rate
// and returns the oracle's decode of the result.
func exportClip(t *testing.T, in string, fps int64, o govideo.ClipOptions) [][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.mov")
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(fps, 1)})
	tl.AddClip(in, o)
	if err := retryOnVM(t, func() error {
		return tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	}); err != nil {
		t.Fatalf("Export from %s: %v", filepath.Base(in), err)
	}
	return oracle.Frames(t, out, cardW, cardH)
}

// checkFields verifies that the frames show the given card numbers.
func checkFields(t *testing.T, frames [][]byte, want func(i int) int, count int) {
	t.Helper()
	if len(frames) != count {
		t.Errorf("%d frames, want %d", len(frames), count)
	}
	bad := 0
	for i, f := range frames {
		if got := counter(f); got != want(i) && bad < 6 {
			t.Errorf("output frame %d shows field %d, want %d", i, got, want(i))
			bad++
		}
	}
}

// combed reports whether a frame holds two fields woven together: the
// lowest bit of the card's counter differs between any two successive
// fields, so its cell is then striped, one row light and the next dark.
func combed(frame []byte) bool {
	c := process.CounterCell(cardW, cardH, process.CounterBits-1)
	x, y := (c.Min.X+c.Max.X)/2, (c.Min.Y+c.Max.Y)/2
	stripes := 0
	for dy := -4; dy < 4; dy++ {
		r0, g0, b0 := rgb(frame, x, y+dy)
		r1, g1, b1 := rgb(frame, x, y+dy+1)
		if d := r0 + g0 + b0 - r1 - g1 - b1; d > 3*100 || d < -3*100 {
			stripes++
		}
	}
	return stripes >= 6
}

func inOrder(i int) int { return i }

// TestDeinterlaceH264 covers interlaced H.264 as x264 writes it
// (macroblock-adaptive frames with picture timing): each frame is shown as
// its two fields, in the order the stream gives.
func TestDeinterlaceH264(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	card := fieldCard(t, dir)
	fixture := func(name, scan string) string {
		in := filepath.Join(dir, name)
		args := append([]string{"-i", card, "-vf", toVideoRange + "interlace=scan=" + scan + ":lowpass=off",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p", "-bf", "2",
			"-flags", "+ildct+ilme", "-x264-params", scan + "=1", "-an"}, videoColour...)
		if err := oracle.TryFFmpeg(append(args, in)...); err != nil {
			oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
		}
		return in
	}
	tff := fixture("tff.mp4", "tff")

	t.Run("top field first", func(t *testing.T) {
		checkFields(t, exportClip(t, tff, 50, govideo.ClipOptions{}), inOrder, cardFields)
	})
	t.Run("bottom field first", func(t *testing.T) {
		checkFields(t, exportClip(t, fixture("bff.mp4", "bff"), 50, govideo.ClipOptions{}), inOrder, cardFields)
	})
	t.Run("matroska", func(t *testing.T) {
		checkFields(t, exportClip(t, fixture("tff.mkv", "tff"), 50, govideo.ClipOptions{}), inOrder, cardFields)
	})
	t.Run("at the frame rate", func(t *testing.T) {
		// One field of every frame, always the first: no comb, no judder.
		frames := exportClip(t, tff, 25, govideo.ClipOptions{})
		checkFields(t, frames, func(i int) int { return 2 * i }, cardFields/2)
		for i, f := range frames {
			if combed(f) {
				t.Fatalf("output frame %d is combed", i)
			}
		}
	})
	t.Run("trimmed", func(t *testing.T) {
		// The cut falls on the second field of a frame.
		frames := exportClip(t, tff, 50, govideo.ClipOptions{Trim: govideo.Range(420*time.Millisecond, 820*time.Millisecond)})
		checkFields(t, frames, func(i int) int { return 21 + i }, 20)
	})
	t.Run("off", func(t *testing.T) {
		frames := exportClip(t, tff, 25, govideo.ClipOptions{Deinterlace: govideo.DeinterlaceOff})
		if len(frames) != cardFields/2 {
			t.Fatalf("%d frames, want %d", len(frames), cardFields/2)
		}
		for i, f := range frames {
			if !combed(f) {
				t.Fatalf("output frame %d is not the woven frame", i)
			}
		}
	})
	t.Run("order forced", func(t *testing.T) {
		// The clip's setting overrides the stream: the fields of each frame
		// come out swapped.
		frames := exportClip(t, tff, 50, govideo.ClipOptions{Deinterlace: govideo.DeinterlaceBottomFirst})
		checkFields(t, frames, func(i int) int { return i ^ 1 }, cardFields)
	})
	t.Run("interlaced format, progressive frames", func(t *testing.T) {
		// A stream that allows fields and never uses them (written for
		// players that insist on an interlaced format) is progressive: its
		// frames are shown whole. The woven card stands in for the picture
		// so that a frame taken apart would show.
		in := filepath.Join(dir, "format.mp4")
		args := append([]string{"-i", card, "-vf", toVideoRange + "interlace=scan=tff:lowpass=off",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "10", "-pix_fmt", "yuv420p",
			"-x264-params", "fake-interlaced=1", "-an"}, videoColour...)
		if err := oracle.TryFFmpeg(append(args, in)...); err != nil {
			oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
		}
		info, err := videoprobe.ProbeFile(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Streams[0].Video.Interlaced {
			oracle.Missing(t, "the oracle's encoder did not mark the stream as allowing fields")
		}
		frames := exportClip(t, in, 25, govideo.ClipOptions{})
		if len(frames) != cardFields/2 {
			t.Fatalf("%d frames, want %d", len(frames), cardFields/2)
		}
		for i, f := range frames {
			if !combed(f) {
				t.Fatalf("output frame %d was deinterlaced, though no picture of the stream is in fields", i)
			}
		}
	})
	t.Run("not marked", func(t *testing.T) {
		// Interlaced pictures in a stream coded as progressive frames: left
		// alone unless the clip says what they are.
		in := filepath.Join(dir, "unmarked.mp4")
		args := append([]string{"-i", card, "-vf", toVideoRange + "interlace=scan=tff:lowpass=off",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "10", "-pix_fmt", "yuv420p", "-an"}, videoColour...)
		if err := oracle.TryFFmpeg(append(args, in)...); err != nil {
			oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
		}
		for i, f := range exportClip(t, in, 25, govideo.ClipOptions{}) {
			if !combed(f) {
				t.Fatalf("output frame %d was deinterlaced, though the stream is progressive", i)
			}
		}
		checkFields(t, exportClip(t, in, 50, govideo.ClipOptions{Deinterlace: govideo.DeinterlaceTopFirst}), inOrder, cardFields)
	})
}

// TestDeinterlaceForcedMJPEG covers a field order given for a codec that is
// decoded lazily and carries no field signalling of its own. It needs no
// operating-system codec.
func TestDeinterlaceForcedMJPEG(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	card := fieldCard(t, dir)
	in := filepath.Join(dir, "fields.mov")
	if err := oracle.TryFFmpeg("-i", card, "-vf", "interlace=scan=bff:lowpass=off", "-c:v", "mjpeg", "-q:v", "2", "-an", in); err != nil {
		oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
	}
	checkFields(t, exportClip(t, in, 50, govideo.ClipOptions{Deinterlace: govideo.DeinterlaceBottomFirst}), inOrder, cardFields)
	frames := exportClip(t, in, 25, govideo.ClipOptions{})
	if len(frames) != cardFields/2 || !combed(frames[1]) {
		t.Errorf("without the setting: %d frames, combed %v; want %d woven frames", len(frames), len(frames) > 1 && combed(frames[1]), cardFields/2)
	}
}
