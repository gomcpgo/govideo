package native

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	mathbits "math/bits"

	"github.com/gomcpgo/govideo/internal/bits"
	"github.com/gomcpgo/govideo/internal/codechdr"
)

// FLAC is written from RFC 9639 alone; section numbers below are its.

const (
	// FLACHeaderSize is the length of what stands before the first frame of
	// a FLAC file as FLACEncoder.Header writes it: the "fLaC" marker and one
	// metadata block, STREAMINFO.
	FLACHeaderSize = 4 + 4 + flacStreamInfoSize

	flacStreamInfoSize = 34
	// flacBlockSize is the number of sample frames in every frame but the
	// last. 4096 is within the streamable subset at every sample rate (§7).
	flacBlockSize = 4096
	// flacBits is the sample size. A side channel has one bit more (§4.2).
	flacBits = 16
	// flacMaxFixed is the highest order of the fixed predictors (§9.2.5).
	flacMaxFixed = 4
	// flacMaxRice is the largest Rice parameter that fits the 4-bit field;
	// 15 there is the escape code, which is not used (§9.2.7).
	flacMaxRice = 14
	// flacMaxPartOrder is the largest partition order of the streamable
	// subset (§7).
	flacMaxPartOrder = 8
)

// The kinds of subframe written (§9.2.1).
const (
	flacConstant = iota
	flacVerbatim
	flacFixed
)

// The channel assignments of a stereo frame (§9.1.3).
const (
	flacLeftRight = 1 // two independent channels
	flacLeftSide  = codechdr.FLACLeftSide
	flacSideRight = codechdr.FLACSideRight
	flacMidSide   = codechdr.FLACMidSide
)

// flacSubframe is how one channel of a block is to be coded.
type flacSubframe struct {
	kind      int
	order     int     // of the fixed predictor
	bits      int     // the size of the subframe
	partOrder int     // the residual is in 1<<partOrder partitions
	params    []uint8 // the Rice parameter of each
}

// FLACEncoder compresses 16-bit mono or stereo sound to FLAC frames, without
// loss. It uses the fixed predictors with partitioned Rice coding, and in
// stereo picks for every frame the cheapest of the four ways to code the
// pair of channels. The Rice parameters, the partition order and the stereo
// coding are chosen by the exact size they give; the predictor by an
// estimate.
//
// It implements codec.AudioStreamEncoder. Samples are taken to 16 bits as
// AppendS16LE takes them.
type FLACEncoder struct {
	rate, channels int
	rateCode       uint32 // of the frame header (§9.1.2)
	rateExtra      uint32 // the rate written after the header, in
	rateExtraBits  int    // this many bits; 0 for none

	pend     []int32 // samples not yet in a frame, interleaved
	pcm      []byte
	sum      hash.Hash // MD5 of the sound as 16-bit little-endian PCM
	frames   int64
	total    int64 // sample frames taken
	minFrame int   // sizes of the frames written, in bytes
	maxFrame int
	done     bool

	w    bits.Writer
	out  []byte // the frames of the last call, end to end
	ends []int
	pkts [][]byte

	ch   [4][]int32 // left, right, mid, side
	plan [4]flacSubframe
	res  []int32
	rice riceSearch
}

// NewFLACEncoder returns an encoder for sound at rate Hz in 1 or 2 channels.
func NewFLACEncoder(rate, channels int) (*FLACEncoder, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("native: FLAC is written in 1 or 2 channels, not %d", channels)
	}
	// STREAMINFO holds the rate in 20 bits (§8.2).
	if rate < 1 || rate >= 1<<20 {
		return nil, fmt.Errorf("native: FLAC cannot hold a sample rate of %d Hz", rate)
	}
	e := &FLACEncoder{rate: rate, channels: channels, sum: md5.New()}
	common := map[int]uint32{88200: 1, 176400: 2, 192000: 3, 8000: 4, 16000: 5, 22050: 6,
		24000: 7, 32000: 8, 44100: 9, 48000: 10, 96000: 11}
	switch code, ok := common[rate]; {
	case ok:
		e.rateCode = code
	case rate%1000 == 0 && rate/1000 < 1<<8:
		e.rateCode, e.rateExtra, e.rateExtraBits = 12, uint32(rate/1000), 8
	case rate < 1<<16:
		e.rateCode, e.rateExtra, e.rateExtraBits = 13, uint32(rate), 16
	case rate%10 == 0 && rate/10 < 1<<16:
		e.rateCode, e.rateExtra, e.rateExtraBits = 14, uint32(rate/10), 16
	}
	// Any other rate is left to STREAMINFO (code 0): valid, though outside
	// the streamable subset.
	return e, nil
}

