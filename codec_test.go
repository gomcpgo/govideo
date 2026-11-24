package govideo_test

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
)

// The codec tests are built on the animated test card: every frame carries
// its own number as sixteen black or white cells, so a decoded file shows
// exactly which frames it holds and in what order, and the beep at the start
// of every second shows where the audio sits against the picture.

const (
	cardW, cardH = 640, 360
	cardFPS      = 30
)

// requireOS skips tests that need the operating-system codecs.
func requireOS(t *testing.T) {
	t.Helper()
	if !govideo.BuildInfo().OSBackend {
		t.Skip("no operating-system codec backend on this platform")
	}
}

// exportCard writes a test card of n frames with a 1 kHz beep each second.
func exportCard(t *testing.T, path string, frames int, o govideo.ExportOptions) {
	t.Helper()
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	tl.AddGenerated(time.Duration(frames)*time.Second/cardFPS, govideo.GenerateOptions{
		Pattern: govideo.PatternTestCard, Tone: 1000, Beep: true})
	if err := tl.Export(context.Background(), path, o); err != nil {
		t.Fatalf("Export %s: %v", filepath.Base(path), err)
	}
}

// transcode re-exports src through govideo's decoders at the card's size and
// rate, into the intermediate profile (whose decoding by the oracle is not
// in doubt), optionally trimmed.
func transcode(t *testing.T, src, dst string, trim govideo.TimeRange, o govideo.ExportOptions) {
	t.Helper()
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	tl.AddClip(src, govideo.ClipOptions{Trim: trim})
	if err := retryOnVM(t, func() error { return tl.Export(context.Background(), dst, o) }); err != nil {
		t.Fatalf("Export %s from %s: %v", filepath.Base(dst), filepath.Base(src), err)
	}
}

func rgb(frame []byte, x, y int) (r, g, b int) {
	p := frame[(y*cardW+x)*3:]
	return int(p[0]), int(p[1]), int(p[2])
}

// counter reads the frame number from a decoded test card frame.
func counter(frame []byte) int {
	n := 0
	for i := 0; i < process.CounterBits; i++ {
		c := process.CounterCell(cardW, cardH, i)
		r, g, b := rgb(frame, (c.Min.X+c.Max.X)/2, (c.Min.Y+c.Max.Y)/2)
		n <<= 1
		if r+g+b > 3*128 {
			n |= 1
		}
	}
	return n
}

// checkCard verifies that path holds test card frames first, first+1, ...
// (count of them), with the colour bars in the right colours.
func checkCard(t *testing.T, path string, first, count, tol int) {
	t.Helper()
	frames := oracle.Frames(t, path, cardW, cardH)
	if len(frames) != count {
		t.Errorf("%s: %d frames, want %d", filepath.Base(path), len(frames), count)
	}
	bad := 0
	for i, f := range frames {
		if got := counter(f); got != first+i && bad < 5 {
			t.Errorf("%s: output frame %d shows source frame %d, want %d", filepath.Base(path), i, got, first+i)
			bad++
		}
	}
	if len(frames) == 0 {
		return
	}
	for _, f := range [][]byte{frames[0], frames[len(frames)-1]} {
		for i := 0; i < 7; i++ {
			want := process.BarColor(i)
			r, g, b := rgb(f, (2*i+1)*cardW/14, cardH/10)
			if !near(r, int(want.R), tol) || !near(g, int(want.G), tol) || !near(b, int(want.B), tol) {
				t.Errorf("%s: bar %d is rgb(%d,%d,%d), want rgb(%d,%d,%d) ±%d", filepath.Base(path), i, r, g, b, want.R, want.G, want.B, tol)
			}
		}
	}
}

