// Package oracle is a test-only harness that runs locally installed ffmpeg
// and ffprobe binaries as black-box oracles (SPEC §11). The binaries are
// never vendored and their source is never read; tests skip when they are
// not installed, or fail if StrictEnv is set. Nothing outside _test files
// may import this package.
package oracle

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// StrictEnv names the environment variable that, set to anything but "" or
// "0", makes a test fail where it would skip because the oracle lacks
// something. CI sets it on the runners whose ffmpeg is known to be complete,
// so that a test cannot stop running without anyone noticing.
const StrictEnv = "GOVIDEO_ORACLE_STRICT"

// Strict reports whether a missing oracle fails tests (see StrictEnv).
func Strict() bool {
	v := os.Getenv(StrictEnv)
	return v != "" && v != "0"
}

// Missing ends a test that cannot run because the oracle is not installed,
// lacks an encoder, or cannot make a fixture: with a skip, or with a failure
// when Strict. Every skip of that kind goes through here; a skip for any
// other reason (no system codecs on this platform, an encoder only one
// platform has) calls t.Skip itself and stays a skip.
func Missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if Strict() {
		t.Fatalf(format+" ("+StrictEnv+" is set: the oracle must be complete here)", args...)
	}
	t.Skipf(format, args...)
}

// Require ends the test, as Missing does, unless ffmpeg and ffprobe are on
// PATH.
func Require(t testing.TB) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			Missing(t, "%s is not installed: no oracle to test against", bin)
		}
	}
}

// FFmpeg runs ffmpeg with the given arguments, failing the test on error.
func FFmpeg(t testing.TB, args ...string) {
	t.Helper()
	if err := TryFFmpeg(args...); err != nil {
		t.Fatalf("ffmpeg %s: %v", strings.Join(args, " "), err)
	}
}

// TryFFmpeg runs ffmpeg and returns its error (with stderr) if it fails.
func TryFFmpeg(args ...string) error {
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &runError{err, stderr.String()}
	}
	return nil
}

// FFmpegLog runs ffmpeg at the "info" log level and returns what it wrote to
// standard error, failing the test on error. Filters that measure (psnr,
// ssim, libvmaf) report there.
func FFmpegLog(t testing.TB, args ...string) string {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "info", "-nostats", "-nostdin"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stderr.String()
}

type runError struct {
	err    error
	stderr string
}

func (e *runError) Error() string { return e.err.Error() + ": " + e.stderr }

// Stream is the subset of ffprobe's per-stream output used by tests.
type Stream struct {
	Index         int               `json:"index"`
	CodecName     string            `json:"codec_name"`
	CodecType     string            `json:"codec_type"`
	CodecTag      string            `json:"codec_tag_string"`
	Profile       string            `json:"profile"`
	Level         int               `json:"level"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	CodedWidth    int               `json:"coded_width"`
	CodedHeight   int               `json:"coded_height"`
	SAR           string            `json:"sample_aspect_ratio"`
	DAR           string            `json:"display_aspect_ratio"`
	PixFmt        string            `json:"pix_fmt"`
	HasBFrames    int               `json:"has_b_frames"`
	FieldOrder    string            `json:"field_order"`
	RFrameRate    string            `json:"r_frame_rate"`
	AvgFrameRate  string            `json:"avg_frame_rate"`
	TimeBase      string            `json:"time_base"`
	StartTime     string            `json:"start_time"`
	Duration      string            `json:"duration"`
	BitRate       string            `json:"bit_rate"`
	NbFrames      string            `json:"nb_frames"`
	NbReadPackets string            `json:"nb_read_packets"`
	ColorRange    string            `json:"color_range"`
	ColorSpace    string            `json:"color_space"`
	ColorTransfer string            `json:"color_transfer"`
	ColorPrim     string            `json:"color_primaries"`
	SampleRate    string            `json:"sample_rate"`
	Channels      int               `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	BitsPerSample int               `json:"bits_per_sample"`
	BitsPerRaw    string            `json:"bits_per_raw_sample"`
	Tags          map[string]string `json:"tags"`
	Disposition   map[string]int    `json:"disposition"`
	SideData      []map[string]any  `json:"side_data_list"`
}

// Format is the subset of ffprobe's format output used by tests.
type Format struct {
	FormatName string            `json:"format_name"`
	NbStreams  int               `json:"nb_streams"`
	StartTime  string            `json:"start_time"`
	Duration   string            `json:"duration"`
	Size       string            `json:"size"`
	BitRate    string            `json:"bit_rate"`
	Tags       map[string]string `json:"tags"`
}

// Result is ffprobe's view of a file.
type Result struct {
	Streams []Stream `json:"streams"`
	Format  Format   `json:"format"`
}

// FFprobe runs ffprobe -show_format -show_streams on path. Extra arguments
// (for example -count_packets) may be given.
func FFprobe(t testing.TB, path string, extra ...string) *Result {
	t.Helper()
	args := append([]string{"-v", "error", "-show_format", "-show_streams", "-of", "json"}, extra...)
	out, err := exec.Command("ffprobe", append(args, path)...).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var r Result
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("ffprobe output: %v", err)
	}
	return &r
}

// Seconds parses an ffprobe decimal-seconds string; ok is false for "N/A".
func Seconds(s string) (v float64, ok bool) {
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// Rotation returns the display-matrix rotation ffprobe reports (0 if none).
func (s *Stream) Rotation() int {
	for _, sd := range s.SideData {
		if r, ok := sd["rotation"].(float64); ok {
			return int(r)
		}
	}
	return 0
}

// Ratio parses "a/b" or "a:b" into a float (0 when undefined).
func Ratio(s string) float64 {
	s = strings.ReplaceAll(s, ":", "/")
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0
	}
	n, _ := strconv.ParseFloat(a, 64)
	d, _ := strconv.ParseFloat(b, 64)
	if d == 0 {
		return 0
	}
	return n / d
}

// FFmpegOutput runs ffmpeg and returns what it wrote to standard output,
// failing the test on error. It is used to decode media to raw samples.
func FFmpegOutput(t testing.TB, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-nostdin"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// Frames decodes every video frame of path to packed RGB24 at w by h.
func Frames(t testing.TB, path string, w, h int) [][]byte {
	t.Helper()
	raw := FFmpegOutput(t, "-i", path, "-map", "0:v:0", "-vf", "scale="+strconv.Itoa(w)+":"+strconv.Itoa(h)+":flags=neighbor",
		"-fps_mode", "passthrough", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	size := w * h * 3
	frames := make([][]byte, 0, len(raw)/size)
	for ; len(raw) >= size; raw = raw[size:] {
		frames = append(frames, raw[:size])
	}
	return frames
}

// Audio decodes an audio stream of path (for example "0:a:0") to mono
// float32 samples at 48 kHz, honouring edit lists as a player would. Extra
// output arguments (a filter, say) may be given.
func Audio(t testing.TB, path, stream string, extra ...string) []float32 {
	t.Helper()
	args := append([]string{"-i", path, "-map", stream}, extra...)
	raw := FFmpegOutput(t, append(args, "-ac", "1", "-ar", "48000", "-f", "f32le", "-")...)
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}
