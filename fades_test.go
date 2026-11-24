package govideo_test

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
)

// Tests of fades, transitions and stills in motion (SPEC §7.7). Everything
// that varies over an output frame is taken at the frame's middle, so frame
// n of a ramp of N frames stands at (n+0.5)/N.

func cardCanvas(bg color.Color) govideo.Canvas {
	return govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1), Background: bg}
}

// render exports the timeline in the intermediate profile and returns the
// file and the oracle's decode of its frames.
func render(t *testing.T, tl *govideo.Timeline) (string, [][]byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.mov")
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return out, oracle.Frames(t, out, cardW, cardH)
}

// frames30 is a duration of n frames of the test canvas.
func frames30(n int) time.Duration { return time.Duration(n) * time.Second / cardFPS }

// mixRGB is the colour a of the way from x to y.
func mixRGB(x, y color.RGBA, a float64) [3]float64 {
	m := func(p, q uint8) float64 { return float64(p) + (float64(q)-float64(p))*a }
	return [3]float64{m(x.R, y.R), m(x.G, y.G), m(x.B, y.B)}
}

// checkColour compares the middle of a frame with a colour.
func checkColour(t *testing.T, frame []byte, n int, want [3]float64, tol float64) {
	t.Helper()
	r, g, b := rgb(frame, cardW/2, cardH/2)
	if math.Abs(float64(r)-want[0]) > tol || math.Abs(float64(g)-want[1]) > tol || math.Abs(float64(b)-want[2]) > tol {
		t.Errorf("frame %d: rgb(%d,%d,%d), want rgb(%.0f,%.0f,%.0f) ±%.0f", n, r, g, b, want[0], want[1], want[2], tol)
	}
}

// sound returns the programme audio of an exported file. The oracle mixes
// the two channels to one at -3 dB each when it writes floating point, so
// the same signal on both comes back 3 dB up; that is taken off again.
func sound(t *testing.T, path string) []float32 {
	t.Helper()
	au := oracle.Audio(t, path, "0:a:0")
	for i := range au {
		au[i] *= math.Sqrt2 / 2
	}
	return au
}

// level returns the amplitude of the sine of the given frequency in the
// 20 ms of audio centred on sec.
func level(au []float32, freq, sec float64) float64 {
	const half = 480
	c := int(sec * 48000)
	var re, im float64
	for i := c - half; i < c+half; i++ {
		if i < 0 || i >= len(au) {
			continue
		}
		// A Hann window keeps a second tone out of the measurement.
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i-c+half)/(2*half))
		ph := 2 * math.Pi * freq * float64(i) / 48000
		re += float64(au[i]) * w * math.Cos(ph)
		im += float64(au[i]) * w * math.Sin(ph)
	}
	return 2 * math.Hypot(re, im) / half
}

