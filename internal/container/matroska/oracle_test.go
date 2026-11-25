package matroska

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gomcpgo/govideo/internal/oracle"
)

// These tests use the locally installed ffmpeg/ffprobe binaries as black-box
// oracles (SPEC §11). Fixtures are generated at test time, never committed.

type ffStream struct {
	CodecName     string `json:"codec_name"`
	CodecType     string `json:"codec_type"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
	NbReadPackets string `json:"nb_read_packets"`
}

type ffOut struct {
	Streams []ffStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func needTools(t *testing.T) {
	t.Helper()
	oracle.Require(t)
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		oracle.Missing(t, "ffmpeg %v: %v\n%s", args, err, out)
	}
}

func ffprobe(t *testing.T, path string) ffOut {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_format",
		"-count_packets", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var o ffOut
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

var (
	srcVideo = []string{"-f", "lavfi", "-i", "testsrc=d=2:s=320x240:r=25"}
	srcAudio = []string{"-f", "lavfi", "-i", "sine=d=2:r=48000"}
)

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestOracle(t *testing.T) {
	needTools(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "in.srt")
	os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n\n2\n00:00:01,000 --> 00:00:01,900\nWorld\n"), 0o644)

	cases := []struct {
		name   string
		args   []string
		codecs []string // expected CodecIDs in track order
	}{
		{"vp8_opus.webm", cat(srcVideo, srcAudio, []string{"-c:v", "libvpx", "-c:a", "libopus"}), []string{"V_VP8", "A_OPUS"}},
		{"vp9_opus.webm", cat(srcVideo, srcAudio, []string{"-c:v", "libvpx-vp9", "-c:a", "libopus"}), []string{"V_VP9", "A_OPUS"}},
		{"h264_aac.mkv", cat(srcVideo, srcAudio, []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac"}), []string{"V_MPEG4/ISO/AVC", "A_AAC"}},
		{"h264_flac.mkv", cat(srcVideo, srcAudio, []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "flac"}), []string{"V_MPEG4/ISO/AVC", "A_FLAC"}},
		{"mjpeg_pcm.mkv", cat(srcVideo, srcAudio, []string{"-c:v", "mjpeg", "-c:a", "pcm_s16le"}), []string{"V_MJPEG", "A_PCM/INT/LIT"}},
		{"mp3.mka", cat(srcAudio, []string{"-c:a", "libmp3lame"}), []string{"A_MPEG/L3"}},
		{"subs.mkv", cat(srcVideo, []string{"-i", srt, "-c:v", "libvpx", "-c:s", "srt"}), []string{"V_VP8", "S_TEXT/UTF8"}},
		{"webvtt.mkv", cat(srcVideo, []string{"-i", srt, "-c:v", "libvpx", "-c:s", "webvtt"}), []string{"V_VP8", "D_WEBVTT/SUBTITLES"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			ffmpeg(t, append(c.args, path)...)
			checkAgainstFFprobe(t, path, c.codecs, false)
		})
	}
}

// A MediaRecorder-style file: written to a pipe, so no Duration and no Cues,
// and (with -live 1) unknown-size clusters.
func TestOracleLive(t *testing.T) {
	needTools(t)
	dir := t.TempDir()
	for _, live := range []string{"0", "1"} {
		t.Run("live"+live, func(t *testing.T) {
			path := filepath.Join(dir, "live"+live+".webm")
			args := cat([]string{"-hide_banner", "-loglevel", "error"}, srcVideo, srcAudio,
				[]string{"-c:v", "libvpx", "-c:a", "libopus", "-live", live, "-f", "webm", "pipe:1"})
			out, err := exec.Command("ffmpeg", args...).Output()
			if err != nil {
				oracle.Missing(t, "ffmpeg: %v", err)
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				t.Fatal(err)
			}
			f := checkAgainstFFprobe(t, path, []string{"V_VP8", "A_OPUS"}, true)
			if !f.DurationFromScan {
				t.Errorf("expected a scanned duration")
			}
			if d := f.Duration; d < 1950*time.Millisecond || d > 2050*time.Millisecond {
				t.Errorf("scanned duration %v, want ~2s", d)
			}
		})
	}
}

func checkAgainstFFprobe(t *testing.T, path string, codecs []string, noDuration bool) *File {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	cr := &countingReader{r: fh}
	f, err := Open(context.Background(), cr, st.Size())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Logf("Open read %d of %d bytes", cr.n, st.Size())
	want := ffprobe(t, path)
	if len(f.Tracks) != len(want.Streams) || len(f.Tracks) != len(codecs) {
		t.Fatalf("%d tracks, ffprobe %d", len(f.Tracks), len(want.Streams))
	}
	wantDoc := "matroska"
	if filepath.Ext(path) == ".webm" {
		wantDoc = "webm"
	}
	if f.DocType != wantDoc {
		t.Errorf("DocType %q", f.DocType)
	}
	counts := map[uint64]int64{}
	sizes := map[uint64]int64{}
	pr := f.NewReader()
	for {
		p, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		counts[p.Track]++
		sizes[p.Track] += int64(len(p.Data))
	}
	for i, tr := range f.Tracks {
		ws := want.Streams[i]
		if tr.CodecID != codecs[i] {
			t.Errorf("track %d codec %q want %q", i, tr.CodecID, codecs[i])
		}
		wantType := map[string]int{"video": TrackVideo, "audio": TrackAudio, "subtitle": TrackSubtitle}[ws.CodecType]
		if tr.Type != wantType {
			t.Errorf("track %d type %d want %s", i, tr.Type, ws.CodecType)
		}
		switch tr.Type {
		case TrackVideo:
			if tr.PixelWidth != ws.Width || tr.PixelHeight != ws.Height {
				t.Errorf("track %d size %dx%d want %dx%d", i, tr.PixelWidth, tr.PixelHeight, ws.Width, ws.Height)
			}
			if tr.VFR {
				t.Errorf("track %d: constant-rate source flagged VFR", i)
			}
		case TrackAudio:
			rate, _ := strconv.Atoi(ws.SampleRate)
			if int(tr.SamplingFrequency) != rate || tr.Channels != ws.Channels {
				t.Errorf("track %d audio %v/%d want %d/%d", i, tr.SamplingFrequency, tr.Channels, rate, ws.Channels)
			}
		}
		n, _ := strconv.ParseInt(ws.NbReadPackets, 10, 64)
		if tr.Frames != n {
			t.Errorf("track %d (%s) frames %d, ffprobe %d", i, tr.CodecID, tr.Frames, n)
		}
		if counts[tr.Number] != tr.Frames {
			t.Errorf("track %d: PacketReader gave %d packets, scan %d", i, counts[tr.Number], tr.Frames)
		}
		if sizes[tr.Number] != tr.TotalBytes {
			t.Errorf("track %d: packet bytes %d, TotalBytes %d", i, sizes[tr.Number], tr.TotalBytes)
		}
		if tr.Frames > 0 && len(tr.FirstFrame) == 0 {
			t.Errorf("track %d: no FirstFrame", i)
		}
	}
	if !noDuration {
		d, err := strconv.ParseFloat(want.Format.Duration, 64)
		if err != nil {
			t.Fatalf("ffprobe duration %q", want.Format.Duration)
		}
		diff := f.Duration - time.Duration(d*float64(time.Second))
		if diff < -40*time.Millisecond || diff > 40*time.Millisecond {
			t.Errorf("duration %v, ffprobe %vs", f.Duration, d)
		}
		if f.DurationFromScan {
			t.Errorf("unexpected scanned duration")
		}
	}
	return f
}
