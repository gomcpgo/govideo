package govideo_test

import (
	"context"
	"errors"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/videoprobe"
)

// TestRealMedia runs a corpus of real files, named by GOVIDEO_TEST_MEDIA,
// through the pipeline and compares the results with the oracle's own
// decoding. Without the variable it is skipped.
//
// The directory either holds media files directly (all treated as video
// clips that must export) or has these sub-directories:
//
//	video/        clips that must export with every profile and match the
//	              oracle's picture, from the start and from a cut inside
//	audio/        files laid over a generated clip with AddAudio; each must
//	              either work or fail with ErrCodecUnavailable
//	stills/       images for AddImage (searched recursively)
//	unsupported/  files that must be refused with an error naming the codec
//	              or the format
func TestRealMedia(t *testing.T) {
	root := os.Getenv("GOVIDEO_TEST_MEDIA")
	if root == "" {
		t.Skip("GOVIDEO_TEST_MEDIA is not set")
	}
	oracle.Require(t)
	if _, err := os.Stat(filepath.Join(root, "video")); err != nil {
		corpusVideo(t, root)
		return
	}
	t.Run("video", func(t *testing.T) { corpusVideo(t, filepath.Join(root, "video")) })
	t.Run("audio", func(t *testing.T) { corpusAudio(t, filepath.Join(root, "audio")) })
	t.Run("stills", func(t *testing.T) { corpusStills(t, filepath.Join(root, "stills")) })
	t.Run("unsupported", func(t *testing.T) { corpusUnsupported(t, filepath.Join(root, "unsupported")) })
}

// corpusFiles lists the regular files under dir, recursively.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") && !strings.EqualFold(filepath.Ext(p), ".md") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	if len(files) == 0 {
		t.Skipf("no files in %s", dir)
	}
	return files
}