// beepOffset returns how many samples late the beep expected at sample
// position at is, found by correlating with the beep's known shape.
func beepOffset(au []float32, at int) (lag int, found bool) {
	const n, ramp, span = 4800, 240, 2400
	ref := make([]float64, n)
	for i := range ref {
		g := 0.25
		if i < ramp {
			g *= float64(i) / ramp
		} else if i >= n-ramp {
			g *= float64(n-i) / ramp
		}
		ref[i] = g * math.Sin(2*math.Pi*1000*float64(i)/48000)
	}
	best := 0.0
	for k := -span; k <= span; k++ {
		if at+k < 0 || at+k+n > len(au) {
			continue
		}
		var c float64
		for i, r := range ref {
			c += r * float64(au[at+k+i])
		}
		if c > best {
			best, lag = c, k
		}
	}
	// A real beep correlates at about n * 0.25² / 2 = 150.
	return lag, best > 75
}

// checkBeeps verifies that a beep starts at each of the given times (in
// seconds), to within tol samples, in the first audio track of path, with
// silence in between.
func checkBeeps(t *testing.T, path string, tol int, at ...float64) {
	t.Helper()
	au := oracle.Audio(t, path, "0:a:0")
	for _, sec := range at {
		lag, ok := beepOffset(au, int(sec*48000))
		if !ok {
			t.Errorf("%s: no beep near %.2fs", filepath.Base(path), sec)
		} else if lag < -tol || lag > tol {
			t.Errorf("%s: beep at %.2fs is %d samples (%.2f ms) off", filepath.Base(path), sec, lag, float64(lag)/48)
		}
		a, b := int((sec+0.3)*48000), int((sec+0.9)*48000)
		if b > len(au) {
			continue
		}
		var sum float64
		for _, v := range au[a:b] {
			sum += float64(v) * float64(v)
		}
		if r := math.Sqrt(sum / float64(b-a)); r > 0.003 {
			t.Errorf("%s: RMS %.4f between beeps after %.2fs, want silence", filepath.Base(path), r, sec)
		}
	}
}

// TestGenerated checks the generated patterns, title text and tones through
// the native intermediate profile.
func TestGenerated(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "gen.mov")
	tl := govideo.NewTimeline(govideo.Canvas{Width: cardW, Height: cardH, FrameRate: govideo.R(cardFPS, 1)})
	sec := time.Second
	tl.AddGenerated(sec, govideo.GenerateOptions{Color: color.RGBA{200, 40, 40, 255}})                                  // 0-1 solid red
	tl.AddGenerated(sec, govideo.GenerateOptions{Pattern: govideo.PatternBars, Tone: 440, ToneGain: 0.5})               // 1-2 bars + tone
	tl.AddGenerated(sec, govideo.GenerateOptions{Pattern: govideo.PatternGradient, Color2: color.RGBA{0, 0, 255, 255}}) // 2-3 black to blue
	tl.AddGenerated(sec, govideo.GenerateOptions{Pattern: govideo.PatternChecker, CellSize: 40, Color: color.White,     // 3-4 checker
		Color2: color.RGBA{0, 160, 0, 255}})
	tl.AddGenerated(sec, govideo.GenerateOptions{Color: color.RGBA{0, 0, 120, 255}, Text: "Title\ncard", TextSize: 120}) // 4-5 title
	tl.AddGenerated(sec, govideo.GenerateOptions{Pattern: govideo.PatternTestCard, Tone: 1000, Beep: true})              // 5-6 card
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatal(err)
	}
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 6*cardFPS {
		t.Fatalf("%d frames, want %d", len(frames), 6*cardFPS)
	}
	type px struct {
		frame, x, y int
		r, g, b     int
		what        string
	}
	checks := []px{
		{15, 320, 180, 200, 40, 40, "solid"},
		{45, 45, 100, 191, 191, 191, "bars: grey"},
		{45, 594, 100, 0, 0, 191, "bars: blue"},
		{45, 2, 330, 0, 0, 0, "bars: ramp start"},
		{45, 637, 330, 255, 255, 255, "bars: ramp end"},
		{75, 2, 180, 0, 0, 0, "gradient start"},
		{75, 320, 180, 0, 0, 128, "gradient middle"},
		{75, 637, 180, 0, 0, 255, "gradient end"},
		{105, 20, 20, 255, 255, 255, "checker cell (0,0)"},
		{105, 60, 20, 0, 160, 0, "checker cell (1,0)"},
		{105, 60, 60, 255, 255, 255, "checker cell (1,1)"},
		{135, 10, 10, 0, 0, 120, "title background"},
	}
	for _, c := range checks {
		r, g, b := rgb(frames[c.frame], c.x, c.y)
		if !near(r, c.r, 10) || !near(g, c.g, 10) || !near(b, c.b, 10) {
			t.Errorf("%s: rgb(%d,%d,%d), want rgb(%d,%d,%d)", c.what, r, g, b, c.r, c.g, c.b)
		}
	}
	// The title is white text in the middle band of the picture, and
	// nowhere else.
	white := func(f []byte, y0, y1 int) (n int) {
		for y := y0; y < y1; y++ {
			for x := 0; x < cardW; x++ {
				if r, g, b := rgb(f, x, y); r > 200 && g > 200 && b > 200 {
					n++
				}
			}
		}
		return n
	}
	if n := white(frames[135], 60, 300); n < 5000 {
		t.Errorf("title text covers %d pixels, want at least 5000", n)
	}
	if n := white(frames[135], 0, 30) + white(frames[135], 330, 360); n != 0 {
		t.Errorf("title text spills to the picture edges (%d pixels)", n)
	}
	for i := 0; i < cardFPS; i++ {
		if got := counter(frames[5*cardFPS+i]); got != i {
			t.Errorf("test card frame %d reads %d", i, got)
		}
	}
	// The sweep bar crosses the picture once a second.
	lit := func(f []byte) int {
		for x := 0; x < cardW; x++ {
			if r, g, b := rgb(f, x, cardH/2+cardH/16); r > 230 && g > 230 && b > 230 {
				return x
			}
		}
		return -1
	}
	if a, b := lit(frames[5*cardFPS+3]), lit(frames[5*cardFPS+27]); a < 40 || a > 90 || b < 520 || b > 600 {
		t.Errorf("sweep bar at x=%d on frame 3 and x=%d on frame 27", a, b)
	}

	au := decodeAudio(t, out, "0:a:0")
	if got := rms(au, 0.1, 0.9); got > 0.001 {
		t.Errorf("solid segment should be silent, RMS %.4f", got)
	}
	if got, want := rms(au, 1.1, 1.9), 0.5/math.Sqrt2; math.Abs(got-want) > 0.01 {
		t.Errorf("tone RMS %.4f, want %.4f", got, want)
	}
	checkBeeps(t, out, 1, 5)
}

