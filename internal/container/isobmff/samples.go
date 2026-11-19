package isobmff

import (
	"errors"
	"io"
	"math"
)

func float64frombits(u uint64) float64 { return math.Float64frombits(u) }

// Sample locates one media sample.
type Sample struct {
	Offset    int64
	Size      uint32
	Dur       uint32 // media timescale
	DTS       int64  // media timescale
	CTSOffset int32
	Sync      bool
}

// PTS returns the composition time of the sample.
func (s Sample) PTS() int64 { return s.DTS + int64(s.CTSOffset) }

// ErrTruncated reports a sample that lies beyond the end of the file: the
// index describes media that was never written or was cut off.
var ErrTruncated = errors.New("isobmff: sample lies beyond the end of the file")

// ErrBadSampleTable reports an inconsistent sample table.
var ErrBadSampleTable = errors.New("isobmff: inconsistent sample table")

// tableSampleCount returns the number of samples described by the stbl.
func (t *Track) tableSampleCount() int64 {
	if len(t.stsz) < 12 {
		return 0
	}
	n := int64(be.Uint32(t.stsz[8:]))
	if !t.stz2 && be.Uint32(t.stsz[4:]) == 0 {
		if max := int64(len(t.stsz)-12) / 4; n > max {
			n = max
		}
	}
	if t.stz2 {
		fs := int64(t.stsz[7])
		var max int64
		switch fs {
		case 4:
			max = int64(len(t.stsz)-12) * 2
		case 8:
			max = int64(len(t.stsz) - 12)
		case 16:
			max = int64(len(t.stsz)-12) / 2
		}
		if n > max {
			n = max
		}
	}
	// stts bounds the count as well (it is authoritative for timing).
	var sttsN int64
	for i := 0; i+8 <= len(t.stts); i += 8 {
		sttsN += int64(be.Uint32(t.stts[i:]))
	}
	if sttsN < n {
		n = sttsN
	}
	return n
}

// uniformSize returns the constant sample size, or 0 if sizes vary.
func (t *Track) uniformSize() uint32 {
	if len(t.stsz) < 12 || t.stz2 {
		return 0
	}
	return be.Uint32(t.stsz[4:])
}

func (t *Track) sampleSize(i int64) uint32 {
	if t.stz2 {
		p := t.stsz[12:]
		switch t.stsz[7] {
		case 4:
			b := p[i/2]
			if i&1 == 0 {
				return uint32(b >> 4)
			}
			return uint32(b & 15)
		case 8:
			return uint32(p[i])
		default:
			return uint32(be.Uint16(p[i*2:]))
		}
	}
	if u := be.Uint32(t.stsz[4:]); u != 0 {
		return u
	}
	return be.Uint32(t.stsz[12+i*4:])
}

func (t *Track) chunkOffset(i int) int64 {
	if t.co64 {
		return int64(be.Uint64(t.stco[i*8:]))
	}
	return int64(be.Uint32(t.stco[i*4:]))
}

func (t *Track) chunkCount() int {
	if t.co64 {
		return len(t.stco) / 8
	}
	return len(t.stco) / 4
}

