package govideo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gomcpgo/govideo/caption"
	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// Profile selects the output codecs.
type Profile int

const (
	// ProfileAuto picks ProfileCompatible when the operating system has
	// H.264 and AAC encoders, else ProfileRoyaltyFree, else
	// ProfileIntermediate.
	ProfileAuto Profile = iota
	// ProfileCompatible is H.264 + AAC-LC in MP4 (needs the OS codecs).
	ProfileCompatible
	// ProfileRoyaltyFree is AV1 + Opus.
	ProfileRoyaltyFree
	// ProfileIntermediate is Motion JPEG + 16-bit LPCM in MOV or MP4: large
	// files that every editor and ffmpeg can read, using only native codecs.
	ProfileIntermediate
)

func (p Profile) String() string {
	return [...]string{"auto", "compatible", "royalty-free", "intermediate"}[p]
}

// Quality is a coarse quality setting. For H.264 it selects a picture quality
// that the encoder holds whatever the content, so the file size follows the
// material: little for a still or a slide, much more for grain or fast
// motion. Set ExportOptions.VideoBitrate when the size must be predictable.
type Quality int

const (
	QualityAuto Quality = iota // same as QualityHigh
	QualityLow
	QualityMedium
	QualityHigh
)

// Progress is passed to ExportOptions.Progress after every output frame. An
// audio-only export reports it several times a second of sound, and there
// Frame and TotalFrames count sample frames of the file (see
// AudioExportOptions).
type Progress struct {
	Frame, TotalFrames int64
	Time, Duration     time.Duration
}

// ExportOptions configures Export.
type ExportOptions struct {
	Profile Profile
	Quality Quality
	// VideoBitrate and AudioBitrate, in bits per second, apply to
	// ProfileCompatible. VideoBitrate replaces the quality-targeted encoding
	// that Quality selects with an average bit rate; AudioBitrate overrides
	// the rate Quality selects.
	VideoBitrate int
	AudioBitrate int
	Progress     func(Progress)
}

func resolveProfile(p Profile) (Profile, error) {
	need := map[Profile][]CodecID{ProfileCompatible: {"h264", "aac"}, ProfileRoyaltyFree: {"av1", "opus"}}
	check := func(p Profile) error {
		for _, id := range need[p] {
			if _, err := Available(id, Encode); err != nil {
				return err
			}
		}
		return nil
	}
	if p != ProfileAuto {
		return p, check(p)
	}
	for _, c := range []Profile{ProfileCompatible, ProfileRoyaltyFree} {
		if check(c) == nil {
			return c, nil
		}
	}
	return ProfileIntermediate, nil
}

// overlay is an added audio file prepared for export.
type overlay struct {
	as            *audioStream
	opts          AudioOptions
	start, length int64 // in 48 kHz frames on the timeline
	pos           int64
	sink          audioSink // its own track, in Track mode
}

// pictures is what the segments of a video export are drawn with. An
// audio-only export has none, and reads no picture.
type pictures struct {
	canvas Canvas
	fit    process.FitMode
	slack  time.Duration // frame selection tolerance (see videoStream.frameAt)
}

// span is where a segment lies on the timeline, before any rounding: from
// begin to end, with fades of the given lengths inside its two ends.
type span struct {
	begin, end      time.Duration
	fadeIn, fadeOut time.Duration
}

// build is an export under way: the source files it has open, and the
// timeline's segments and added audio files prepared for rendering. Export
// and ExportAudio both render from one. They differ in the grid the timeline
// is rounded to (output frames, or samples) and in what is done with the
// result; the sound is mixed the same way for both (mix).
type build struct {
	ctx      context.Context
	opened   []*media
	segs     []*segment
	overlays []*overlay
	decs     *videoDecoders
	total    int64 // length of the programme, in 48 kHz sample frames

	// Mixing: the earliest segment whose sound has not ended, and the
	// buffers the sound is mixed in.
	sounding        int
	prog, side, inc []float32
}

func newBuild(ctx context.Context) *build {
	return &build{ctx: ctx, decs: &videoDecoders{}}
}