func TestFades(t *testing.T) {
	oracle.Require(t)
	bg := color.RGBA{0, 0, 160, 255}
	fg := color.RGBA{220, 200, 40, 255}
	const total, fadeIn, fadeOut = 60, 15, 30
	opacity := func(n int) float64 {
		g := 1.0
		if n < fadeIn {
			g = (float64(n) + 0.5) / fadeIn
		}
		if left := total - n; left <= fadeOut {
			g *= (float64(left) - 0.5) / fadeOut
		}
		return g
	}
	// The envelope of the sound at a time, which runs through the same
	// points as the picture's steps.
	gain := func(sec float64) float64 {
		return math.Min(1, sec*cardFPS/fadeIn) * math.Min(1, (total-sec*cardFPS)/fadeOut)
	}

	tl := govideo.NewTimeline(cardCanvas(bg))
	tl.AddGenerated(frames30(total), govideo.GenerateOptions{Color: fg, Tone: 1000, ToneGain: 0.5,
		FadeIn: frames30(fadeIn), FadeOut: frames30(fadeOut)})
	out, frames := render(t, tl)
	if len(frames) != total {
		t.Fatalf("%d frames, want %d: a fade must not change the length", len(frames), total)
	}
	for n, f := range frames {
		checkColour(t, f, n, mixRGB(bg, fg, opacity(n)), 8)
	}
	au := sound(t, out)
	for _, sec := range []float64{0.05, 0.1, 0.25, 0.4, 0.75, 1.1, 1.25, 1.5, 1.75, 1.95} {
		if got, want := level(au, 1000, sec), 0.5*gain(sec); math.Abs(got-want) > 0.01 {
			t.Errorf("tone at %.2fs: amplitude %.3f, want %.3f", sec, got, want)
		}
	}

	// A clip: a file at half the output frame rate, so that every source
	// frame is shown twice, each time at its own opacity; and its sound.
	src := filepath.Join(t.TempDir(), "bars.mov")
	stl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS/2, 1)})
	stl.AddGenerated(2*time.Second, govideo.GenerateOptions{Color: fg, Tone: 1000, ToneGain: 0.5})
	if err := stl.Export(context.Background(), src, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatal(err)
	}
	tl = govideo.NewTimeline(cardCanvas(bg))
	tl.AddClip(src, govideo.ClipOptions{FadeIn: frames30(fadeIn), FadeOut: frames30(fadeOut)})
	out, frames = render(t, tl)
	if len(frames) != total {
		t.Fatalf("clip: %d frames, want %d", len(frames), total)
	}
	for n, f := range frames {
		checkColour(t, f, n, mixRGB(bg, fg, opacity(n)), 10)
	}
	au = sound(t, out)
	for _, sec := range []float64{0.1, 0.25, 0.75, 1.25, 1.75} {
		if got, want := level(au, 1000, sec), 0.5*gain(sec); math.Abs(got-want) > 0.01 {
			t.Errorf("clip: tone at %.2fs: amplitude %.3f, want %.3f", sec, got, want)
		}
	}

	// A still, and fades longer than the segment: they multiply, and the
	// picture never reaches full strength.
	still := filepath.Join(t.TempDir(), "still.png")
	writePNG(t, still, 320, 180, fg)
	tl = govideo.NewTimeline(cardCanvas(bg))
	tl.AddImage(still, frames30(20), govideo.ImageOptions{FadeIn: frames30(40), FadeOut: frames30(40)})
	_, frames = render(t, tl)
	if len(frames) != 20 {
		t.Fatalf("still: %d frames, want 20", len(frames))
	}
	for n, f := range frames {
		g := (float64(n) + 0.5) / 40 * (float64(20-n) - 0.5) / 40
		checkColour(t, f, n, mixRGB(bg, fg, g), 8)
	}
}

func TestCrossfade(t *testing.T) {
	oracle.Require(t)
	red, green, blue := color.RGBA{200, 30, 30, 255}, color.RGBA{30, 180, 30, 255}, color.RGBA{30, 30, 200, 255}
	tl := govideo.NewTimeline(cardCanvas(nil))
	tl.AddGenerated(time.Second, govideo.GenerateOptions{Color: red, Tone: 440, ToneGain: 0.5})
	tl.AddGenerated(time.Second, govideo.GenerateOptions{Color: green, Tone: 1000, ToneGain: 0.5,
		Transition: govideo.Transition{Type: govideo.Crossfade, Duration: time.Second / 2}})
	tl.AddGenerated(time.Second, govideo.GenerateOptions{Color: blue})
	out, frames := render(t, tl)
	// The transition takes its length off the timeline.
	if len(frames) != 75 {
		t.Fatalf("%d frames, want 75", len(frames))
	}
	for n, f := range frames {
		want := mixRGB(red, red, 0)
		switch {
		case n >= 45:
			want = mixRGB(blue, blue, 0)
		case n >= 30:
			want = mixRGB(green, green, 0)
		case n >= 15:
			want = mixRGB(red, green, (float64(n-15)+0.5)/15)
		}
		checkColour(t, f, n, want, 8)
	}
	au := sound(t, out)
	if want := 2.5 * 48000; math.Abs(float64(len(au))-want) > 2 {
		t.Errorf("%d audio samples, want %.0f", len(au), want)
	}
	for _, c := range []struct{ sec, x float64 }{
		{0.25, 0}, {0.55, 0.1}, {0.625, 0.25}, {0.75, 0.5}, {0.875, 0.75}, {0.95, 0.9}, {1.25, 1},
	} {
		// Equal power: the outgoing tone by the cosine, the incoming by the sine.
		if got, want := level(au, 440, c.sec), 0.5*math.Cos(c.x*math.Pi/2); math.Abs(got-want) > 0.01 {
			t.Errorf("outgoing tone at %.3fs: amplitude %.3f, want %.3f", c.sec, got, want)
		}
		if got, want := level(au, 1000, c.sec), 0.5*math.Sin(c.x*math.Pi/2); math.Abs(got-want) > 0.01 {
			t.Errorf("incoming tone at %.3fs: amplitude %.3f, want %.3f", c.sec, got, want)
		}
	}
	// The loudness of the two together does not dip through the join.
	for sec := 0.52; sec < 1; sec += 0.04 {
		var sum float64
		a := int(sec * 48000)
		for _, v := range au[a-960 : a+960] {
			sum += float64(v) * float64(v)
		}
		if got, want := math.Sqrt(sum/1920), 0.5/math.Sqrt2; math.Abs(got-want) > 0.02 {
			t.Errorf("RMS at %.2fs is %.3f, want %.3f throughout", sec, got, want)
		}
	}
	if got := level(au, 1000, 2); got > 0.001 {
		t.Errorf("the cut to a silent segment leaves a tone of amplitude %.3f", got)
	}

	// A transition with fades on both segments: each picture is faded over
	// the background first, then the two are mixed.
	bg := color.RGBA{90, 90, 90, 255}
	tl = govideo.NewTimeline(cardCanvas(bg))
	tl.AddGenerated(frames30(20), govideo.GenerateOptions{Color: red, FadeOut: frames30(20)})
	tl.AddGenerated(frames30(20), govideo.GenerateOptions{Color: green, FadeIn: frames30(20),
		Transition: govideo.Transition{Duration: frames30(10)}})
	_, frames = render(t, tl)
	if len(frames) != 30 {
		t.Fatalf("with fades: %d frames, want 30", len(frames))
	}
	for n, f := range frames {
		a := mixRGB(bg, red, (float64(20-n)-0.5)/20)
		b := mixRGB(bg, green, (float64(n-10)+0.5)/20)
		w := 0.0
		switch {
		case n >= 20:
			w = 1
		case n >= 10:
			w = (float64(n-10) + 0.5) / 10
		}
		checkColour(t, f, n, [3]float64{a[0] + (b[0]-a[0])*w, a[1] + (b[1]-a[1])*w, a[2] + (b[2]-a[2])*w}, 8)
	}
}