// finishStats computes the per-track statistics from the sample tables and
// any fragments. It walks table entries, never per-sample data except stsz.
func (t *Track) finishStats() {
	n := t.tableSampleCount()
	t.SampleCount = n
	// Durations.
	type dc struct {
		delta uint32
		count int64
	}
	var hist []dc
	add := func(delta uint32, count int64) {
		for i := range hist {
			if hist[i].delta == delta {
				hist[i].count += count
				return
			}
		}
		if len(hist) < 64 {
			hist = append(hist, dc{delta, count})
		}
	}
	var lastDelta uint32
	remain := n
	for i := 0; i+8 <= len(t.stts) && remain > 0; i += 8 {
		c := int64(be.Uint32(t.stts[i:]))
		d := be.Uint32(t.stts[i+4:])
		if c > remain {
			c = remain
		}
		remain -= c
		t.MediaDuration += uint64(c) * uint64(d)
		add(d, c)
		lastDelta = d
	}
	// Sizes.
	if n > 0 {
		if u := t.uniformSize(); u != 0 {
			t.TotalBytes = int64(u) * n
		} else {
			for i := int64(0); i < n; i++ {
				t.TotalBytes += int64(t.sampleSize(i))
			}
		}
	}
	// First composition time.
	haveFirst := false
	if n > 0 {
		haveFirst = true
		if len(t.ctts) >= 8 {
			t.FirstCTS = int64(int32(be.Uint32(t.ctts[4:])))
			// The smallest composition time among the leading samples is the
			// presentation start (B-frame reordering).
			t.MinCTS = t.FirstCTS
			var dts int64
			it := t.Iter()
			for k := 0; k < 64; k++ {
				s, err := it.Next()
				if err != nil {
					break
				}
				if c := s.PTS(); c < t.MinCTS {
					t.MinCTS = c
				}
				dts = s.DTS
			}
			_ = dts
		}
	}
	// Fragments.
	for i, s := range t.frag {
		t.SampleCount++
		t.MediaDuration += uint64(s.Dur)
		t.TotalBytes += int64(s.Size)
		add(s.Dur, 1)
		lastDelta = s.Dur
		if !haveFirst {
			haveFirst = true
			t.FirstCTS = s.PTS()
			t.MinCTS = t.FirstCTS
		}
		if i < 64 && n == 0 {
			if c := s.PTS(); c < t.MinCTS {
				t.MinCTS = c
			}
		}
	}
	if len(t.frag) > 0 && t.SyncCount < 0 {
		var sync int64
		for _, s := range t.frag {
			if s.Sync {
				sync++
			}
		}
		if sync != int64(len(t.frag)) || n == 0 {
			t.SyncCount = sync
			if sync == int64(len(t.frag)) && n == 0 {
				t.SyncCount = -1
			}
		}
	}
	// The last sample commonly carries an odd duration; ignore one sample of
	// lastDelta when judging variability.
	var best dc
	distinct := 0
	var lo, hi uint32
	for _, h := range hist {
		c := h.count
		if h.delta == lastDelta {
			c--
		}
		if h.count > best.count {
			best = h
		}
		if c <= 0 {
			continue
		}
		if distinct == 0 || h.delta < lo {
			lo = h.delta
		}
		if distinct == 0 || h.delta > hi {
			hi = h.delta
		}
		distinct++
	}
	t.CommonDelta = best.delta
	t.VariableRate = distinct > 1 && hi-lo > 1
}

// SampleIter walks a track's samples in decode order: first the sample
// tables, then any movie fragments.
type SampleIter struct {
	t *Track
	n int64 // table sample count
	i int64 // next sample index

	dts               int64
	sttsI             int
	sttsLeft          uint32
	sttsDelta         uint32
	cttsI             int
	cttsLeft          uint32
	cttsOff           int32
	stscI             int
	chunk             int // current chunk index (0-based)
	inChunk, perChunk uint32
	off               int64
	nextSync          int
	fragI             int
	sizeOverride      uint32
	started           bool
}

// Iter returns an iterator positioned at the first sample.
func (t *Track) Iter() *SampleIter {
	return &SampleIter{t: t, n: t.tableSampleCount(), chunk: -1}
}

// SetUniformSize overrides the per-sample size. QuickTime PCM tracks often
// store a sample size of 1 in stsz; the caller supplies the real frame size.
func (it *SampleIter) SetUniformSize(n uint32) { it.sizeOverride = n }

func (it *SampleIter) size(i int64) uint32 {
	if it.sizeOverride != 0 {
		return it.sizeOverride
	}
	return it.t.sampleSize(i)
}

// advanceChunk moves to the chunk holding the next sample.
func (it *SampleIter) advanceChunk() error {
	t := it.t
	it.chunk++
	if it.chunk >= t.chunkCount() {
		return ErrBadSampleTable
	}
	for it.stscI+12 <= len(t.stsc) {
		first := be.Uint32(t.stsc[it.stscI:])
		if first == 0 || int(first-1) > it.chunk {
			break
		}
		it.perChunk = be.Uint32(t.stsc[it.stscI+4:])
		it.stscI += 12
	}
	if it.perChunk == 0 {
		return ErrBadSampleTable
	}
	it.inChunk = 0
	it.off = t.chunkOffset(it.chunk)
	return nil
}