// close releases the decoders and closes the source files.
func (b *build) close() {
	for _, sg := range b.segs {
		if sg.video != nil {
			sg.video.close()
		}
		if sg.audio != nil {
			sg.audio.close()
		}
	}
	for _, ov := range b.overlays {
		ov.as.close()
	}
	b.decs.close()
	for _, m := range b.opened {
		m.close()
	}
}

func (b *build) open(path string) (*media, error) {
	m, err := openMedia(b.ctx, path)
	if err == nil {
		b.opened = append(b.opened, m)
	}
	return m, err
}

// segments prepares the timeline's items. Each is handed to place with its
// position on the timeline, which place rounds to the grid of the output:
// boundaries are rounded on the cumulative timeline, so rounding never
// accumulates. With a nil pic no picture is read or decoded: a clip gives
// its sound and its length, a still its length.
func (b *build) segments(t *Timeline, pic *pictures, place func(*segment, span)) error {
	var (
		cum  time.Duration // where the previous segment ends on the timeline
		free time.Duration // how much of it its own transition leaves unmixed
	)
	for i, it := range t.items {
		sg := &segment{gain: 1}
		if pic != nil {
			sg.cv, sg.fit = pic.canvas, pic.fit
		}
		b.segs = append(b.segs, sg) // owned from here on, so its decoders get closed
		var dur time.Duration
		switch it.kind {
		case itemImage:
			if pic != nil {
				cv, fit := pic.canvas, pic.fit
				from, to := it.image.From.normal(), it.image.To.normal()
				for _, p := range []Placement{from, to} {
					if err := p.check(); err != nil {
						return fmt.Errorf("govideo: %s: %w", it.path, err)
					}
				}
				img, err := decodeStill(it.path, cv.Background)
				if err != nil {
					return err
				}
				switch {
				case from != to:
					sg.zoom = newPanZoom(img, cv, fit, from, to)
				case from != (Placement{Scale: 1}):
					sg.still = newPanZoom(img, cv, fit, from, to).frame(0) // placed, but at rest
				default:
					sg.still = process.Fit(img, cv.Width, cv.Height, fit, cv.Background)
				}
			}
			dur = it.dur
		case itemGenerated:
			if pic != nil {
				g, err := newGenerator(pic.canvas, it.gen)
				if err != nil {
					return err
				}
				if g.animated() {
					sg.gen = g
				} else {
					sg.still = g.frame(0)
				}
			}
			if it.gen.Tone > 0 {
				sg.tone = &tone{freq: it.gen.Tone, gain: it.gen.ToneGain, beep: it.gen.Beep}
				if sg.tone.gain <= 0 {
					sg.tone.gain = 0.25
				}
			}
			dur = it.dur
		case itemClip:
			m, err := b.open(it.path)
			if err != nil {
				return err
			}
			vs, as := m.stream(videoprobe.Video), m.stream(videoprobe.Audio)
			// A clip's time starts at its first video frame. Files often
			// start their video a little late (after audio encoder priming,
			// or a B-frame delay); playing that gap would repeat the first
			// frame and make the join to the previous clip stutter. Audio
			// keeps its position relative to the picture.
			var src time.Duration
			switch {
			case vs != nil:
				sg.origin = max(vs.StartTime, 0)
				src = m.videoSpan(vs)
			case as != nil:
				src = as.StartTime + as.Duration
			default:
				return fmt.Errorf("govideo: %s: no video or audio stream", it.path)
			}
			start, end := it.clip.Trim.Start, it.clip.Trim.End
			if end <= 0 || end > src {
				end = src
			}
			if start < 0 {
				start = 0
			}
			if start >= end {
				return fmt.Errorf("govideo: %s: trim range is empty (source is %v long)", it.path, src)
			}
			sg.trim, dur = start, end-start
			switch {
			case pic == nil:
			case vs != nil:
				if sg.video, err = openVideo(b.ctx, m, vs, sg.origin+start, pic.slack, it.clip.Deinterlace, b.decs); err != nil {
					return err
				}
			default:
				cv := pic.canvas
				sg.still = process.Fit(codec.NewVideoFrame(cv.Width, cv.Height), cv.Width, cv.Height, process.Stretch, cv.Background)
			}
			if as != nil && !it.clip.Mute {
				if sg.audio, err = openAudio(b.ctx, m, as, sg.origin+start); err != nil {
					return err
				}
			}
			if it.clip.Gain != 0 {
				sg.gain = float32(it.clip.Gain)
			}
		}
		if dur <= 0 {
			return fmt.Errorf("govideo: %s: duration must be positive", it.label())
		}
		fadeIn, fadeOut, tr := it.joining()
		if fadeIn < 0 || fadeOut < 0 || tr.Duration < 0 {
			return fmt.Errorf("govideo: %s: fade and transition durations must not be negative", it.label())
		}
		if tr.Type != Crossfade {
			return fmt.Errorf("govideo: %s: unknown transition type %d", it.label(), tr.Type)
		}
		// A transition lays the start of this segment over the end of the
		// one before. Neither may be mixed twice over: then no more than two
		// segments are ever on together.
		switch over := tr.Duration; {
		case over == 0:
		case i == 0:
			return fmt.Errorf("govideo: %s: the first segment cannot have a transition: nothing comes before it", it.label())
		case over > free:
			return fmt.Errorf("govideo: %s: the %v transition is longer than what the segment before it has left (%v)", it.label(), over, free)
		case over > dur:
			return fmt.Errorf("govideo: %s: the %v transition is longer than the segment (%v)", it.label(), over, dur)
		}
		begin := cum - tr.Duration
		cum = begin + dur
		free = dur - tr.Duration
		place(sg, span{begin: begin, end: cum, fadeIn: fadeIn, fadeOut: fadeOut})
		if sg.zoom != nil {
			sg.zoom.frames = sg.frames
		}
		b.total = sg.aend()
	}
	return nil
}

