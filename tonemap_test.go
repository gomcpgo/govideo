package govideo_test

import (
	"context"
	"encoding/binary"
	"image"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gomcpgo/govideo"
	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/oracle"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// TestToneMappedClips takes HLG and PQ clips through the pipeline: 10-bit
// HEVC made by the oracle's encoder from patches of known signal levels,
// read back as a single frame and as an export. Each patch must come out as
// the tone mapping makes it from the samples that went in, which holds the
// whole path to it: the file's colour signalling, the decoder's 10-bit
// pictures and their layout, and the mapping being applied once.
//
// What the mapping itself should give is the business of the process
// package's tests. Two things are checked here without it all the same,
// since they are what a viewer would see go wrong: a grey of an HLG clip
// keeps its level, and a colour comes out more saturated than it went in
// (shown as it is, a BT.2020 picture looks washed out).
func TestToneMappedClips(t *testing.T) {
	oracle.Require(t)
	requireOS(t)
	const (
		patch      = 40
		cols, rows = 8, 4
		w, h       = cols * patch, rows * patch
		count      = 12
	)
	for _, c := range []struct {
		name, trc, kind string
		hdr             process.HDR
	}{
		{"hlg", "arib-std-b67", "hlg", process.HDR{Transfer: process.TransferHLG, Primaries: 9}},
		{"pq", "smpte2084", "pq", process.HDR{Transfer: process.TransferPQ, Primaries: 9}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The signal levels of each patch's R', G' and B': a row of
			// greys, then colours, stronger row by row.
			var levels [rows][cols][3]float64
			for x := range cols {
				g := 0.08 + 0.11*float64(x)
				levels[0][x] = [3]float64{g, g, g}
				for y := 1; y < rows; y++ {
					base, spread := 0.25+0.07*float64(x), 0.08*float64(y)
					e := [3]float64{base, base, base}
					e[x%3] += spread
					e[(x+1)%3] -= spread / 2
					levels[y][x] = e
				}
			}
			src := (*codec.DeepFrame)(nil).Reuse(w, h)
			const kr, kb = 0.2627, 0.0593
			for py := range rows {
				for px := range cols {
					e := levels[py][px]
					yy := kr*e[0] + (1-kr-kb)*e[1] + kb*e[2]
					q := func(v float64) uint16 { return uint16(math.Round(v)) << 6 }
					y, cb, cr := q(64+876*yy), q(512+896*(e[2]-yy)/(2*(1-kb))), q(512+896*(e[0]-yy)/(2*(1-kr)))
					for i := range patch * patch {
						src.Y[(py*patch+i/patch)*w+px*patch+i%patch] = y
					}
					for i := range patch / 2 * patch / 2 {
						at := (py*patch/2+i/(patch/2))*src.CStride + px*patch/2 + i%(patch/2)
						src.Cb[at], src.Cr[at] = cb, cr
					}
				}
			}
			var one []byte
			for _, plane := range [][]uint16{src.Y, src.Cb, src.Cr} {
				for _, v := range plane {
					one = binary.LittleEndian.AppendUint16(one, v>>6)
				}
			}
			dir := t.TempDir()
			raw, clip := filepath.Join(dir, "patches.yuv"), filepath.Join(dir, c.name+".mp4")
			var all []byte
			for range count {
				all = append(all, one...)
			}
			if err := os.WriteFile(raw, all, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := oracle.TryFFmpeg("-f", "rawvideo", "-pix_fmt", "yuv420p10le", "-s", strconv.Itoa(w)+"x"+strconv.Itoa(h), "-r", "24", "-i", raw,
				"-vf", "setparams=color_primaries=bt2020:color_trc="+c.trc+":colorspace=bt2020nc:range=tv",
				"-c:v", "libx265", "-preset", "veryfast", "-crf", "8", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
				"-x265-params", "log-level=error", clip); err != nil {
				oracle.Missing(t, "the oracle cannot make a 10-bit HEVC clip: %v", err)
			}
			info, err := videoprobe.ProbeFile(context.Background(), clip)
			if err != nil || len(info.Streams) == 0 || info.Streams[0].Video == nil {
				t.Fatalf("probe: %v", err)
			}
			if v := info.Streams[0].Video; v.HDR != c.kind || v.BitDepth != 10 {
				t.Fatalf("the clip is probed as HDR %q of %d bits, want %q of 10", v.HDR, v.BitDepth, c.kind)
			}

			mapper, err := process.NewToneMapper(c.hdr)
			if err != nil {
				t.Fatal(err)
			}
			want := process.ToRGBA(mapper.Map(nil, src))
			// check compares the middle of every patch of a picture, given
			// as a function from a pixel to its R, G and B, with want.
			check := func(what string, at func(x, y int) [3]int, tol int) {
				t.Helper()
				for py := range rows {
					for px := range cols {
						x, y := px*patch+patch/2, py*patch+patch/2
						got, o := at(x, y), want.PixOffset(x, y)
						for i := range got {
							if d := got[i] - int(want.Pix[o+i]); d < -tol || d > tol {
								t.Errorf("%s: patch %d,%d is %v, want %v", what, px, py, got, want.Pix[o:o+3])
								break
							}
						}
					}
				}
			}
			// The mapping's own promises, on the picture it gave.
			for px := range cols {
				o := want.PixOffset(px*patch+patch/2, patch/2)
				if g := levels[0][px][0]; c.kind == "hlg" && math.Abs(float64(want.Pix[o+1])/255-g) > 0.02 {
					t.Errorf("an HLG grey at %.2f comes out at %.3f", g, float64(want.Pix[o+1])/255)
				}
				o = want.PixOffset(px*patch+patch/2, 3*patch+patch/2)
				e, p := levels[3][px], want.Pix[o:o+3]
				in := max(e[0], e[1], e[2]) - min(e[0], e[1], e[2])
				out := float64(max(p[0], p[1], p[2])-min(p[0], p[1], p[2])) / 255
				if out < 1.15*in {
					t.Errorf("patch %d,3: the spread of R'G'B' is %.3f, and was %.3f in the signal: the colour is no stronger", px, out, in)
				}
			}

			img, err := govideo.Frame(context.Background(), clip, govideo.FrameOptions{})
			if err != nil {
				t.Fatalf("Frame: %v", err)
			}
			rgba, ok := img.(*image.RGBA)
			if !ok || rgba.Rect.Dx() != w || rgba.Rect.Dy() != h {
				t.Fatalf("Frame gave a %T of %v", img, img.Bounds())
			}
			check("Frame", func(x, y int) [3]int {
				o := rgba.PixOffset(x, y)
				return [3]int{int(rgba.Pix[o]), int(rgba.Pix[o+1]), int(rgba.Pix[o+2])}
			}, 4)

			out := filepath.Join(dir, c.name+".mov")
			tl := govideo.NewTimeline(govideo.Canvas{Width: w, Height: h, FrameRate: govideo.R(24, 1)})
			tl.AddClip(clip, govideo.ClipOptions{})
			if err := retryOnVM(t, func() error {
				return tl.Export(context.Background(), out, govideo.ExportOptions{Profile: govideo.ProfileIntermediate})
			}); err != nil {
				t.Fatalf("Export: %v", err)
			}
			frames := oracle.Frames(t, out, w, h)
			if len(frames) != count {
				t.Fatalf("%d frames exported, want %d", len(frames), count)
			}
			for _, n := range []int{0, count - 1} {
				f := frames[n]
				check("export, frame "+strconv.Itoa(n), func(x, y int) [3]int {
					p := f[(y*w+x)*3:]
					return [3]int{int(p[0]), int(p[1]), int(p[2])}
				}, 10)
			}
		})
	}
}
