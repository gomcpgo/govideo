package govideo_test

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// These tests pin down behaviour that real camera and phone files exposed.

// TestMirroredDisplayMatrix covers clips whose display matrix reflects the
// picture (front cameras write them).
func TestMirroredDisplayMatrix(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 30, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	in := filepath.Join(dir, "flipped.mov")
	if err := oracle.TryFFmpeg("-display_hflip", "-i", src, "-c", "copy", in); err != nil {
		oracle.Missing(t, "the oracle cannot write a reflecting display matrix: %v", err)
	}
	info, err := videoprobe.ProbeFile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if v := info.Streams[0].Video; !v.Mirrored || v.Rotation != 0 {
		t.Fatalf("probe: rotation %d mirrored %v, want 0 true", v.Rotation, v.Mirrored)
	}
	out := filepath.Join(dir, "out.mov")
	transcode(t, in, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 30 {
		t.Fatalf("%d frames, want 30", len(frames))
	}
	// The bars now run right to left.
	for i := 0; i < 7; i++ {
		want := process.BarColor(6 - i)
		r, g, b := rgb(frames[0], (2*i+1)*cardW/14, cardH/10)
		if !near(r, int(want.R), 14) || !near(g, int(want.G), 14) || !near(b, int(want.B), 14) {
			t.Errorf("bar %d is rgb(%d,%d,%d), want the mirrored rgb(%d,%d,%d)", i, r, g, b, want.R, want.G, want.B)
		}
	}
}

// TestTruncatedFile covers a file cut short (an interrupted copy or
// download): what is there plays, and the export does not fail.
func TestTruncatedFile(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 60, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mov")
	if err := os.WriteFile(cut, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mov")
	transcode(t, cut, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 60 {
		t.Fatalf("%d frames, want 60 (the index still describes the whole clip)", len(frames))
	}
	lastGood := -1
	for i, f := range frames {
		n := counter(f)
		switch {
		case n == i && lastGood == i-1:
			lastGood = i
		case n != lastGood:
			t.Fatalf("output frame %d shows source frame %d; want it to hold frame %d", i, n, lastGood)
		}
	}
	if lastGood < 15 || lastGood > 45 {
		t.Errorf("playback held from frame %d; about half of 60 frames survive the cut", lastGood)
	}
}

// TestTruncatedFileTrim covers a trim that starts past the point where a
// file was cut short. The index still lists sync samples there, but a seek
// must not start at one that was never written: the picture holds on the
// last complete frame, as it does without the trim.
func TestTruncatedFileTrim(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 60, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	whole := filepath.Join(dir, "whole.mp4")
	if err := oracle.TryFFmpeg("-i", src, "-c:v", "libx264", "-preset", "veryfast", "-crf", "16", "-pix_fmt", "yuv420p",
		"-g", "10", "-bf", "0", "-an", "-movflags", "+faststart", whole); err != nil {
		oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
	}
	data, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mp4")
	if err := os.WriteFile(cut, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mov")
	transcode(t, cut, out, govideo.Range(1500*time.Millisecond, 2*time.Second), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 15 {
		t.Fatalf("%d frames, want 15", len(frames))
	}
	held := counter(frames[0])
	for i, f := range frames {
		if n := counter(f); n != held {
			t.Fatalf("output frame %d shows source frame %d; want it to hold frame %d", i, n, held)
		}
	}
	if held < 15 || held > 44 {
		t.Errorf("playback holds frame %d; about half of 60 frames survive the cut, and the trim starts at frame 45", held)
	}
}

// TestSeekOpenGOP covers a trim into a stream whose sync samples are not
// IDR pictures. The pictures that follow such a sample in the file but come
// before it on screen refer to the group of pictures the seek skipped, and
// the system decoder, once fed them, failed on every later frame.
func TestSeekOpenGOP(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	for _, name := range []string{"opengop.mp4", "opengop.mkv"} {
		t.Run(name, func(t *testing.T) {
			in := filepath.Join(dir, name)
			// Interlaced coding with reference B-frames, as broadcast
			// encoders write it.
			if err := oracle.TryFFmpeg("-i", src,
				"-vf", "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt709",
				"-c:v", "libx264", "-profile:v", "main", "-preset", "medium", "-crf", "16", "-pix_fmt", "yuv420p",
				"-flags", "+ildct+ilme", "-bf", "3", "-refs", "4", "-g", "20", "-x264-params", "open-gop=1:b-pyramid=strict:b-adapt=0",
				"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv", "-an", in); err != nil {
				oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
			}
			out := filepath.Join(dir, name+".out.mov")
			transcode(t, in, out, govideo.Range(1700*time.Millisecond, 2500*time.Millisecond),
				govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, out, 51, 24, 14)
		})
	}
}

// TestEditListPreroll covers a file whose edit list starts between sync
// samples, as a stream copy cut with -ss writes it. The packets before time
// zero are not shown, but the first pictures refer to them: they have to be
// decoded, with or without a trim.
func TestEditListPreroll(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	whole := filepath.Join(dir, "whole.mp4")
	if err := oracle.TryFFmpeg("-i", src,
		"-vf", "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt709",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "16", "-pix_fmt", "yuv420p", "-bf", "2", "-g", "20",
		"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv", "-an", whole); err != nil {
		oracle.Missing(t, "the oracle cannot make this fixture: %v", err)
	}
	// Keyframes at frames 20 and 40; the copy starts at frame 30.
	in := filepath.Join(dir, "cut.mp4")
	oracle.FFmpeg(t, "-ss", "1", "-i", whole, "-c", "copy", in)
	out := filepath.Join(dir, "out.mov")
	transcode(t, in, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	checkCard(t, out, 30, 60, 14)
	trimmed := filepath.Join(dir, "trimmed.mov")
	transcode(t, in, trimmed, govideo.Range(500*time.Millisecond, time.Second), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	checkCard(t, trimmed, 45, 15, 14)
}

// TestMultichannelAACOrder checks that 5.1 AAC, which codes the centre
// channel first, is downmixed with left on the left, and at the ITU-R
// BS.775 levels without normalisation (govideo#2): fronts at unity, centre
// and surrounds at -3 dB.
func TestMultichannelAACOrder(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	const h = math.Sqrt2 / 2
	// lavfi's sine has a peak of 1/8.
	const tone = 0.125 / math.Sqrt2
	for _, c := range []struct {
		layout, channel string
		left, right     float64 // expected gain of the tone into each output channel
	}{
		{"5.1", "FL", 1, 0}, {"5.1", "FR", 0, 1}, {"5.1", "FC", h, h},
		{"5.1", "BL", h, 0}, {"5.1", "BR", 0, h}, {"5.1", "LFE", 0, 0},
		{"7.1", "FL", 1, 0}, {"7.1", "FR", 0, 1}, {"7.1", "FC", h, h}, {"7.1", "LFE", 0, 0},
		{"7.1", "SL", h, 0}, {"7.1", "SR", 0, h}, {"7.1", "BL", h, 0}, {"7.1", "BR", 0, h},
	} {
		t.Run(c.layout+"_"+c.channel, func(t *testing.T) {
			in := filepath.Join(dir, c.layout+c.channel+".m4a")
			if err := oracle.TryFFmpeg("-f", "lavfi", "-i", "sine=f=440:d=2:r=48000", "-af", "pan="+c.layout+"|"+c.channel+"=c0",
				"-c:a", "aac", "-b:a", "256k", in); err != nil {
				oracle.Missing(t, "the oracle cannot make %s AAC: %v", c.layout, err)
			}
			tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180, FrameRate: govideo.R(30, 1)})
			tl.AddGenerated(2*time.Second, govideo.GenerateOptions{})
			tl.AddAudio(in, govideo.AudioOptions{})
			out := filepath.Join(dir, c.layout+c.channel+".mov")
			if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
				t.Fatal(err)
			}
			l := oracle.Audio(t, out, "0:a:0", "-af", "pan=mono|c0=c0")
			r := oracle.Audio(t, out, "0:a:0", "-af", "pan=mono|c0=c1")
			level := func(s []float32) float64 {
				var sum float64
				for _, v := range s[9600:86400] {
					sum += float64(v) * float64(v)
				}
				return math.Sqrt(sum / 76800)
			}
			gl, gr := level(l), level(r)
			if (gl > 0.005) != (c.left > 0) || (gr > 0.005) != (c.right > 0) {
				t.Errorf("tone in %s came out at left %.4f, right %.4f", c.channel, gl, gr)
			}
			for _, side := range []struct{ got, gain float64 }{{gl, c.left}, {gr, c.right}} {
				if side.gain == 0 {
					continue
				}
				if db := 20 * math.Log10(side.got/(tone*side.gain)); math.Abs(db) > 0.5 {
					t.Errorf("tone in %s is %+.2f dB from the BS.775 downmix level (left %.4f, right %.4f)", c.channel, db, gl, gr)
				}
			}
		})
	}
}

// TestWebPRange checks that lossy WebP, which stores limited-range samples,
// keeps its black and white.
func TestWebPRange(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	webp, _ := writeWebP(t, dir, "white.webp", whiteWebP)
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	tl.AddImage(webp, time.Second/2, govideo.ImageOptions{})
	out := filepath.Join(dir, "out.mov")
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatal(err)
	}
	r, g, b := rgb(oracle.Frames(t, out, cardW, cardH)[0], cardW/2, cardH/2)
	if r < 247 || g < 247 || b < 247 {
		t.Errorf("white WebP came out rgb(%d,%d,%d)", r, g, b)
	}
}

// TestUnsupportedInputErrors checks that inputs govideo cannot read are
// refused with an error that says what they are.
func TestUnsupportedInputErrors(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	export := func(add func(*govideo.Timeline)) error {
		tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180})
		add(tl)
		return tl.Export(context.Background(), filepath.Join(dir, "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	}
	var cu *govideo.ErrCodecUnavailable
	var uf *videoprobe.UnsupportedFormatError

	// A bare audio file of a codec with no decoder is a missing codec.
	ac3 := filepath.Join(dir, "a.ac3")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "sine=f=440:d=1", "-c:a", "ac3", ac3)
	if err := export(func(tl *govideo.Timeline) { tl.AddClip(ac3, govideo.ClipOptions{}) }); !errors.As(err, &cu) || cu.Codec != "ac3" {
		t.Errorf("AC-3 file: %v, want ErrCodecUnavailable for ac3", err)
	}
	// A bare MPEG audio file of layer II is read by videoprobe, as an MP3
	// file is, but only layer III has a decoder.
	mp2 := filepath.Join(dir, "a.mp2")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "sine=f=440:d=1", "-c:a", "mp2", mp2)
	for name, add := range map[string]func(*govideo.Timeline){
		"clip": func(tl *govideo.Timeline) { tl.AddClip(mp2, govideo.ClipOptions{}) },
		"audio": func(tl *govideo.Timeline) {
			tl.AddGenerated(time.Second, govideo.GenerateOptions{})
			tl.AddAudio(mp2, govideo.AudioOptions{})
		},
	} {
		if err := export(add); !errors.As(err, &cu) || cu.Codec != "mp2" || cu.Reason == "" {
			t.Errorf("MP2 file as %s: %v, want ErrCodecUnavailable for mp2", name, err)
		}
	}
	ts := filepath.Join(dir, "a.ts")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=160x120:r=25", "-c:v", "mpeg2video", "-f", "mpegts", ts)
	if err := export(func(tl *govideo.Timeline) { tl.AddClip(ts, govideo.ClipOptions{}) }); !errors.As(err, &uf) || uf.Format != "mpegts" {
		t.Errorf("transport stream: %v, want UnsupportedFormatError for mpegts", err)
	}
	heic := filepath.Join(dir, "a.heic")
	if err := os.WriteFile(heic, []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic\x00\x00\x00\x08free"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := export(func(tl *govideo.Timeline) { tl.AddImage(heic, time.Second, govideo.ImageOptions{}) }); !errors.As(err, &cu) || cu.Codec != "heic" {
		t.Errorf("HEIC still: %v, want ErrCodecUnavailable for heic", err)
	}
}

// TestLowBitrateAACInPlace covers a video whose sound is encoded at a low bit
// rate. The system encoder's delay is different there from what it reports
// in its default mode, and the track's edit list is made from the report: at
// 64 kbit/s the sound of a 48 kHz stereo track came 1024 samples (21 ms)
// before the picture. The sound of the video is compared with the same
// timeline's sound written as PCM; the beeps show a slip of a sample.
func TestLowBitrateAACInPlace(t *testing.T) {
	requireOS(t)
	dir := t.TempDir()
	tl := govideo.NewTimeline(govideo.Canvas{Width: 160, Height: 120, FrameRate: govideo.R(25, 1)})
	tl.AddGenerated(2*time.Second, govideo.GenerateOptions{Pattern: govideo.PatternTestCard, Tone: 1000, Beep: true})
	want := exportAudio(t, tl, filepath.Join(dir, "sound.wav"), govideo.AudioExportOptions{})
	for _, rate := range []int{64000, 192000} {
		video := filepath.Join(dir, "video.mp4")
		if err := tl.Export(context.Background(), video, govideo.ExportOptions{Profile: govideo.ProfileCompatible, AudioBitrate: rate}); err != nil {
			t.Fatal(err)
		}
		back := govideo.NewTimeline(govideo.Canvas{})
		back.AddClip(video, govideo.ClipOptions{})
		got := exportAudio(t, back, filepath.Join(dir, "video.wav"), govideo.AudioExportOptions{})
		if db := snr(t, want, got); db < 25 {
			t.Errorf("AAC at %d bit/s: the video's sound against the programme: %.1f dB, want 25 or more: it is out of place", rate, db)
		}
	}
}
