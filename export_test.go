package govideo_test

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/videoprobe"
)

func writePNG(t *testing.T, path string, w, h int, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// decodeAudio returns the output's first audio track as mono-summed s16
// samples at 48 kHz, decoded by the ffmpeg oracle.
func decodeAudio(t *testing.T, path, stream string) []float64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", stream, "-f", "s16le", "-ac", "1", "-ar", "48000", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg decode audio: %v", err)
	}
	s := make([]float64, len(out)/2)
	for i := range s {
		s[i] = float64(int16(binary.LittleEndian.Uint16(out[i*2:]))) / 32768
	}
	return s
}

func rms(s []float64, fromSec, toSec float64) float64 {
	a, b := int(fromSec*48000), int(toSec*48000)
	if b > len(s) {
		b = len(s)
	}
	var sum float64
	for _, v := range s[a:b] {
		sum += v * v
	}
	return math.Sqrt(sum / float64(b-a))
}

// pixel returns the RGB value at (x, y) of the frame shown at time sec.
func pixel(t *testing.T, path string, sec float64, w, h, x, y int) (r, g, b int) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-ss", time.Duration(sec*1e9).String(), "-i", path,
		"-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil || len(out) < w*h*3 {
		t.Fatalf("ffmpeg decode frame at %.2fs: %v (%d bytes)", sec, err, len(out))
	}
	p := out[(y*w+x)*3:]
	return int(p[0]), int(p[1]), int(p[2])
}

func near(a, b, tol int) bool { return a-b <= tol && b-a <= tol }

