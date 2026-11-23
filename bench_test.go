package govideo

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo/videoprobe"
)

// benchClips returns the files to benchmark decoding on: those named in
// GOVIDEO_BENCH (separated by spaces), else a 1080p test card encoded here.
// The card is flat colour and costs the decoder little, so what the
// benchmark shows on it is mostly govideo's own share of the work.
func benchClips(b *testing.B) []string {
	if files := strings.Fields(os.Getenv("GOVIDEO_BENCH")); len(files) > 0 {
		return files
	}
	if !BuildInfo().OSBackend {
		b.Skip("no operating-system codec backend on this platform")
	}
	path := filepath.Join(b.TempDir(), "card1080.mp4")
	tl := NewTimeline(Canvas{Width: 1920, Height: 1080, FrameRate: R(30, 1)})
	tl.AddGenerated(3*time.Second, GenerateOptions{Pattern: PatternTestCard})
	if err := tl.Export(context.Background(), path, ExportOptions{Profile: ProfileCompatible}); err != nil {
		b.Fatal(err)
	}
	return []string{path}
}

// BenchmarkDecode reads the video stream of each file as an export does,
// through frame selection, and reports the time and the memory allocated
// per source frame. "all" shows every frame; "1in4" shows every fourth, as
// a 120 fps clip on a 30 fps canvas does, so three frames in four are
// decoded and never looked at.
func BenchmarkDecode(b *testing.B) {
	ctx := context.Background()
	for _, path := range benchClips(b) {
		for _, step := range []struct {
			name  string
			every int
		}{{"all", 1}, {"1in4", 4}} {
			b.Run(filepath.Base(path)+"/"+step.name, func(b *testing.B) {
				decs := &videoDecoders{}
				defer decs.close()
				frames := int64(0)
				var m0, m1 runtime.MemStats
				runtime.ReadMemStats(&m0)
				start := time.Now()
				for i := 0; i < b.N; i++ {
					m, err := openMedia(ctx, path)
					if err != nil {
						b.Fatal(err)
					}
					vs := m.stream(videoprobe.Video)
					if vs == nil || vs.Video.FrameRate.Float() <= 0 {
						b.Fatalf("%s: no video stream with a frame rate", path)
					}
					v, err := openVideo(ctx, m, vs, 0, 0, DeinterlaceOff, decs)
					if err != nil {
						b.Fatal(err)
					}
					span := time.Duration(float64(step.every) / vs.Video.FrameRate.Float() * 1e9)
					var seq int64
					for at := vs.StartTime; at < vs.StartTime+vs.Duration; at += span {
						if _, seq, err = v.frameAt(at); err != nil {
							b.Fatal(err)
						}
					}
					frames += seq
					v.close()
					m.close()
				}
				elapsed := time.Since(start)
				runtime.ReadMemStats(&m1)
				b.ReportMetric(float64(elapsed.Microseconds())/float64(frames), "µs/frame")
				b.ReportMetric(float64(m1.TotalAlloc-m0.TotalAlloc)/float64(frames)/1e3, "kB/frame")
			})
		}
	}
}

// BenchmarkCuts reads 32 half-second cuts of one file, as the clips of a
// timeline are read one after another: each seeks to the keyframe before
// its cut, decodes up to it and shows fifteen frames. "shared" gives every
// cut the decoder session of the one before, as an export does; "fresh"
// opens a session per cut.
func BenchmarkCuts(b *testing.B) {
	ctx := context.Background()
	src := benchClips(b)[0]
	m, err := openMedia(ctx, src)
	if err != nil {
		b.Fatal(err)
	}
	defer m.close()
	vs := m.stream(videoprobe.Video)
	if vs == nil || vs.Duration < time.Second {
		b.Fatalf("%s: no video stream of a second or more", src)
	}
	for _, mode := range []string{"shared", "fresh"} {
		b.Run(mode, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				decs := &videoDecoders{}
				for n := 0; n < 32; n++ {
					if mode == "fresh" && n > 0 {
						decs.close()
						decs = &videoDecoders{}
					}
					at := vs.StartTime + time.Duration(n*7%16)*(vs.Duration-500*time.Millisecond)/16
					v, err := openVideo(ctx, m, vs, at, 0, DeinterlaceOff, decs)
					if err != nil {
						b.Fatal(err)
					}
					for f := 0; f < 15; f++ {
						if _, _, err := v.frameAt(at + time.Duration(f)*time.Second/30); err != nil {
							b.Fatal(err)
						}
					}
					v.close()
				}
				decs.close()
			}
		})
	}
}