// TestCompatibleExport encodes the test card with the operating system's
// H.264 and AAC encoders and has the oracle confirm the result.
func TestCompatibleExport(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	for _, name := range []string{"card.mp4", "card.mov"} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(dir, name)
			exportCard(t, out, 90, govideo.ExportOptions{Profile: govideo.ProfileCompatible})
			if err := oracle.TryFFmpeg("-xerror", "-i", out, "-f", "null", "-"); err != nil {
				t.Fatalf("ffmpeg cannot decode the export: %v", err)
			}
			pr := oracle.FFprobe(t, out, "-count_packets")
			if len(pr.Streams) != 2 {
				t.Fatalf("%d streams, want 2", len(pr.Streams))
			}
			v, a := pr.Streams[0], pr.Streams[1]
			if v.CodecName != "h264" || v.Profile != "High" || v.PixFmt != "yuv420p" || v.Width != cardW || v.Height != cardH ||
				v.RFrameRate != "30/1" || v.NbReadPackets != "90" {
				t.Errorf("video: %s %s %s %dx%d %s, %s packets", v.CodecName, v.Profile, v.PixFmt, v.Width, v.Height, v.RFrameRate, v.NbReadPackets)
			}
			if v.ColorSpace != "bt709" || v.ColorPrim != "bt709" || v.ColorTransfer != "bt709" || v.ColorRange != "tv" {
				t.Errorf("colour tags: matrix %q primaries %q transfer %q range %q, want BT.709 limited",
					v.ColorSpace, v.ColorPrim, v.ColorTransfer, v.ColorRange)
			}
			if a.CodecName != "aac" || a.Profile != "LC" || a.SampleRate != "48000" || a.Channels != 2 {
				t.Errorf("audio: %s %s %s Hz %d ch", a.CodecName, a.Profile, a.SampleRate, a.Channels)
			}
			for i, st := range pr.Streams {
				if d, _ := oracle.Seconds(st.Duration); math.Abs(d-3) > 0.001 {
					t.Errorf("stream %d lasts %.4fs, want 3.0000", i, d)
				}
				if s, _ := oracle.Seconds(st.StartTime); s != 0 {
					t.Errorf("stream %d starts at %.4fs, want 0", i, s)
				}
			}
			if br, _ := oracle.Seconds(v.BitRate); br < 100_000 || br > 2_500_000 {
				t.Errorf("video bit rate %.0f is implausible for %dx%d", br, cardW, cardH)
			}
			checkCard(t, out, 0, 90, 12)
			checkBeeps(t, out, 12, 0, 1, 2)

			// And back in through govideo's own demuxer and the system
			// decoders: the frames and the beeps must still line up.
			back := filepath.Join(dir, name+".back.mov")
			transcode(t, out, back, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, back, 0, 90, 14)
			checkBeeps(t, back, 12, 0, 1, 2)
		})
	}
}