// Encode takes interleaved samples and returns the frames they complete.
// The slices are valid until the next call to Encode or Flush.
func (e *FLACEncoder) Encode(samples []float32) ([][]byte, error) {
	if e.done {
		return nil, errors.New("native: FLAC encoder used after Flush")
	}
	if len(samples)%e.channels != 0 {
		return nil, fmt.Errorf("native: %d samples are no whole number of frames of %d channels", len(samples), e.channels)
	}
	e.out, e.ends, e.pcm = e.out[:0], e.ends[:0], e.pcm[:0]
	for _, s := range samples {
		v := s16(s)
		e.pcm = append(e.pcm, byte(v), byte(v>>8))
		e.pend = append(e.pend, int32(v))
	}
	e.sum.Write(e.pcm)
	e.total += int64(len(samples) / e.channels)

	full, used := flacBlockSize*e.channels, 0
	for ; len(e.pend)-used >= full; used += full {
		if err := e.frame(e.pend[used : used+full]); err != nil {
			return nil, err
		}
	}
	e.pend = append(e.pend[:0], e.pend[used:]...)
	return e.packets(), nil
}

// Flush ends the stream and returns its last frame, which holds the samples
// that did not fill a block. The STREAMINFO block is complete after it.
func (e *FLACEncoder) Flush() ([][]byte, error) {
	if e.done {
		return nil, nil
	}
	e.done = true
	e.out, e.ends = e.out[:0], e.ends[:0]
	if len(e.pend) > 0 {
		if err := e.frame(e.pend); err != nil {
			return nil, err
		}
		e.pend = e.pend[:0]
	}
	return e.packets(), nil
}

func (e *FLACEncoder) packets() [][]byte {
	e.pkts = e.pkts[:0]
	from := 0
	for _, end := range e.ends {
		e.pkts = append(e.pkts, e.out[from:end])
		from = end
	}
	return e.pkts
}

// Config returns the STREAMINFO block (§8.2), without a metadata block
// header: 34 bytes. The sample count, the frame sizes and the MD5 of the
// sound cover what has been encoded so far, and are final after Flush.
func (e *FLACEncoder) Config() []byte {
	b := make([]byte, 0, flacStreamInfoSize)
	b = binary.BigEndian.AppendUint16(b, flacBlockSize)
	b = binary.BigEndian.AppendUint16(b, flacBlockSize)
	b = append(b, byte(e.minFrame>>16), byte(e.minFrame>>8), byte(e.minFrame))
	b = append(b, byte(e.maxFrame>>16), byte(e.maxFrame>>8), byte(e.maxFrame))
	total := uint64(e.total)
	if total >= 1<<36 {
		total = 0 // "unknown": the field has 36 bits
	}
	b = binary.BigEndian.AppendUint64(b, uint64(e.rate)<<44|uint64(e.channels-1)<<41|(flacBits-1)<<36|total)
	return e.sum.Sum(b)
}

// Header returns what stands before the first frame of a FLAC file: the
// stream marker and the STREAMINFO block, marked as the last metadata block
// (§6, §8.1). It is FLACHeaderSize bytes at any time, so a file can be
// started with it and have it written again once Flush has made it final.
func (e *FLACEncoder) Header() []byte {
	return append([]byte{'f', 'L', 'a', 'C', 0x80, 0, 0, flacStreamInfoSize}, e.Config()...)
}

// FrameSize is the number of sample frames in every packet but the last.
func (e *FLACEncoder) FrameSize() int { return flacBlockSize }

// Priming is 0: a FLAC stream starts on its first sample.
func (e *FLACEncoder) Priming() int { return 0 }

// Bitrate is 0: the size follows the sound.
func (e *FLACEncoder) Bitrate() int { return 0 }

// Close does nothing; it is there for codec.AudioStreamEncoder.
func (e *FLACEncoder) Close() error { return nil }

