package process

import (
	"image/color"
	"math"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

// FitSize returns the size, in canvas pixels, at which Fit shows a srcW×srcH
// picture on a canvasW×canvasH canvas in the given mode, before any
// rounding to whole pixels: the whole picture inside the canvas for Contain,
// the canvas inside the picture for Cover, the canvas itself for Stretch.
func FitSize(srcW, srcH, canvasW, canvasH int, mode FitMode) (w, h float64) {
	if srcW <= 0 || srcH <= 0 {
		return 0, 0
	}
	rx := float64(canvasW) / float64(srcW)
	ry := float64(canvasH) / float64(srcH)
	switch mode {
	case Stretch:
		return float64(canvasW), float64(canvasH)
	case Cover:
		rx = math.Max(rx, ry)
	default:
		rx = math.Min(rx, ry)
	}
	return float64(srcW) * rx, float64(srcH) * rx
}

// Placer draws pictures on a canvas at positions and sizes that need not be
// whole pixels. That is what a picture in slow motion across the canvas
// needs (pan and zoom over a still): placed on whole pixels it would move
// in visible steps. A Placer keeps its working memory between calls; the
// zero value is ready to use, and it must not be used by two goroutines at
// once.
type Placer struct {
	mid []byte
	acc []int32
}

// Place draws f on a canvasW×canvasH canvas of colour bg (nil is black),
// scaled to w×h canvas pixels with its top left corner at (x, y). The
// picture may lie partly or wholly outside the canvas; what the canvas does
// not show of it is not computed.
//
// The picture is resampled with the bicubic filter Scale uses, positioned
// to a fraction of a pixel. Where an edge of the picture falls inside a
// pixel, that pixel is mixed with the background in proportion, so a moving
// edge moves smoothly too. f is converted to BT.709 limited range first;
// the result is written to into, which is reused when it is large enough
// (it may be nil, but must not be f), and returned.
func (p *Placer) Place(into, f *codec.VideoFrame, canvasW, canvasH int, x, y, w, h float64, bg color.Color) *codec.VideoFrame {
	canvasW, canvasH = max(canvasW, 0), max(canvasH, 0)
	into = into.Reuse(canvasW, canvasH)
	by, bcb, bcr := colorTo709(bg)
	fillFrame(into, by, bcb, bcr)
	if f == nil || f.Width <= 0 || f.Height <= 0 || !(w > 0) || !(h > 0) ||
		math.IsInf(w, 0) || math.IsInf(h, 0) || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return into
	}
	f = ConvertTo709Limited(f)
	p.plane(into.Y, into.YStride, canvasW, canvasH, f.Y, f.YStride, f.Width, f.Height, x, y, w, h, by)
	// The chroma planes hold one sample for two luma samples each way, and
	// one more than half when the size is odd; they cover that much more of
	// the canvas.
	cw, ch := into.ChromaSize()
	scw, sch := f.ChromaSize()
	cwf := w * float64(2*scw) / float64(f.Width) / 2
	chf := h * float64(2*sch) / float64(f.Height) / 2
	p.plane(into.Cb, into.CStride, cw, ch, f.Cb, f.CStride, scw, sch, x/2, y/2, cwf, chf, bcb)
	p.plane(into.Cr, into.CStride, cw, ch, f.Cr, f.CStride, scw, sch, x/2, y/2, cwf, chf, bcr)
	return into
}

// span returns the output samples d0 to d1-1 that a picture from off to
// off+size touches on an axis of n samples.
func span(off, size float64, n int) (d0, d1 int) {
	lo, hi := math.Floor(off), math.Ceil(off+size)
	if lo >= float64(n) || hi <= 0 {
		return 0, 0
	}
	return int(math.Max(lo, 0)), int(math.Min(hi, float64(n)))
}

// cover returns how much of output sample d a picture from off to off+size
// covers, from 0 to 1.
func cover(d int, off, size float64) float64 {
	return math.Max(0, math.Min(float64(d+1), off+size)-math.Max(float64(d), off))
}

// plane draws one plane: the sw×sh source over the w×h rectangle at (x, y)
// of a dw×dh plane already filled with bg.
func (p *Placer) plane(dst []byte, dstride, dw, dh int, src []byte, sstride, sw, sh int, x, y, w, h float64, bg byte) {
	x0, x1 := span(x, w, dw)
	y0, y1 := span(y, h, dh)
	if x1 <= x0 || y1 <= y0 {
		return
	}
	fh := makeFilterAt(sw, x0, x1, x, w)
	fv := makeFilterAt(sh, y0, y1, y, h)
	// Horizontal pass, over the source rows the vertical pass will read.
	mw := x1 - x0
	r0 := int(fv.start[0])
	r1 := int(fv.start[len(fv.start)-1]) + fv.taps
	if n := mw * (r1 - r0); cap(p.mid) < n {
		p.mid = make([]byte, n)
	}
	mid := p.mid[:mw*(r1-r0)]
	for r := r0; r < r1; r++ {
		kernels.ResampleH(mid[(r-r0)*mw:(r-r0+1)*mw], src[r*sstride:r*sstride+sw], fh.start, fh.w, fh.taps)
	}
	if cap(p.acc) < mw {
		p.acc = make([]int32, mw)
	}
	for j := y0; j < y1; j++ {
		i := j - y0
		kernels.ResampleV(dst[j*dstride+x0:j*dstride+x1], mid, mw, mw, int(fv.start[i])-r0,
			fv.w[i*fv.taps:(i+1)*fv.taps], p.acc)
	}
	// Edges that fall inside a pixel: mix that row or column with the
	// background by the part of it the picture covers.
	for _, j := range [2]int{y0, y1 - 1} {
		if c := cover(j, y, h); c < 1 && (j == y0 || y1-1 != y0) {
			row := dst[j*dstride+x0 : j*dstride+x1]
			kernels.BlendTo(row, row, bg, blendWeight(1-c))
		}
	}
	for _, i := range [2]int{x0, x1 - 1} {
		c := cover(i, x, w)
		if c >= 1 || (i != x0 && x1-1 == x0) {
			continue
		}
		wgt := blendWeight(1 - c)
		for j := y0; j < y1; j++ {
			px := dst[j*dstride+i : j*dstride+i+1]
			kernels.BlendTo(px, px, bg, wgt)
		}
	}
}
