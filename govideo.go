// Package govideo combines video clips, still images, audio tracks and
// captions into one exported video.
//
// govideo natively implements only codecs whose patents have expired or that
// are royalty-free. Patent-pooled codecs (H.264, HEVC, AAC) are reached only
// through operating-system codecs; where the OS has none, they are
// unavailable and operations fail with ErrCodecUnavailable. See PATENTS.md.
//
// A program builds a Timeline, adds clips, images, audio and captions, and
// calls Export:
//
//	tl := govideo.NewTimeline(govideo.Canvas{Width: 1920, Height: 1080, FrameRate: govideo.R(30, 1)})
//	tl.AddImage("title.png", 3*time.Second, govideo.ImageOptions{})
//	tl.AddClip("clip.mov", govideo.ClipOptions{})
//	tl.AddAudio("music.wav", govideo.AudioOptions{Mode: govideo.Mix, Gain: 0.3})
//	err := tl.Export(ctx, "out.mov", govideo.ExportOptions{})
//
// Media inspection lives in the videoprobe sub-package.
package govideo

import (
	"errors"
	"fmt"
	"time"
)

// Rational is a rational number, used for frame rates and time bases.
type Rational struct{ Num, Den int64 }

// R returns the rational num/den.
func R(num, den int64) Rational { return Rational{num, den} }

// Float returns the value as a float64 (0 if Den is 0).
func (r Rational) Float() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// TimeRange is a half-open span [Start, End) within a source.
type TimeRange struct{ Start, End time.Duration }

// Range returns the TimeRange [start, end).
func Range(start, end time.Duration) TimeRange { return TimeRange{start, end} }

// ErrNotImplemented marks features that are specified but not built yet.
var ErrNotImplemented = errors.New("govideo: not implemented yet")

// ErrCodecUnavailable reports that a codec cannot be used on this host.
// Patent-pooled codecs exist only where the operating system provides them;
// there is no fallback implementation.
type ErrCodecUnavailable struct {
	Codec  CodecID
	Op     Op
	Reason string
	Hint   string
}

func (e *ErrCodecUnavailable) Error() string {
	s := fmt.Sprintf("govideo: codec %q unavailable for %s", e.Codec, e.Op)
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}