// thumbs decodes video to 64x36 RGB frames after applying a filter chain.
func thumbs(t *testing.T, filter string, args ...string) [][]byte {
	t.Helper()
	args = append(args, "-map", "0:v:0", "-vf", filter+"scale=64:36", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	raw := oracle.FFmpegOutput(t, args...)
	const size = 64 * 36 * 3
	var out [][]byte
	for ; len(raw) >= size; raw = raw[size:] {
		out = append(out, raw[:size])
	}
	return out
}

// hdrTone is the oracle's rendering of a high dynamic range file in the
// standard range, for the corpus to compare with: zscale to linear light,
// the "hable" curve of its tonemap filter, and zscale back to BT.709. It is
// another curve than the pipeline's (the oracle has none like it), picked as
// the nearest of those it has. Measured on the eight such files of the
// corpus, as thumbnails: 28 to 42 dB between the two, so they are held to
// the floor every other file is held to. That catches a wrong frame, a
// picture the wrong way up and one that was not tone-mapped (16 to 24 dB
// from the pipeline's on most of the files). What the tone mapping should
// give exactly is tested in the process package.
const hdrTone = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p,"

// corpusFloor is the PSNR a thumbnail of an export must reach against the
// oracle's picture of the source.
const corpusFloor = 26.0

// corpusTone returns the filters that go first in the oracle's chain for the
// picture of a corpus file: hdrTone for a high dynamic range file, nothing
// for any other. Where the oracle has no zscale filter it ends the test of
// such a file with a skip, so it is called once everything else about the
// export has been checked.
func corpusTone(t *testing.T, in string) string {
	t.Helper()
	info, err := videoprobe.ProbeFile(context.Background(), in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	for _, st := range info.Streams {
		if st.Kind != videoprobe.Video {
			continue
		}
		if st.Video.HDR == "" {
			break
		}
		if !oracle.HasFilter("zscale") {
			t.Skip("a high dynamic range file, and the oracle's ffmpeg has no zscale filter to tone-map it with: the picture is not compared (put a build that has it first on PATH; testmedia/README.md)")
		}
		return hdrTone
	}
	return ""
}

func psnr(a, b []byte) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return 10 * math.Log10(255*255/(sum/float64(len(a))+1e-3))
}

func corpusVideo(t *testing.T, dir string) {
	profiles := map[string]govideo.Profile{"intermediate.mov": govideo.ProfileIntermediate}
	if govideo.BuildInfo().OSBackend {
		profiles["compatible.mp4"] = govideo.ProfileCompatible
	}
	tmp := t.TempDir()
	const fit = "scale=640:360:force_original_aspect_ratio=decrease,pad=640:360:(ow-iw)/2:(oh-ih)/2,fps=30,"
	for _, in := range corpusFiles(t, dir) {
		name := filepath.Base(in)
		for suffix, profile := range profiles {
			t.Run(name+"/"+suffix, func(t *testing.T) {
				tl := govideo.NewTimeline(govideo.Canvas{Width: 640, Height: 360, FrameRate: govideo.R(30, 1)})
				tl.AddClip(in, govideo.ClipOptions{Trim: govideo.Range(0, 3*time.Second)})
				out := filepath.Join(tmp, name+"."+suffix)
				if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: profile}); err != nil {
					t.Fatalf("export: %v", err)
				}
				defer os.Remove(out)
				if err := oracle.TryFFmpeg("-xerror", "-i", out, "-f", "null", "-"); err != nil {
					t.Fatalf("ffmpeg cannot decode the export: %v", err)
				}
				pr := oracle.FFprobe(t, out)
				var d0 float64
				for i, st := range pr.Streams {
					d, _ := oracle.Seconds(st.Duration)
					if i == 0 {
						d0 = d
					}
					if d <= 0 || d > 3.05 || math.Abs(d-d0) > 0.05 {
						t.Errorf("stream %d lasts %.3fs (video %.3fs, trim 3s)", i, d, d0)
					}
				}
				// The picture must be what the oracle sees in the source,
				// rotated and fitted the same way. A frame either side is
				// allowed: the two pick frames by different rounding.
				tone := corpusTone(t, in)
				ours := thumbs(t, "", "-i", out)
				ref := thumbs(t, tone+fit, "-t", "3", "-i", in)
				if len(ours) == 0 || len(ref) == 0 {
					t.Fatalf("no frames to compare (%d exported, %d from the oracle)", len(ours), len(ref))
				}
				last := min(len(ours), len(ref)) - 1
				for _, n := range []int{0, last / 2, last} {
					best := 0.0
					for k := max(n-3, 0); k <= min(n+3, len(ref)-1); k++ {
						best = max(best, psnr(ours[n], ref[k]))
					}
					if best < corpusFloor {
						t.Errorf("frame %d differs from the oracle's decode (PSNR %.1f dB, want %.0f)", n, best, corpusFloor)
					} else if tone != "" {
						t.Logf("frame %d: %.1f dB from the oracle's tone-mapped picture", n, best)
					}
				}
			})
		}
		// Interlaced pictures are shown a field at a time. A frame that still
		// holds two fields has, wherever something moves, rows that differ
		// more from their neighbours than from the rows two away.
		t.Run(name+"/fields", func(t *testing.T) {
			info, err := videoprobe.ProbeFile(context.Background(), in)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			var v *videoprobe.VideoInfo
			for _, st := range info.Streams {
				if st.Kind == videoprobe.Video {
					v = st.Video
					break
				}
			}
			if v == nil || !v.Interlaced || !govideo.BuildInfo().OSBackend {
				t.Skip("not an interlaced stream that can be decoded here")
			}
			w, h := v.Width, v.Height
			tl := govideo.NewTimeline(govideo.Canvas{Width: w, Height: h, FrameRate: govideo.R(30, 1)})
			tl.AddClip(in, govideo.ClipOptions{Trim: govideo.Range(0, 2*time.Second)})
			out := filepath.Join(tmp, name+".fields.mov")
			if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
				t.Fatalf("export: %v", err)
			}
			defer os.Remove(out)
			raw := oracle.FFmpegOutput(t, "-i", out, "-pix_fmt", "gray", "-f", "rawvideo", "-")
			if len(raw) < w*h {
				t.Fatal("no frames to look at")
			}
			for n := 0; (n+1)*w*h <= len(raw); n++ {
				f := raw[n*w*h : (n+1)*w*h]
				var next, second float64
				for y := 0; y+2 < h; y++ {
					for x := 0; x < w; x++ {
						a := float64(f[y*w+x])
						next += math.Abs(a - float64(f[(y+1)*w+x]))
						second += math.Abs(a - float64(f[(y+2)*w+x]))
					}
				}
				if next > second {
					t.Fatalf("output frame %d is combed: rows differ from the next by %.2f a sample, from the one after by %.2f",
						n, next/float64(w*(h-2)), second/float64(w*(h-2)))
				}
			}
		})
		// A trim that starts inside the file makes the decoder start at the
		// keyframe before the cut (MP4 sync samples, the Matroska keyframe
		// index). The pictures must still be the ones the oracle reaches by
		// decoding from the start.
		t.Run(name+"/trim", func(t *testing.T) {
			info, err := videoprobe.ProbeFile(context.Background(), in)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			var span time.Duration
			for _, st := range info.Streams {
				if st.Kind == videoprobe.Video {
					span = st.Duration
					break
				}
			}
			// Whole tenths of a second are whole frames at 30 fps.
			cut := min(span*2/3, 8*time.Second).Truncate(100 * time.Millisecond)
			if cut < time.Second {
				t.Skipf("the video lasts %v: too short to cut into", span)
			}
			tl := govideo.NewTimeline(govideo.Canvas{Width: 640, Height: 360, FrameRate: govideo.R(30, 1)})
			tl.AddClip(in, govideo.ClipOptions{Trim: govideo.Range(cut, cut+time.Second)})
			out := filepath.Join(tmp, name+".trim.mov")
			if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
				t.Fatalf("export: %v", err)
			}
			defer os.Remove(out)
			tone := corpusTone(t, in)
			ours := thumbs(t, "", "-i", out)
			ref := thumbs(t, tone+fit, "-t", strconv.FormatFloat((cut+1500*time.Millisecond).Seconds(), 'f', 3, 64), "-i", in)
			off := int(cut * 30 / time.Second)
			if len(ours) == 0 || len(ref) == 0 {
				t.Fatalf("no frames to compare (%d exported, %d from the oracle)", len(ours), len(ref))
			}
			last := min(len(ours), len(ref)-off) - 1
			if len(ref) <= off {
				// The file was cut short before the trim: the picture holds
				// on the last frame that is there.
				t.Logf("the oracle decodes %d frames and the cut is at frame %d: expecting the last frame, held", len(ref), off)
				last = len(ours) - 1
			}
			for _, n := range []int{0, last / 2, last} {
				best := 0.0
				for k := max(min(off+n, len(ref)-1)-3, 0); k <= min(off+n+3, len(ref)-1); k++ {
					best = max(best, psnr(ours[n], ref[k]))
				}
				if best < corpusFloor {
					t.Errorf("frame %d after the cut at %v differs from the oracle's decode (PSNR %.1f dB, want %.0f)", n, cut, best, corpusFloor)
				}
			}
			// The sound is cut at the same place, and its decoder too starts
			// near the cut, not at the start of the file (govideo#23). The
			// clip's time starts at its first video frame; the oracle is
			// asked to keep the audio where its timestamps put it, with
			// silence before a late start.
			var origin time.Duration
			sounds := false
			for _, st := range info.Streams {
				switch st.Kind {
				case videoprobe.Video:
					if origin == 0 {
						origin = max(st.StartTime, 0)
					}
				case videoprobe.Audio:
					sounds = true
				}
			}
			if !sounds {
				return
			}
			sound := oracle.Audio(t, in, "0:a:0", "-af", "aresample=async=1:first_pts=0,aformat=channel_layouts=stereo")
			from := int((origin + cut) * 48000 / time.Second)
			if from >= len(sound) {
				return
			}
			sound = sound[from:]
			var energy float64
			for _, v := range sound[:min(len(sound), 48000)] {
				energy += float64(v) * float64(v)
			}
			if energy < 48000*2e-3*2e-3 {
				t.Logf("too quiet after the cut to compare the sound")
				return
			}
			// As in corpusAudio: decoders differ in the SBR delay they remove.
			if best, n := correlate(oracle.Audio(t, out, "0:a:0"), sound, 2048); n > 0 && best < 0.9 {
				t.Errorf("audio after the cut at %v correlates %.3f with the oracle's decode, want at least 0.9 (wrong timing or content)", cut, best)
			} else if n > 0 {
				t.Logf("audio after the cut at %v correlates %.3f with the oracle's decode", cut, best)
			}
		})
	}
}