// checkDissolve verifies frames of joined test card cuts. shows says what
// output frame f holds: card frame a alone (w 0), or card frames a and b
// mixed with weight w on b. In a mix each cell of the counter is as light
// as the weights of the frames in which it is white.
func checkDissolve(t *testing.T, frames [][]byte, count int, shows func(f int) (a, b int, w float64)) {
	t.Helper()
	if len(frames) != count {
		t.Fatalf("%d frames, want %d", len(frames), count)
	}
	bad := 0
	for f, frame := range frames {
		a, b, w := shows(f)
		if w == 0 {
			if got := counter(frame); got != a && bad < 6 {
				t.Errorf("output frame %d shows card frame %d, want %d", f, got, a)
				bad++
			}
			continue
		}
		for i := 0; i < process.CounterBits; i++ {
			bit := func(n int) float64 { return float64(n >> (process.CounterBits - 1 - i) & 1) }
			c := process.CounterCell(cardW, cardH, i)
			r, g, bl := rgb(frame, (c.Min.X+c.Max.X)/2, (c.Min.Y+c.Max.Y)/2)
			got := float64(r+g+bl) / 3
			if want := 255 * (bit(a)*(1-w) + bit(b)*w); math.Abs(got-want) > 14 && bad < 6 {
				t.Errorf("output frame %d (cards %d and %d, weight %.2f): counter cell %d is %.0f, want %.0f", f, a, b, w, i, got, want)
				bad++
			}
		}
	}
}

// crossfadeCuts joins three cuts of a 90-frame test card file with
// ten-frame crossfades and checks every frame: which card frames the two
// clips are at through each transition, and after it.
func crossfadeCuts(t *testing.T, src string) {
	t.Helper()
	tl := govideo.NewTimeline(cardCanvas(nil))
	x := govideo.Transition{Type: govideo.Crossfade, Duration: frames30(10)}
	tl.AddClip(src, govideo.ClipOptions{Trim: govideo.Range(0, frames30(30))})
	tl.AddClip(src, govideo.ClipOptions{Trim: govideo.Range(frames30(60), frames30(90)), Transition: x})
	tl.AddClip(src, govideo.ClipOptions{Trim: govideo.Range(frames30(30), frames30(60)), Transition: x})
	_, frames := render(t, tl)
	// Cut 1 is output frames 0-29 (cards 0-29), cut 2 frames 20-49 (cards
	// 60-89), cut 3 frames 40-69 (cards 30-59).
	checkDissolve(t, frames, 70, func(f int) (int, int, float64) {
		switch {
		case f < 20:
			return f, 0, 0
		case f < 30:
			return f, 60 + f - 20, (float64(f-20) + 0.5) / 10
		case f < 40:
			return 60 + f - 20, 0, 0
		case f < 50:
			return 60 + f - 20, 30 + f - 40, (float64(f-40) + 0.5) / 10
		}
		return 30 + f - 40, 0, 0
	})
}

