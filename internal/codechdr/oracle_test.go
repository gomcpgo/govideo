package codechdr

// Differential tests against locally installed ffmpeg/ffprobe binaries, used
// strictly as black-box oracles (SPEC §11). Fixtures are generated into
// t.TempDir() and never committed.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func needOracle(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed; skipping oracle test", bin)
		}
	}
}

// generate runs ffmpeg and returns the output path. With optional set, an
// encoder failure skips the test (hardware/OS encoders are not always
// usable).
func generate(t *testing.T, optional bool, out string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), out)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, path)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		if optional {
			t.Skipf("ffmpeg could not produce fixture: %v\n%s", err, b)
		}
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(full, " "), err, b)
	}
	return path
}

type probeStream struct {
	CodecName      string `json:"codec_name"`
	Profile        string `json:"profile"`
	Level          int    `json:"level"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	CodedWidth     int    `json:"coded_width"`
	CodedHeight    int    `json:"coded_height"`
	PixFmt         string `json:"pix_fmt"`
	SAR            string `json:"sample_aspect_ratio"`
	FieldOrder     string `json:"field_order"`
	RFrameRate     string `json:"r_frame_rate"`
	ColorRange     string `json:"color_range"`
	ColorSpace     string `json:"color_space"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	HasBFrames     int    `json:"has_b_frames"`
	Refs           int    `json:"refs"`
	SampleRate     string `json:"sample_rate"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	BitsPerRaw     string `json:"bits_per_raw_sample"`
}

func ffprobe(t *testing.T, path string) probeStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var doc struct {
		Streams []probeStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || len(doc.Streams) == 0 {
		t.Fatalf("ffprobe output: %v\n%s", err, out)
	}
	return doc.Streams[0]
}

// findBox does a naive search for a box by fourcc and returns its payload.
// Fixtures are written with faststart so the first hit is inside moov.
func findBox(t *testing.T, path, fourcc string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for from := 0; ; {
		i := bytes.Index(data[from:], []byte(fourcc))
		if i < 0 {
			t.Fatalf("box %q not found in %s", fourcc, path)
		}
		i += from
		if i >= 4 {
			size := int(binary.BigEndian.Uint32(data[i-4:]))
			if size >= 8 && i-4+size <= len(data) {
				b := data[i+4 : i-4+size]
				dump(t, fourcc, b)
				return b
			}
		}
		from = i + 4
	}
}

// dump logs fixtures as hex when CODECHDR_DUMP is set (used to refresh the
// fuzz seeds).
func dump(t *testing.T, what string, b []byte) {
	if os.Getenv("CODECHDR_DUMP") != "" {
		if len(b) > 200 {
			b = b[:200]
		}
		fmt.Printf("DUMP %s %s %s\n", t.Name(), what, hex.EncodeToString(b))
	}
}

func pixFmtInfo(pf string) (depth int, chroma string) {
	depth = 8
	for _, d := range []int{10, 12} {
		if strings.Contains(pf, "p"+strconv.Itoa(d)) || strings.HasPrefix(pf, "gray"+strconv.Itoa(d)) {
			depth = d
		}
	}
	switch {
	case strings.HasPrefix(pf, "gray"):
		chroma = "4:0:0"
	case strings.Contains(pf, "420"):
		chroma = "4:2:0"
	case strings.Contains(pf, "422"):
		chroma = "4:2:2"
	case strings.Contains(pf, "444"):
		chroma = "4:4:4"
	}
	return
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

type cmpOpts struct {
	skipLevel, skipCoded, skipSize, wantColor, skipProfile bool
}

func compareVideo(t *testing.T, v *Video, p probeStream, o cmpOpts) {
	t.Helper()
	if v.Codec != p.CodecName {
		t.Errorf("codec %q, ffprobe %q", v.Codec, p.CodecName)
	}
	if !o.skipProfile && v.Profile != p.Profile {
		t.Errorf("profile %q, ffprobe %q", v.Profile, p.Profile)
	}
	if !o.skipLevel && v.Level != p.Level {
		t.Errorf("level %d, ffprobe %d", v.Level, p.Level)
	}
	if !o.skipSize && (v.Width != p.Width || v.Height != p.Height) {
		t.Errorf("size %dx%d, ffprobe %dx%d", v.Width, v.Height, p.Width, p.Height)
	}
	if !o.skipCoded && !o.skipSize && v.Codec != "h264" && (v.CodedWidth != p.CodedWidth || v.CodedHeight != p.CodedHeight) {
		t.Errorf("coded size %dx%d, ffprobe %dx%d", v.CodedWidth, v.CodedHeight, p.CodedWidth, p.CodedHeight)
	}
	depth, chroma := pixFmtInfo(p.PixFmt)
	if v.BitDepth != depth || v.Chroma != chroma {
		t.Errorf("format %d-bit %s, ffprobe pix_fmt %s", v.BitDepth, v.Chroma, p.PixFmt)
	}
	if p.SAR != "" && p.SAR != "0:1" {
		n, d := v.SARNum, v.SARDen
		if n == 0 {
			n, d = 1, 1
		}
		g := gcd(n, d)
		if got := fmt.Sprintf("%d:%d", n/g, d/g); got != p.SAR {
			t.Errorf("SAR %s, ffprobe %s", got, p.SAR)
		}
	}
	if v.Codec == "h264" {
		inter := p.FieldOrder != "progressive" && p.FieldOrder != "unknown" && p.FieldOrder != ""
		if v.Interlaced != inter {
			t.Errorf("interlaced %v, ffprobe field_order %q", v.Interlaced, p.FieldOrder)
		}
		// ffprobe reports the display size as coded size until it decodes,
		// so check the macroblock alignment invariant instead.
		if v.CodedWidth%16 != 0 || v.CodedHeight%16 != 0 || v.CodedWidth < v.Width || v.CodedWidth-v.Width >= 16 ||
			v.CodedHeight < v.Height || v.CodedHeight-v.Height >= 32 {
			t.Errorf("coded size %dx%d inconsistent with %dx%d", v.CodedWidth, v.CodedHeight, v.Width, v.Height)
		}
		if v.RefFrames < 1 || v.RefFrames > 16 {
			t.Errorf("refs %d", v.RefFrames)
		}
		if v.HasBFrames >= 0 && v.HasBFrames != p.HasBFrames {
			t.Errorf("has_b_frames %d, ffprobe %d", v.HasBFrames, p.HasBFrames)
		}
	}
	if v.HasTiming {
		g := gcd(int(v.TimeNum), int(v.TimeDen))
		if got := fmt.Sprintf("%d/%d", int(v.TimeNum)/g, int(v.TimeDen)/g); got != p.RFrameRate {
			t.Errorf("frame rate %s, ffprobe %s", got, p.RFrameRate)
		}
	}
	if got := PrimariesName(v.ColorPrimaries); got != p.ColorPrimaries {
		t.Errorf("primaries %q, ffprobe %q", got, p.ColorPrimaries)
	}
	if got := TransferName(v.ColorTransfer); got != p.ColorTransfer {
		t.Errorf("transfer %q, ffprobe %q", got, p.ColorTransfer)
	}
	if got := MatrixName(v.ColorMatrix); got != p.ColorSpace {
		t.Errorf("matrix %q, ffprobe %q", got, p.ColorSpace)
	}
	if o.wantColor && (!v.ColorRangeKnown || v.ColorPrimaries == 2 || v.ColorTransfer == 2 || v.ColorMatrix == 2) {
		t.Errorf("colour description not found: %d/%d/%d", v.ColorPrimaries, v.ColorTransfer, v.ColorMatrix)
	}
	if v.ColorRangeKnown && p.ColorRange != "" {
		want := "tv"
		if v.FullRange {
			want = "pc"
		}
		if want != p.ColorRange {
			t.Errorf("range %q, ffprobe %q", want, p.ColorRange)
		}
	}
}

func src(size string) []string {
	return []string{"-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=25", "-frames:v", "6"}
}

var hlg = []string{"-vf", "setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc:range=tv"}
var pq = []string{"-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv"}
var bt709 = []string{"-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestH264Oracle(t *testing.T) {
	needOracle(t)
	x264 := func(extra ...string) []string {
		return append([]string{"-c:v", "libx264", "-preset", "ultrafast"}, extra...)
	}
	cases := []struct {
		name     string
		size     string
		args     []string
		optional bool
		opts     cmpOpts
	}{
		{"baseline", "320x240", x264("-profile:v", "baseline", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"main", "320x240", x264("-profile:v", "main", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"high", "320x240", []string{"-c:v", "libx264", "-preset", "medium", "-profile:v", "high", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"high10", "320x240", x264("-profile:v", "high10", "-pix_fmt", "yuv420p10le"), false, cmpOpts{}},
		{"high422", "320x240", x264("-profile:v", "high422", "-pix_fmt", "yuv422p"), false, cmpOpts{}},
		{"high422-10", "320x240", x264("-profile:v", "high422", "-pix_fmt", "yuv422p10le"), false, cmpOpts{}},
		{"high444", "320x240", x264("-profile:v", "high444", "-pix_fmt", "yuv444p"), false, cmpOpts{}},
		{"crop-242", "320x242", x264("-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"crop-odd-444", "321x243", x264("-profile:v", "high444", "-pix_fmt", "yuv444p"), false, cmpOpts{}},
		{"1080p", "1920x1080", x264("-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"interlaced", "720x576", []string{"-c:v", "libx264", "-preset", "medium", "-flags", "+ildct+ilme", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"interlaced-1080", "1920x1080", []string{"-c:v", "libx264", "-preset", "fast", "-flags", "+ildct+ilme", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"sar-4-3", "320x240", x264("-vf", "setsar=4/3", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"sar-ext", "320x240", x264("-vf", "setsar=7/5", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"hlg", "320x240", cat(x264("-pix_fmt", "yuv420p10le"), hlg), false, cmpOpts{wantColor: true}},
		{"bt709", "320x240", cat(x264("-pix_fmt", "yuv420p"), bt709), false, cmpOpts{wantColor: true}},
		{"fullrange", "320x240", cat(x264("-pix_fmt", "yuvj420p"), []string{"-color_range", "pc"}), false, cmpOpts{}},
		{"scaling-lists", "320x240", []string{"-c:v", "libx264", "-preset", "medium", "-x264-params", "cqm=jvt", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"ntsc-rate", "320x240", x264("-r", "30000/1001", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"videotoolbox", "640x360", []string{"-c:v", "h264_videotoolbox", "-b:v", "1M", "-pix_fmt", "yuv420p"}, true, cmpOpts{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := generate(t, c.optional, "v.mp4", cat(src(c.size), c.args, []string{"-movflags", "+faststart"})...)
			v, err := ParseAVCC(findBox(t, path, "avcC"))
			if err != nil {
				t.Fatal(err)
			}
			compareVideo(t, v, ffprobe(t, path), c.opts)
		})
	}
}

func TestHEVCOracle(t *testing.T) {
	needOracle(t)
	x265 := func(extra ...string) []string {
		return append([]string{"-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "log-level=error"}, extra...)
	}
	cases := []struct {
		name     string
		size     string
		args     []string
		optional bool
		opts     cmpOpts
	}{
		{"main", "320x240", x265("-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"main-medium", "640x360", []string{"-c:v", "libx265", "-preset", "medium", "-tag:v", "hvc1", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"main10", "320x240", x265("-pix_fmt", "yuv420p10le"), false, cmpOpts{}},
		{"422-10", "320x240", x265("-pix_fmt", "yuv422p10le"), false, cmpOpts{}},
		{"444", "320x240", x265("-pix_fmt", "yuv444p"), false, cmpOpts{}},
		{"crop-242", "322x242", x265("-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"1080p", "1920x1080", x265("-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"hlg", "320x240", cat(x265("-pix_fmt", "yuv420p10le"), hlg), false, cmpOpts{wantColor: true}},
		{"pq", "320x240", cat(x265("-pix_fmt", "yuv420p10le"), pq), false, cmpOpts{wantColor: true}},
		{"sar", "320x240", x265("-vf", "setsar=4/3", "-pix_fmt", "yuv420p"), false, cmpOpts{}},
		{"scaling-lists", "320x240", []string{"-c:v", "libx265", "-preset", "medium", "-tag:v", "hvc1", "-x265-params", "log-level=error:scaling-list=default", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"long-gop-rps", "320x240", []string{"-c:v", "libx265", "-preset", "slow", "-tag:v", "hvc1", "-x265-params", "log-level=error:ref=4:bframes=4", "-pix_fmt", "yuv420p"}, false, cmpOpts{}},
		{"videotoolbox", "640x360", cat([]string{"-c:v", "hevc_videotoolbox", "-b:v", "1M", "-tag:v", "hvc1", "-pix_fmt", "yuv420p10le", "-profile:v", "main10"}, hlg), true, cmpOpts{wantColor: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := generate(t, c.optional, "v.mp4", cat(src(c.size), c.args, []string{"-movflags", "+faststart"})...)
			v, err := ParseHVCC(findBox(t, path, "hvcC"))
			if err != nil {
				t.Fatal(err)
			}
			p := ffprobe(t, path)
			compareVideo(t, v, p, c.opts)
			if v.HasBFrames >= 0 && v.HasBFrames != p.HasBFrames {
				t.Errorf("has_b_frames %d, ffprobe %d", v.HasBFrames, p.HasBFrames)
			}
		})
	}
}

func TestAV1Oracle(t *testing.T) {
	needOracle(t)
	cases := []struct {
		name string
		size string
		args []string
		opts cmpOpts
	}{
		{"8bit", "320x240", []string{"-pix_fmt", "yuv420p"}, cmpOpts{}},
		{"10bit", "320x240", []string{"-pix_fmt", "yuv420p10le"}, cmpOpts{}},
		{"odd-size", "322x246", []string{"-pix_fmt", "yuv420p"}, cmpOpts{}},
		{"1080p", "1920x1080", []string{"-pix_fmt", "yuv420p"}, cmpOpts{}},
		{"hlg", "320x240", cat([]string{"-pix_fmt", "yuv420p10le"}, hlg), cmpOpts{wantColor: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := cat(src(c.size), []string{"-c:v", "libsvtav1", "-preset", "12", "-svtav1-params", "lp=1"}, c.args, []string{"-movflags", "+faststart"})
			path := generate(t, false, "v.mp4", args...)
			rec := findBox(t, path, "av1C")
			v, err := ParseAV1C(rec)
			if err != nil {
				t.Fatal(err)
			}
			if v.Width == 0 {
				t.Fatalf("av1C had no parsable sequence header")
			}
			p := ffprobe(t, path)
			compareVideo(t, v, p, c.opts)

			// The fixed av1C fields alone must agree as well.
			fixed, err := ParseAV1C(rec[:4])
			if err != nil {
				t.Fatal(err)
			}
			if fixed.Profile != v.Profile || fixed.Level != v.Level || fixed.BitDepth != v.BitDepth || fixed.Chroma != v.Chroma {
				t.Errorf("av1C fixed fields %+v disagree with sequence header %+v", fixed, v)
			}
		})
	}
}

// ivfFirstFrame returns the first frame of an IVF file.
func ivfFirstFrame(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 44 || string(data[:4]) != "DKIF" {
		t.Fatalf("not an IVF file")
	}
	n := int(binary.LittleEndian.Uint32(data[32:]))
	if 44+n > len(data) {
		t.Fatalf("short IVF frame")
	}
	f := data[44 : 44+n]
	dump(t, "frame", f)
	return f
}

func TestVP9Oracle(t *testing.T) {
	needOracle(t)
	cases := []struct {
		name  string
		size  string
		args  []string
		color bool
	}{
		{"profile0", "320x240", []string{"-pix_fmt", "yuv420p"}, false},
		{"profile0-bt709", "322x242", cat([]string{"-pix_fmt", "yuv420p"}, bt709), true},
		{"profile1-444", "320x240", []string{"-pix_fmt", "yuv444p"}, false},
		{"profile2", "320x240", []string{"-pix_fmt", "yuv420p10le"}, false},
		{"profile2-hlg", "320x240", cat([]string{"-pix_fmt", "yuv420p10le"}, hlg), true},
		{"profile3-422-10", "320x240", []string{"-pix_fmt", "yuv422p10le"}, false},
	}
	enc := []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mp4 := generate(t, false, "v.mp4", cat(src(c.size), enc, c.args, []string{"-movflags", "+faststart"})...)
			p := ffprobe(t, mp4)
			v, err := ParseVPCC(findBox(t, mp4, "vpcC"))
			if err != nil {
				t.Fatal(err)
			}
			compareVideo(t, v, p, cmpOpts{skipLevel: true, skipSize: true, wantColor: c.color})

			ivf := generate(t, false, "v.ivf", cat(src(c.size), enc, c.args)...)
			pi := ffprobe(t, ivf)
			f, err := ParseVP9Frame(ivfFirstFrame(t, ivf))
			if err != nil {
				t.Fatal(err)
			}
			compareVideo(t, f, pi, cmpOpts{skipLevel: true})
			if len(ivfFirstFrame(t, ivf)) > 64 {
				if _, err := ParseVP9Frame(ivfFirstFrame(t, ivf)[:64]); err != nil {
					t.Errorf("64 bytes should be enough: %v", err)
				}
			}
		})
	}
}

func TestVP8Oracle(t *testing.T) {
	needOracle(t)
	for _, size := range []string{"320x240", "322x242", "1920x1080"} {
		t.Run(size, func(t *testing.T) {
			ivf := generate(t, false, "v.ivf", cat(src(size), []string{"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-pix_fmt", "yuv420p"})...)
			v, err := ParseVP8Frame(ivfFirstFrame(t, ivf))
			if err != nil {
				t.Fatal(err)
			}
			compareVideo(t, v, ffprobe(t, ivf), cmpOpts{skipLevel: true, skipCoded: true})
		})
	}
}

func sine(rate int, layout string) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d", rate), "-t", "0.5",
		"-af", "aformat=channel_layouts=" + layout}
}

func compareAudio(t *testing.T, a *Audio, p probeStream, skipProfile bool) {
	t.Helper()
	if a.Codec != p.CodecName {
		t.Errorf("codec %q, ffprobe %q", a.Codec, p.CodecName)
	}
	if !skipProfile && a.Profile != p.Profile {
		t.Errorf("profile %q, ffprobe %q", a.Profile, p.Profile)
	}
	if strconv.Itoa(a.SampleRate) != p.SampleRate {
		t.Errorf("sample rate %d, ffprobe %s", a.SampleRate, p.SampleRate)
	}
	if a.Channels != p.Channels {
		t.Errorf("channels %d, ffprobe %d", a.Channels, p.Channels)
	}
	if a.ChannelLayout != p.ChannelLayout {
		t.Errorf("layout %q, ffprobe %q", a.ChannelLayout, p.ChannelLayout)
	}
}

func TestAACOracle(t *testing.T) {
	needOracle(t)
	cases := []struct {
		name     string
		rate     int
		layout   string
		enc      []string
		optional bool
	}{
		{"lc-mono-44k", 44100, "mono", []string{"-c:a", "aac"}, false},
		{"lc-stereo-48k", 48000, "stereo", []string{"-c:a", "aac"}, false},
		{"lc-stereo-22k", 22050, "stereo", []string{"-c:a", "aac"}, false},
		{"lc-5.1-48k", 48000, "5.1", []string{"-c:a", "aac"}, false},
		{"lc-3.0", 48000, "3.0", []string{"-c:a", "aac"}, false},
		{"lc-5.0", 48000, "5.0", []string{"-c:a", "aac"}, false},
		{"lc-7.1", 48000, "7.1", []string{"-c:a", "aac"}, true},
		{"lc-at-stereo", 44100, "stereo", []string{"-c:a", "aac_at"}, true},
		{"he-at-stereo", 44100, "stereo", []string{"-c:a", "aac_at", "-profile:a", "4", "-b:a", "48k"}, true},
		{"hev2-at-stereo", 44100, "stereo", []string{"-c:a", "aac_at", "-profile:a", "28", "-b:a", "32k"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := generate(t, c.optional, "a.mp4", cat(sine(c.rate, c.layout), c.enc, []string{"-movflags", "+faststart"})...)
			oti, asc, _, _, err := ParseESDS(findBox(t, path, "esds"))
			if err != nil {
				t.Fatal(err)
			}
			if oti != 0x40 {
				t.Fatalf("objectTypeIndication %#x", oti)
			}
			dump(t, "asc", asc)
			a, err := ParseAudioSpecificConfig(asc)
			if err != nil {
				t.Fatal(err)
			}
			compareAudio(t, a, ffprobe(t, path), false)
		})
	}
}

func TestOpusOracle(t *testing.T) {
	needOracle(t)
	for _, layout := range []string{"mono", "stereo", "5.1"} {
		t.Run(layout, func(t *testing.T) {
			enc := []string{"-c:a", "libopus", "-mapping_family", "1"}
			if layout != "5.1" {
				enc = enc[:2]
			}
			mp4 := generate(t, false, "a.mp4", cat(sine(48000, layout), enc, []string{"-movflags", "+faststart"})...)
			a, err := ParseDOps(findBox(t, mp4, "dOps"))
			if err != nil {
				t.Fatal(err)
			}
			compareAudio(t, a, ffprobe(t, mp4), true)

			mka := generate(t, false, "a.mka", cat(sine(48000, layout), enc)...)
			data, _ := os.ReadFile(mka)
			i := bytes.Index(data, []byte("OpusHead"))
			if i < 0 {
				t.Fatal("OpusHead not found")
			}
			end := min(i+19+2+8, len(data))
			dump(t, "OpusHead", data[i:end])
			h, err := ParseOpusHead(data[i:end])
			if err != nil {
				t.Fatal(err)
			}
			compareAudio(t, h, ffprobe(t, mka), true)
			if h.PreSkip != a.PreSkip || h.PreSkip == 0 {
				t.Errorf("pre-skip OpusHead %d, dOps %d", h.PreSkip, a.PreSkip)
			}
		})
	}
}

func TestFLACOracle(t *testing.T) {
	needOracle(t)
	cases := []struct {
		name, layout, fmt string
		rate, depth       int
	}{
		{"16bit-stereo", "stereo", "s16", 44100, 16},
		{"24bit-stereo", "stereo", "s32", 96000, 24},
		{"16bit-mono", "mono", "s16", 48000, 16},
		{"16bit-5.1", "5.1", "s16", 48000, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := []string{"-c:a", "flac", "-sample_fmt", c.fmt}
			raw := generate(t, false, "a.flac", cat(sine(c.rate, c.layout), enc)...)
			data, _ := os.ReadFile(raw)
			dump(t, "fLaC", data[:min(42, len(data))])
			a, err := ParseFLACStreamInfo(data[:min(42, len(data))])
			if err != nil {
				t.Fatal(err)
			}
			compareAudio(t, a, ffprobe(t, raw), true)
			if a.BitDepth != c.depth {
				t.Errorf("bit depth %d, want %d", a.BitDepth, c.depth)
			}
			if a.TotalSamples != int64(c.rate/2) {
				t.Errorf("total samples %d, want %d", a.TotalSamples, c.rate/2)
			}

			mp4 := generate(t, false, "a.mp4", cat(sine(c.rate, c.layout), enc, []string{"-strict", "-2", "-movflags", "+faststart"})...)
			box := findBox(t, mp4, "dfLa")
			b, err := ParseFLACStreamInfo(box)
			if err != nil {
				t.Fatal(err)
			}
			bare, err := ParseFLACStreamInfo(box[8:42])
			if err != nil {
				t.Fatal(err)
			}
			pm := ffprobe(t, mp4)
			// The MP4 muxer adds its own channel layout box, so only the
			// STREAMINFO-derived fields are compared here.
			if strconv.Itoa(b.SampleRate) != pm.SampleRate || b.Channels != pm.Channels {
				t.Errorf("dfLa %d Hz %d ch, ffprobe %s Hz %d ch", b.SampleRate, b.Channels, pm.SampleRate, pm.Channels)
			}
			if strconv.Itoa(b.BitDepth) != pm.BitsPerRaw {
				t.Errorf("dfLa bit depth %d, ffprobe %s", b.BitDepth, pm.BitsPerRaw)
			}
			if *b != *bare {
				t.Errorf("dfLa %+v differs from bare %+v", b, bare)
			}
		})
	}
}

func TestMPEGAudioOracle(t *testing.T) {
	needOracle(t)
	cases := []struct {
		name, layout string
		rate         int
		enc          []string
		bitrate      int
		mux          []string
	}{
		{"mp3-44k-stereo", "stereo", 44100, []string{"-c:a", "libmp3lame", "-b:a", "128k"}, 128000, []string{"-id3v2_version", "0", "-write_xing", "0", "-f", "mp3"}},
		{"mp3-48k-mono", "mono", 48000, []string{"-c:a", "libmp3lame", "-b:a", "64k"}, 64000, []string{"-id3v2_version", "0", "-write_xing", "0", "-f", "mp3"}},
		{"mp3-22k-stereo", "stereo", 22050, []string{"-c:a", "libmp3lame", "-b:a", "32k"}, 32000, []string{"-id3v2_version", "0", "-write_xing", "0", "-f", "mp3"}},
		{"mp3-8k-mono", "mono", 8000, []string{"-c:a", "libmp3lame", "-b:a", "8k"}, 8000, []string{"-id3v2_version", "0", "-write_xing", "0", "-f", "mp3"}},
		{"mp2-48k-stereo", "stereo", 48000, []string{"-c:a", "mp2", "-b:a", "192k"}, 192000, []string{"-f", "mp2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := generate(t, false, "a.mp3", cat(sine(c.rate, c.layout), c.enc, c.mux)...)
			data, _ := os.ReadFile(path)
			dump(t, "mpa", data[:4])
			a, err := ParseMPEGAudioHeader(data)
			if err != nil {
				t.Fatal(err)
			}
			compareAudio(t, a, ffprobe(t, path), true)
			if a.BitRate != c.bitrate {
				t.Errorf("bitrate %d, want %d", a.BitRate, c.bitrate)
			}
		})
	}
}