// TestExportBitrates checks that Quality and the explicit bit rates steer
// the encoders.
func TestExportBitrates(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	noise := filepath.Join(dir, "noise.wav")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "anoisesrc=d=4:c=pink:r=48000:a=0.5", "-ac", "2", "-c:a", "pcm_s16le", noise)
	rate := func(name string, o govideo.ExportOptions) (video, audio float64) {
		out := filepath.Join(dir, name)
		tl := govideo.NewTimeline(govideo.Canvas{Width: 1280, Height: 720, FrameRate: govideo.R(30, 1)})
		tl.AddGenerated(4*time.Second, govideo.GenerateOptions{Pattern: govideo.PatternTestCard})
		tl.AddAudio(noise, govideo.AudioOptions{}) // noise needs every bit the encoder is allowed
		o.Profile = govideo.ProfileCompatible
		if err := tl.Export(context.Background(), out, o); err != nil {
			t.Fatal(err)
		}
		pr := oracle.FFprobe(t, out)
		video, _ = oracle.Seconds(pr.Streams[0].BitRate)
		audio, _ = oracle.Seconds(pr.Streams[1].BitRate)
		return video, audio
	}
	_, aHigh := rate("high.mp4", govideo.ExportOptions{})
	_, aLow := rate("low.mp4", govideo.ExportOptions{Quality: govideo.QualityLow})
	// The system encoder treats the rate as a long-term average and stays
	// under it on a short clip.
	if aHigh < 140000 || aHigh > 200000 || aLow < 95000 || aLow > 135000 {
		t.Errorf("audio bit rates %.0f (high) and %.0f (low), want about 192000 and 128000", aHigh, aLow)
	}
	// An explicit rate selects an average bit rate. Encoders differ in how
	// closely they keep to it on a clip this short, so compare two rates.
	v, a := rate("explicit.mp4", govideo.ExportOptions{VideoBitrate: 300_000, AudioBitrate: 96_000})
	vMore, _ := rate("explicit-more.mp4", govideo.ExportOptions{VideoBitrate: 3_000_000})
	if v > 700_000 || v >= vMore || a < 70000 || a > 100000 {
		t.Errorf("explicit bit rates gave video %.0f (and %.0f when asked for ten times as much) and audio %.0f, want about 300000 and 96000", v, vMore, a)
	}

	// Otherwise video is encoded to a quality, not a rate, where the encoder
	// can do that: the presets order the sizes, and a picture that never
	// changes costs next to nothing.
	if enc, err := oscodec.NewH264Encoder(oscodec.EncodeConfig{Width: 64, Height: 64, FrameNum: 30, FrameDen: 1, Quality: 0.5}); err != nil {
		t.Skipf("no quality-targeted encoding here: %v", err)
	} else {
		enc.Close()
	}
	vHigh, _ := rate("vhigh.mp4", govideo.ExportOptions{})
	vLow, _ := rate("vlow.mp4", govideo.ExportOptions{Quality: govideo.QualityLow})
	if vLow >= vHigh {
		t.Errorf("video bit rates %.0f (low) and %.0f (high): the preset does not steer the encoder", vLow, vHigh)
	}
	still := filepath.Join(dir, "still.mp4")
	tl := govideo.NewTimeline(govideo.Canvas{Width: 1280, Height: 720, FrameRate: govideo.R(30, 1)})
	tl.AddGenerated(4*time.Second, govideo.GenerateOptions{Pattern: govideo.PatternBars})
	if err := tl.Export(context.Background(), still, govideo.ExportOptions{Profile: govideo.ProfileCompatible}); err != nil {
		t.Fatal(err)
	}
	if v, _ := oracle.Seconds(oracle.FFprobe(t, still).Streams[0].BitRate); v > 200_000 || v >= vHigh {
		t.Errorf("unchanging bars took %.0f bit/s (the animated card took %.0f)", v, vHigh)
	}
}

