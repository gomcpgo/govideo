package govideo

import (
	"fmt"
	"math"
	"time"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/process"
)

// This file holds what a segment does once it is on the timeline: produce
// its picture and its sound for an output frame, faded at its ends, and
// move a still across the canvas. Mixing two segments through a transition
// is done by the render loop in export.go (SPEC §7.7).
//
// Everything that varies over time is taken at the middle of the output
// frame it is for: the opacity of a fade, the weights of a crossfade, the
// position of a moving still. An output frame stands for a span of time,
// and the middle of a straight ramp is its mean over the span, so the
// picture's fade agrees with the sound's, which is ramped sample by sample.
// It also makes a fade in and a fade out mirror images, and wastes no frame
// on a picture that is wholly background.

// normal returns the placement with its defaults filled in.
func (p Placement) normal() Placement {
	if p.Scale == 0 {
		p.Scale = 1
	}
	return p
}

// check reports a placement that cannot be drawn.
func (p Placement) check() error {
	for _, v := range [...]float64{p.Scale, p.X, p.Y} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("placement %+v is not finite", p)
		}
	}
	if p.Scale < 0 {
		return fmt.Errorf("placement Scale must not be negative (%v)", p.Scale)
	}
	return nil
}

// panZoom draws a still that moves across the canvas: at a placement that
// goes from one given to another over the frames of its segment.
type panZoom struct {
	src      *codec.VideoFrame // the picture, no larger than it is ever shown
	w, h     float64           // its size on the canvas at Scale 1
	from, to Placement
	frames   int64 // length of the segment
	cv       Canvas
	placer   process.Placer
	out      *codec.VideoFrame
}

// newPanZoom prepares img, a decoded still, for showing on canvas cv at
// placements from and to and between.
func newPanZoom(img *codec.VideoFrame, cv Canvas, fit process.FitMode, from, to Placement) *panZoom {
	img = process.ConvertTo709Limited(img)
	w, h := process.FitSize(img.Width, img.Height, cv.Width, cv.Height, fit)
	// A photograph is usually several times the size of the canvas, and
	// resampling it for every frame would cost in proportion to its size,
	// not the canvas's. It is reduced once, to the largest size it will be
	// shown at; the frames are then resampled from that, by a factor that
	// is 1 at the largest placement.
	big := math.Max(from.Scale, to.Scale)
	tw := int(math.Min(float64(img.Width), math.Ceil(w*big)))
	th := int(math.Min(float64(img.Height), math.Ceil(h*big)))
	if tw, th = max(tw, 1), max(th, 1); tw < img.Width || th < img.Height {
		img = process.Scale(img, tw, th)
	}
	return &panZoom{src: img, w: w, h: h, from: from, to: to, frames: 1, cv: cv}
}

// frame draws frame n of the segment. The frame is the panZoom's own, and
// is drawn over by the next call.
func (z *panZoom) frame(n int64) *codec.VideoFrame {
	t := (float64(n) + 0.5) / float64(max(z.frames, 1))
	mix := func(a, b float64) float64 { return a + (b-a)*t }
	s := mix(z.from.Scale, z.to.Scale)
	w, h := z.w*s, z.h*s
	cx := float64(z.cv.Width) * (0.5 + mix(z.from.X, z.to.X))
	cy := float64(z.cv.Height) * (0.5 + mix(z.from.Y, z.to.Y))
	z.out = z.placer.Place(z.out, z.src, z.cv.Width, z.cv.Height, cx-w/2, cy-h/2, w, h, z.cv.Background)
	return z.out
}

// segment is one timeline item prepared for export. It occupies the output
// frames start to start+frames-1; where a transition joins it to the
// segment before, the first of those are also the last of that segment.
type segment struct {
	start, frames int64
	trim          time.Duration
	origin        time.Duration     // source time of the clip's first video frame
	video         *videoStream      // nil for stills and audio-only clips
	still         *codec.VideoFrame // canvas-sized
	audio         *audioStream      // nil for silence
	gen           *generator        // animated generated picture
	zoom          *panZoom          // still in motion
	tone          *tone             // generated audio
	gain          float32

	cv  Canvas
	fit process.FitMode

	// The picture of a clip is fitted to the canvas when its source frame
	// changes; fitted is that picture, of source frame fitSeq.
	fitted *codec.VideoFrame
	fitSeq int64

	// Fades, in output frames for the picture and in 48 kHz sample frames
	// for the sound. The segment's sound starts at sample frame astart of
	// the programme and is alen long; apos is how much of it has been
	// produced. An audio-only export sets these and no frames.
	fadeIn, fadeOut   int64
	afadeIn, afadeOut int64
	astart, alen      int64
	apos              int64
}

// end is the output frame after the segment's last.
func (sg *segment) end() int64 { return sg.start + sg.frames }

// aend is the sample frame of the programme after the last of the segment's
// sound.
func (sg *segment) aend() int64 { return sg.astart + sg.alen }

// picture returns the segment's frame n as a canvas-sized picture, and a
// number that changes whenever the picture does (a still's never does). The
// picture is valid until the next call.
func (sg *segment) picture(n int64) (*codec.VideoFrame, int64, error) {
	switch {
	case sg.video != nil:
		// Pick the source frame for this output time.
		fps := sg.cv.FrameRate
		ts := sg.origin + sg.trim + time.Duration(float64(n)*float64(fps.Den)/float64(fps.Num)*1e9)
		src, seq, err := sg.video.frameAt(ts)
		if err != nil {
			return nil, 0, err
		}
		if sg.fitted == nil || seq != sg.fitSeq {
			sg.fitted, sg.fitSeq = process.Fit(src, sg.cv.Width, sg.cv.Height, sg.fit, sg.cv.Background), seq
		}
		return sg.fitted, seq, nil
	case sg.gen != nil:
		return sg.gen.frame(n), n + 1, nil
	case sg.zoom != nil:
		return sg.zoom.frame(n), n + 1, nil
	}
	return sg.still, 0, nil
}

// opacity is how much of the segment's picture frame n shows over the
// background: 1 away from the fades.
func (sg *segment) opacity(n int64) float64 {
	g := 1.0
	if n < sg.fadeIn {
		g = (float64(n) + 0.5) / float64(sg.fadeIn)
	}
	if left := sg.frames - n; left <= sg.fadeOut {
		g *= (float64(left) - 0.5) / float64(sg.fadeOut)
	}
	return g
}

// sound writes the next len(dst)/2 sample frames of the segment's sound to
// dst, which must be zeroed, with the clip's gain and the fades applied.
func (sg *segment) sound(dst []float32) error {
	switch {
	case sg.audio != nil:
		// A short read leaves silence: a clip's audio is padded to the
		// length of its video.
		if _, err := sg.audio.read(dst); err != nil {
			return err
		}
		if sg.gain != 1 {
			process.ApplyGain(dst, sg.gain)
		}
	case sg.tone != nil:
		sg.tone.fill(dst)
	}
	if sg.afadeIn > 0 || sg.afadeOut > 0 {
		process.ApplyFade(dst, 2, sg.apos, sg.afadeIn, sg.afadeOut, sg.alen)
	}
	sg.apos += int64(len(dst) / 2)
	return nil
}

// done ends the segment once its last frame is rendered: the decoder of a
// clip goes on to the next clip.
func (sg *segment) done() {
	if sg.video != nil {
		sg.video.close()
	}
}