func TestCrossfadeClips(t *testing.T) {
	oracle.Require(t)
	src := filepath.Join(t.TempDir(), "card.mov")
	exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	crossfadeCuts(t, src)
}

// TestCrossfadeOSCodec has two clips of one H.264 file on screen together,
// each with a decoder of its own for as long as that lasts.
func TestCrossfadeOSCodec(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	src := filepath.Join(t.TempDir(), "card.mp4")
	cardClip(t, src, cardW, cardH, 90)
	before := oscodec.VideoDecoderStats()
	crossfadeCuts(t, src)
	after := oscodec.VideoDecoderStats()
	if after.Pictures != before.Pictures {
		t.Errorf("%d decoded pictures were not released", after.Pictures-before.Pictures)
	}
	if n := after.Sessions - before.Sessions; n != 2 {
		t.Errorf("%d decoder sessions opened, want 2: one for each of two clips shown together", n)
	}
}

// TestFadesCompatible sends faded and repeated pictures through the H.264
// encoder, whose sink keeps the last picture to repeat it.
func TestFadesCompatible(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	still := filepath.Join(dir, "still.png")
	fg := color.RGBA{200, 60, 40, 255}
	writePNG(t, still, 320, 180, fg)
	tl := govideo.NewTimeline(cardCanvas(nil))
	tl.AddImage(still, time.Second, govideo.ImageOptions{FadeIn: frames30(6), FadeOut: frames30(6)})
	tl.AddGenerated(time.Second, govideo.GenerateOptions{Color: color.RGBA{40, 60, 200, 255},
		Transition: govideo.Transition{Duration: frames30(6)}})
	out := filepath.Join(dir, "out.mp4")
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileCompatible}); err != nil {
		t.Fatal(err)
	}
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 54 {
		t.Fatalf("%d frames, want 54", len(frames))
	}
	black := color.RGBA{0, 0, 0, 255}
	checkColour(t, frames[0], 0, mixRGB(black, fg, 0.5/6), 12)
	checkColour(t, frames[3], 3, mixRGB(black, fg, 3.5/6), 12)
	for _, n := range []int{6, 15, 23} {
		checkColour(t, frames[n], n, mixRGB(fg, fg, 0), 12)
	}
	for _, n := range []int{30, 40, 53} {
		checkColour(t, frames[n], n, [3]float64{40, 60, 200}, 12)
	}
}

func TestTransitionErrors(t *testing.T) {
	sec := time.Second
	x := func(d time.Duration) govideo.Transition { return govideo.Transition{Duration: d} }
	still := filepath.Join(t.TempDir(), "still.png")
	writePNG(t, still, 64, 36, color.White)
	for _, c := range []struct {
		name  string
		build func(tl *govideo.Timeline)
		want  string
	}{
		{"first segment", func(tl *govideo.Timeline) {
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(sec / 2)})
		}, "nothing comes before it"},
		{"longer than the segment", func(tl *govideo.Timeline) {
			tl.AddGenerated(2*sec, govideo.GenerateOptions{})
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(3 * sec / 2)})
		}, "longer than the segment"},
		{"longer than the one before", func(tl *govideo.Timeline) {
			tl.AddGenerated(sec, govideo.GenerateOptions{})
			tl.AddGenerated(2*sec, govideo.GenerateOptions{Transition: x(3 * sec / 2)})
		}, "has left"},
		{"two transitions over one segment", func(tl *govideo.Timeline) {
			tl.AddGenerated(sec, govideo.GenerateOptions{})
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(3 * sec / 5)})
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(3 * sec / 5)})
		}, "has left"},
		{"negative transition", func(tl *govideo.Timeline) {
			tl.AddGenerated(sec, govideo.GenerateOptions{})
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(-sec)})
		}, "must not be negative"},
		{"negative fade", func(tl *govideo.Timeline) {
			tl.AddImage(still, sec, govideo.ImageOptions{FadeOut: -sec})
		}, "must not be negative"},
		{"unknown type", func(tl *govideo.Timeline) {
			tl.AddGenerated(sec, govideo.GenerateOptions{})
			tl.AddGenerated(sec, govideo.GenerateOptions{Transition: govideo.Transition{Type: 9, Duration: sec / 2}})
		}, "unknown transition type"},
		{"negative scale", func(tl *govideo.Timeline) {
			tl.AddImage(still, sec, govideo.ImageOptions{To: govideo.Placement{Scale: -1}})
		}, "Scale must not be negative"},
		{"placement not a number", func(tl *govideo.Timeline) {
			tl.AddImage(still, sec, govideo.ImageOptions{From: govideo.Placement{X: math.NaN()}})
		}, "not finite"},
	} {
		tl := govideo.NewTimeline(cardCanvas(nil))
		c.build(tl)
		err := tl.Export(context.Background(), filepath.Join(t.TempDir(), "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", c.name, err, c.want)
		}
	}
	// Two transitions that exactly share a segment between them are allowed.
	tl := govideo.NewTimeline(cardCanvas(nil))
	tl.AddGenerated(sec, govideo.GenerateOptions{})
	tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(sec / 2)})
	tl.AddGenerated(sec, govideo.GenerateOptions{Transition: x(sec / 2)})
	if err := tl.Export(context.Background(), filepath.Join(t.TempDir(), "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Errorf("transitions that meet in the middle of a segment: %v", err)
	}
}

