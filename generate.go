package govideo

import (
	"image"
	"image/color"
	"math"
	"time"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/process"
)

// Pattern selects the picture of a generated segment.
type Pattern int

const (
	// PatternSolid is one colour (GenerateOptions.Color).
	PatternSolid Pattern = iota
	// PatternBars is 75% colour bars over a black-to-white ramp.
	PatternBars
	// PatternGradient blends from Color to Color2, left to right, or top to
	// bottom with Vertical.
	PatternGradient
	// PatternChecker is a checkerboard of Color and Color2.
	PatternChecker
	// PatternTestCard is animated: colour bars with a bar that sweeps across
	// once per second, a timecode, and the frame number as a row of sixteen
	// black or white cells along the bottom edge (most significant bit
	// first). Every frame differs, so it exercises inter-frame codecs and
	// lets a test identify each decoded frame.
	PatternTestCard
)

func (p Pattern) String() string {
	if p < PatternSolid || p > PatternTestCard {
		return "unknown"
	}
	return [...]string{"solid", "bars", "gradient", "checker", "test-card"}[p]
}

// GenerateOptions describes a segment that govideo synthesises instead of
// reading from a file: a background pattern, optional text over it, and an
// optional tone. The zero value is black and silent.
type GenerateOptions struct {
	Pattern  Pattern
	Color    color.Color // solid colour, or first colour of a gradient or checkerboard; default black
	Color2   color.Color // second colour of a gradient or checkerboard; default white
	Vertical bool        // run the gradient from top to bottom
	CellSize int         // checkerboard cell in canvas pixels; default one eighth of the height

	// Text is drawn centred over the pattern, for title cards. Lines are
	// separated by "\n". The built-in typeface is Latin only.
	Text      string
	TextColor color.Color // default white
	TextSize  float64     // in canvas pixels; default one tenth of the height

	// Tone is the frequency in Hz of a sine tone; 0 leaves the segment
	// silent. With Beep the tone sounds only for the first tenth of every
	// second, which makes audio/video alignment easy to check.
	Tone     float64
	ToneGain float64 // linear amplitude; default 0.25
	Beep     bool
}

// AddGenerated appends a synthesised segment lasting d: a test pattern, a
// plain or graduated background, or a title card.
func (t *Timeline) AddGenerated(d time.Duration, o GenerateOptions) {
	t.items = append(t.items, item{kind: itemGenerated, dur: d, gen: o})
}

// generator renders the pictures of a generated segment at the canvas size.
type generator struct {
	opts GenerateOptions
	base *image.RGBA
	fps  Rational
}

func newGenerator(cv Canvas, o GenerateOptions) (*generator, error) {
	if o.Color == nil {
		o.Color = color.Black
	}
	if o.Color2 == nil {
		o.Color2 = color.White
	}
	if o.TextColor == nil {
		o.TextColor = color.White
	}
	if o.TextSize <= 0 {
		o.TextSize = float64(cv.Height) / 10
	}
	if o.CellSize <= 0 {
		o.CellSize = max(cv.Height/8, 1)
	}
	w, h := cv.Width, cv.Height
	g := &generator{opts: o, fps: cv.FrameRate}
	switch o.Pattern {
	case PatternBars, PatternTestCard:
		g.base = process.Bars(w, h)
	case PatternGradient:
		g.base = process.Gradient(w, h, o.Color, o.Color2, o.Vertical)
	case PatternChecker:
		g.base = process.Checker(w, h, o.CellSize, o.Color, o.Color2)
	default:
		g.base = process.Solid(w, h, o.Color)
	}
	if o.Text != "" {
		cy := h / 2
		if o.Pattern == PatternTestCard {
			cy = h * 2 / 5 // clear of the timecode and the sweep
		}
		if err := process.DrawText(g.base, o.Text, w/2, cy, o.TextSize, w-w/10, o.TextColor); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// animated reports whether the picture changes from frame to frame.
func (g *generator) animated() bool { return g.opts.Pattern == PatternTestCard }

// frame renders frame n of the segment.
func (g *generator) frame(n int64) *codec.VideoFrame {
	if !g.animated() {
		return process.FromImage(g.base)
	}
	return process.FromImage(process.TestCard(g.base, n, g.fps.Num, g.fps.Den))
}

// tone synthesises the audio of a generated segment.
type tone struct {
	freq, gain float64
	beep       bool
	pos        int64 // sample frames produced so far
}

// fill writes the next len(dst)/2 stereo frames.
func (t *tone) fill(dst []float32) {
	const (
		beepLen = outRate / 10
		ramp    = outRate / 200 // 5 ms edges keep the beep free of clicks
	)
	for i := 0; i+1 < len(dst); i += 2 {
		g := t.gain
		if t.beep {
			switch p := t.pos % outRate; {
			case p >= beepLen:
				g = 0
			case p < ramp:
				g *= float64(p) / ramp
			case p >= beepLen-ramp:
				g *= float64(beepLen-p) / ramp
			}
		}
		v := float32(g * math.Sin(2*math.Pi*t.freq*float64(t.pos)/outRate))
		dst[i], dst[i+1] = v, v
		t.pos++
	}
}
