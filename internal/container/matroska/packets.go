package matroska

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"time"
)

const (
	maxBlockSize    = 512 << 20
	maxInflatedSize = 256 << 20
)

// Packet is one frame of one track.
type Packet struct {
	Track     uint64        // track number
	Timestamp time.Duration // presentation time
	Duration  time.Duration // BlockDuration, else the track DefaultDuration, else 0
	Keyframe  bool
	Data      []byte
}

// PacketReader iterates the frames of all tracks in file order.
type PacketReader struct {
	f     *File
	w     walker
	queue []*Packet
	done  bool
	err   error
}

// NewReader returns a reader positioned at the first cluster. Readers are
// independent; each may be used from one goroutine.
func (f *File) NewReader() *PacketReader {
	return f.NewReaderContext(context.Background())
}

// NewReaderContext is NewReader with cancellation.
func (f *File) NewReaderContext(ctx context.Context) *PacketReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &PacketReader{f: f, w: walker{
		rd:  &reader{r: f.r, size: f.size},
		ctx: ctx, pos: f.segStart, segEnd: f.segEnd,
	}}
}

// NewReaderAt returns a reader that starts at the Cluster holding the last
// keyframe of a video track presented at or before t, so that a decoder need
// not start from the beginning of the file. The keyframes come from Open's
// scan of every Cluster rather than from Cues, which files written live lack
// and which may list only some keyframes.
//
// Packets of the track that precede the keyframe in its Cluster belong to the
// previous group of pictures: decoding starts at the first packet of the track
// with Keyframe set. ok is false when the scan found no keyframe at or before
// t; the reader then starts at the first Cluster, as NewReaderContext's does.
func (f *File) NewReaderAt(ctx context.Context, track uint64, t time.Duration) (pr *PacketReader, ok bool) {
	pr = f.NewReaderContext(ctx)
	tr := f.byNum[track]
	if tr == nil {
		return pr, false
	}
	pos := int64(-1)
	for _, sp := range tr.seek {
		// Keyframes come in presentation order in any sane file; stopping
		// at the first one past t keeps an out-of-order index safe, since
		// starting early only costs time.
		if f.ticks(sp.ts) > t {
			break
		}
		pos = sp.pos
	}
	if pos < 0 {
		return pr, false
	}
	pr.w.pos = pos
	return pr, true
}

// Next returns the next packet, or io.EOF at the end of the file (including a
// truncated end). Laced blocks are split into one packet per frame. Packets
// of tracks with a non-nil Track.Err are skipped. A frame that fails to
// decompress yields an error; iteration may continue.
func (pr *PacketReader) Next() (*Packet, error) {
	for {
		if len(pr.queue) > 0 {
			p := pr.queue[0]
			pr.queue = pr.queue[1:]
			return p, nil
		}
		if pr.done {
			return nil, pr.err
		}
		b, ok := pr.w.next()
		if !ok {
			pr.done, pr.err = true, io.EOF
			if pr.w.err != nil {
				pr.err = pr.w.err
			} else if pr.w.rd.err != nil {
				pr.err = pr.w.rd.err
			}
			return nil, pr.err
		}
		t := pr.f.byNum[b.track]
		if t != nil && t.Err != nil {
			continue
		}
		if b.size <= 0 || b.size > maxBlockSize {
			continue
		}
		data := make([]byte, b.size)
		if n, err := pr.f.r.ReadAt(data, b.off); int64(n) < b.size {
			pr.done = true
			if err == nil || err == io.EOF {
				err = io.EOF // file shorter than declared: treat as truncation
			}
			pr.err = err
			return nil, err
		}
		frames := [][]byte{data}
		if b.lacing != laceNone {
			sizes, hdr, ok := laceSizes(data, b.lacing, b.size)
			if !ok {
				continue // corrupt lacing: drop the block
			}
			frames = frames[:0]
			p := int64(hdr)
			for _, s := range sizes {
				frames = append(frames, data[p:p+s:p+s])
				p += s
			}
		}
		var frameDur, step time.Duration
		if t != nil {
			frameDur, step = t.DefaultDuration, t.DefaultDuration
		}
		if b.dur >= 0 {
			frameDur = pr.f.ticks(b.dur) / time.Duration(len(frames))
			if step == 0 {
				step = frameDur
			}
		}
		base := pr.f.ticks(b.ts)
		var derr error
		for i, fr := range frames {
			if t != nil {
				if fr, derr = t.decode(fr); derr != nil {
					break
				}
			}
			pr.queue = append(pr.queue, &Packet{
				Track:     b.track,
				Timestamp: base + time.Duration(i)*step,
				Duration:  frameDur,
				Keyframe:  b.key,
				Data:      fr,
			})
		}
		if derr != nil {
			pr.queue = nil
			return nil, fmt.Errorf("matroska: track %d: %w", b.track, derr)
		}
	}
}

// decode undoes the track's frame ContentEncoding.
func (t *Track) decode(fr []byte) ([]byte, error) {
	switch {
	case t.zlib:
		zr, err := zlib.NewReader(bytes.NewReader(fr))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, maxInflatedSize+1))
		if err != nil {
			return nil, err
		}
		if len(out) > maxInflatedSize {
			return nil, fmt.Errorf("compressed frame too large")
		}
		return out, nil
	case len(t.strip) > 0:
		out := make([]byte, 0, len(t.strip)+len(fr))
		return append(append(out, t.strip...), fr...), nil
	}
	return fr, nil
}