// TestDecodeOSCodecs feeds govideo files made by other encoders, covering
// the stream features that matter to a decoder: B-frame reordering, range
// and matrix signalling, 10-bit HEVC, AAC priming, Matroska packaging.
func TestDecodeOSCodecs(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})

	// MJPEG is full-range BT.601; each fixture states how it converts.
	tv709 := "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt709"
	tag709 := []string{"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv"}
	x264 := []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "16", "-pix_fmt", "yuv420p"}
	aac := []string{"-c:a", "aac", "-b:a", "160k"}
	cat := func(parts ...[]string) (out []string) {
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	fixtures := []struct {
		name string
		args []string
		tol  int // colour tolerance
	}{
		{"h264_bframes.mp4", cat([]string{"-vf", tv709}, x264, []string{"-bf", "3", "-b_strategy", "0", "-g", "45"}, tag709, aac), 14},
		{"h264_baseline.mp4", cat([]string{"-vf", tv709}, x264, []string{"-profile:v", "baseline"}, tag709, aac), 14},
		{"h264_bpyramid_mkv.mkv", cat([]string{"-vf", tv709}, x264, []string{"-bf", "5", "-b-pyramid", "normal", "-refs", "4", "-g", "20"}, tag709, aac), 14},
		{"h264_bt601.mp4", cat([]string{"-vf", "scale=in_range=full:in_color_matrix=bt601:out_range=tv:out_color_matrix=bt601"}, x264,
			[]string{"-colorspace", "smpte170m", "-color_primaries", "smpte170m", "-color_trc", "smpte170m", "-color_range", "tv"}, aac), 14},
		{"h264_fullrange.mp4", cat([]string{"-vf", "scale=in_range=full:in_color_matrix=bt601:out_range=full:out_color_matrix=bt709"},
			[]string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "16", "-pix_fmt", "yuvj420p",
				"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "pc"}, aac), 14},
		{"h264_aac_44k_mono.mp4", cat([]string{"-vf", tv709}, x264, tag709, []string{"-c:a", "aac", "-b:a", "96k", "-ar", "44100", "-ac", "1"}), 14},
		{"hevc_main.mp4", cat([]string{"-vf", tv709, "-c:v", "libx265", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
			"-tag:v", "hvc1", "-x265-params", "log-level=error:bframes=3"}, tag709, aac), 14},
		{"hevc_main10.mov", cat([]string{"-vf", tv709, "-c:v", "libx265", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p10le",
			"-tag:v", "hvc1", "-x265-params", "log-level=error"}, tag709, aac), 14},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			in := filepath.Join(dir, fx.name)
			if err := oracle.TryFFmpeg(cat([]string{"-i", src}, fx.args, []string{in})...); err != nil {
				t.Skipf("the oracle cannot make this fixture: %v", err)
			}
			out := filepath.Join(dir, fx.name+".out.mov")
			transcode(t, in, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, out, 0, 90, fx.tol)
			checkBeeps(t, out, 12, 0, 1, 2)

			// A trim that starts between keyframes: the decoder has to
			// start at an earlier sync sample and discard up to the cut.
			cut := filepath.Join(dir, fx.name+".cut.mov")
			transcode(t, in, cut, govideo.Range(1700*time.Millisecond, 2500*time.Millisecond),
				govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, cut, 51, 24, fx.tol)
		})
	}
}