// correlate returns how well ours follows ref (1 is a perfect match) at the
// shift of up to span samples either way that suits best, over the first two
// seconds, and how many samples that was measured on: 0 when there are too
// few.
func correlate(ours, ref []float32, span int) (best float64, n int) {
	n = min(len(ours), len(ref), 2*48000) - 2*span
	if n < 4800 {
		return 0, 0
	}
	best = -1.0
	for lag := -span; lag <= span; lag++ {
		var xy, xx, yy float64
		for i := span; i < span+n; i++ {
			x, y := float64(ours[i+lag]), float64(ref[i])
			xy, xx, yy = xy+x*y, xx+x*x, yy+y*y
		}
		best = max(best, xy/math.Sqrt(xx*yy+1e-30))
	}
	return best, n
}

func corpusAudio(t *testing.T, dir string) {
	tmp := t.TempDir()
	for _, in := range corpusFiles(t, dir) {
		name := filepath.Base(in)
		t.Run(name, func(t *testing.T) {
			tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180, FrameRate: govideo.R(30, 1)})
			tl.AddGenerated(3*time.Second, govideo.GenerateOptions{})
			tl.AddAudio(in, govideo.AudioOptions{})
			out := filepath.Join(tmp, name+".mov")
			err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			var cu *govideo.ErrCodecUnavailable
			if errors.As(err, &cu) {
				t.Logf("refused: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			defer os.Remove(out)
			ours := oracle.Audio(t, out, "0:a:0")
			ref := oracle.Audio(t, in, "0:a:0")
			var energy float64
			for _, v := range ours {
				energy += float64(v) * float64(v)
			}
			if energy == 0 && len(ref) > 0 {
				t.Fatal("the export is silent")
			}
			info, perr := videoprobe.ProbeFile(context.Background(), in)
			if perr != nil || len(info.Streams) == 0 || info.Streams[0].Audio == nil {
				return
			}
			// oracle.Audio folds both sides to mono, so the reference has to
			// reach stereo the way the export does for the levels to compare.
			// The oracle can decode more channels than the container declares
			// (parametric stereo makes two of a mono core), and it is what it
			// decodes that its filters are given.
			ch := info.Streams[0].Audio.Channels
			for _, st := range oracle.FFprobe(t, in).Streams {
				if st.CodecType == "audio" && st.Channels > 0 {
					ch = st.Channels
					break
				}
			}
			switch {
			case ch == 1: // duplicated at unity
				ref = oracle.Audio(t, in, "0:a:0", "-af", "pan=stereo|c0=c0|c1=c0")
			case ch > 2: // the oracle's stereo downmix; its 5.1-to-mono weights differ
				ref = oracle.Audio(t, in, "0:a:0", "-af", "aformat=sample_fmts=flt:channel_layouts=stereo")
			}
			// PCM must agree with the oracle sample for sample. AAC decoders
			// differ in how much SBR delay they remove (up to about 30 ms),
			// so there the best alignment within that range is taken.
			span := 0
			if !strings.HasPrefix(info.Streams[0].Codec, "pcm") {
				span = 2048
			}
			best, n := correlate(ours, ref, span)
			if n == 0 {
				return
			}
			if best < 0.97 {
				t.Errorf("audio correlates %.3f with the oracle's decode, want at least 0.97 (wrong timing or content)", best)
			}
			// The level must match too: multichannel sources are downmixed
			// without normalisation, as the oracle does (govideo#2).
			var eo, er float64
			for i := span; i < span+n; i++ {
				eo += float64(ours[i]) * float64(ours[i])
				er += float64(ref[i]) * float64(ref[i])
			}
			if er > 1e-6*float64(n) {
				if db := 10 * math.Log10(eo/er); math.Abs(db) > 1 {
					t.Errorf("audio is %+.1f dB from the oracle's level", db)
				}
			}
			// A trim that starts inside the file starts the decoder near the
			// cut (govideo#23). What follows must still be what the oracle
			// has there.
			cut := min(time.Duration(len(ref))*time.Second/48000*2/3, 20*time.Second).Truncate(100 * time.Millisecond)
			if cut < time.Second {
				return
			}
			tl = govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180, FrameRate: govideo.R(30, 1)})
			tl.AddGenerated(time.Second, govideo.GenerateOptions{})
			tl.AddAudio(in, govideo.AudioOptions{Trim: govideo.Range(cut, 0)})
			trimmed := filepath.Join(tmp, name+".trim.mov")
			if err := tl.Export(context.Background(), trimmed, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
				t.Fatalf("export from %v: %v", cut, err)
			}
			defer os.Remove(trimmed)
			after := ref[int(cut*48000/time.Second):]
			if best, n := correlate(oracle.Audio(t, trimmed, "0:a:0"), after, span); n > 0 && best < 0.97 {
				t.Errorf("audio from %v correlates %.3f with the oracle's decode, want at least 0.97 (wrong timing or content)", cut, best)
			}
		})
	}
}

