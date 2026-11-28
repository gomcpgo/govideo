package govideo

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"time"

	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

// FrameOptions selects and sizes the picture that Frame returns.
type FrameOptions struct {
	// At is the time of the frame, counted from the first frame of the
	// video as ClipOptions.Trim counts it. Frame returns the picture that
	// is on screen at that time; a time past the end gives the last frame.
	At time.Duration
	// Width and Height size the picture, in pixels. With both zero it has
	// the size the video is displayed at. With one of them zero, that one
	// follows from the other and the shape of the video. With both set, the
	// picture is the largest of the video's shape that fits inside them:
	// nothing is cropped and no bars are added.
	Width, Height int
}

// maxFrameSide bounds the size that can be asked of Frame.
const maxFrameSide = 16384

// Frame decodes one picture of a video file: a poster frame, a thumbnail,
// or a look at what is at a given time. The picture is upright (the file's
// rotation is applied), has square pixels, and is an *image.RGBA.
//
// The video is decoded from the last sync sample before the time asked for,
// so the cost does not grow with the position in the file. Interlaced
// H.264 gives one field of the frame, deinterlaced, and a high dynamic
// range video (HLG, PQ) is tone-mapped to the standard range. A file whose
// codec has no decoder on this host is refused with an
// *ErrCodecUnavailable.
func Frame(ctx context.Context, path string, o FrameOptions) (image.Image, error) {
	if o.At < 0 {
		return nil, errors.New("govideo: FrameOptions.At must not be negative")
	}
	if o.Width < 0 || o.Height < 0 || o.Width > maxFrameSide || o.Height > maxFrameSide {
		return nil, fmt.Errorf("govideo: FrameOptions size %dx%d is out of range (0 to %d)", o.Width, o.Height, maxFrameSide)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := openMedia(ctx, path)
	if err != nil {
		return nil, err
	}
	defer m.close()
	vs := m.stream(videoprobe.Video)
	if vs == nil {
		return nil, fmt.Errorf("govideo: %s: no video stream", path)
	}
	// As for a clip, time starts at the first frame, which a file may
	// present a little after zero.
	at := max(vs.StartTime, 0) + o.At
	decs := &videoDecoders{}
	defer decs.close()
	v, err := openVideo(ctx, m, vs, at, 0, DeinterlaceAuto, decs)
	if err != nil {
		return nil, err
	}
	defer v.close()
	f, _, err := v.frameAt(at)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w, h := frameSize(f.Width, f.Height, vs.Video, o)
	if w != f.Width || h != f.Height {
		f = process.Scale(f, w, h)
	}
	return process.ToRGBA(f), nil
}

// frameSize returns the size of the picture Frame makes of a decoded frame
// of fw by fh (already upright).
func frameSize(fw, fh int, vi *videoprobe.VideoInfo, o FrameOptions) (w, h int) {
	// Pixels that are not square are made so by widening or narrowing the
	// picture; the pixel shape is that of the stored picture, before it is
	// turned.
	dw, dh := float64(fw), float64(fh)
	if sar := vi.SAR.Float(); sar > 0 && sar != 1 && sar < 16 && sar > 1.0/16 {
		if vi.Rotation%180 != 0 {
			dh *= sar
		} else {
			dw *= sar
		}
	}
	round := func(d float64) int { return max(1, min(int(math.Round(d)), maxFrameSide)) }
	switch {
	case o.Width > 0 && o.Height > 0:
		s := math.Min(float64(o.Width)/dw, float64(o.Height)/dh)
		return min(round(dw*s), o.Width), min(round(dh*s), o.Height)
	case o.Width > 0:
		return o.Width, round(dh * float64(o.Width) / dw)
	case o.Height > 0:
		return round(dw * float64(o.Height) / dh), o.Height
	}
	return round(dw), round(dh)
}