// TestConcatMixedCodecs joins clips of different codecs, sizes and rates
// into one H.264 + AAC file.
func TestConcatMixedCodecs(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	dir := t.TempDir()
	p := func(n string) string { return filepath.Join(dir, n) }
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "color=c=0xff0000:s=1280x720:r=24:d=1", "-f", "lavfi", "-i", "sine=f=440:d=1:r=44100",
		"-vf", "scale=out_color_matrix=bt709", "-colorspace", "bt709", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-bf", "2", "-c:a", "aac", p("red.mp4"))
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "color=c=0x00ff00:s=360x640:r=60:d=1",
		"-c:v", "mjpeg", "-q:v", "3", p("green.mov"))
	if err := oracle.TryFFmpeg("-f", "lavfi", "-i", "color=c=0x0000ff:s=640x480:r=25:d=1", "-f", "lavfi", "-i", "sine=f=880:d=1:r=48000",
		"-c:v", "libx265", "-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params", "log-level=error", "-c:a", "aac", p("blue.mp4")); err != nil {
		t.Skipf("the oracle has no HEVC encoder: %v", err)
	}
	tl := govideo.NewTimeline(govideo.Canvas{Width: 640, Height: 360, FrameRate: govideo.R(30, 1)})
	tl.AddClip(p("red.mp4"), govideo.ClipOptions{})
	tl.AddClip(p("green.mov"), govideo.ClipOptions{})
	tl.AddClip(p("blue.mp4"), govideo.ClipOptions{})
	out := p("out.mp4")
	if err := retryOnVM(t, func() error {
		return tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileCompatible})
	}); err != nil {
		t.Fatal(err)
	}
	frames := oracle.Frames(t, out, cardW, cardH)
	if len(frames) != 90 {
		t.Fatalf("%d frames, want 90", len(frames))
	}
	for _, c := range []struct {
		frame, x, r, g, b int
		what              string
	}{
		{0, 320, 255, 0, 0, "first red frame"}, {29, 320, 255, 0, 0, "last red frame"},
		{30, 320, 0, 255, 0, "first green frame"}, {59, 320, 0, 255, 0, "last green frame"}, {45, 100, 0, 0, 0, "green pillarbox"},
		{60, 320, 0, 0, 255, "first blue frame"}, {89, 320, 0, 0, 255, "last blue frame"}, {75, 40, 0, 0, 0, "blue pillarbox"},
	} {
		r, g, b := rgb(frames[c.frame], c.x, 180)
		if !near(r, c.r, 14) || !near(g, c.g, 14) || !near(b, c.b, 14) {
			t.Errorf("%s: rgb(%d,%d,%d), want rgb(%d,%d,%d)", c.what, r, g, b, c.r, c.g, c.b)
		}
	}
	au := decodeAudio(t, out, "0:a:0")
	const tone = 0.125 / math.Sqrt2
	for _, c := range []struct {
		from, to, want float64
		what           string
	}{{0.1, 0.9, tone, "440 Hz clip"}, {1.1, 1.9, 0, "silent clip"}, {2.1, 2.9, tone, "880 Hz clip"}} {
		if got := rms(au, c.from, c.to); math.Abs(got-c.want) > 0.006 {
			t.Errorf("%s: RMS %.4f, want %.4f", c.what, got, c.want)
		}
	}
}