// audio prepares the files added with AddAudio, once the segments have given
// the programme its length. track makes the output track of a file in Track
// mode; where it is nil there are no separate tracks and those files are
// left out.
func (b *build) audio(t *Timeline, track func(AudioOptions) (audioSink, error)) error {
	for _, a := range t.audio {
		if a.opts.Mode == Track && track == nil {
			continue
		}
		m, err := b.open(a.path)
		if err != nil {
			return err
		}
		as := m.stream(videoprobe.Audio)
		if as == nil {
			return fmt.Errorf("govideo: %s: no audio stream", a.path)
		}
		src := as.StartTime + as.Duration
		start, end := a.opts.Trim.Start, a.opts.Trim.End
		if end <= 0 || end > src {
			end = src
		}
		if start < 0 {
			start = 0
		}
		if start >= end {
			return fmt.Errorf("govideo: %s: trim range is empty (source is %v long)", a.path, src)
		}
		ov := &overlay{opts: a.opts, start: durToFrames(a.opts.Start), length: durToFrames(end - start)}
		if ov.start < 0 {
			return fmt.Errorf("govideo: %s: Start must not be negative", a.path)
		}
		if ov.start >= b.total {
			continue // entirely beyond the end of the timeline
		}
		ov.length = min(ov.length, b.total-ov.start)
		if ov.as, err = openAudio(b.ctx, m, as, start); err != nil {
			return err
		}
		b.overlays = append(b.overlays, ov)
		if a.opts.Mode == Track {
			if ov.sink, err = track(a.opts); err != nil {
				return err
			}
		}
	}
	return nil
}

