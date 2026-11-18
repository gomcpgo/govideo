package govideo

import (
	"image/color"
	"time"
)

// FitMode says how a source picture is placed on the canvas.
type FitMode int

const (
	Contain FitMode = iota // scale to fit; letterbox or pillarbox with the background
	Cover                  // scale to fill; crop the overflow
	Stretch                // scale to the canvas, ignoring aspect ratio
)

// Canvas is the fixed output picture format of a timeline.
type Canvas struct {
	Width, Height int         // default 1920x1080
	FrameRate     Rational    // constant output rate; default 30/1
	Fit           FitMode     // how sources of other shapes are placed
	Background    color.Color // padding colour; default black
}

// ClipOptions configures a clip added with AddClip.
type ClipOptions struct {
	Trim TimeRange // part of the source to use; the zero value uses all of it
	Mute bool      // drop the clip's own audio
	Gain float64   // linear gain for the clip's audio; 0 means 1.0
}

// ImageOptions configures a still image added with AddImage.
type ImageOptions struct{}

// AudioMode says how an added audio file joins the output.
type AudioMode int

const (
	Mix     AudioMode = iota // sum into the programme audio
	Track                    // carry as a separate audio track
	Replace                  // substitute the programme audio over its span
)

// AudioOptions configures an audio file added with AddAudio.
type AudioOptions struct {
	Mode     AudioMode
	Gain     float64       // linear gain; 0 means 1.0
	Start    time.Duration // position on the timeline
	Trim     TimeRange     // part of the source to use; zero value uses all
	FadeIn   time.Duration
	FadeOut  time.Duration
	Language string // ISO 639-2 code for Track mode
}

// CaptionMode says how captions are delivered.
type CaptionMode int

const (
	Soft CaptionMode = iota // a selectable subtitle track
	Burn                    // rendered into the picture (not implemented yet)
)

// CaptionStyle controls burned-in captions.
type CaptionStyle struct {
	FontSize   float64     // in canvas pixels
	Color      color.Color // text colour
	Outline    color.Color
	Background color.Color // nil for no box
	MarginV    int         // distance from the bottom edge, canvas pixels
}

// DefaultCaptionStyle is white text with a black outline near the bottom.
var DefaultCaptionStyle = CaptionStyle{FontSize: 48, Color: color.White, Outline: color.Black, MarginV: 60}

// CaptionOptions configures captions added with AddCaptions.
type CaptionOptions struct {
	Mode     CaptionMode
	Style    CaptionStyle
	Offset   time.Duration // shift applied to every cue
	Language string        // ISO 639-2 code of the subtitle track
	Sidecar  bool          // also write a .vtt file next to the output
}

type itemKind int

const (
	itemClip itemKind = iota
	itemImage
)

type item struct {
	kind  itemKind
	path  string
	dur   time.Duration // images
	clip  ClipOptions
	image ImageOptions
}

type audioItem struct {
	path string
	opts AudioOptions
}

type captionItem struct {
	path string
	opts CaptionOptions
}

// Timeline is an ordered sequence of clips and still images, plus audio
// files and captions laid over it. Nothing is read until Export.
type Timeline struct {
	canvas   Canvas
	items    []item
	audio    []audioItem
	captions []captionItem
}

// NewTimeline returns an empty timeline with the given canvas.
func NewTimeline(c Canvas) *Timeline {
	if c.Width <= 0 || c.Height <= 0 {
		c.Width, c.Height = 1920, 1080
	}
	if c.FrameRate.Num <= 0 || c.FrameRate.Den <= 0 {
		c.FrameRate = R(30, 1)
	}
	if c.Background == nil {
		c.Background = color.Black
	}
	return &Timeline{canvas: c}
}

// AddClip appends a video (or audio-only) file to the timeline.
func (t *Timeline) AddClip(path string, o ClipOptions) {
	t.items = append(t.items, item{kind: itemClip, path: path, clip: o})
}

// AddImage appends a still image shown for d, with silence on the audio side.
func (t *Timeline) AddImage(path string, d time.Duration, o ImageOptions) {
	t.items = append(t.items, item{kind: itemImage, path: path, dur: d, image: o})
}

// AddAudio lays an audio file over the timeline.
func (t *Timeline) AddAudio(path string, o AudioOptions) {
	t.audio = append(t.audio, audioItem{path, o})
}

// AddCaptions attaches an SRT or WebVTT file, timed against the timeline.
func (t *Timeline) AddCaptions(path string, o CaptionOptions) {
	t.captions = append(t.captions, captionItem{path, o})
}