// TestUnsupportedCodecError checks that a codec with no decoder is reported
// as such, by name.
func TestUnsupportedCodecError(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "mpeg4.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=320x240:r=25", "-c:v", "mpeg4", in)
	tl := govideo.NewTimeline(govideo.Canvas{})
	tl.AddClip(in, govideo.ClipOptions{})
	err := tl.Export(context.Background(), filepath.Join(dir, "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	var cu *govideo.ErrCodecUnavailable
	if !errors.As(err, &cu) || cu.Codec != "mpeg4" || cu.Op != govideo.Decode {
		t.Fatalf("got %v, want ErrCodecUnavailable for mpeg4 decode", err)
	}
	if fmt.Sprint(err) == "" || cu.Reason == "" {
		t.Errorf("error lacks a reason: %v", err)
	}
}

// TestDecodeAVI reads Motion JPEG and PCM from AVI files, including frames
// that leave out their Huffman tables, as cameras write them.
func TestDecodeAVI(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "card.mov")
	exportCard(t, src, 90, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
	for _, fx := range []struct {
		name string
		args []string
	}{
		{"s16_stereo.avi", []string{"-c:v", "mjpeg", "-q:v", "2", "-c:a", "pcm_s16le"}},
		{"u8_mono_11k.avi", []string{"-c:v", "mjpeg", "-q:v", "2", "-c:a", "pcm_u8", "-ac", "1", "-ar", "11025"}},
		{"no_huffman_tables.avi", nil},
	} {
		t.Run(fx.name, func(t *testing.T) {
			in := filepath.Join(dir, fx.name)
			args := fx.args
			if fx.name == "no_huffman_tables.avi" {
				// The mjpegadump bitstream filter rewrites frames in the
				// table-less "AVI1" form.
				args = []string{"-c:v", "mjpeg", "-q:v", "2", "-huffman", "default", "-bsf:v", "mjpegadump", "-c:a", "pcm_s16le"}
			}
			if err := oracle.TryFFmpeg(append(append([]string{"-i", src}, args...), in)...); err != nil {
				t.Skipf("the oracle cannot make this fixture: %v", err)
			}
			out := filepath.Join(dir, fx.name+".mov")
			transcode(t, in, out, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, out, 0, 90, 14)
			tol := 2
			if fx.name == "u8_mono_11k.avi" {
				tol = 12 // the beep's edges are band-limited at 11 kHz
			}
			checkBeeps(t, out, tol, 0, 1, 2)
		})
	}
}

// TestBFrameExport encodes the test card with B-frames, where decode order
// and presentation order differ, and checks that players see every frame
// once, in order, starting at time zero and in step with the sound.
func TestBFrameExport(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	defer govideo.HookH264(func(c *oscodec.EncodeConfig) { c.Reorder = true }, nil)()
	dir := t.TempDir()
	for _, name := range []string{"card.mp4", "card.mov"} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(dir, name)
			exportCard(t, out, 90, govideo.ExportOptions{Profile: govideo.ProfileCompatible})
			if err := oracle.TryFFmpeg("-xerror", "-i", out, "-f", "null", "-"); err != nil {
				t.Fatalf("ffmpeg cannot decode the export: %v", err)
			}
			pr := oracle.FFprobe(t, out, "-count_packets")
			v := pr.Streams[0]
			if v.HasBFrames == 0 {
				t.Fatal("the stream has no B-frames, so the test shows nothing")
			}
			if v.NbReadPackets != "90" || v.RFrameRate != "30/1" || v.AvgFrameRate != "30/1" {
				t.Errorf("%s packets at %s (average %s), want 90 at 30/1", v.NbReadPackets, v.RFrameRate, v.AvgFrameRate)
			}
			for i, st := range pr.Streams {
				if d, _ := oracle.Seconds(st.Duration); math.Abs(d-3) > 0.001 {
					t.Errorf("stream %d lasts %.4fs, want 3.0000", i, d)
				}
				if s, _ := oracle.Seconds(st.StartTime); s != 0 {
					t.Errorf("stream %d starts at %.4fs, want 0", i, s)
				}
			}
			checkCard(t, out, 0, 90, 12)
			checkBeeps(t, out, 12, 0, 1, 2)

			// Back in through govideo's own demuxer and the system decoder,
			// whole and trimmed to a point between keyframes.
			back := filepath.Join(dir, name+".back.mov")
			transcode(t, out, back, govideo.TimeRange{}, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, back, 0, 90, 14)
			checkBeeps(t, back, 12, 0, 1, 2)
			transcode(t, out, back, govideo.Range(time.Second+100*time.Millisecond, 2*time.Second), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			checkCard(t, back, 33, 27, 14)
		})
	}
}
