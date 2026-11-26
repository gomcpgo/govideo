package govideo_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec/native"
	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/wav"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/videoprobe"
)

// The audio-only export is tested, as far as it can be, without the oracle:
// a WAVE file is read back by the module's own parser, an .m4a through the
// module's own decoders, and a FLAC file is held to the WAVE file of the
// same sound, so the tests run where ffmpeg is not installed and (for WAVE
// and FLAC) where there are no system codecs.

// writeTone writes a 16-bit WAVE file sec long holding a sine of freq Hz,
// with one channel for each level given: the peak of that channel's sine, 1
// being full scale.
func writeTone(t *testing.T, path string, rate int, sec, freq float64, levels ...float64) {
	t.Helper()
	n := int(sec * float64(rate))
	hdr, err := wav.PCMHeader(len(levels), rate, 16, int64(n))
	if err != nil {
		t.Fatal(err)
	}
	b := hdr
	for i := 0; i < n; i++ {
		v := math.Sin(2 * math.Pi * freq * float64(i) / float64(rate))
		for _, l := range levels {
			b = binary.LittleEndian.AppendUint16(b, uint16(int16(math.Round(32767*l*v))))
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pcmFile is the samples of an audio file, interleaved.
type pcmFile struct {
	rate, channels int
	s              []float64
}

// frames is the number of sample frames.
func (a pcmFile) frames() int { return len(a.s) / a.channels }

// level is the RMS of channel ch over the sample frames from to to.
func (a pcmFile) level(ch, from, to int) float64 {
	var sum float64
	for i := from; i < to; i++ {
		v := a.s[i*a.channels+ch]
		sum += v * v
	}
	return math.Sqrt(sum / float64(to-from))
}

// freq is the frequency of channel ch over the sample frames from to to,
// from the number of times the signal rises through zero.
func (a pcmFile) freq(ch, from, to int) float64 {
	n := 0
	for i := from + 1; i < to; i++ {
		if a.s[(i-1)*a.channels+ch] < 0 && a.s[i*a.channels+ch] >= 0 {
			n++
		}
	}
	return float64(n) * float64(a.rate) / float64(to-from)
}

// readWAV reads a 16-bit WAVE file with the module's own parser.
func readWAV(t *testing.T, path string) pcmFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := wav.Open(context.Background(), strings.NewReader(string(b)), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	if f.Bits != 16 || f.Float || f.DataOffset != wav.HeaderSize || f.DataOffset+f.DataSize != int64(len(b)) {
		t.Fatalf("%s: %d-bit (float %v), data at %d, %d bytes of %d: want 16-bit PCM after a %d-byte header, to the end of the file",
			filepath.Base(path), f.Bits, f.Float, f.DataOffset, f.DataSize, len(b), wav.HeaderSize)
	}
	a := pcmFile{rate: f.SampleRate, channels: f.Channels, s: make([]float64, f.DataSize/2)}
	for i := range a.s {
		a.s[i] = float64(int16(binary.LittleEndian.Uint16(b[f.DataOffset+int64(i)*2:]))) / 32768
	}
	return a
}

// exportAudio runs ExportAudio and reads the WAVE file it wrote.
func exportAudio(t *testing.T, tl *govideo.Timeline, path string, o govideo.AudioExportOptions) pcmFile {
	t.Helper()
	if err := tl.ExportAudio(context.Background(), path, o); err != nil {
		t.Fatalf("ExportAudio %s: %v", filepath.Base(path), err)
	}
	return readWAV(t, path)
}

// snr compares two sounds of one layout, in decibels: the level of the
// first against that of their difference. Sounds that are a sample or two
// out of step score a few decibels; the same sound through a lossy codec
// scores tens.
func snr(t *testing.T, ref, got pcmFile) float64 {
	t.Helper()
	if ref.rate != got.rate || ref.channels != got.channels || len(ref.s) != len(got.s) {
		t.Fatalf("comparing %d frames of %d Hz in %d channels with %d of %d Hz in %d",
			ref.frames(), ref.rate, ref.channels, got.frames(), got.rate, got.channels)
	}
	var sig, diff float64
	for i, v := range ref.s {
		sig += v * v
		diff += (v - got.s[i]) * (v - got.s[i])
	}
	return 10 * math.Log10(sig/(diff+1e-12))
}

func within(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// toneRMS is the level of a generated tone at its default gain: a sine with
// a peak of a quarter of full scale.
const toneRMS = 0.25 / math.Sqrt2

// TestExportAudioWAV exports tones and silence as WAVE files and reads them
// back. The segments' lengths are no whole number of the canvas's frames:
// the sound must change on the sample their times name, where a video export
// would move each join to a frame.
func TestExportAudioWAV(t *testing.T) {
	dir := t.TempDir()
	tl := govideo.NewTimeline(govideo.Canvas{FrameRate: govideo.R(30, 1)})
	tl.AddGenerated(510500*time.Microsecond, govideo.GenerateOptions{Tone: 1000}) // 15.3 frames
	tl.AddGenerated(250*time.Millisecond, govideo.GenerateOptions{})
	tl.AddGenerated(400*time.Millisecond, govideo.GenerateOptions{Tone: 440})
	// Where the tone stops and the next starts, and the length, in 48 kHz
	// samples. On frames the tone would stop at 24000.
	const stop, start, total = 24504, 36504, 55704

	t.Run("48 kHz stereo", func(t *testing.T) {
		path := filepath.Join(dir, "full.wav")
		var calls []govideo.Progress
		a := exportAudio(t, tl, path, govideo.AudioExportOptions{Progress: func(p govideo.Progress) { calls = append(calls, p) }})
		if a.rate != 48000 || a.channels != 2 || a.frames() != total {
			t.Fatalf("%d frames of %d Hz in %d channels, want %d of 48000 Hz in 2", a.frames(), a.rate, a.channels, total)
		}
		for ch := 0; ch < 2; ch++ {
			if l := a.level(ch, 0, stop); !within(l, toneRMS, 0.002) {
				t.Errorf("channel %d: level %.4f through the first tone, want %.4f", ch, l, toneRMS)
			}
			// The tone runs up to the sample before the join, and the next
			// one from the sample after the silence: a cycle of each.
			if l := a.level(ch, stop-48, stop); !within(l, toneRMS, 0.01) {
				t.Errorf("channel %d: level %.4f over the last cycle of the first tone, want %.4f", ch, l, toneRMS)
			}
			if l := a.level(ch, start, start+109); !within(l, toneRMS, 0.01) {
				t.Errorf("channel %d: level %.4f over the first cycle of the second tone, want %.4f", ch, l, toneRMS)
			}
			for i := stop; i < start; i++ {
				if v := a.s[i*2+ch]; v != 0 {
					t.Fatalf("channel %d: sample %d of the silence is %v", ch, i, v)
				}
			}
		}
		if f := a.freq(0, 0, 24000); !within(f, 1000, 2) {
			t.Errorf("first tone at %.0f Hz, want 1000", f)
		}
		if f := a.freq(0, start, total); !within(f, 440, 5) {
			t.Errorf("second tone at %.0f Hz, want 440", f)
		}

		// Progress runs to the end, in sample frames of the file.
		if len(calls) < 10 {
			t.Fatalf("%d progress calls for 1.16 s of sound, want about 12", len(calls))
		}
		for i, p := range calls {
			if p.TotalFrames != total || p.Duration != calls[0].Duration || (i > 0 && (p.Frame <= calls[i-1].Frame || p.Time <= calls[i-1].Time)) {
				t.Fatalf("progress call %d: %+v after %+v", i, p, calls[max(i-1, 0)])
			}
		}
		last := calls[len(calls)-1]
		if last.Frame != total || last.Time != last.Duration || !within(last.Duration.Seconds(), 1.1605, 1e-6) {
			t.Errorf("last progress call %+v, want frame %d of %d at 1.1605 s", last, total, total)
		}

		info, err := videoprobe.ProbeFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if s := info.Streams; info.Format != "wav" || len(s) != 1 || s[0].Codec != "pcm_s16le" || s[0].Audio.SampleRate != 48000 ||
			s[0].Audio.Channels != 2 || !within(info.Duration.Seconds(), 1.1605, 1e-6) {
			t.Errorf("videoprobe: %s, %v, streams %+v", info.Format, info.Duration, info.Streams)
		}
	})

	t.Run("16 kHz mono", func(t *testing.T) {
		a := exportAudio(t, tl, filepath.Join(dir, "speech.wav"), govideo.AudioExportOptions{SampleRate: 16000, Channels: 1})
		if a.rate != 16000 || a.channels != 1 || a.frames() != total/3 {
			t.Fatalf("%d frames of %d Hz in %d channels, want %d of 16000 Hz in 1", a.frames(), a.rate, a.channels, total/3)
		}
		// The resampler's filter rings for a few milliseconds about a join.
		const edge = 100
		if l := a.level(0, 0, stop/3-edge); !within(l, toneRMS, 0.002) {
			t.Errorf("level %.4f through the first tone, want %.4f: both channels held it, so the mono mix keeps its level", l, toneRMS)
		}
		if l := a.level(0, stop/3+edge, start/3-edge); l > 1e-4 {
			t.Errorf("level %.5f through the silence", l)
		}
		if l := a.level(0, start/3+edge, total/3-edge); !within(l, toneRMS, 0.002) {
			t.Errorf("level %.4f through the second tone, want %.4f", l, toneRMS)
		}
		// The join is where it is at 48 kHz: the tone is still there a
		// millisecond before it, and gone a millisecond after.
		if l := a.level(0, stop/3-32, stop/3-16); l < toneRMS/2 {
			t.Errorf("level %.4f a millisecond before the tone ends", l)
		}
		if l := a.level(0, stop/3+16, stop/3+32); l > toneRMS/8 {
			t.Errorf("level %.4f a millisecond after the tone ends", l)
		}
		if f := a.freq(0, 0, 8000); !within(f, 1000, 2) {
			t.Errorf("first tone at %.0f Hz, want 1000", f)
		}
	})

	t.Run("44.1 kHz", func(t *testing.T) {
		a := exportAudio(t, tl, filepath.Join(dir, "cd.wav"), govideo.AudioExportOptions{SampleRate: 44100})
		// 1.1605 s is 51178.05 samples.
		if a.rate != 44100 || a.channels != 2 || a.frames() != 51178 {
			t.Fatalf("%d frames of %d Hz in %d channels, want 51178 of 44100 Hz in 2", a.frames(), a.rate, a.channels)
		}
		if f := a.freq(1, 0, 22050); !within(f, 1000, 2) {
			t.Errorf("first tone at %.0f Hz, want 1000", f)
		}
	})
}

// TestExportAudioIsTheSoundOfExport renders one timeline both ways. Its
// joins fall on frames, so the two layouts agree, and the audio-only export
// must then hold the samples of the video's programme track: through a
// fade, a crossfade and a mixed-in file. The video's sound is read back by
// exporting the video file's audio in turn.
func TestExportAudioIsTheSoundOfExport(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	music, other := filepath.Join(dir, "music.wav"), filepath.Join(dir, "other.wav")
	writeTone(t, music, 44100, 2, 330, 0.5, 0.3)
	writeTone(t, other, 48000, 2, 2500, 0.9)

	tl := govideo.NewTimeline(govideo.Canvas{Width: 160, Height: 120, FrameRate: govideo.R(25, 1)})
	tl.AddGenerated(1200*time.Millisecond, govideo.GenerateOptions{Pattern: govideo.PatternTestCard, Tone: 1000, Beep: true, FadeIn: 200 * time.Millisecond})
	tl.AddGenerated(time.Second, govideo.GenerateOptions{Pattern: govideo.PatternBars, Tone: 440,
		Transition: govideo.Transition{Duration: 400 * time.Millisecond}, FadeOut: 200 * time.Millisecond})
	tl.AddAudio(music, govideo.AudioOptions{Mode: govideo.Mix, Gain: 0.5, Start: 400 * time.Millisecond,
		Trim: govideo.Range(100*time.Millisecond, time.Second), FadeIn: 200 * time.Millisecond, FadeOut: 200 * time.Millisecond})
	// A separate track of the video; no part of the programme, so not in
	// the audio-only export.
	tl.AddAudio(other, govideo.AudioOptions{Mode: govideo.Track})

	video := filepath.Join(dir, "video.mov")
	if err := tl.Export(ctx, video, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatal(err)
	}
	fromVideo := govideo.NewTimeline(govideo.Canvas{})
	fromVideo.AddClip(video, govideo.ClipOptions{})
	want := exportAudio(t, fromVideo, filepath.Join(dir, "video.wav"), govideo.AudioExportOptions{})
	got := exportAudio(t, tl, filepath.Join(dir, "direct.wav"), govideo.AudioExportOptions{})

	if want.frames() != 86400 || got.frames() != 86400 { // 1.8 s
		t.Fatalf("%d frames from the video and %d direct, want 86400", want.frames(), got.frames())
	}
	// The video's samples have been through 16 bits twice.
	worst, at := 0.0, 0
	for i := range want.s {
		if d := math.Abs(want.s[i] - got.s[i]); d > worst {
			worst, at = d, i
		}
	}
	if worst > 2.0/32768 {
		t.Errorf("the audio-only export differs from the video's sound by %.5f at frame %d (%.3f s)", worst, at/2, float64(at/2)/48000)
	}
	if l := got.level(0, 0, 86400); l < 0.05 {
		t.Errorf("level %.4f: the export is all but silent", l)
	}
}

// TestExportAudioSources takes the sound from files: a trimmed clip with a
// gain, a muted clip, and a file that replaces the programme for a while.
// The source has its tone on the left channel only.
func TestExportAudioSources(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "left.wav")
	writeTone(t, src, 44100, 3, 500, 0.5, 0)

	tl := govideo.NewTimeline(govideo.Canvas{})
	tl.AddClip(src, govideo.ClipOptions{Trim: govideo.Range(time.Second, 2*time.Second), Gain: 0.5})
	tl.AddGenerated(500*time.Millisecond, govideo.GenerateOptions{})
	tl.AddClip(src, govideo.ClipOptions{Trim: govideo.Range(0, 500*time.Millisecond), Mute: true})
	tl.AddAudio(src, govideo.AudioOptions{Mode: govideo.Replace, Start: time.Second, Trim: govideo.Range(0, 250*time.Millisecond)})
	const half = 0.5 / math.Sqrt2 // the RMS of the source's tone

	st := exportAudio(t, tl, filepath.Join(dir, "stereo.wav"), govideo.AudioExportOptions{})
	if st.frames() != 96000 {
		t.Fatalf("%d frames, want 96000", st.frames())
	}
	if l, r := st.level(0, 480, 47520), st.level(1, 480, 47520); !within(l, half/2, 0.002) || r > 1e-4 {
		t.Errorf("clip: left %.4f and right %.4f, want %.4f (the tone at half gain) and silence", l, r, half/2)
	}
	if l := st.level(0, 48480, 59520); !within(l, half, 0.002) {
		t.Errorf("replacing file: left %.4f, want %.4f", l, half)
	}
	if l := st.level(0, 60480, 96000); l > 1e-4 {
		t.Errorf("silence and muted clip: left %.5f", l)
	}

	mono := exportAudio(t, tl, filepath.Join(dir, "mono.wav"), govideo.AudioExportOptions{SampleRate: 16000, Channels: 1})
	if mono.frames() != 32000 {
		t.Fatalf("%d frames, want 32000", mono.frames())
	}
	// One channel of two: the mono mix holds it at half level.
	if l := mono.level(0, 160, 15840); !within(l, half/4, 0.002) {
		t.Errorf("clip in mono: %.4f, want %.4f", l, half/4)
	}
	if l := mono.level(0, 16160, 19840); !within(l, half/2, 0.002) {
		t.Errorf("replacing file in mono: %.4f, want %.4f", l, half/2)
	}
	if f := mono.freq(0, 0, 16000); !within(f, 500, 2) {
		t.Errorf("tone at %.0f Hz, want 500", f)
	}
}

// TestExportAudioM4A writes AAC in several layouts and reads each file back
// through the module's own decoders: it must hold the sound of the WAVE
// export of the same timeline, in step with it to the sample, which it is
// only if the encoder's priming is cut by the file's edit list. The beeps
// make a slip of a sample audible to the comparison.
func TestExportAudioM4A(t *testing.T) {
	requireOS(t)
	dir := t.TempDir()
	ctx := context.Background()
	tl := govideo.NewTimeline(govideo.Canvas{})
	tl.AddGenerated(2510500*time.Microsecond, govideo.GenerateOptions{Tone: 1000, Beep: true})
	tl.AddGenerated(700*time.Millisecond, govideo.GenerateOptions{Tone: 440, Transition: govideo.Transition{Duration: 300 * time.Millisecond}})
	const total48 = 139704 // 2.9105 s

	for _, c := range []struct {
		rate, channels, bitrate int
		minSNR                  float64
	}{
		{16000, 1, 0, 20},        // for speech recognition: the encoder's own bit rate
		{48000, 2, 0, 30},        // the defaults
		{44100, 2, 96000, 30},    // a rate asked for
		{8000, 1, 1_000_000, 15}, // far more than the encoder has for the layout
		{24000, 2, 1, 15},        // and far less
	} {
		t.Run(fmt.Sprintf("%d Hz, %d channels, %d bit/s", c.rate, c.channels, c.bitrate), func(t *testing.T) {
			name := fmt.Sprintf("%d-%d", c.rate, c.channels)
			o := govideo.AudioExportOptions{SampleRate: c.rate, Channels: c.channels}
			ref := exportAudio(t, tl, filepath.Join(dir, name+".wav"), o)
			frames := (total48*c.rate + 24000) / 48000
			if ref.frames() != frames {
				t.Fatalf("the WAVE export has %d frames, want %d", ref.frames(), frames)
			}
			m4a := filepath.Join(dir, name+".m4a")
			o.Bitrate = c.bitrate
			if err := tl.ExportAudio(ctx, m4a, o); err != nil {
				t.Fatal(err)
			}

			info, err := videoprobe.ProbeFile(ctx, m4a)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Streams) != 1 {
				t.Fatalf("%d streams, want 1", len(info.Streams))
			}
			s := info.Streams[0]
			if s.Codec != "aac" || s.Profile != "LC" || s.Audio.SampleRate != c.rate || s.Audio.Channels != c.channels || !info.FastStart {
				t.Errorf("videoprobe: %s %s, %d Hz, %d channels, faststart %v", s.Codec, s.Profile, s.Audio.SampleRate, s.Audio.Channels, info.FastStart)
			}
			// The file plays for the samples of the programme, exactly.
			if want := float64(frames) / float64(c.rate); !within(info.Duration.Seconds(), want, 1e-6) || !within(s.Duration.Seconds(), want, 1e-6) {
				t.Errorf("videoprobe: file %v and stream %v long, want %.6f s", info.Duration, s.Duration, want)
			}

			// Back through the decoder, to the same layout.
			back := govideo.NewTimeline(govideo.Canvas{})
			back.AddClip(m4a, govideo.ClipOptions{})
			got := exportAudio(t, back, filepath.Join(dir, name+".back.wav"), govideo.AudioExportOptions{SampleRate: c.rate, Channels: c.channels})
			if got.frames() != frames {
				t.Fatalf("%d frames decoded from the .m4a, want %d", got.frames(), frames)
			}
			if db := snr(t, ref, got); db < c.minSNR {
				t.Errorf("the .m4a against the WAVE export: %.1f dB, want %.0f: not the same sound, or out of step", db, c.minSNR)
			} else {
				t.Logf("%.1f dB against the WAVE export; %d bit/s; %d bytes", db, s.BitRate, info.Size)
			}

			// And as ffmpeg reads it.
			t.Run("oracle", func(t *testing.T) {
				oracle.Require(t)
				r := oracle.FFprobe(t, m4a)
				if len(r.Streams) != 1 {
					t.Fatalf("ffprobe: %d streams", len(r.Streams))
				}
				fs := r.Streams[0]
				if fs.CodecName != "aac" || fs.Profile != "LC" || fs.SampleRate != fmt.Sprint(c.rate) || fs.Channels != c.channels {
					t.Errorf("ffprobe: %s %s, %s Hz, %d channels", fs.CodecName, fs.Profile, fs.SampleRate, fs.Channels)
				}
				if !strings.Contains(r.Format.FormatName, "m4a") {
					t.Errorf("ffprobe: format %q", r.Format.FormatName)
				}
				raw := oracle.FFmpegOutput(t, "-i", m4a, "-f", "s16le", "-")
				dec := pcmFile{rate: c.rate, channels: c.channels, s: make([]float64, len(raw)/2)}
				for i := range dec.s {
					dec.s[i] = float64(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
				}
				if dec.frames() != frames {
					t.Fatalf("ffmpeg decodes %d frames, want %d", dec.frames(), frames)
				}
				if db := snr(t, ref, dec); db < c.minSNR {
					t.Errorf("ffmpeg's decode against the WAVE export: %.1f dB, want %.0f", db, c.minSNR)
				}
			})
		})
	}
}

// TestExportAudioFLAC writes each layout as WAVE and as FLAC. FLAC is
// lossless, so the two must hold the same samples, and that is checked
// three ways. The checksum in the FLAC header is that of the WAVE file's
// samples. The file is, byte for byte, the header and frames the encoder
// gives for those samples, and the encoder's own tests decode such frames
// back to what went in (codec/native). And where the oracle is installed,
// ffmpeg decodes the file to the WAVE file's samples.
func TestExportAudioFLAC(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src := filepath.Join(dir, "src.wav")
	writeTone(t, src, 44100, 2, 330, 0.6, 0.2) // louder on the left
	tl := govideo.NewTimeline(govideo.Canvas{})
	tl.AddClip(src, govideo.ClipOptions{})
	tl.AddGenerated(300*time.Millisecond, govideo.GenerateOptions{})
	tl.AddGenerated(1210500*time.Microsecond, govideo.GenerateOptions{Tone: 1000, Beep: true})
	tl.AddGenerated(700*time.Millisecond, govideo.GenerateOptions{Tone: 440, Transition: govideo.Transition{Duration: 300 * time.Millisecond}})
	const total48 = 187704 // 3.9105 s

	for _, c := range []struct{ rate, channels int }{
		{0, 0},     // the defaults: 48 kHz stereo
		{16000, 1}, // for speech recognition
		{44100, 2},
		{22050, 1},
		{96000, 2},
		{37000, 1}, // a rate the frame header has no code for
	} {
		rate, channels := c.rate, c.channels
		if rate == 0 {
			rate, channels = 48000, 2
		}
		t.Run(fmt.Sprintf("%d Hz, %d channels", rate, channels), func(t *testing.T) {
			name := fmt.Sprintf("%d-%d", rate, channels)
			o := govideo.AudioExportOptions{SampleRate: c.rate, Channels: c.channels}
			ref := exportAudio(t, tl, filepath.Join(dir, name+".wav"), o)
			frames := (total48*rate + 24000) / 48000
			if ref.frames() != frames || ref.rate != rate || ref.channels != channels {
				t.Fatalf("the WAVE export has %d frames at %d Hz in %d channels, want %d at %d Hz in %d",
					ref.frames(), ref.rate, ref.channels, frames, rate, channels)
			}
			wave, err := os.ReadFile(filepath.Join(dir, name+".wav"))
			if err != nil {
				t.Fatal(err)
			}
			pcm := wave[wav.HeaderSize:]

			path := filepath.Join(dir, name+".flac")
			var last govideo.Progress
			o.Progress = func(p govideo.Progress) { last = p }
			if err := tl.ExportAudio(ctx, path, o); err != nil {
				t.Fatal(err)
			}
			if last.Frame != int64(frames) || last.TotalFrames != int64(frames) {
				t.Errorf("last progress call %+v, want frame %d of %d", last, frames, frames)
			}
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(file) < native.FLACHeaderSize || string(file[:4]) != "fLaC" {
				t.Fatalf("%d bytes, starting %q", len(file), file[:min(4, len(file))])
			}
			a, err := codechdr.ParseFLACStreamInfo(file[:native.FLACHeaderSize])
			if err != nil {
				t.Fatal(err)
			}
			if a.SampleRate != rate || a.Channels != channels || a.BitDepth != 16 || a.TotalSamples != int64(frames) {
				t.Errorf("STREAMINFO: %d samples of %d bits at %d Hz in %d channels, want %d of 16 bits at %d Hz in %d",
					a.TotalSamples, a.BitDepth, a.SampleRate, a.Channels, frames, rate, channels)
			}
			if got, want := file[native.FLACHeaderSize-16:native.FLACHeaderSize], md5.Sum(pcm); !bytes.Equal(got, want[:]) {
				t.Errorf("STREAMINFO MD5 %x; the WAVE file's samples have %x", got, want)
			}

			// The frames of those samples, from the encoder directly.
			enc, err := native.NewFLACEncoder(rate, channels)
			if err != nil {
				t.Fatal(err)
			}
			in := make([]float32, len(pcm)/2)
			for i := range in {
				in[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32767
			}
			var body []byte
			for _, step := range []func() ([][]byte, error){func() ([][]byte, error) { return enc.Encode(in) }, enc.Flush} {
				packets, err := step()
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range packets {
					body = append(body, p...)
				}
			}
			if want := append(enc.Header(), body...); !bytes.Equal(file, want) {
				t.Errorf("the file (%d bytes) is not the header and frames of the WAVE file's samples (%d bytes)", len(file), len(want))
			}
			if len(file) > len(pcm)/2 {
				t.Errorf("%d bytes for %d of PCM: tones should compress to less than half", len(file), len(pcm))
			}
			t.Logf("%d bytes, %.3f of the PCM", len(file), float64(len(file))/float64(len(pcm)))

			t.Run("oracle", func(t *testing.T) {
				oracle.Require(t)
				r := oracle.FFprobe(t, path)
				if len(r.Streams) != 1 {
					t.Fatalf("ffprobe: %d streams", len(r.Streams))
				}
				fs := r.Streams[0]
				dur, _ := oracle.Seconds(fs.Duration)
				if r.Format.FormatName != "flac" || fs.CodecName != "flac" || fs.SampleRate != fmt.Sprint(rate) || fs.Channels != channels ||
					!within(dur, float64(frames)/float64(rate), 1e-5) {
					t.Errorf("ffprobe: %s, %s at %s Hz in %d channels, %s s; want flac at %d Hz in %d channels, %.6f s",
						r.Format.FormatName, fs.CodecName, fs.SampleRate, fs.Channels, fs.Duration, rate, channels, float64(frames)/float64(rate))
				}
				if raw := oracle.FFmpegOutput(t, "-i", path, "-c:a", "pcm_s16le", "-f", "s16le", "-"); !bytes.Equal(raw, pcm) {
					t.Errorf("ffmpeg decodes %d bytes that are not the %d of the WAVE file", len(raw), len(pcm))
				}
			})
		})
	}

	// What is written cannot be read back yet: there is no FLAC decoder,
	// and the error says so.
	back := govideo.NewTimeline(govideo.Canvas{})
	back.AddClip(filepath.Join(dir, "16000-1.flac"), govideo.ClipOptions{})
	var cu *govideo.ErrCodecUnavailable
	if err := back.ExportAudio(ctx, filepath.Join(dir, "back.wav"), govideo.AudioExportOptions{}); !errors.As(err, &cu) || cu.Codec != "flac" || cu.Op != govideo.Decode {
		t.Errorf("a FLAC file as a clip: %v, want ErrCodecUnavailable for flac decode", err)
	}
}

// TestExportAudioErrors checks what ExportAudio refuses, and that a refusal
// or a cancelled export leaves no file behind.
func TestExportAudioErrors(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tone := func(d time.Duration) *govideo.Timeline {
		tl := govideo.NewTimeline(govideo.Canvas{})
		tl.AddGenerated(d, govideo.GenerateOptions{Tone: 440})
		return tl
	}
	missing := govideo.NewTimeline(govideo.Canvas{})
	missing.AddClip(filepath.Join(dir, "nothing.wav"), govideo.ClipOptions{})
	first := govideo.NewTimeline(govideo.Canvas{})
	first.AddGenerated(time.Second, govideo.GenerateOptions{Transition: govideo.Transition{Duration: time.Second / 2}})

	for _, c := range []struct {
		name string
		tl   *govideo.Timeline
		file string
		o    govideo.AudioExportOptions
		want string // part of the message
	}{
		{"a format there is no encoder for", tone(time.Second), "a.mp3", govideo.AudioExportOptions{}, ".wav, .flac or .m4a"},
		{"a video extension", tone(time.Second), "a.mp4", govideo.AudioExportOptions{}, ".wav, .flac or .m4a"},
		{"three channels", tone(time.Second), "a.wav", govideo.AudioExportOptions{Channels: 3}, "1 or 2 channels"},
		{"a negative channel count", tone(time.Second), "a.wav", govideo.AudioExportOptions{Channels: -1}, "1 or 2 channels"},
		{"a sample rate too low", tone(time.Second), "a.wav", govideo.AudioExportOptions{SampleRate: 500}, "sample rate"},
		{"a sample rate too high", tone(time.Second), "a.wav", govideo.AudioExportOptions{SampleRate: 1_000_000}, "sample rate"},
		{"an empty timeline", govideo.NewTimeline(govideo.Canvas{}), "a.wav", govideo.AudioExportOptions{}, "no clips"},
		{"a missing source", missing, "a.wav", govideo.AudioExportOptions{}, "nothing.wav"},
		{"a transition on the first segment", first, "a.wav", govideo.AudioExportOptions{}, "first segment"},
		// Seven hours at 48 kHz in stereo: more than a WAVE file's 4 GB,
		// known before any of it is rendered.
		{"more than a WAVE file holds", tone(7 * time.Hour), "a.wav", govideo.AudioExportOptions{}, "does not fit in a WAV file"},
	} {
		err := c.tl.ExportAudio(ctx, filepath.Join(out, c.file), c.o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error that mentions %q", c.name, err, c.want)
		}
	}

	// AAC comes from the system: without one the codec is missing, and
	// says so; with one, a sample rate it does not have is refused.
	err := tone(time.Second).ExportAudio(ctx, filepath.Join(out, "a.m4a"), govideo.AudioExportOptions{SampleRate: 96000})
	if govideo.BuildInfo().OSBackend {
		if err == nil || !strings.Contains(err.Error(), "96000 Hz") {
			t.Errorf("AAC at 96 kHz: %v, want an error that names the sample rate", err)
		}
	} else {
		var cu *govideo.ErrCodecUnavailable
		if !errors.As(err, &cu) || cu.Codec != "aac" || cu.Op != govideo.Encode {
			t.Errorf(".m4a with no system encoder: %v, want ErrCodecUnavailable for aac encode", err)
		}
	}

	// Cancelled part of the way through.
	cancelling, cancel := context.WithCancel(ctx)
	o := govideo.AudioExportOptions{Progress: func(p govideo.Progress) {
		if p.Time >= time.Second {
			cancel()
		}
	}}
	if err := tone(time.Minute).ExportAudio(cancelling, filepath.Join(out, "long.wav"), o); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled export: %v, want context.Canceled", err)
	}
	cancel()

	left, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("a failed export left %s behind", e.Name())
	}

	// An export replaces a file that is there, and only once it is whole.
	path := filepath.Join(out, "a.wav")
	if err := os.WriteFile(path, []byte("what was here before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := missing.ExportAudio(ctx, path, govideo.AudioExportOptions{}); err == nil {
		t.Fatal("missing source: no error")
	}
	if b, _ := os.ReadFile(path); string(b) != "what was here before" {
		t.Errorf("a failed export changed the file that was at its path")
	}
	if a := exportAudio(t, tone(time.Second/10), path, govideo.AudioExportOptions{}); a.frames() != 4800 {
		t.Errorf("%d frames, want 4800", a.frames())
	}
}
