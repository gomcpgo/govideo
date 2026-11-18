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
	"github.com/gomcpgo/govideo/codec/native"
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

// Quality is a coarse quality setting.
type Quality int

const (
	QualityAuto Quality = iota // same as QualityHigh
	QualityLow
	QualityMedium
	QualityHigh
)

// Progress is passed to ExportOptions.Progress after every output frame.
type Progress struct {
	Frame, TotalFrames int64
	Time, Duration     time.Duration
}

// ExportOptions configures Export.
type ExportOptions struct {
	Profile  Profile
	Quality  Quality
	Progress func(Progress)
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

// segment is one timeline item prepared for export.
type segment struct {
	frames int64 // output frames
	trim   time.Duration
	video  *videoStream      // nil for stills and audio-only clips
	still  *codec.VideoFrame // canvas-sized
	audio  *audioStream      // nil for silence
	gain   float32
}

// overlay is an added audio file prepared for export.
type overlay struct {
	as            *audioStream
	opts          AudioOptions
	start, length int64 // in 48 kHz frames on the timeline
	pos           int64
	track         int // mux track for Track mode
}

// Export renders the timeline to path. The container is chosen by the file
// extension (.mp4, .m4v or .mov). Memory use does not grow with the output
// length. A missing codec is reported as an *ErrCodecUnavailable.
func (t *Timeline) Export(ctx context.Context, path string, o ExportOptions) (err error) {
	profile, err := resolveProfile(o.Profile)
	if err != nil {
		return err
	}
	if profile != ProfileIntermediate {
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
		return errors.New("govideo: the timeline has no clips or images")
	}
	for _, c := range t.captions {
		if c.opts.Mode == Burn {
			return fmt.Errorf("govideo: burned-in captions: %w", ErrNotImplemented)
		}
	}
	cv := t.canvas
	fps := cv.FrameRate
	fit := process.FitMode(cv.Fit)

	var opened []*media
	defer func() {
		for _, m := range opened {
			m.close()
		}
	}()
	open := func(p string) (*media, error) {
		m, err := openMedia(ctx, p)
		if err == nil {
			opened = append(opened, m)
		}
		return m, err
	}

	// Prepare segments. Boundaries are rounded on the cumulative timeline so
	// rounding never accumulates.
	var segs []*segment
	var cum time.Duration
	frameAtTime := func(d time.Duration) int64 {
		return int64(d.Seconds()*fps.Float() + 0.5)
	}
	for _, it := range t.items {
		sg := &segment{gain: 1}
		var dur time.Duration
		switch it.kind {
		case itemImage:
			img, err := decodeStill(it.path)
			if err != nil {
				return err
			}
			sg.still = process.Fit(img, cv.Width, cv.Height, fit, cv.Background)
			dur = it.dur
		case itemClip:
			m, err := open(it.path)
			if err != nil {
				return err
			}
			vs, as := m.stream(videoprobe.Video), m.stream(videoprobe.Audio)
			var src time.Duration
			switch {
			case vs != nil:
				src = vs.StartTime + vs.Duration
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
			if vs != nil {
				if sg.video, err = openVideo(ctx, m, vs); err != nil {
					return err
				}
			} else {
				sg.still = process.Fit(codec.NewVideoFrame(cv.Width, cv.Height), cv.Width, cv.Height, process.Stretch, cv.Background)
			}
			if as != nil && !it.clip.Mute {
				if sg.audio, err = openAudio(ctx, m, as, start); err != nil {
					return err
				}
			}
			if it.clip.Gain != 0 {
				sg.gain = float32(it.clip.Gain)
			}
		}
		if dur <= 0 {
			return fmt.Errorf("govideo: %s: duration must be positive", it.path)
		}
		first := frameAtTime(cum)
		cum += dur
		sg.frames = frameAtTime(cum) - first
		if sg.frames > 0 {
			segs = append(segs, sg)
		}
	}
	var totalFrames int64
	for _, sg := range segs {
		totalFrames += sg.frames
	}
	if totalFrames == 0 {
		return errors.New("govideo: the timeline is shorter than one frame")
	}
	// audioPos maps an output frame index to a 48 kHz sample position.
	audioPos := func(frame int64) int64 { return frame * outRate * fps.Den / fps.Num }
	totalAudio := audioPos(totalFrames)
	total := time.Duration(float64(totalFrames) / fps.Float() * 1e9)

	// Muxer and tracks.
	dir := filepath.Dir(path)
	scratch, err := os.CreateTemp(dir, ".govideo-*.mdat")
	if err != nil {
		return err
	}
	defer func() {
		scratch.Close()
		os.Remove(scratch.Name())
	}()
	mux := isobmff.NewMuxer(brand, scratch)
	scale := int64(1)
	for fps.Num*scale < 10000 {
		scale *= 2
	}
	if fps.Num*scale > 1<<31 || fps.Den*scale > 1<<31 {
		return fmt.Errorf("govideo: unsupported frame rate %v", fps)
	}
	var ventry, aentry []byte
	if brand == isobmff.BrandMOV {
		ventry = isobmff.VisualEntry("jpeg", cv.Width, cv.Height, "Photo - JPEG")
		aentry = isobmff.AudioEntry("sowt", 2, 16, outRate)
	} else {
		ventry = isobmff.VisualEntry("mp4v", cv.Width, cv.Height, "", isobmff.ESDS(0x6c, 0x04, nil, 0, 0, 0))
		aentry = isobmff.AudioEntry("ipcm", 2, 16, outRate, isobmff.PcmC(true, 16))
	}
	vtrack := mux.AddTrack(isobmff.TrackConfig{Handler: "vide", Timescale: uint32(fps.Num * scale),
		Entry: ventry, Width: cv.Width, Height: cv.Height, AllSync: true})
	audioCfg := isobmff.TrackConfig{Handler: "soun", Timescale: outRate, Entry: aentry, SampleSize: 4, AltGroup: 1}
	atrack := mux.AddTrack(audioCfg)

	var overlays []*overlay
	for _, a := range t.audio {
		m, err := open(a.path)
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
		ov := &overlay{opts: a.opts, start: durToFrames(a.opts.Start), length: durToFrames(end - start), track: -1}
		if ov.start < 0 {
			return fmt.Errorf("govideo: %s: Start must not be negative", a.path)
		}
		if ov.start >= totalAudio {
			continue // entirely beyond the end of the timeline
		}
		ov.length = min(ov.length, totalAudio-ov.start)
		if ov.as, err = openAudio(ctx, m, as, start); err != nil {
			return err
		}
		if a.opts.Mode == Track {
			cfg := audioCfg
			cfg.Language, cfg.Disabled = a.opts.Language, true
			ov.track = mux.AddTrack(cfg)
		}
		overlays = append(overlays, ov)
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

	// Render.
	enc := &native.MJPEGEncoder{Quality: map[Quality]int{QualityLow: 70, QualityMedium: 82}[o.Quality]}
	var (
		frame    int64
		prog     []float32
		side     []float32
		pcm      []byte
		lastKey  [2]int64
		lastData []byte
	)
	for si, sg := range segs {
		for n := int64(0); n < sg.frames; n++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Video: pick the source frame for this output time.
			key := [2]int64{int64(si) + 1, 0}
			var pic *codec.VideoFrame
			if sg.video != nil {
				ts := sg.trim + time.Duration(float64(n)*float64(fps.Den)/float64(fps.Num)*1e9)
				src, seq, err := sg.video.frameAt(ts)
				if err != nil {
					return err
				}
				key[1] = seq
				if key != lastKey {
					pic = process.Fit(src, cv.Width, cv.Height, fit, cv.Background)
				}
			} else if key != lastKey {
				pic = sg.still
			}
			if pic != nil {
				if lastData, err = enc.Encode(pic); err != nil {
					return err
				}
				lastKey = key
			}
			if err := mux.WriteSample(vtrack, lastData, uint32(fps.Den*scale), true, 0); err != nil {
				return err
			}

			// Audio for the span of this frame.
			a0, a1 := audioPos(frame), audioPos(frame+1)
			count := int(a1 - a0)
			prog = resize(prog, count*2)
			if sg.audio != nil {
				got, err := sg.audio.read(prog)
				if err != nil {
					return err
				}
				_ = got // a short read leaves silence: audio is padded to the video length
				if sg.gain != 1 {
					process.ApplyGain(prog, sg.gain)
				}
			}
			for _, ov := range overlays {
				lo, hi := max(a0, ov.start), min(a1, ov.start+ov.length)
				side = resize(side, count*2)
				if hi > lo {
					part := side[(lo-a0)*2 : (hi-a0)*2]
					if _, err := ov.as.read(part); err != nil {
						return err
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
				if ov.track >= 0 && count > 0 {
					process.SoftClip(side)
					pcm = native.AppendS16LE(pcm[:0], side)
					if err := mux.WriteSample(ov.track, pcm, 0, true, 0); err != nil {
						return err
					}
				}
			}
			if count > 0 {
				process.SoftClip(prog)
				pcm = native.AppendS16LE(pcm[:0], prog)
				if err := mux.WriteSample(atrack, pcm, 0, true, 0); err != nil {
					return err
				}
			}
			frame++
			if o.Progress != nil {
				o.Progress(Progress{Frame: frame, TotalFrames: totalFrames,
					Time: time.Duration(float64(frame) / fps.Float() * 1e9), Duration: total})
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

	out, err := os.CreateTemp(dir, ".govideo-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	bw := bufio.NewWriterSize(out, 1<<20)
	if err = mux.Finish(bw); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = out.Chmod(0o644); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	err = os.Rename(out.Name(), path)
	return err
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
