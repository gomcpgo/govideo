package matroska

import (
	"context"
	"math"
)

// Lacing modes (block flag bits 0x06).
const (
	laceNone  = 0
	laceXiph  = 1
	laceFixed = 2
	laceEBML  = 3
)

// block describes one SimpleBlock or Block without its payload.
type block struct {
	track  uint64
	ts     int64 // absolute, in segment ticks
	dur    int64 // BlockDuration in ticks, -1 if absent
	key    bool
	lacing int
	laced  int   // frames-1 from the lacing header (0 when not laced)
	off    int64 // payload offset (the lacing header when laced)
	size   int64 // payload size
}

// walker iterates the blocks of a Segment in file order, reading element and
// block headers only.
type walker struct {
	rd     *reader
	ctx    context.Context
	pos    int64
	segEnd int64
	// onTop, if set, is called for each non-Cluster Segment-level element.
	onTop func(id uint32, off, size int64)

	inCluster  bool
	clusterPos int64 // offset of the current Cluster's element header
	clusterEnd int64 // -1 = unknown size
	clusterTS  int64
	n          int
	err        error // context error
}

func (w *walker) next() (block, bool) {
	rd := w.rd
	for {
		w.n++
		if w.n&1023 == 0 {
			if err := w.ctx.Err(); err != nil {
				w.err = err
				return block{}, false
			}
		}
		if rd.err != nil {
			return block{}, false
		}
		if w.inCluster && w.clusterEnd >= 0 && w.pos >= w.clusterEnd {
			w.inCluster = false
			w.pos = w.clusterEnd
		}
		if w.pos >= w.segEnd {
			return block{}, false
		}
		id, size, h, ok := rd.header(w.pos)
		if !ok {
			return block{}, false // truncated or corrupt: keep what we have
		}
		data := w.pos + int64(h)

		if !w.inCluster {
			if id == idCluster {
				w.inCluster = true
				w.clusterPos = w.pos
				w.clusterTS = 0
				w.clusterEnd = -1
				if size >= 0 {
					w.clusterEnd = w.segEnd
					if size < w.segEnd-data {
						w.clusterEnd = data + size
					}
				}
				w.pos = data
				continue
			}
			if size < 0 {
				return block{}, false
			}
			avail := size
			if avail > w.segEnd-data {
				avail = w.segEnd - data
			}
			if w.onTop != nil {
				w.onTop(id, data, avail)
			}
			if avail < size {
				return block{}, false
			}
			w.pos = data + size
			continue
		}

		if w.clusterEnd < 0 && isTopLevel(id) {
			w.inCluster = false // unknown-size cluster ends here
			continue
		}
		if size < 0 {
			return block{}, false
		}
		limit := w.segEnd
		if w.clusterEnd >= 0 {
			limit = w.clusterEnd
		}
		if size > limit-data {
			return block{}, false // truncated final element
		}
		w.pos = data + size
		switch id {
		case idTimestamp:
			v := rd.uint(data, size)
			if v > math.MaxInt64/2 {
				v = math.MaxInt64 / 2
			}
			w.clusterTS = int64(v)
		case idSimpleBlock:
			if b, flags, ok := w.blockHeader(data, size); ok {
				b.key = flags&0x80 != 0
				b.dur = -1
				return b, true
			}
		case idBlockGroup:
			var b block
			found, ref := false, false
			dur := int64(-1)
			end := data + size
			for p := data; p < end; {
				cid, csize, ch, ok := rd.header(p)
				if !ok || csize < 0 {
					break
				}
				cdata := p + int64(ch)
				if csize > end-cdata {
					break
				}
				switch cid {
				case idBlock:
					if !found {
						b, _, found = w.blockHeader(cdata, csize)
					}
				case idBlockDuration:
					if v := rd.uint(cdata, csize); v <= math.MaxInt64/2 {
						dur = int64(v)
					}
				case idReferenceBlock:
					ref = true
				}
				p = cdata + csize
			}
			if found {
				b.key = !ref
				b.dur = dur
				return b, true
			}
		}
	}
}

// blockHeader parses the (Simple)Block header: track number vint, signed
// 16-bit relative timestamp and flags.
func (w *walker) blockHeader(data, size int64) (b block, flags byte, ok bool) {
	n := 16
	if size < int64(n) {
		n = int(size)
	}
	h := w.rd.at(data, n)
	track, l, _, vok := vint(h)
	if !vok || len(h) < l+3 {
		return b, 0, false
	}
	rel := int64(int16(uint16(h[l])<<8 | uint16(h[l+1])))
	flags = h[l+2]
	b.track = track
	b.ts = w.clusterTS + rel
	if b.ts < 0 {
		b.ts = 0
	}
	b.lacing = int(flags>>1) & 3
	b.off = data + int64(l) + 3
	b.size = size - int64(l) - 3
	if b.lacing != laceNone {
		if len(h) < l+4 {
			return b, 0, false
		}
		b.laced = int(h[l+3])
	}
	return b, flags, true
}

// laceSizes parses a lacing header. data is a prefix of the block payload and
// total the full payload length. It returns the frame sizes and the header
// length; ok is false if the header is invalid or does not fit in data.
func laceSizes(data []byte, lacing int, total int64) (sizes []int64, hdr int, ok bool) {
	if len(data) < 1 || total < 1 {
		return nil, 0, false
	}
	n := int(data[0]) + 1
	pos := 1
	sizes = make([]int64, n)
	var sum int64
	switch lacing {
	case laceXiph:
		for i := 0; i < n-1; i++ {
			for {
				if pos >= len(data) {
					return nil, 0, false
				}
				c := data[pos]
				pos++
				sizes[i] += int64(c)
				if c != 255 {
					break
				}
			}
			sum += sizes[i]
		}
	case laceEBML:
		var prev int64
		for i := 0; i < n-1; i++ {
			if pos >= len(data) {
				return nil, 0, false
			}
			v, l, _, vok := vint(data[pos:])
			if !vok {
				return nil, 0, false
			}
			pos += l
			if i == 0 {
				prev = int64(v)
			} else {
				// signed vint: subtract half the range
				prev += int64(v) - (int64(1)<<(7*uint(l)-1) - 1)
			}
			if prev < 0 || prev > total {
				return nil, 0, false
			}
			sizes[i] = prev
			sum += prev
		}
	case laceFixed:
		rest := total - 1
		if rest%int64(n) != 0 {
			return nil, 0, false
		}
		for i := range sizes {
			sizes[i] = rest / int64(n)
		}
		return sizes, 1, true
	default:
		return nil, 0, false
	}
	last := total - int64(pos) - sum
	if last < 0 {
		return nil, 0, false
	}
	sizes[n-1] = last
	return sizes, pos, true
}