// frame encodes one block, given interleaved, and appends it to e.out.
func (e *FLACEncoder) frame(x []int32) error {
	// A frame number is coded in at most 31 bits (§9.1.5).
	if e.frames >= 1<<31 {
		return errors.New("native: the sound is longer than a FLAC stream of this block size can number")
	}
	n := len(x) / e.channels
	for c := range e.ch {
		if cap(e.ch[c]) < n {
			e.ch[c] = make([]int32, flacBlockSize)
		}
		e.ch[c] = e.ch[c][:n]
	}
	left, right, mid, side := e.ch[0], e.ch[1], e.ch[2], e.ch[3]

	// Which channels are written, and in how many bits a sample.
	first, second, assign := 0, -1, uint32(0)
	if e.channels == 1 {
		copy(left, x)
		e.planSubframe(left, flacBits, &e.plan[0])
	} else {
		for i := range left {
			l, r := x[2*i], x[2*i+1]
			left[i], right[i] = l, r
			mid[i], side[i] = (l+r)>>1, l-r // §4.2
		}
		e.planSubframe(left, flacBits, &e.plan[0])
		e.planSubframe(right, flacBits, &e.plan[1])
		e.planSubframe(mid, flacBits, &e.plan[2])
		e.planSubframe(side, flacBits+1, &e.plan[3])
		l, r, m, s := e.plan[0].bits, e.plan[1].bits, e.plan[2].bits, e.plan[3].bits
		first, second, assign = 0, 1, flacLeftRight
		best := l + r
		if l+s < best {
			best, first, second, assign = l+s, 0, 3, flacLeftSide
		}
		if s+r < best {
			best, first, second, assign = s+r, 3, 1, flacSideRight
		}
		if m+s < best {
			first, second, assign = 2, 3, flacMidSide
		}
	}

	start := len(e.out)
	w := &e.w
	w.Reset(e.out)
	// The header (§9.1): the sync code with the fixed-block-size bit, the
	// block size, the sample rate, the channels, the sample size (16 bits
	// is code 4) and the frame number.
	w.U(0xfff8, 16)
	sizeCode, sizeBits := flacBlockSizeCode(n)
	w.U(sizeCode, 4)
	w.U(e.rateCode, 4)
	w.U(assign, 4)
	w.U(4<<1, 4)
	flacWriteNumber(w, uint64(e.frames))
	w.U(uint32(n-1), sizeBits)
	w.U(e.rateExtra, e.rateExtraBits)
	w.U(uint32(flacCRC8(w.Bytes()[start:])), 8)

	e.writeSubframe(w, e.ch[first], subframeBits(first), &e.plan[first])
	if second >= 0 {
		e.writeSubframe(w, e.ch[second], subframeBits(second), &e.plan[second])
	}
	w.Align()
	e.out = binary.BigEndian.AppendUint16(w.Bytes(), flacCRC16(w.Bytes()[start:]))

	size := len(e.out) - start
	if e.frames == 0 || size < e.minFrame {
		e.minFrame = size
	}
	e.maxFrame = max(e.maxFrame, size)
	e.ends = append(e.ends, len(e.out))
	e.frames++
	return nil
}

// subframeBits is the sample size of channel c of e.ch: the side channel,
// a difference of two samples, needs a bit more.
func subframeBits(c int) int {
	if c == 3 {
		return flacBits + 1
	}
	return flacBits
}

// flacBlockSizeCode is the header code of a block of n samples, and the
// number of bits in which n-1 follows the header when there is no code for
// it (§9.1.1).
func flacBlockSizeCode(n int) (code uint32, extraBits int) {
	switch {
	case n == 192:
		return 1, 0
	case n == 576 || n == 1152 || n == 2304 || n == 4608:
		return uint32(1 + mathbits.Len(uint(n/576))), 0
	case n >= 256 && n&(n-1) == 0:
		return uint32(mathbits.Len(uint(n/256))) + 7, 0
	case n <= 256:
		return 6, 8
	}
	return 7, 16
}

// flacWriteNumber writes a frame number in the variable-length code of the
// frame header: UTF-8's scheme, carried on to seven bytes (§9.1.5).
func flacWriteNumber(w *bits.Writer, v uint64) {
	if v < 0x80 {
		w.U(uint32(v), 8)
		return
	}
	n := 2 // bytes: the first carries 7-n bits, the others 6 each
	for v>>(uint(5*n)+1) != 0 {
		n++
	}
	w.U(0xff<<uint(8-n)|uint32(v>>(uint(6*(n-1)))), 8)
	for i := n - 2; i >= 0; i-- {
		w.U(0x80|uint32(v>>uint(6*i))&0x3f, 8)
	}
}