// mix returns the programme sound of the sample frames a0 up to a1 (48 kHz
// stereo): the segments that are on, crossfaded where a transition joins
// two, with the added audio files mixed in or put in their place, and the
// whole kept within full scale. A file in Track mode is written to its own
// track on the way. Spans must be asked for in order and without gaps; how
// the programme is cut into them makes no difference to the samples. The
// slice is the build's own, good until the next call.
func (b *build) mix(a0, a1 int64) ([]float32, error) {
	count := int(a1 - a0)
	b.prog = resize(b.prog, count*2)
	prog := b.prog
	// At any moment one segment is on, or two that a transition joins: the
	// outgoing one (out) and the incoming one (in). The span is rendered in
	// runs that end where a segment starts or ends.
	for pos := a0; pos < a1; {
		for b.sounding+1 < len(b.segs) && b.segs[b.sounding].aend() <= pos {
			b.sounding++
		}
		out := b.segs[b.sounding]
		stop := min(a1, out.aend())
		var in *segment
		if b.sounding+1 < len(b.segs) {
			if next := b.segs[b.sounding+1]; next.astart <= pos {
				in = next
			} else {
				stop = min(stop, next.astart)
			}
		}
		if stop <= pos {
			break // past the end of the timeline: silence
		}
		part := prog[(pos-a0)*2 : (stop-a0)*2]
		if err := out.sound(part); err != nil {
			return nil, err
		}
		if in != nil {
			b.inc = resize(b.inc, len(part))
			if err := in.sound(b.inc); err != nil {
				return nil, err
			}
			process.Crossfade(part, b.inc, 2, pos-in.astart, out.aend()-in.astart)
		}
		pos = stop
	}
	for _, ov := range b.overlays {
		lo, hi := max(a0, ov.start), min(a1, ov.start+ov.length)
		b.side = resize(b.side, count*2)
		side := b.side
		if hi > lo {
			part := side[(lo-a0)*2 : (hi-a0)*2]
			if _, err := ov.as.read(part); err != nil {
				return nil, err
			}
			if g := ov.opts.Gain; g != 0 && g != 1 {
				process.ApplyGain(part, float32(g))
			}
			if ov.opts.FadeIn > 0 || ov.opts.FadeOut > 0 {
				process.ApplyFade(part, 2, ov.pos, durToFrames(ov.opts.FadeIn), durToFrames(ov.opts.FadeOut), ov.length)
			}
			ov.pos += hi - lo
			switch ov.opts.Mode {
			case Mix:
				process.MixInto(prog[(lo-a0)*2:], part)
			case Replace:
				copy(prog[(lo-a0)*2:], part)
			}
		}
		if ov.sink != nil && count > 0 {
			process.SoftClip(side)
			if err := ov.sink.write(side); err != nil {
				return nil, err
			}
		}
	}
	process.SoftClip(prog)
	return prog, nil
}

// output is the file an export writes. It is made under a temporary name
// beside its destination and takes the destination's name only when it is
// complete, so an export that fails or is cancelled leaves nothing at the
// path it was given: no file, and never part of one.
type output struct {
	*os.File
	path string
	done bool
}

func createOutput(path string) (*output, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".govideo-*.tmp")
	if err != nil {
		return nil, err
	}
	return &output{File: f, path: path}, nil
}

// commit closes the complete file and gives it its name.
func (o *output) commit() error {
	if err := o.Chmod(0o644); err != nil {
		return err
	}
	if err := o.Close(); err != nil {
		return err
	}
	if err := os.Rename(o.Name(), o.path); err != nil {
		return err
	}
	o.done = true
	return nil
}

// discard removes the file unless it was committed.
func (o *output) discard() {
	if !o.done {
		o.Close()
		os.Remove(o.Name())
	}
}

// scratchFile makes the file a muxer spools its media data to, beside the
// output at path. remove closes and deletes it.
func scratchFile(path string) (f *os.File, remove func(), err error) {
	if f, err = os.CreateTemp(filepath.Dir(path), ".govideo-*.mdat"); err != nil {
		return nil, nil, err
	}
	return f, func() {
		f.Close()
		os.Remove(f.Name())
	}, nil
}

