package process

import (
	"image/color"
	"math"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

// FitMode selects how a frame is fitted to the output canvas.
type FitMode int

const (
	// Contain scales the frame to fit inside the canvas and pads the rest
	// with the background colour (letterbox/pillarbox).
	Contain FitMode = iota
	// Cover scales the frame to fill the canvas and crops the overflow.
	Cover
	// Stretch scales the frame to the canvas, ignoring aspect ratio.
	Stretch
)

// resampleFilter is a per-output-sample FIR filter bank.
type resampleFilter struct {
	start []int32
	w     []int16
	taps  int
}

// catmullRom is the bicubic kernel with a = -0.5.
func catmullRom(t float64) float64 {
	t = math.Abs(t)
	switch {
	case t < 1:
		return (1.5*t-2.5)*t*t + 1
	case t < 2:
		return ((-0.5*t+2.5)*t-4)*t + 2
	}
	return 0
}

// makeFilter builds a bicubic filter from srcN to dstN samples. When
// downscaling the kernel is widened by the scale factor so it also
// low-passes (no aliasing). Weights for each output sum to exactly
// 1<<kernels.FilterShift, so flat areas are preserved bit-exactly.
func makeFilter(srcN, dstN int) resampleFilter {
	ratio := float64(srcN) / float64(dstN)
	scale := math.Max(1, ratio)
	support := 2 * scale
	taps := int(math.Ceil(2*support)) + 1
	eff := taps
	if eff > srcN {
		eff = srcN
	}
	f := resampleFilter{start: make([]int32, dstN), w: make([]int16, dstN*eff), taps: eff}
	tmp := make([]float64, eff)
	const one = 1 << kernels.FilterShift
	for i := 0; i < dstN; i++ {
		center := (float64(i)+0.5)*ratio - 0.5
		left := int(math.Ceil(center - support))
		start := left
		if start > srcN-eff {
			start = srcN - eff
		}
		if start < 0 {
			start = 0
		}
		for k := range tmp {
			tmp[k] = 0
		}
		var sum float64
		for j := left; j < left+taps; j++ {
			wgt := catmullRom((float64(j) - center) / scale)
			if wgt == 0 {
				continue
			}
			idx := j
			if idx < 0 {
				idx = 0
			} else if idx > srcN-1 {
				idx = srcN - 1
			}
			tmp[idx-start] += wgt
			sum += wgt
		}
		ws := f.w[i*eff : (i+1)*eff]
		total, best := 0, 0
		for k, v := range tmp {
			q := int(math.Round(v / sum * one))
			ws[k] = int16(q)
			total += q
			if q > int(ws[best]) {
				best = k
			}
		}
		ws[best] += int16(one - total)
		f.start[i] = int32(start)
	}
	return f
}

// scalePlane resamples a sw×sh plane to dw×dh.
func scalePlane(src []byte, sstride, sw, sh, dw, dh int) []byte {
	dst := make([]byte, dw*dh)
	if sw == 0 || sh == 0 || dw == 0 || dh == 0 {
		return dst
	}
	// Horizontal pass into a dw×sh intermediate.
	mid, mstride := src, sstride
	if sw != dw {
		fh := makeFilter(sw, dw)
		mid, mstride = make([]byte, dw*sh), dw
		for y := 0; y < sh; y++ {
			kernels.ResampleH(mid[y*dw:(y+1)*dw], src[y*sstride:y*sstride+sw], fh.start, fh.w, fh.taps)
		}
	}
	if sh == dh {
		for y := 0; y < dh; y++ {
			copy(dst[y*dw:(y+1)*dw], mid[y*mstride:y*mstride+dw])
		}
		return dst
	}
	fv := makeFilter(sh, dh)
	acc := make([]int32, dw)
	for y := 0; y < dh; y++ {
		kernels.ResampleV(dst[y*dw:(y+1)*dw], mid, mstride, dw, int(fv.start[y]),
			fv.w[y*fv.taps:(y+1)*fv.taps], acc)
	}
	return dst
}

// Scale resamples a frame to w×h with a separable bicubic filter (widened
// when downscaling). The result is always a new frame.
func Scale(f *codec.VideoFrame, w, h int) *codec.VideoFrame {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	cw, ch := (w+1)/2, (h+1)/2
	scw, sch := f.ChromaSize()
	out := &codec.VideoFrame{
		Width: w, Height: h, YStride: w, CStride: cw,
		Matrix: f.Matrix, FullRange: f.FullRange,
	}
	out.Y = scalePlane(f.Y, f.YStride, f.Width, f.Height, w, h)
	out.Cb = scalePlane(f.Cb, f.CStride, scw, sch, cw, ch)
	out.Cr = scalePlane(f.Cr, f.CStride, scw, sch, cw, ch)
	return out
}

func fillFrame(f *codec.VideoFrame, y, cb, cr byte) {
	for i := range f.Y {
		f.Y[i] = y
	}
	for i := range f.Cb {
		f.Cb[i] = cb
		f.Cr[i] = cr
	}
}

// blit copies a w×h region from src at (sx,sy) to dst at (dx,dy). All
// offsets must be even so the 4:2:0 chroma planes line up.
func blit(dst *codec.VideoFrame, dx, dy int, src *codec.VideoFrame, sx, sy, w, h int) {
	for y := 0; y < h; y++ {
		copy(dst.Y[(dy+y)*dst.YStride+dx:(dy+y)*dst.YStride+dx+w],
			src.Y[(sy+y)*src.YStride+sx:(sy+y)*src.YStride+sx+w])
	}
	cw, ch := (w+1)/2, (h+1)/2
	dcw, dch := dst.ChromaSize()
	scw, sch := src.ChromaSize()
	cdx, cdy, csx, csy := dx/2, dy/2, sx/2, sy/2
	cw = min(cw, dcw-cdx, scw-csx)
	ch = min(ch, dch-cdy, sch-csy)
	for y := 0; y < ch; y++ {
		d := (cdy+y)*dst.CStride + cdx
		s := (csy+y)*src.CStride + csx
		copy(dst.Cb[d:d+cw], src.Cb[s:s+cw])
		copy(dst.Cr[d:d+cw], src.Cr[s:s+cw])
	}
}

// Fit places a frame on a canvasW×canvasH canvas (square pixels assumed).
// The input is converted to BT.709 limited range first, and the result is
// always exactly the canvas size in that format. A nil bg means black.
func Fit(f *codec.VideoFrame, canvasW, canvasH int, mode FitMode, bg color.Color) *codec.VideoFrame {
	if canvasW < 0 {
		canvasW = 0
	}
	if canvasH < 0 {
		canvasH = 0
	}
	out := codec.NewVideoFrame(canvasW, canvasH)
	by, bcb, bcr := colorTo709(bg)
	fillFrame(out, by, bcb, bcr)
	if f == nil || f.Width <= 0 || f.Height <= 0 || canvasW == 0 || canvasH == 0 {
		return out
	}
	f = ConvertTo709Limited(f)
	if mode == Stretch {
		return Scale(f, canvasW, canvasH)
	}
	rx := float64(canvasW) / float64(f.Width)
	ry := float64(canvasH) / float64(f.Height)
	if mode == Cover {
		s := math.Max(rx, ry)
		sw := max(canvasW, int(math.Round(float64(f.Width)*s)))
		sh := max(canvasH, int(math.Round(float64(f.Height)*s)))
		scaled := f
		if sw != f.Width || sh != f.Height {
			scaled = Scale(f, sw, sh)
		}
		blit(out, 0, 0, scaled, ((sw-canvasW)/2)&^1, ((sh-canvasH)/2)&^1, canvasW, canvasH)
		return out
	}
	s := math.Min(rx, ry)
	fitDim := func(src, canvas int) int {
		d := int(math.Round(float64(src) * s))
		d = max(1, min(d, canvas))
		// An odd picture size inside a larger canvas would share its last
		// chroma sample with the padding; round up to even to avoid a fringe.
		if d < canvas && d&1 == 1 {
			d++
		}
		return d
	}
	sw, sh := fitDim(f.Width, canvasW), fitDim(f.Height, canvasH)
	scaled := f
	if sw != f.Width || sh != f.Height {
		scaled = Scale(f, sw, sh)
	}
	blit(out, ((canvasW-sw)/2)&^1, ((canvasH-sh)/2)&^1, scaled, 0, 0, sw, sh)
	return out
}