// planSubframe decides how to code the samples x of one channel, each of
// bps bits: as a constant, with the fixed predictor that gives the smallest
// subframe, or verbatim where prediction does not pay.
func (e *FLACEncoder) planSubframe(x []int32, bps int, sf *flacSubframe) {
	n := len(x)
	constant := true
	for _, v := range x[1:] {
		if v != x[0] {
			constant = false
			break
		}
	}
	if constant {
		sf.kind, sf.bits = flacConstant, 8+bps
		return
	}
	sf.kind = flacVerbatim
	best := int64(8 + n*bps)
	if cap(e.res) < n {
		e.res = make([]int32, flacBlockSize)
	}
	res := e.res[:n]
	// The residual of each order is the difference of the one before
	// (§9.2.5); the first order samples are not predicted. Every order is
	// priced roughly and the best of them exactly: on real sound that
	// gives the sizes pricing all five exactly does, in half the time
	// (SPEC §14.1).
	order, least := 0, int64(0)
	copy(res, x)
	for o := 0; o <= flacMaxFixed && o < n; o++ {
		for i := n - 1; i >= o && o > 0; i-- {
			res[i] -= res[i-1]
		}
		if rough := e.rice.estimate(res, o) + int64(o*bps); o == 0 || rough < least {
			order, least = o, rough
		}
	}
	flacResidual(res, x, order)
	size, partOrder, params := e.rice.cost(res, order)
	if size += int64(8 + order*bps); size < best {
		best = size
		sf.kind, sf.order, sf.partOrder = flacFixed, order, partOrder
		sf.params = append(sf.params[:0], params...)
	}
	sf.bits = int(best)
}

// writeSubframe writes the samples x of one channel as planned (§9.2).
func (e *FLACEncoder) writeSubframe(w *bits.Writer, x []int32, bps int, sf *flacSubframe) {
	// The first byte: a zero bit, the type, and no wasted bits.
	switch sf.kind {
	case flacConstant:
		w.U(0, 8)
		w.S(x[0], bps)
	case flacVerbatim:
		w.U(1<<1, 8)
		for _, v := range x {
			w.S(v, bps)
		}
	case flacFixed:
		w.U(uint32(8|sf.order)<<1, 8)
		for _, v := range x[:sf.order] {
			w.S(v, bps)
		}
		n := len(x)
		res := e.res[:n]
		flacResidual(res, x, sf.order)
		// The residual (§9.2.7): Rice coding with 4-bit parameters, in
		// partitions of equal length; the first is short by the samples
		// that were not predicted.
		w.U(0, 2)
		w.U(uint32(sf.partOrder), 4)
		size, i := n>>uint(sf.partOrder), sf.order
		for j, k := range sf.params {
			w.U(uint32(k), 4)
			for end := (j + 1) * size; i < end; i++ {
				flacWriteRice(w, flacFold(res[i]), uint(k))
			}
		}
	}
}

// flacResidual sets res to what the fixed predictor of the given order
// leaves of x; res[:order] is not meaningful.
func flacResidual(res, x []int32, order int) {
	copy(res, x)
	for o := 1; o <= order; o++ {
		for i := len(res) - 1; i >= o; i-- {
			res[i] -= res[i-1]
		}
	}
}

// flacFold maps a residual to an unsigned number, small for a small
// magnitude: 0, -1, 1, -2, 2 become 0, 1, 2, 3, 4 (§9.2.7.2).
func flacFold(r int32) uint32 { return uint32(r<<1) ^ uint32(r>>31) }

// flacWriteRice writes u with Rice parameter k: u>>k in unary, then the low
// k bits.
func flacWriteRice(w *bits.Writer, u uint32, k uint) {
	q := u >> k
	if q+uint32(k) < 32 {
		w.U(1<<k|u&(1<<k-1), int(q)+1+int(k))
		return
	}
	w.Unary(q)
	w.U(u, int(k))
}

// riceSearch finds the cheapest partitioned Rice coding of a residual. It
// holds the working memory, so that a search allocates nothing.
type riceSearch struct {
	u      []uint32 // the residual, folded
	sums   []uint64 // for each parameter tried, and each partition: Σ u>>k
	params []uint8  // the best parameters at each partition order
}

