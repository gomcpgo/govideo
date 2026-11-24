package process

import (
	"image/color"
	"math"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

// blendWeight turns a fraction into a kernel weight, clamped to 0..1.
func blendWeight(t float64) int32 {
	if !(t > 0) { // also NaN
		return 0
	}
	if t >= 1 {
		return 1 << kernels.BlendShift
	}
	return int32(math.Round(t * (1 << kernels.BlendShift)))
}

// blendPlanes runs one blend over the three planes of a frame, row by row,
// so that strides wider than the picture are left alone. row is given the
// plane (0, 1, 2 for Y', Cb, Cr) and the row's number in it.
func blendPlanes(into, a *codec.VideoFrame, row func(dst, a []byte, plane, y int)) {
	w, h := a.Width, a.Height
	cw, ch := a.ChromaSize()
	for y := 0; y < h; y++ {
		row(into.Y[y*into.YStride:y*into.YStride+w], a.Y[y*a.YStride:], 0, y)
	}
	for y := 0; y < ch; y++ {
		row(into.Cb[y*into.CStride:y*into.CStride+cw], a.Cb[y*a.CStride:], 1, y)
		row(into.Cr[y*into.CStride:y*into.CStride+cw], a.Cr[y*a.CStride:], 2, y)
	}
}

// BlendInto mixes two pictures of one size: a where t is 0, b where t is 1
// and the weighted mean of the two between, which is a dissolve from a to b
// as t rises. The mix is made on the gamma-encoded Y'CbCr samples, as video
// mixers and editors do it. Both pictures are converted to BT.709 limited
// range first. The result is written to into, which is reused when it is
// large enough (it may be nil, and may be a or b themselves), and returned.
// Pictures of different sizes cannot be mixed: a is then returned as it is.
func BlendInto(into, a, b *codec.VideoFrame, t float64) *codec.VideoFrame {
	a, b = ConvertTo709Limited(a), ConvertTo709Limited(b)
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return a
	}
	w := blendWeight(t)
	into = reuseFor(into, a, b)
	blendPlanes(into, a, func(dst, arow []byte, plane, y int) {
		var brow []byte
		switch plane {
		case 0:
			brow = b.Y[y*b.YStride:]
		case 1:
			brow = b.Cb[y*b.CStride:]
		default:
			brow = b.Cr[y*b.CStride:]
		}
		kernels.Blend(dst, arow, brow, w)
	})
	return into
}

// FadeInto mixes a picture with a plain colour: the colour where t is 0,
// the picture where t is 1. It is BlendInto with a frame of that colour for
// a, without the frame; c is taken as Fit takes its background (nil is
// black). into is reused when it is large enough and may be f itself.
func FadeInto(into, f *codec.VideoFrame, c color.Color, t float64) *codec.VideoFrame {
	f = ConvertTo709Limited(f)
	if f == nil {
		return nil
	}
	w := blendWeight(1 - t)
	cy, ccb, ccr := colorTo709(c)
	into = reuseFor(into, f, nil)
	blendPlanes(into, f, func(dst, row []byte, plane, _ int) {
		kernels.BlendTo(dst, row, [...]byte{cy, ccb, ccr}[plane], w)
	})
	return into
}

// reuseFor returns into sized for a picture like a. When into holds the
// planes of one of the inputs it is used as it stands, since Reuse would
// repack them under the reader.
func reuseFor(into, a, b *codec.VideoFrame) *codec.VideoFrame {
	shares := func(f *codec.VideoFrame) bool {
		return into != nil && f != nil && len(into.Y) > 0 && len(f.Y) > 0 && &into.Y[0] == &f.Y[0]
	}
	if shares(a) || shares(b) {
		into.Matrix, into.FullRange = 1, false
		return into
	}
	return into.Reuse(a.Width, a.Height)
}