// writeTarget writes a w×h picture of a mid grey with a white square of the
// given side in its middle.
func writeTarget(t *testing.T, path string, w, h, side int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{60, 60, 60, 255}
			if x >= (w-side)/2 && x < (w+side)/2 && y >= (h-side)/2 && y < (h+side)/2 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// square measures the white square of a target in a decoded frame: its
// centre and its width and height, to a fraction of a pixel, from how much
// lighter than the grey each pixel is.
func square(frame []byte) (cx, cy, w, h float64) {
	var sum, sx, sy float64
	var rowMax, colMax float64
	cols := make([]float64, cardW)
	for y := 0; y < cardH; y++ {
		var row float64
		for x := 0; x < cardW; x++ {
			r, g, b := rgb(frame, x, y)
			v := math.Min(1, (float64(r+g+b)/3-60)/195)
			if v < 0.1 {
				v = 0 // the grey, give or take the codec's noise
			}
			row += v
			cols[x] += v
			sum += v
			sx += v * (float64(x) + 0.5)
			sy += v * (float64(y) + 0.5)
		}
		rowMax = math.Max(rowMax, row)
	}
	for _, c := range cols {
		colMax = math.Max(colMax, c)
	}
	if sum == 0 {
		return 0, 0, 0, 0
	}
	// The fullest row holds the square's width of white, the fullest
	// column its height.
	return sx / sum, sy / sum, rowMax, colMax
}

func TestPanZoom(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	bg := color.RGBA{0, 0, 160, 255}
	// Twice the canvas: at rest the square is 100 pixels wide in the middle.
	wide := filepath.Join(dir, "wide.png")
	writeTarget(t, wide, 2*cardW, 2*cardH, 200)
	export := func(o govideo.ImageOptions, path string, n int) [][]byte {
		t.Helper()
		tl := govideo.NewTimeline(cardCanvas(bg))
		tl.AddImage(path, frames30(n), o)
		_, frames := render(t, tl)
		if len(frames) != n {
			t.Fatalf("%d frames, want %d", len(frames), n)
		}
		return frames
	}

	// Zoom in to twice the size, about the centre.
	const n = 30
	for i, f := range export(govideo.ImageOptions{To: govideo.Placement{Scale: 2}}, wide, n) {
		s := 1 + (float64(i)+0.5)/n
		cx, cy, w, h := square(f)
		if math.Abs(cx-cardW/2) > 0.5 || math.Abs(cy-cardH/2) > 0.5 {
			t.Errorf("zoom, frame %d: centre at (%.2f, %.2f), want the middle of the canvas", i, cx, cy)
		}
		if math.Abs(w-100*s) > 1.5 || math.Abs(h-100*s) > 1.5 {
			t.Errorf("zoom, frame %d: square is %.1f by %.1f, want %.1f", i, w, h, 100*s)
		}
	}

	// Zoom out, from a placement off centre to the picture at rest.
	for i, f := range export(govideo.ImageOptions{From: govideo.Placement{Scale: 1.5, X: 0.2, Y: -0.1}}, wide, n) {
		u := (float64(i) + 0.5) / n
		s := 1.5 - 0.5*u
		cx, cy, w, _ := square(f)
		wx, wy := cardW*(0.5+0.2*(1-u)), cardH*(0.5-0.1*(1-u))
		if math.Abs(cx-wx) > 0.5 || math.Abs(cy-wy) > 0.5 || math.Abs(w-100*s) > 1.5 {
			t.Errorf("zoom out, frame %d: square %.1f wide at (%.2f, %.2f), want %.1f at (%.2f, %.2f)", i, w, cx, cy, 100*s, wx, wy)
		}
	}

	// A slow pan: under half a pixel a frame, and every frame moves by
	// that much. Placed on whole pixels the picture would stand still on
	// some frames and jump on others.
	pan := export(govideo.ImageOptions{From: govideo.Placement{Scale: 1.5, X: 0.01}, To: govideo.Placement{Scale: 1.5, X: -0.01}}, wide, n)
	step := 0.02 * cardW / n
	var last float64
	for i, f := range pan {
		cx, cy, w, _ := square(f)
		if want := cardW * (0.5 + 0.01 - 0.02*(float64(i)+0.5)/n); math.Abs(cx-want) > 0.25 || math.Abs(cy-cardH/2) > 0.25 || math.Abs(w-150) > 1.5 {
			t.Errorf("pan, frame %d: square %.1f wide at (%.2f, %.2f), want 150 at (%.2f, %d)", i, w, cx, cy, want, cardH/2)
		}
		if i > 0 && math.Abs(last-cx-step) > 0.2 {
			t.Errorf("pan, frame %d: moved %.2f pixels, want %.2f", i, last-cx, step)
		}
		last = cx
	}

	// At rest at a placement of its own: half size, with the canvas
	// background round it.
	for i, f := range export(govideo.ImageOptions{From: govideo.Placement{Scale: 0.5, X: 0.25}, To: govideo.Placement{Scale: 0.5, X: 0.25}}, wide, 3) {
		cx, cy, w, h := square(f)
		if math.Abs(cx-cardW*0.75) > 0.5 || math.Abs(cy-cardH/2) > 0.5 || math.Abs(w-50) > 1.5 || math.Abs(h-50) > 1.5 {
			t.Errorf("placed, frame %d: square %.1f by %.1f at (%.2f, %.2f)", i, w, h, cx, cy)
		}
		for _, p := range [][2]int{{20, 20}, {cardW*3/4 - 170, cardH / 2}, {cardW*3/4 + 170, cardH / 2}, {cardW * 3 / 4, cardH/2 - 100}} {
			if r, g, b := rgb(f, p[0], p[1]); !near(r, 0, 10) || !near(g, 0, 10) || !near(b, 160, 10) {
				t.Errorf("placed, frame %d: rgb(%d,%d,%d) at (%d,%d), want the background", i, r, g, b, p[0], p[1])
			}
		}
		if r, g, b := rgb(f, cardW*3/4-150, cardH/2); !near(r, 60, 10) || !near(g, 60, 10) || !near(b, 60, 10) {
			t.Errorf("placed, frame %d: rgb(%d,%d,%d) inside the picture's left edge, want its grey", i, r, g, b)
		}
	}

	// A picture of another shape: it sits between bars at first, and
	// magnified it fills the width of the canvas; the Fit mode decides
	// where it starts from.
	tall := filepath.Join(dir, "tall.png")
	writeTarget(t, tall, 2*cardH, 2*cardH, 200)
	frames := export(govideo.ImageOptions{To: govideo.Placement{Scale: 2}}, tall, n)
	for _, c := range []struct {
		frame, x int
		want     color.RGBA
	}{{0, 100, bg}, {0, cardW - 100, bg}, {0, 150, color.RGBA{60, 60, 60, 255}}, {n - 1, 5, color.RGBA{60, 60, 60, 255}}, {n - 1, cardW - 5, color.RGBA{60, 60, 60, 255}}} {
		if r, g, b := rgb(frames[c.frame], c.x, 20); !near(r, int(c.want.R), 10) || !near(g, int(c.want.G), 10) || !near(b, int(c.want.B), 10) {
			t.Errorf("square picture, frame %d: rgb(%d,%d,%d) at x=%d, want rgb(%d,%d,%d)", c.frame, r, g, b, c.x, c.want.R, c.want.G, c.want.B)
		}
	}
	for i, f := range frames {
		s := 1 + (float64(i)+0.5)/n
		if _, _, w, _ := square(f); math.Abs(w-100*s) > 1.5 {
			t.Errorf("square picture, frame %d: square is %.1f wide, want %.1f", i, w, 100*s)
		}
	}
}