// writeMovie writes the muxer's finished file to path.
func writeMovie(mux *isobmff.Muxer, path string) error {
	out, err := createOutput(path)
	if err != nil {
		return err
	}
	defer out.discard()
	bw := bufio.NewWriterSize(out, 1<<20)
	if err := mux.Finish(bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return out.commit()
}

// Export renders the timeline to path. The container is chosen by the file
// extension (.mp4, .m4v or .mov). Memory use does not grow with the output
// length. A missing codec is reported as an *ErrCodecUnavailable.
//
// For the sound alone, see ExportAudio.
func (t *Timeline) Export(ctx context.Context, path string, o ExportOptions) error {
	profile, err := resolveProfile(o.Profile)
	if err != nil {
		return err
	}
	if profile == ProfileRoyaltyFree {
		return fmt.Errorf("govideo: profile %s: %w", profile, ErrNotImplemented)
	}
	var brand isobmff.Brand
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mov":
		brand = isobmff.BrandMOV
	case ".mp4", ".m4v":
		brand = isobmff.BrandMP4
	default:
		return fmt.Errorf("govideo: unsupported output extension %q (use .mp4 or .mov)", filepath.Ext(path))
	}
	if len(t.items) == 0 {
		return errors.New("govideo: the timeline has no clips, images or generated segments")
	}
	for _, c := range t.captions {
		if c.opts.Mode == Burn {
			return fmt.Errorf("govideo: burned-in captions: %w", ErrNotImplemented)
		}
	}
	cv := t.canvas
	if profile == ProfileCompatible && (cv.Width%2 != 0 || cv.Height%2 != 0) {
		return fmt.Errorf("govideo: H.264 needs an even canvas size, not %dx%d", cv.Width, cv.Height)
	}
	fps := cv.FrameRate

	b := newBuild(ctx)
	defer b.close()

	// Segments lie on whole output frames, and their sound on the samples
	// of those frames. audioPos maps an output frame index to a 48 kHz
	// sample position.
	frameAtTime := func(d time.Duration) int64 {
		return int64(d.Seconds()*fps.Float() + 0.5)
	}
	audioPos := func(frame int64) int64 { return frame * outRate * fps.Den / fps.Num }
	pic := &pictures{canvas: cv, fit: process.FitMode(cv.Fit),
		// Frame selection tolerance: four tenths of an output frame (see
		// videoStream.frameAt).
		slack: time.Duration(0.4 * float64(fps.Den) / float64(fps.Num) * 1e9)}
	err = b.segments(t, pic, func(sg *segment, s span) {
		sg.start = frameAtTime(s.begin)
		sg.frames = frameAtTime(s.end) - sg.start
		sg.fadeIn, sg.fadeOut = frameAtTime(s.fadeIn), frameAtTime(s.fadeOut)
		// The sound is faded over the samples of the frames the picture is
		// faded over.
		first, last := audioPos(sg.start), audioPos(sg.end())
		sg.astart, sg.alen = first, last-first
		sg.afadeIn = audioPos(sg.start+sg.fadeIn) - first
		sg.afadeOut = last - audioPos(sg.end()-sg.fadeOut)
	})
	if err != nil {
		return err
	}
	segs := b.segs
	totalFrames := segs[len(segs)-1].end()
	if totalFrames == 0 {
		return errors.New("govideo: the timeline is shorter than one frame")
	}
	totalAudio := b.total
	total := time.Duration(float64(totalFrames) / fps.Float() * 1e9)

	// Muxer and tracks.
	scratch, remove, err := scratchFile(path)
	if err != nil {
		return err
	}
	defer remove()
	mux := isobmff.NewMuxer(brand, scratch)
	scale := int64(1)
	for fps.Num*scale < 10000 {
		scale *= 2
	}
	if fps.Num*scale > 1<<31 || fps.Den*scale > 1<<31 {
		return fmt.Errorf("govideo: unsupported frame rate %v", fps)
	}
	outs := &outputs{profile: profile, brand: brand, mux: mux, canvas: cv, opts: o}
	defer outs.close()
	vsink, err := outs.video(uint32(fps.Num*scale), uint32(fps.Den*scale))
	if err != nil {
		return err
	}
	asink, err := outs.audio("", false)
	if err != nil {
		return err
	}

	err = b.audio(t, func(a AudioOptions) (audioSink, error) { return outs.audio(a.Language, true) })
	if err != nil {
		return err
	}

	type subTrack struct {
		track   int
		samples []caption.Sample
	}
	var subs []subTrack
	for i, c := range t.captions {
		f, err := os.Open(c.path)
		if err != nil {
			return err
		}
		cues, err := caption.Parse(f, c.path)
		f.Close()
		if err != nil {
			return fmt.Errorf("govideo: %s: %w", c.path, err)
		}
		kept := cues[:0]
		for _, q := range cues {
			q.Start, q.End = max(q.Start+c.opts.Offset, 0), min(q.End+c.opts.Offset, total)
			if q.End > q.Start {
				kept = append(kept, q)
			}
		}
		tr := mux.AddTrack(isobmff.TrackConfig{Handler: "sbtl", Timescale: 1000, Entry: isobmff.Tx3gEntry(),
			Width: cv.Width, Height: cv.Height, Language: c.opts.Language, AllSync: true, AltGroup: 2, Disabled: i > 0})
		subs = append(subs, subTrack{tr, caption.Tx3gSamples(kept, total)})
		if c.opts.Sidecar {
			side := strings.TrimSuffix(path, filepath.Ext(path))
			if i > 0 {
				side += fmt.Sprintf(".%d", i+1)
			}
			sf, err := os.Create(side + ".vtt")
			if err != nil {
				return err
			}
			err = caption.WriteWebVTT(sf, kept)
			if cerr := sf.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}

	// Render. At any output frame one segment is on, or two that a
	// transition joins: the outgoing one (out) and the incoming one (in).
	var (
		lastKey [2]int64
		// Frames that mixed pictures are written to, reused for every frame:
		// the two segments' pictures over the background, and their mix.
		shadeOut, shadeIn, mixed *codec.VideoFrame
		cur                      int // the earliest segment still on
	)
	// shaded returns frame n of a segment as it shows through its fades.
	shaded := func(sg *segment, n int64, pic *codec.VideoFrame, scratch **codec.VideoFrame) *codec.VideoFrame {
		g := sg.opacity(n)
		if g >= 1 {
			return pic
		}
		*scratch = process.FadeInto(*scratch, pic, cv.Background, g)
		return *scratch
	}
	for frame := int64(0); frame < totalFrames; frame++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for segs[cur].end() <= frame {
			segs[cur].done()
			cur++
		}
		out := segs[cur]
		var in *segment
		if cur+1 < len(segs) && segs[cur+1].start <= frame {
			in = segs[cur+1]
		}

		// Video. A picture that is the same as the last one written is not
		// sent again: the sink repeats it.
		n := frame - out.start
		src, id, err := out.picture(n)
		if err != nil {
			return err
		}
		var pic *codec.VideoFrame
		if in == nil && out.opacity(n) >= 1 {
			if key := [2]int64{int64(cur) + 1, id}; key != lastKey {
				pic, lastKey = src, key
			}
		} else {
			pic = shaded(out, n, src, &shadeOut)
			if in != nil {
				m := frame - in.start
				next, _, err := in.picture(m)
				if err != nil {
					return err
				}
				next = shaded(in, m, next, &shadeIn)
				mixed = process.BlendInto(mixed, pic, next, (float64(m)+0.5)/float64(out.end()-in.start))
				pic = mixed
			}
			lastKey = [2]int64{} // a mix is never repeated
		}
		if err := vsink.write(pic); err != nil {
			return err
		}

		// Audio for the span of this frame.
		prog, err := b.mix(audioPos(frame), audioPos(frame+1))
		if err != nil {
			return err
		}
		if len(prog) > 0 {
			if err := asink.write(prog); err != nil {
				return err
			}
		}
		if o.Progress != nil {
			o.Progress(Progress{Frame: frame + 1, TotalFrames: totalFrames,
				Time: time.Duration(float64(frame+1) / fps.Float() * 1e9), Duration: total})
		}
	}

	if err := vsink.finish(); err != nil {
		return err
	}
	if err := asink.finish(totalAudio); err != nil {
		return err
	}
	for _, ov := range b.overlays {
		if ov.sink != nil {
			if err := ov.sink.finish(totalAudio); err != nil {
				return err
			}
		}
	}

	// Subtitle samples are tiny; they go after the audio and video data.
	for _, st := range subs {
		for _, s := range st.samples {
			a, b := (s.Start + time.Millisecond/2).Milliseconds(), (s.Start + s.Duration + time.Millisecond/2).Milliseconds()
			if b <= a {
				continue
			}
			if err := mux.WriteSample(st.track, s.Data, uint32(b-a), true, 0); err != nil {
				return err
			}
		}
	}

	return writeMovie(mux, path)
}

// resize returns s with length n, zeroed.
func resize(s []float32, n int) []float32 {
	if cap(s) < n {
		return make([]float32, n)
	}
	s = s[:n]
	clear(s)
	return s
}