func corpusStills(t *testing.T, dir string) {
	tmp := t.TempDir()
	for _, in := range corpusFiles(t, dir) {
		name := filepath.Base(in)
		t.Run(name, func(t *testing.T) {
			if p := stillPSNR(t, in, filepath.Join(tmp, name+".mov")); p < 32 {
				t.Errorf("picture differs from the oracle's (PSNR %.1f dB): wrong orientation, colour or transparency", p)
			}
		})
	}
}

// stillPSNR exports the still at in for a second, to out (removed again),
// and returns how near its picture is to the oracle's reading of the same
// file, in dB, both reduced to thumbnails.
func stillPSNR(t *testing.T, in, out string) float64 {
	t.Helper()
	// A background that is neither black nor white, so transparent
	// images are told apart from ones composited over either.
	tl := govideo.NewTimeline(govideo.Canvas{Width: 640, Height: 360, FrameRate: govideo.R(30, 1),
		Background: color.NRGBA{32, 64, 192, 255}})
	tl.AddImage(in, time.Second, govideo.ImageOptions{})
	if err := tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate}); err != nil {
		t.Fatalf("export: %v", err)
	}
	defer os.Remove(out)
	// Both pictures go to RGB at full size before they are reduced.
	// Reduced as 4:2:0, a thumbnail has one chroma sample for every
	// 20x20 pixels of the picture, which smears a saturated edge (a
	// cut-out on the coloured background) differently on each side.
	ours := thumbs(t, "format=gbrp,", "-i", out)
	if len(ours) != 30 {
		t.Fatalf("%d frames, want 30", len(ours))
	}
	// The oracle lays the image over the same background, at the even
	// offsets Fit uses; for an opaque image that is just the padding.
	raw := oracle.FFmpegOutput(t, "-f", "lavfi", "-i", "color=c=0x2040C0:s=640x360", "-i", in, "-filter_complex",
		"[1:v]scale=640:360:force_original_aspect_ratio=decrease[fg];[0:v][fg]overlay=trunc((W-w)/4)*2:trunc((H-h)/4)*2:format=auto,format=gbrp,scale=64:36",
		"-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	if len(raw) != len(ours[0]) {
		t.Fatal("the oracle cannot read the image")
	}
	return psnr(ours[0], raw)
}