// estimate is roughly the size in bits of the cheapest coding of
// res[order:], from the sums of the smallest partitions alone: each is
// priced as if its samples were spread evenly below twice their mean. It is
// good enough to rank predictors by, and takes one pass.
func (s *riceSearch) estimate(res []int32, order int) (size int64) {
	n := len(res)
	top := flacTopPartOrder(n, order)
	psize := n >> uint(top)
	for from, to := order, psize; to <= n; from, to = to, to+psize {
		var sum uint64
		for _, r := range res[from:to] {
			sum += uint64(flacFold(r))
		}
		c := uint64(to - from)
		k := max(min(mathbits.Len64(sum/c), flacMaxRice+1)-1, 0)
		size += int64(c*uint64(k+1) + sum>>uint(k) - c/2 + 4)
	}
	return size
}

// flacTopPartOrder is the largest partition order for a block of n samples
// with a predictor of the given order: it must divide the block and leave
// samples in the first partition (§9.2.7).
func flacTopPartOrder(n, order int) (top int) {
	for top < flacMaxPartOrder && n&(2<<uint(top)-1) == 0 && n>>uint(top+1) > order {
		top++
	}
	return top
}

// cost returns the size in bits of the cheapest coding of res[order:], the
// residual of a block of len(res) samples whose first order are not
// predicted, with the partition order and the Rice parameters that give it.
// params is valid until the next call.
//
// Coded with parameter k, a partition of c samples takes c·(k+1) + Σ u>>k
// bits, and that sum over two partitions is the sum of theirs. So the sums
// are taken once, for the smallest partitions, and added in pairs for each
// order below: every parameter of every partition order is priced exactly.
func (s *riceSearch) cost(res []int32, order int) (size int64, partOrder int, params []uint8) {
	n := len(res)
	top := flacTopPartOrder(n, order)
	parts, psize := 1<<uint(top), n>>uint(top)
	if cap(s.u) < n {
		s.u = make([]uint32, max(n, flacBlockSize))
	}
	u := s.u[:n]

	// The parameters worth pricing lie about log2 of the mean of a
	// partition; take the range the smallest partitions span, and a little
	// more.
	lo, hi := flacMaxRice, 0
	for j := 0; j < parts; j++ {
		from, to := j*psize, (j+1)*psize
		if j == 0 {
			from = order
		}
		var sum uint64
		for i := from; i < to; i++ {
			u[i] = flacFold(res[i])
			sum += uint64(u[i])
		}
		k := min(mathbits.Len64(sum/uint64(to-from)), flacMaxRice+1) - 1
		lo, hi = min(lo, max(k, 0)), max(hi, k)
	}
	lo, hi = max(lo-2, 0), min(hi+1, flacMaxRice)
	nk := hi - lo + 1

	if cap(s.sums) < nk*parts {
		s.sums = make([]uint64, (flacMaxRice+1)<<flacMaxPartOrder)
	}
	sums := s.sums[:nk*parts]
	for j := 0; j < parts; j++ {
		from, to := j*psize, (j+1)*psize
		if j == 0 {
			from = order
		}
		for ki := 0; ki < nk; ki++ {
			k := uint(lo + ki)
			var t uint64
			for _, v := range u[from:to] {
				t += uint64(v >> k)
			}
			sums[ki*parts+j] = t
		}
	}

	// The parameters of partition order p are kept at params[1<<p-1:].
	if cap(s.params) < 2*parts {
		s.params = make([]uint8, 2<<flacMaxPartOrder)
	}
	size = -1
	for p := top; p >= 0; p-- {
		np := 1 << uint(p)
		level := s.params[np-1 : 2*np-1]
		var total int64
		for j := range level {
			c := int64(n >> uint(p))
			if j == 0 {
				c -= int64(order)
			}
			least := int64(-1)
			for ki := 0; ki < nk; ki++ {
				if b := c*int64(lo+ki+1) + int64(sums[ki*parts+j]); least < 0 || b < least {
					least, level[j] = b, uint8(lo+ki)
				}
			}
			total += 4 + least
		}
		if size < 0 || total <= size {
			size, partOrder = total, p
		}
		for ki := 0; ki < nk && p > 0; ki++ {
			row := sums[ki*parts:]
			for j := 0; j < np/2; j++ {
				row[j] = row[2*j] + row[2*j+1]
			}
		}
	}
	// The coding method and the partition order take 6 bits.
	return size + 6, partOrder, s.params[1<<uint(partOrder)-1 : 2<<uint(partOrder)-1]
}

// flacCRC8 is the checksum of a frame header (§9.1.8).
func flacCRC8(b []byte) uint8 { return codechdr.FLACCRC8(b) }

// flacCRC16 is the checksum of a frame (§9.3).
func flacCRC16(b []byte) uint16 { return codechdr.FLACCRC16(0, b) }