func (it *SampleIter) timing() (dur uint32, cts int32) {
	t := it.t
	for it.sttsLeft == 0 && it.sttsI+8 <= len(t.stts) {
		it.sttsLeft = be.Uint32(t.stts[it.sttsI:])
		it.sttsDelta = be.Uint32(t.stts[it.sttsI+4:])
		it.sttsI += 8
	}
	for it.cttsLeft == 0 && it.cttsI+8 <= len(t.ctts) {
		it.cttsLeft = be.Uint32(t.ctts[it.cttsI:])
		it.cttsOff = int32(be.Uint32(t.ctts[it.cttsI+4:]))
		it.cttsI += 8
	}
	if it.cttsLeft > 0 {
		cts = it.cttsOff
	}
	return it.sttsDelta, cts
}

// Next returns the next sample, or io.EOF.
func (it *SampleIter) Next() (Sample, error) {
	t := it.t
	if it.i >= it.n {
		if it.fragI < len(t.frag) {
			s := t.frag[it.fragI]
			it.fragI++
			return s, nil
		}
		return Sample{}, io.EOF
	}
	if it.chunk < 0 || it.inChunk >= it.perChunk {
		if err := it.advanceChunk(); err != nil {
			return Sample{}, err
		}
	}
	dur, cts := it.timing()
	s := Sample{Offset: it.off, Size: it.size(it.i), Dur: dur, DTS: it.dts, CTSOffset: cts}
	if t.stss == nil {
		s.Sync = true
	} else {
		for it.nextSync+4 <= len(t.stss) && int64(be.Uint32(t.stss[it.nextSync:])) < it.i+1 {
			it.nextSync += 4
		}
		s.Sync = it.nextSync+4 <= len(t.stss) && int64(be.Uint32(t.stss[it.nextSync:])) == it.i+1
	}
	it.step(1)
	return s, nil
}

// step consumes n samples that share the current chunk and stts run.
func (it *SampleIter) step(n uint32) {
	sz := it.size(it.i)
	it.off += int64(sz) * int64(n)
	it.dts += int64(it.sttsDelta) * int64(n)
	it.i += int64(n)
	it.inChunk += n
	if it.sttsLeft >= n {
		it.sttsLeft -= n
	} else {
		it.sttsLeft = 0
	}
	if it.cttsLeft >= n {
		it.cttsLeft -= n
	} else {
		it.cttsLeft = 0
	}
}

// NextBlock returns up to max consecutive samples as one contiguous byte
// range. It is meant for constant-size tracks (PCM), where one "sample" is a
// single audio frame; s.Size is the size of each sample and count the number
// of samples in the block. For variable-size tracks it returns one sample.
func (it *SampleIter) NextBlock(max int) (s Sample, count int, err error) {
	t := it.t
	uniform := it.sizeOverride != 0 || t.uniformSize() != 0
	if it.i >= it.n || !uniform || max <= 1 {
		s, err = it.Next()
		return s, 1, err
	}
	if it.chunk < 0 || it.inChunk >= it.perChunk {
		if err := it.advanceChunk(); err != nil {
			return Sample{}, 0, err
		}
	}
	dur, _ := it.timing()
	n := it.perChunk - it.inChunk
	if it.sttsLeft < n {
		n = it.sttsLeft
	}
	if int64(n) > it.n-it.i {
		n = uint32(it.n - it.i)
	}
	if n > uint32(max) {
		n = uint32(max)
	}
	if n == 0 {
		return Sample{}, 0, ErrBadSampleTable
	}
	s = Sample{Offset: it.off, Size: it.size(it.i), Dur: dur, DTS: it.dts, Sync: true}
	it.step(n)
	return s, int(n), nil
}

// ReadSample reads the bytes of a sample (or of count consecutive samples
// returned by NextBlock) after validating the range against the file size.
func (f *File) ReadSample(s Sample, count int, buf []byte) ([]byte, error) {
	n := int64(s.Size) * int64(count)
	if s.Offset < 0 || n < 0 || n > 1<<30 {
		return nil, ErrBadSampleTable
	}
	if s.Offset+n > f.size {
		return nil, ErrTruncated
	}
	if int64(cap(buf)) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := f.r.ReadAt(buf, s.Offset); err != nil {
		return nil, err
	}
	return buf, nil
}