func corpusUnsupported(t *testing.T, dir string) {
	tmp := t.TempDir()
	for _, in := range corpusFiles(t, dir) {
		name := filepath.Base(in)
		t.Run(name, func(t *testing.T) {
			tl := govideo.NewTimeline(govideo.Canvas{Width: 320, Height: 180, FrameRate: govideo.R(30, 1)})
			if ext := strings.ToLower(filepath.Ext(in)); ext == ".heic" || ext == ".avif" {
				tl.AddImage(in, time.Second, govideo.ImageOptions{})
			} else {
				tl.AddClip(in, govideo.ClipOptions{})
			}
			err := tl.Export(context.Background(), filepath.Join(tmp, "out.mov"), govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			var cu *govideo.ErrCodecUnavailable
			var uf *videoprobe.UnsupportedFormatError
			switch {
			case err == nil:
				t.Error("the file exported; it does not belong in unsupported/")
			case errors.As(err, &cu):
				if cu.Codec == "" || cu.Reason == "" {
					t.Errorf("error names no codec or reason: %v", err)
				}
				t.Logf("refused: codec %s", cu.Codec)
			case errors.As(err, &uf):
				t.Logf("refused: format %s", uf.Format)
			default:
				t.Errorf("refused with a generic error: %v", err)
			}
			if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
				t.Errorf("a failed export left %d files behind", len(entries))
			}
		})
	}
}
