package govideo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/internal/container/wav"
	"github.com/gomcpgo/govideo/process"
)

// The sample rates an audio-only export may ask for. A format may take
// fewer: the system AAC encoder of macOS has 8 to 48 kHz.
const (
	minAudioRate = 1000
	maxAudioRate = 384000
)

// AudioExportOptions configures ExportAudio.
type AudioExportOptions struct {
	// SampleRate is the sample rate of the file in Hz; 0 means 48000.
	// Speech recognition commonly wants 16000.
	SampleRate int
	// Channels is 1 or 2; 0 means 2. With 1 the two channels of the
	// programme are mixed to mono, each at half level.
	Channels int
	// Bitrate, in bits per second, applies to AAC (.m4a). An encoder has a
	// list of rates for each sample rate and channel count, and the one
	// nearest to Bitrate is used. 0 leaves the choice to the encoder: on
	// macOS 24 kbit/s for 16 kHz mono and 128 kbit/s for 48 kHz stereo.
	// WAVE and FLAC keep every sample and have no bit rate to choose.
	Bitrate int
	// Progress, if set, is called through the export, about ten times for
	// each second of sound. Frame and TotalFrames count sample frames of
	// the file.
	Progress func(Progress)
}

// ExportAudio renders the sound of the timeline, and nothing else, to an
// audio file at path. The format follows the file extension. .wav is 16-bit
// PCM, and .flac the same samples compressed without loss, to about half
// the size (less for clean sound, more for noisy); both are written on
// every platform. .m4a is AAC-LC from the operating system's encoder, much
// smaller and not exact, and where there is no such encoder the error is an
// *ErrCodecUnavailable. Memory use does not grow with the length.
//
// The sound is the programme audio that Export gives a video: the clips'
// own sound, tones, silence through stills, with their fades and
// crossfades, and the files added with AddAudio in the Mix and Replace
// modes. A file in Track mode is a separate track of a video and is left
// out. No picture is read, so a clip counts for its sound and its length
// even where its video cannot be decoded, and captions play no part.
//
// With no frames to keep to, the timeline is laid out on samples: a segment
// starts and ends at the 48 kHz sample nearest its time, where Export moves
// each join to the nearest frame of the canvas. The programme is mixed at
// 48 kHz in stereo, as it is for a video, then mixed down and resampled to
// what the options ask for.
func (t *Timeline) ExportAudio(ctx context.Context, path string, o AudioExportOptions) error {
	rate, channels := o.SampleRate, o.Channels
	if rate == 0 {
		rate = outRate
	}
	if channels == 0 {
		channels = 2
	}
	if channels != 1 && channels != 2 {
		return fmt.Errorf("govideo: an audio export has 1 or 2 channels, not %d", channels)
	}
	if rate < minAudioRate || rate > maxAudioRate {
		return fmt.Errorf("govideo: unsupported sample rate %d Hz (use %d to %d)", rate, minAudioRate, maxAudioRate)
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".wav":
	case ".flac":
		if _, err := Available("flac", Encode); err != nil {
			return err
		}
	case ".m4a":
		if _, err := Available("aac", Encode); err != nil {
			return err
		}
	default:
		return fmt.Errorf("govideo: unsupported audio output extension %q (use .wav, .flac or .m4a)", filepath.Ext(path))
	}
	if len(t.items) == 0 {
		return errors.New("govideo: the timeline has no clips, images or generated segments")
	}

	b := newBuild(ctx)
	defer b.close()
	err := b.segments(t, nil, func(sg *segment, s span) {
		sg.astart = durToFrames(s.begin)
		sg.alen = durToFrames(s.end) - sg.astart
		sg.afadeIn, sg.afadeOut = durToFrames(s.fadeIn), durToFrames(s.fadeOut)
	})
	if err != nil {
		return err
	}
	if b.total == 0 {
		return errors.New("govideo: the timeline is shorter than one sample")
	}
	if err := b.audio(t, nil); err != nil {
		return err
	}
	// The length of the file in sample frames: the programme's, at the
	// file's rate, to the nearest (as the resampler ends it).
	atRate := func(n int64) int64 { return (n*int64(rate) + outRate/2) / outRate }
	length := func(n int64) time.Duration { return time.Duration(float64(n) / outRate * 1e9) }
	total := atRate(b.total)

	var (
		sink   audioSink
		commit func() error
	)
	switch ext {
	case ".wav":
		// Known before anything is rendered, so hours of work are not
		// thrown away at the end.
		if total > wav.MaxDataSize/int64(2*channels) {
			return fmt.Errorf("govideo: %v of sound at %d Hz in %d channels does not fit in a WAV file (4 GB): lower the sample rate, or write .flac or .m4a",
				length(b.total).Round(time.Second), rate, channels)
		}
		out, err := createOutput(path)
		if err != nil {
			return err
		}
		defer out.discard()
		if sink, err = newWAVSink(out.File, rate, channels); err != nil {
			return err
		}
		commit = out.commit
	case ".flac":
		out, err := createOutput(path)
		if err != nil {
			return err
		}
		defer out.discard()
		if sink, err = newFLACSink(out.File, rate, channels); err != nil {
			return fmt.Errorf("govideo: %w", err)
		}
		commit = out.commit
	case ".m4a":
		scratch, remove, err := scratchFile(path)
		if err != nil {
			return err
		}
		defer remove()
		mux := isobmff.NewMuxer(isobmff.BrandM4A, scratch)
		if total <= math.MaxUint32 {
			// The file's length is written in samples, so that it plays
			// for exactly the samples of the programme and not to the
			// nearest millisecond. (It takes more than a day of sound to
			// overflow that.)
			mux.SetMovieTimescale(uint32(rate))
		}
		s, err := newAACSink(mux, isobmff.BrandM4A, isobmff.TrackConfig{}, rate, channels, o.Bitrate)
		if err != nil {
			return fmt.Errorf("govideo: AAC at %d Hz in %d channels: %w", rate, channels, err)
		}
		defer s.close()
		sink = s
		commit = func() error { return writeMovie(mux, path) }
	}

	// Render: a tenth of a second of programme at a time, taken to the
	// file's channels and rate and written.
	cf := newConform(rate, channels)
	var written int64
	put := func(samples []float32) error {
		if len(samples) == 0 {
			return nil
		}
		written += int64(len(samples) / channels)
		return sink.write(samples)
	}
	for pos := int64(0); pos < b.total; {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(pos+outRate/10, b.total)
		prog, err := b.mix(pos, end)
		if err != nil {
			return err
		}
		if err := put(cf.process(prog)); err != nil {
			return err
		}
		pos = end
		if o.Progress != nil {
			o.Progress(Progress{Frame: atRate(pos), TotalFrames: total, Time: length(pos), Duration: length(b.total)})
		}
	}
	if err := put(cf.flush()); err != nil {
		return err
	}
	if err := sink.finish(written); err != nil {
		return err
	}
	return commit()
}

// conform takes programme sound, 48 kHz stereo, to the channel count and
// sample rate of an audio-only export. The channels are mixed down first, so
// only what is kept is resampled.
type conform struct {
	mono bool
	rs   *process.Resampler
	buf  []float32
}

func newConform(rate, channels int) *conform {
	return &conform{mono: channels == 1, rs: process.NewResampler(outRate, rate, channels)}
}

// process returns what can be produced so far of the output for the
// programme given up to now.
func (c *conform) process(prog []float32) []float32 {
	if c.mono {
		c.buf = process.StereoToMono(c.buf[:0], prog)
		prog = c.buf
	}
	return c.rs.Process(prog)
}

// flush returns the rest of the output once the programme has ended.
func (c *conform) flush() []float32 { return c.rs.Flush() }