func TestExportEndToEnd(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	p := func(name string) string { return filepath.Join(dir, name) }

	// Sources: a landscape MJPEG+PCM clip (solid blue, 440 Hz tone), a
	// portrait Matroska clip (solid green, silent), a red still, a quiet
	// 44.1 kHz mono music bed, and captions.
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "color=c=0x0000ff:s=320x240:r=25:d=2", "-f", "lavfi", "-i", "sine=f=440:d=2:r=48000",
		"-c:v", "mjpeg", "-q:v", "3", "-c:a", "pcm_s16le", "-ac", "2", p("blue.mov"))
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "color=c=0x00ff00:s=180x320:r=30:d=1.5",
		"-c:v", "mjpeg", "-q:v", "3", p("green.mkv"))
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "sine=f=1000:d=10:r=44100", "-c:a", "pcm_s16le", "-ac", "1", p("music.wav"))
	writePNG(t, p("red.png"), 400, 400, color.RGBA{255, 0, 0, 255})
	srt := "1\n00:00:00,500 --> 00:00:01,500\nHello\n\n2\n00:00:02,000 --> 00:00:03,000\nWorld\n"
	if err := os.WriteFile(p("subs.srt"), []byte(srt), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"out.mov", "out.mp4"} {
		t.Run(name, func(t *testing.T) {
			tl := govideo.NewTimeline(govideo.Canvas{Width: 640, Height: 360, FrameRate: govideo.R(30, 1)})
			tl.AddImage(p("red.png"), time.Second, govideo.ImageOptions{})                                           // 0.0 – 1.0
			tl.AddClip(p("blue.mov"), govideo.ClipOptions{Trim: govideo.Range(500*time.Millisecond, 2*time.Second)}) // 1.0 – 2.5
			tl.AddClip(p("green.mkv"), govideo.ClipOptions{})                                                        // 2.5 – 4.0
			tl.AddAudio(p("music.wav"), govideo.AudioOptions{Mode: govideo.Mix, Gain: 0.25, Start: 3 * time.Second}) // 3.0 – 4.0
			tl.AddAudio(p("music.wav"), govideo.AudioOptions{Mode: govideo.Track, Language: "fra"})
			tl.AddCaptions(p("subs.srt"), govideo.CaptionOptions{Language: "eng", Sidecar: true})

			var last govideo.Progress
			out := p(name)
			err := tl.Export(context.Background(), out, govideo.ExportOptions{Progress: func(pr govideo.Progress) { last = pr }})
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if last.Frame != 120 || last.TotalFrames != 120 {
				t.Errorf("progress ended at %d/%d, want 120/120", last.Frame, last.TotalFrames)
			}

			// The oracle must decode the whole file without complaint.
			if err := oracle.TryFFmpeg("-xerror", "-i", out, "-map", "0:v", "-map", "0:a", "-f", "null", "-"); err != nil {
				t.Fatalf("ffmpeg cannot decode the export: %v", err)
			}
			pr := oracle.FFprobe(t, out, "-count_packets")
			if len(pr.Streams) != 4 {
				t.Fatalf("got %d streams, want 4 (video, audio, audio, subtitle)", len(pr.Streams))
			}
			v, a, a2, s := pr.Streams[0], pr.Streams[1], pr.Streams[2], pr.Streams[3]
			if v.CodecName != "mjpeg" || v.Width != 640 || v.Height != 360 || v.RFrameRate != "30/1" || v.NbReadPackets != "120" {
				t.Errorf("video: %s %dx%d %s, %s packets", v.CodecName, v.Width, v.Height, v.RFrameRate, v.NbReadPackets)
			}
			if a.CodecName != "pcm_s16le" || a.SampleRate != "48000" || a.Channels != 2 {
				t.Errorf("audio: %s %s Hz %d ch", a.CodecName, a.SampleRate, a.Channels)
			}
			if a2.CodecName != "pcm_s16le" || a2.Tags["language"] != "fra" {
				t.Errorf("second audio: %s language %q", a2.CodecName, a2.Tags["language"])
			}
			if s.CodecName != "mov_text" || s.Tags["language"] != "eng" {
				t.Errorf("subtitle: %s language %q", s.CodecName, s.Tags["language"])
			}
			for i, st := range pr.Streams {
				if d, _ := oracle.Seconds(st.Duration); math.Abs(d-4) > 0.034 {
					t.Errorf("stream %d duration %.3f, want 4.000 (A/V sync within one frame)", i, d)
				}
			}

			// The moov box must come first (faststart), as videoprobe sees it.
			info, err := videoprobe.ProbeFile(context.Background(), out)
			if err != nil {
				t.Fatal(err)
			}
			if !info.FastStart || info.Fragmented {
				t.Errorf("FastStart=%v Fragmented=%v, want true/false", info.FastStart, info.Fragmented)
			}

			// Picture content: red still filling the height (pillarboxed),
			// the blue clip, then the portrait green clip pillarboxed.
			type px struct {
				sec     float64
				x, y    int
				r, g, b int
				what    string
			}
			for _, c := range []px{
				{0.5, 320, 180, 255, 0, 0, "red still, centre"},
				{0.5, 20, 180, 0, 0, 0, "red still, left pillarbox"},
				{1.7, 320, 180, 0, 0, 255, "blue clip, centre"},
				{3.2, 320, 180, 0, 255, 0, "green clip, centre"},
				{3.2, 100, 180, 0, 0, 0, "green clip, left pillarbox"},
			} {
				r, g, b := pixel(t, out, c.sec, 640, 360, c.x, c.y)
				if !near(r, c.r, 12) || !near(g, c.g, 12) || !near(b, c.b, 12) {
					t.Errorf("%s at %.1fs: got rgb(%d,%d,%d), want rgb(%d,%d,%d)", c.what, c.sec, r, g, b, c.r, c.g, c.b)
				}
			}

			// Audio content. ffmpeg's sine source has amplitude 1/8.
			const tone = 0.125 / math.Sqrt2
			au := decodeAudio(t, out, "0:a:0")
			if got := rms(au, 0.1, 0.9); got > 0.001 {
				t.Errorf("still image should be silent, RMS %.4f", got)
			}
			// ffmpeg's mono-to-stereo upmix of the clip fixture applies -3 dB per channel.
			if got := rms(au, 1.1, 2.4); math.Abs(got-tone/math.Sqrt2) > 0.004 {
				t.Errorf("clip tone RMS %.4f, want %.4f", got, tone/math.Sqrt2)
			}
			if got := rms(au, 2.6, 2.9); got > 0.001 {
				t.Errorf("silent clip should be silent, RMS %.4f", got)
			}
			if got := rms(au, 3.1, 3.9); math.Abs(got-tone*0.25) > 0.003 {
				t.Errorf("mixed music RMS %.4f, want %.4f", got, tone*0.25)
			}
			if got := rms(decodeAudio(t, out, "0:a:1"), 0.1, 3.9); math.Abs(got-tone) > 0.005 {
				t.Errorf("separate music track RMS %.4f, want %.4f", got, tone)
			}

			// Captions: the oracle extracts the track back to SRT.
			back := p(name + ".srt")
			oracle.FFmpeg(t, "-i", out, "-map", "0:s:0", back)
			txt, _ := os.ReadFile(back)
			for _, want := range []string{"00:00:00,500 --> 00:00:01,500", "Hello", "00:00:02,000 --> 00:00:03,000", "World"} {
				if !contains(string(txt), want) {
					t.Errorf("extracted captions lack %q:\n%s", want, txt)
				}
			}
			if _, err := os.Stat(p(name[:3] + ".vtt")); err != nil {
				t.Errorf("sidecar: %v", err)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestExportCodecUnavailable(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "h264.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=1:s=320x240:r=25", "-c:v", "libx264", "-pix_fmt", "yuv420p", in)
	tl := govideo.NewTimeline(govideo.Canvas{})
	tl.AddClip(in, govideo.ClipOptions{})
	err := tl.Export(context.Background(), filepath.Join(dir, "out.mov"), govideo.ExportOptions{})
	var cu *govideo.ErrCodecUnavailable
	if !errors.As(err, &cu) || cu.Codec != "h264" || cu.Op != govideo.Decode {
		t.Fatalf("got %v, want ErrCodecUnavailable for h264 decode", err)
	}
	// An explicit profile whose codecs are missing fails the same way.
	tl = govideo.NewTimeline(govideo.Canvas{})
	tl.AddImage("unused.png", time.Second, govideo.ImageOptions{})
	err = tl.Export(context.Background(), filepath.Join(dir, "out.mp4"), govideo.ExportOptions{Profile: govideo.ProfileCompatible})
	if !errors.As(err, &cu) || cu.Op != govideo.Encode {
		t.Fatalf("got %v, want ErrCodecUnavailable for encode", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("failed exports left files behind: %v", entries)
	}
}

func TestRegistry(t *testing.T) {
	ids := map[govideo.CodecID]bool{}
	for _, c := range govideo.Codecs() {
		ids[c.ID] = true
	}
	if !ids["pcm"] || !ids["mjpeg"] || ids["h264"] {
		t.Errorf("unexpected codec list: %v", ids)
	}
	if _, err := govideo.Available("pcm_s24le", govideo.Decode); err != nil {
		t.Errorf("pcm_s24le should be decodable: %v", err)
	}
	if _, err := govideo.Available("aac", govideo.Encode); err == nil {
		t.Error("aac must not be available natively")
	}
}
