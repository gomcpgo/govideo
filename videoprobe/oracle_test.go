package videoprobe_test

import (
	"context"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/videoprobe"
)

const (
	vsrc = "testsrc2=d=2:s=320x240:r=25"
	asrc = "sine=f=440:d=2:r=48000"
)

type fixture struct {
	name string
	args []string // ffmpeg arguments, output path appended
	opt  bool     // optional: skip if ffmpeg cannot build it
	// expectations beyond the oracle comparison
	hdr   string
	frag  bool
	noDur bool // ffprobe reports no duration (live WebM)
}

func lavfi(v, a string) []string {
	var out []string
	if v != "" {
		out = append(out, "-f", "lavfi", "-i", v)
	}
	if a != "" {
		out = append(out, "-f", "lavfi", "-i", a)
	}
	return out
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var fixtures = []fixture{
	{name: "h264_aac.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart"})},
	{name: "h264_nofast.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac"})},
	{name: "h264_main_ntsc.mov", args: cat(lavfi("testsrc2=d=2:s=1280x720:r=30000/1001", asrc), []string{"-c:v", "libx264", "-profile:v", "main", "-pix_fmt", "yuv420p", "-c:a", "pcm_s16le"})},
	{name: "h264_odd_sar.mp4", args: cat(lavfi("testsrc2=d=2:s=322x242:r=24", ""), []string{"-vf", "setsar=4/3", "-c:v", "libx264", "-pix_fmt", "yuv420p"})},
	{name: "h264_frag.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+frag_keyframe+empty_moov+default_base_moof"}), frag: true},
	{name: "h264_frag_legacy.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx264", "-g", "10", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+frag_keyframe"}), frag: true},
	{name: "h264_high10_interlaced.mp4", args: cat(lavfi("testsrc2=d=2:s=720x576:r=25", ""), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p10le", "-flags", "+ildct+ilme", "-x264opts", "tff=1"})},
	{name: "h264_422.mov", args: cat(lavfi(vsrc, ""), []string{"-c:v", "libx264", "-pix_fmt", "yuv422p10le"})},
	{name: "h264_vt.mov", args: cat(lavfi("testsrc2=d=2:s=1920x1080:r=30", asrc), []string{"-c:v", "h264_videotoolbox", "-b:v", "4M", "-c:a", "aac_at"}), opt: true},
	{name: "hevc_main.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx265", "-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-c:a", "aac"})},
	{name: "hevc_hlg.mov", args: cat(lavfi("testsrc2=d=2:s=1920x1080:r=30", ""), []string{"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
		"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc", "-color_range", "tv",
		"-x265-params", "colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc:range=limited"}), hdr: "hlg"},
	{name: "hevc_pq.mp4", args: cat(lavfi(vsrc, ""), []string{"-c:v", "libx265", "-pix_fmt", "yuv420p10le",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc"}), hdr: "pq"},
	{name: "hevc_vt_hlg.mov", args: cat(lavfi("testsrc2=d=2:s=1920x1080:r=30", ""), []string{"-c:v", "hevc_videotoolbox", "-profile:v", "main10", "-pix_fmt", "p010le", "-b:v", "6M", "-tag:v", "hvc1",
		"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc"}), opt: true},
	{name: "av1_opus.mp4", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libsvtav1", "-preset", "12", "-c:a", "libopus"})},
	{name: "av1_10bit.mkv", args: cat(lavfi(vsrc, ""), []string{"-c:v", "libsvtav1", "-preset", "12", "-pix_fmt", "yuv420p10le"})},
	{name: "vp9_opus.webm", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libvpx-vp9", "-b:v", "300k", "-cpu-used", "8", "-c:a", "libopus"})},
	{name: "vp8_opus.webm", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libvpx", "-b:v", "300k", "-c:a", "libopus"})},
	{name: "vp9_live.webm", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libvpx-vp9", "-b:v", "300k", "-cpu-used", "8", "-c:a", "libopus", "-f", "webm", "-live", "1"}), noDur: true},
	{name: "vp9.mp4", args: cat(lavfi(vsrc, ""), []string{"-c:v", "libvpx-vp9", "-b:v", "300k", "-cpu-used", "8"})},
	{name: "h264_flac.mkv", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "flac"})},
	{name: "h264_mp3.mkv", args: cat(lavfi(vsrc, "sine=f=440:d=2:r=44100"), []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "libmp3lame"})},
	{name: "mjpeg_pcm.mov", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_s16le"})},
	{name: "pcm24.mov", args: cat(lavfi("", asrc), []string{"-c:a", "pcm_s24le", "-ac", "2"})},
	{name: "flac.mp4", args: cat(lavfi("", "sine=f=440:d=2:r=44100"), []string{"-c:a", "flac", "-strict", "-2"})},
	{name: "mp3.mp4", args: cat(lavfi("", "sine=f=440:d=2:r=44100"), []string{"-c:a", "libmp3lame", "-ac", "2"})},
	{name: "aac_51.m4a", args: cat(lavfi("", asrc), []string{"-c:a", "aac", "-ac", "6"})},
	{name: "audio.wav", args: cat(lavfi("", asrc), []string{"-c:a", "pcm_s16le", "-ac", "2"})},
	{name: "mjpeg_pcm.avi", args: cat(lavfi(vsrc, asrc), []string{"-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_s16le"})},
	{name: "mjpeg_pcm_u8.avi", args: cat(lavfi(vsrc, "sine=f=440:d=2:r=11025"), []string{"-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_u8", "-ac", "1"})},
	{name: "mjpeg_ntsc_f32.avi", args: cat(lavfi("testsrc2=d=2:s=640x360:r=30000/1001", "sine=f=440:d=2:r=44100"), []string{"-c:v", "mjpeg", "-q:v", "5", "-c:a", "pcm_f32le", "-ac", "2"})},
	{name: "prores.mov", args: cat(lavfi(vsrc, ""), []string{"-c:v", "prores_videotoolbox", "-profile:v", "hq"}), opt: true},
}

func TestAgainstFFprobe(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	for _, fx := range fixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			path := filepath.Join(dir, fx.name)
			if err := oracle.TryFFmpeg(append(append([]string{}, fx.args...), path)...); err != nil {
				// Optional fixtures need a hardware or OS encoder; any
				// fixture is skipped by an ffmpeg built without its encoder.
				if fx.opt || strings.Contains(err.Error(), "Unknown encoder") {
					t.Skipf("fixture not buildable here: %v", err)
				}
				t.Fatal(err)
			}
			want := oracle.FFprobe(t, path, "-count_packets")
			got, err := videoprobe.ProbeFile(context.Background(), path)
			if err != nil {
				t.Fatalf("ProbeFile: %v", err)
			}
			if _, err := got.JSON(); err != nil {
				t.Fatalf("JSON: %v", err)
			}
			compare(t, fx, got, want)
		})
	}
}

func compare(t *testing.T, fx fixture, got *videoprobe.Info, want *oracle.Result) {
	t.Helper()
	if len(got.Streams) != len(want.Streams) {
		t.Fatalf("stream count: got %d, want %d", len(got.Streams), len(want.Streams))
	}
	if got.Fragmented != fx.frag {
		t.Errorf("Fragmented = %v, want %v", got.Fragmented, fx.frag)
	}
	// One frame of the slowest stream is the duration tolerance (SPEC §11).
	tol := 0.045
	if d, ok := oracle.Seconds(want.Format.Duration); ok && !fx.noDur {
		if math.Abs(got.Duration.Seconds()-d) > tol {
			t.Errorf("format duration: got %.4f, want %.4f", got.Duration.Seconds(), d)
		}
	} else if math.Abs(got.Duration.Seconds()-2) > 0.1 {
		t.Errorf("scanned duration: got %.4f, want about 2", got.Duration.Seconds())
	}
	if st, ok := oracle.Seconds(want.Format.StartTime); ok && math.Abs(got.StartTime.Seconds()-st) > tol {
		t.Errorf("format start_time: got %.4f, want %.4f", got.StartTime.Seconds(), st)
	}
	for i, w := range want.Streams {
		g := got.Streams[i]
		pre := "stream " + strconv.Itoa(i) + " (" + w.CodecName + "): "
		if g.Kind.String() != w.CodecType {
			t.Errorf(pre+"kind: got %s, want %s", g.Kind, w.CodecType)
		}
		if g.Codec != w.CodecName {
			t.Errorf(pre+"codec: got %q, want %q", g.Codec, w.CodecName)
		}
		if w.Profile != "" && w.Profile != "unknown" && g.Profile != w.Profile && w.CodecName != "mjpeg" && w.CodecName != "flac" && w.CodecName != "mp3" {
			t.Errorf(pre+"profile: got %q, want %q", g.Profile, w.Profile)
		}
		if d, ok := oracle.Seconds(w.Duration); ok && math.Abs(g.Duration.Seconds()-d) > tol {
			t.Errorf(pre+"duration: got %.4f, want %.4f", g.Duration.Seconds(), d)
		}
		if st, ok := oracle.Seconds(w.StartTime); ok && math.Abs(g.StartTime.Seconds()-st) > tol {
			t.Errorf(pre+"start_time: got %.4f, want %.4f", g.StartTime.Seconds(), st)
		}
		if n, err := strconv.ParseInt(w.NbReadPackets, 10, 64); err == nil && w.CodecType == "video" && g.Frames != n {
			t.Errorf(pre+"frames: got %d, want %d", g.Frames, n)
		}
		switch w.CodecType {
		case "video":
			v := g.Video
			if v == nil {
				t.Error(pre + "no VideoInfo")
				continue
			}
			if v.Width != w.Width || v.Height != w.Height {
				t.Errorf(pre+"size: got %dx%d, want %dx%d", v.Width, v.Height, w.Width, w.Height)
			}
			if w.Level > 0 && (w.CodecName == "h264" || w.CodecName == "hevc" || w.CodecName == "av1") {
				if j := levelOf(t, got, i); j != w.Level {
					t.Errorf(pre+"level: got %d (%s), want %d", j, g.Level, w.Level)
				}
			}
			if ws := oracle.Ratio(w.SAR); ws > 0 && math.Abs(v.SAR.Float()-ws) > 1e-3 {
				t.Errorf(pre+"SAR: got %v, want %s", v.SAR, w.SAR)
			}
			if wd := oracle.Ratio(w.DAR); wd > 0 && math.Abs(v.DAR.Float()-wd) > 1e-2 {
				t.Errorf(pre+"DAR: got %v, want %s", v.DAR, w.DAR)
			}
			if wr := oracle.Ratio(w.AvgFrameRate); wr > 0 && math.Abs(v.AvgFrameRate.Float()-wr)/wr > 0.02 {
				t.Errorf(pre+"avg_frame_rate: got %v, want %s", v.AvgFrameRate, w.AvgFrameRate)
			}
			if wr := oracle.Ratio(w.RFrameRate); wr > 0 && math.Abs(v.FrameRate.Float()-wr)/wr > 0.001 && !v.Interlaced {
				t.Errorf(pre+"r_frame_rate: got %v, want %s", v.FrameRate, w.RFrameRate)
			}
			if v.VFR {
				t.Error(pre + "flagged VFR, but the fixture is constant rate")
			}
			if wantRot := ((-w.Rotation() % 360) + 360) % 360; v.Rotation != wantRot {
				t.Errorf(pre+"rotation: got %d, want %d (ffprobe %d)", v.Rotation, wantRot, w.Rotation())
			}
			if bits, chroma := pixFmt(w.PixFmt); bits > 0 {
				if v.BitDepth != bits {
					t.Errorf(pre+"bit depth: got %d, want %d (%s)", v.BitDepth, bits, w.PixFmt)
				}
				if v.Chroma != chroma && v.Chroma != "" {
					t.Errorf(pre+"chroma: got %s, want %s (%s)", v.Chroma, chroma, w.PixFmt)
				}
			}
			wantInterlaced := w.FieldOrder != "" && w.FieldOrder != "progressive" && w.FieldOrder != "unknown"
			if v.Interlaced != wantInterlaced {
				t.Errorf(pre+"interlaced: got %v, want field_order %q", v.Interlaced, w.FieldOrder)
			}
			for _, c := range []struct{ name, got, want string }{
				{"color_range", v.ColorRange, w.ColorRange},
				{"color_space", v.ColorMatrix, w.ColorSpace},
				{"color_transfer", v.ColorTransfer, w.ColorTransfer},
				{"color_primaries", v.ColorPrimaries, w.ColorPrim},
			} {
				// ffprobe leaves unknown values out; the JPEG colour model is a decoder convention.
				if c.want != "" && c.want != "unknown" && c.got != c.want && w.CodecName != "mjpeg" && w.CodecName != "prores" {
					t.Errorf(pre+"%s: got %q, want %q", c.name, c.got, c.want)
				}
			}
			if v.HDR != fx.hdr {
				t.Errorf(pre+"HDR: got %q, want %q", v.HDR, fx.hdr)
			}
		case "audio":
			a := g.Audio
			if a == nil {
				t.Error(pre + "no AudioInfo")
				continue
			}
			if strconv.Itoa(a.SampleRate) != w.SampleRate {
				t.Errorf(pre+"sample rate: got %d, want %s", a.SampleRate, w.SampleRate)
			}
			if a.Channels != w.Channels {
				t.Errorf(pre+"channels: got %d, want %d", a.Channels, w.Channels)
			}
			if w.ChannelLayout != "" && w.ChannelLayout != "unknown" && a.ChannelLayout != w.ChannelLayout {
				t.Errorf(pre+"layout: got %q, want %q", a.ChannelLayout, w.ChannelLayout)
			}
			if strings.HasPrefix(w.CodecName, "pcm") && w.BitsPerSample > 0 && a.BitDepth != w.BitsPerSample {
				t.Errorf(pre+"bit depth: got %d, want %d", a.BitDepth, w.BitsPerSample)
			}
		}
	}
}

func levelOf(t *testing.T, info *videoprobe.Info, i int) int {
	s := info.Streams[i]
	parts := strings.SplitN(s.Level, ".", 2)
	if len(parts) != 2 {
		return -1
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	switch s.Codec {
	case "h264":
		return major*10 + minor
	case "hevc":
		return major*30 + minor*3
	case "av1":
		return (major-2)<<2 | minor
	}
	return -1
}

func pixFmt(p string) (bits int, chroma string) {
	if p == "" {
		return 0, ""
	}
	bits = 8
	for _, b := range []int{10, 12} {
		if strings.Contains(p, "p"+strconv.Itoa(b)) || strings.Contains(p, "p0"+strconv.Itoa(b)) {
			bits = b
		}
	}
	switch {
	case strings.Contains(p, "420"), strings.HasPrefix(p, "nv12"), strings.HasPrefix(p, "p010"):
		chroma = "4:2:0"
	case strings.Contains(p, "422"), strings.HasPrefix(p, "p210"):
		chroma = "4:2:2"
	case strings.Contains(p, "444"):
		chroma = "4:4:4"
	default:
		return 0, ""
	}
	return bits, chroma
}

func TestFastStart(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	for _, c := range []struct {
		name  string
		flags string
		want  bool
	}{{"fast.mp4", "+faststart", true}, {"slow.mp4", "0", false}} {
		path := filepath.Join(dir, c.name)
		oracle.FFmpeg(t, "-f", "lavfi", "-i", vsrc, "-c:v", "libx264", "-movflags", c.flags, path)
		info, err := videoprobe.ProbeFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if info.FastStart != c.want {
			t.Errorf("%s: FastStart = %v, want %v", c.name, info.FastStart, c.want)
		}
	}
}

// TestRotation checks the tkhd display matrix for all four orientations.
func TestRotation(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mp4")
	oracle.FFmpeg(t, "-f", "lavfi", "-i", vsrc, "-c:v", "libx264", "-pix_fmt", "yuv420p", base)
	for _, deg := range []int{0, 90, 180, 270} {
		path := filepath.Join(dir, "rot"+strconv.Itoa(deg)+".mp4")
		oracle.FFmpeg(t, "-display_rotation", strconv.Itoa(deg), "-i", base, "-c", "copy", path)
		want := ((-oracle.FFprobe(t, path).Streams[0].Rotation() % 360) + 360) % 360
		info, err := videoprobe.ProbeFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		got := info.Streams[0].Video.Rotation
		if got != want {
			t.Errorf("display_rotation %d: got %d, want %d", deg, got, want)
		}
		if deg != 0 && got == 0 {
			t.Errorf("display_rotation %d: fixture carries no rotation", deg)
		}
	}
}

// TestVFR checks that dropped frames with preserved timestamps are flagged.
func TestVFR(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	for _, name := range []string{"vfr.mp4", "vfr.mkv"} {
		path := filepath.Join(dir, name)
		oracle.FFmpeg(t, "-f", "lavfi", "-i", "testsrc2=d=4:s=320x240:r=30",
			"-vf", "select='not(eq(mod(n,7),3))*not(eq(mod(n,5),1))'", "-fps_mode", "vfr", "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
		info, err := videoprobe.ProbeFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		v := info.Streams[0].Video
		if !v.VFR {
			t.Errorf("%s: VFR not flagged (rate %v, avg %v)", name, v.FrameRate, v.AvgFrameRate)
		}
		want := oracle.Ratio(oracle.FFprobe(t, path).Streams[0].AvgFrameRate)
		// For Matroska ffprobe echoes the nominal rate; only MP4 carries a measured average.
		if name == "vfr.mp4" && math.Abs(v.AvgFrameRate.Float()-want)/want > 0.03 {
			t.Errorf("%s: avg frame rate %v, want %.3f", name, v.AvgFrameRate, want)
		}
	}
}
