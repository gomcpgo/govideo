package process

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/oracle"
)

// TestToneMapAgainstZscale compares the first half of the tone mapping, from
// the samples to linear light in BT.709 primaries, with the oracle's zscale
// filter (the zimg library): the range and matrix of the Y'CbCr, the
// transfer function and the change of primaries, each of which the oracle
// does in its own way. The oracle has no curve like the one that follows, so
// that is applied to its light here, by the reference of
// TestToneMapAgainstReference.
//
// PQ is compared in colour. HLG is compared in greys only: the oracle
// applies HLG's OOTF to each component, where BT.2100 applies it to the
// luminance, as is done here; measured, a colour differs by a fifth.
func TestToneMapAgainstZscale(t *testing.T) {
	oracle.Require(t)
	if !oracle.HasFilter("zscale") {
		// Not oracle.Missing: the filter is in fuller builds of ffmpeg only
		// (Homebrew's ffmpeg-full; Debian's and Ubuntu's ffmpeg), and a
		// runner without one is not short of anything it can be given.
		t.Skip("the oracle's ffmpeg has no zscale filter")
	}
	const (
		patch = 16
		cols  = 32
		rows  = 8
		w, h  = cols * patch, rows * patch
	)
	for _, c := range []struct {
		name  string
		hdr   HDR
		trc   string
		greys bool
	}{
		{"pq", HDR{TransferPQ, 9}, "smpte2084", false},
		{"hlg", HDR{TransferHLG, 9}, "arib-std-b67", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(11, 12))
			s := refSource{c.hdr, 9, false, 10}
			// Flat patches, so that the chroma of a pixel is not a matter
			// of how each side reads 4:2:0.
			src := (*codec.DeepFrame)(nil).Reuse(w, h)
			for py := range rows {
				for px := range cols {
					// A signal level for each component, most of them near
					// one another as in real pictures; then as Y'CbCr.
					base := rng.Float64()
					var e [3]float64
					for i := range e {
						e[i] = base
						if !c.greys {
							e[i] = math.Min(math.Max(base+(rng.Float64()-0.5)*0.5, 0), 1)
						}
					}
					yy := kr2020*e[0] + (1-kr2020-kb2020)*e[1] + kb2020*e[2]
					q := func(v float64) uint16 { return uint16(math.Round(v)) << 6 }
					y, cb, cr := q(64+876*yy), q(512+896*(e[2]-yy)/(2*(1-kb2020))), q(512+896*(e[0]-yy)/(2*(1-kr2020)))
					for i := range patch * patch {
						src.Y[(py*patch+i/patch)*w+px*patch+i%patch] = y
					}
					for i := range patch / 2 * patch / 2 {
						at := (py*patch/2+i/(patch/2))*src.CStride + px*patch/2 + i%(patch/2)
						src.Cb[at], src.Cr[at] = cb, cr
					}
				}
			}
			raw := make([]byte, 0, 2*(len(src.Y)+len(src.Cb)+len(src.Cr)))
			for _, plane := range [][]uint16{src.Y, src.Cb, src.Cr} {
				for _, v := range plane {
					raw = binary.LittleEndian.AppendUint16(raw, v>>6)
				}
			}
			in := filepath.Join(t.TempDir(), "patches.yuv")
			if err := os.WriteFile(in, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			// Linear light in BT.709 primaries, 1 being 1000 cd/m², as three
			// planes of float32: green, blue, red.
			out := oracle.FFmpegOutput(t, "-f", "rawvideo", "-pix_fmt", "yuv420p10le", "-s", strconv.Itoa(w)+"x"+strconv.Itoa(h), "-i", in,
				"-vf", "setparams=color_primaries=bt2020:color_trc="+c.trc+":colorspace=bt2020nc:range=tv,"+
					"zscale=t=linear:p=bt709:npl=1000,format=gbrpf32le",
				"-f", "rawvideo", "-pix_fmt", "gbrpf32le", "-")
			if len(out) != 3*4*w*h {
				t.Fatalf("the oracle gave %d bytes, want %d", len(out), 3*4*w*h)
			}
			light := func(plane, x, y int) float64 {
				return float64(math.Float32frombits(binary.LittleEndian.Uint32(out[4*(plane*w*h+y*w+x):])))
			}
			m, err := NewToneMapper(c.hdr)
			if err != nil {
				t.Fatal(err)
			}
			dst := m.Map(nil, src)
			var worst, worstLight float64
			for py := range rows {
				for px := range cols {
					x, y := px*patch+patch/2, py*patch+patch/2
					theirs := [3]float64{light(2, x, y), light(0, x, y), light(1, x, y)}
					ours := refLight(s, src.Y[y*w+x], src.Cb[y/2*src.CStride+x/2], src.Cr[y/2*src.CStride+x/2])
					for i := range ours {
						worstLight = math.Max(worstLight, math.Abs(ours[i]-theirs[i])/math.Max(math.Abs(theirs[i]), 0.01))
					}
					wy, wcb, wcr := refYCbCr(refCurve(theirs))
					for _, d := range []float64{
						float64(dst.Y[y*w+x]) - wy, float64(dst.Cb[y/2*dst.CStride+x/2]) - wcb, float64(dst.Cr[y/2*dst.CStride+x/2]) - wcr,
					} {
						worst = math.Max(worst, math.Abs(d))
					}
				}
			}
			t.Logf("light within %.2f%% of the oracle's; the picture within %.2f steps of the one made from the oracle's light", 100*worstLight, worst)
			if worst > 1.5 {
				t.Errorf("the picture is up to %.2f steps from the one made from the oracle's light", worst)
			}
		})
	}
}
