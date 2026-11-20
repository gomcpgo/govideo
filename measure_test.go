package govideo_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// TestMeasureH264 is a measurement, not a check: it encodes the start of
// every clip of the real-file corpus with a range of H.264 encoder settings
// and writes size and quality for each to a CSV file, so that a change of
// default can be judged by size at equal quality. Quality is measured by the
// oracle against the exact pictures given to the encoder.
//
//	GOVIDEO_TEST_MEDIA=/path/to/videos GOVIDEO_MEASURE=/tmp/h264.csv \
//	    go test . -run TestMeasureH264 -v -timeout 2h
//
// GOVIDEO_MEASURE_ONLY, if set, limits the run to file names containing it.
func TestMeasureH264(t *testing.T) {
	root, dst := os.Getenv("GOVIDEO_TEST_MEDIA"), os.Getenv("GOVIDEO_MEASURE")
	if root == "" || dst == "" {
		t.Skip("GOVIDEO_TEST_MEDIA and GOVIDEO_MEASURE are not both set")
	}
	oracle.Require(t)
	requireOS(t)
	if _, err := os.Stat(filepath.Join(root, "video")); err == nil {
		root = filepath.Join(root, "video")
	}
	const w, h, fps, seconds = 1280, 720, 30, 5
	pixels := float64(w * h * fps)

	type variant struct {
		name string
		cfg  oscodec.EncodeConfig
	}
	var variants []variant
	for _, reorder := range []bool{false, true} {
		suffix := map[bool]string{false: "", true: "+b"}[reorder]
		// Average bit rate at the bits-per-pixel targets of the three
		// Quality presets.
		for _, bpp := range []float64{0.045, 0.08, 0.13} {
			variants = append(variants, variant{fmt.Sprintf("abr%.3f%s", bpp, suffix),
				oscodec.EncodeConfig{Bitrate: int(bpp * pixels), Reorder: reorder}})
		}
		for _, q := range []float64{0.45, 0.5, 0.55, 0.6, 0.65, 0.7} {
			variants = append(variants, variant{fmt.Sprintf("q%.2f%s", q, suffix),
				oscodec.EncodeConfig{Quality: q, Reorder: reorder}})
		}
	}

	csv, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer csv.Close()
	fmt.Fprintln(csv, "file,variant,reorder,kbps,psnr_y,psnr_avg,ssim,vmaf,encode_s")

	tmp := t.TempDir()
	ref, out := filepath.Join(tmp, "ref.yuv"), filepath.Join(tmp, "out.mp4")
	number := func(log, pattern string) string {
		m := regexp.MustCompile(pattern).FindStringSubmatch(log)
		if m == nil {
			return ""
		}
		return m[1]
	}
	only := os.Getenv("GOVIDEO_MEASURE_ONLY")
	for _, in := range corpusFiles(t, root) {
		name := filepath.Base(in)
		if !strings.Contains(name, only) {
			continue
		}
		for i, v := range variants {
			// The pictures are the same for every variant, so they are
			// recorded once.
			var tap func(*codec.VideoFrame)
			var rf *os.File
			var rw *bufio.Writer
			if i == 0 {
				if rf, err = os.Create(ref); err != nil {
					t.Fatal(err)
				}
				rw = bufio.NewWriterSize(rf, 1<<20)
				tap = func(f *codec.VideoFrame) {
					for y := range f.Height {
						rw.Write(f.Y[y*f.YStride : y*f.YStride+f.Width])
					}
					for _, p := range [][]byte{f.Cb, f.Cr} {
						for y := range f.Height / 2 {
							rw.Write(p[y*f.CStride : y*f.CStride+f.Width/2])
						}
					}
				}
			}
			restore := govideo.HookH264(func(c *oscodec.EncodeConfig) {
				c.Bitrate, c.Quality, c.MaxBitrate, c.Reorder = v.cfg.Bitrate, v.cfg.Quality, v.cfg.MaxBitrate, v.cfg.Reorder
			}, tap)
			tl := govideo.NewTimeline(govideo.Canvas{Width: w, Height: h, FrameRate: govideo.R(fps, 1)})
			tl.AddClip(in, govideo.ClipOptions{Trim: govideo.Range(0, seconds*time.Second)})
			start := time.Now()
			err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileCompatible})
			took := time.Since(start)
			restore()
			if rf != nil {
				rw.Flush()
				rf.Close()
			}
			if err != nil {
				t.Logf("%s: %v", name, err)
				break
			}
			pr := oracle.FFprobe(t, out)
			rate, _ := oracle.Seconds(pr.Streams[0].BitRate)
			log := oracle.FFmpegLog(t, "-i", out, "-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", fmt.Sprintf("%dx%d", w, h),
				"-r", strconv.Itoa(fps), "-i", ref, "-lavfi",
				"[0:v]split=3[a][b][c];[1:v]split=3[x][y][z];[a][x]psnr;[b][y]ssim;[c][z]libvmaf=n_threads=8:n_subsample=5", "-f", "null", "-")
			fmt.Fprintf(csv, "%s,%s,%v,%.0f,%s,%s,%s,%s,%.2f\n", name, v.name, v.cfg.Reorder, rate/1000,
				number(log, `PSNR y:([\d.]+)`), number(log, `average:([\d.]+)`), number(log, `SSIM .*All:([\d.]+)`),
				number(log, `VMAF score: ([\d.]+)`), took.Seconds())
		}
		csv.Sync()
	}
}
