package native

import (
	"encoding/binary"
	"errors"
	"fmt"
	mathbits "math/bits"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/codechdr"
)

// The FLAC decoder, like the encoder, is written from RFC 9639 alone;
// section numbers are its.

// ErrNotFLAC reports a packet that does not start with the header of a FLAC
// frame.
var ErrNotFLAC = errors.New("native: not a FLAC frame")

// flacMaxLPC is the highest order of a linear predictor (§9.2.6).
const flacMaxLPC = 32

// FLACDecoder decodes FLAC: every kind of subframe, 4 to 32 bits a sample,
// 1 to 8 channels, blocks of any size. The samples come out as float32 with
// full scale at 1, so sound of more than 24 bits loses its lowest bits
// there; up to 24 bits the conversion is exact.
//
// A frame stands alone: nothing is carried from one to the next, so a
// decoder can start at any frame of a stream. A frame that fails its
// checksum, or whose content is not valid, comes out as silence of the
// length its header states, which the header's own checksum vouches for:
// what a damaged predictor makes of its residual can be noise at full
// scale, and the sound after the damage stays where it belongs.
type FLACDecoder struct {
	rate, channels, depth int // of the stream; 0 until known

	ch   [8][]int32 // a block of each channel
	wide []int64    // the side channel of a 32-bit frame, which takes 33 bits
	res  []int32    // the residual of a subframe
	coef [flacMaxLPC]int32
	bits flacBitReader
}

// NewFLACDecoder returns a decoder for the stream that config describes: a
// STREAMINFO block in any form codechdr.ParseFLACStreamInfo reads (bare,
// with its block header, a dfLa box, or behind the "fLaC" marker as in a
// Matroska CodecPrivate). Every frame must then have the rate, channels and
// sample size stated there. With no config the first frame decoded sets
// them, and a frame that leaves its rate or sample size to STREAMINFO
// cannot be decoded.
func NewFLACDecoder(config []byte) (*FLACDecoder, error) {
	d := &FLACDecoder{}
	if len(config) == 0 {
		return d, nil
	}
	a, err := codechdr.ParseFLACStreamInfo(config)
	if err != nil {
		return nil, fmt.Errorf("native: FLAC stream information: %w", err)
	}
	// §8.2: 4 to 32 bits; the channel count has three bits and the rate is
	// checked by the parser.
	if a.BitDepth < 4 {
		return nil, fmt.Errorf("native: FLAC does not hold samples of %d bits", a.BitDepth)
	}
	d.rate, d.channels, d.depth = a.SampleRate, a.Channels, a.BitDepth
	return d, nil
}

// Decode decodes a packet: one frame. Bytes after the frame are not looked
// at. The frame gives as many samples of every channel as its header
// states, as silence where it cannot be decoded; the errors are a packet
// that does not start with a frame header, and a frame of another format
// than the stream's.
func (d *FLACDecoder) Decode(packet []byte) (*codec.AudioFrame, error) {
	h, ok := codechdr.FLACFrameHeader(packet)
	if !ok {
		return nil, ErrNotFLAC
	}
	rate, depth := h.SampleRate, h.BitDepth
	if rate == 0 {
		rate = d.rate
	}
	if depth == 0 {
		depth = d.depth
	}
	if rate == 0 || depth == 0 {
		return nil, errors.New("native: a FLAC frame leaves its format to a STREAMINFO block, and the decoder was given none")
	}
	if d.rate == 0 {
		d.rate, d.channels, d.depth = rate, h.Channels, depth
	}
	// A format that changes on the way is allowed in a stream and not in a
	// file (§9); nothing downstream could follow it.
	if rate != d.rate || h.Channels != d.channels || depth != d.depth {
		return nil, fmt.Errorf("native: a FLAC frame of %d Hz, %d channels and %d bits in a stream of %d Hz, %d channels and %d bits",
			rate, h.Channels, depth, d.rate, d.channels, d.depth)
	}
	out := &codec.AudioFrame{SampleRate: rate, Channels: h.Channels,
		Samples: make([]float32, h.BlockSize*h.Channels)}
	if !d.frame(h, depth, packet) {
		return out, nil // silence
	}
	scale := 1 / float32(uint64(1)<<uint(depth-1))
	for c := 0; c < h.Channels; c++ {
		pcm := out.Samples[c:]
		for i, v := range d.ch[c][:h.BlockSize] {
			pcm[i*h.Channels] = float32(v) * scale
		}
	}
	return out, nil
}

// frame decodes the frame at the start of data, whose header is h, into
// d.ch: a block of each channel, left and right restored. It reports whether
// the frame is whole and valid, checksum included.
func (d *FLACDecoder) frame(h codechdr.FLACFrame, depth int, data []byte) bool {
	// After the subframes, zero bits to the end of the byte, then the
	// checksum of everything before it (§9.3).
	end, ok := d.subframes(h, depth, data)
	if !ok || end+2 > len(data) || codechdr.FLACCRC16(0, data[:end]) != binary.BigEndian.Uint16(data[end:]) {
		return false
	}
	d.stereo(h, depth)
	return true
}

// flacSide is which of a frame's two channels holds the difference of left
// and right, or -1 where the channels are coded each on its own.
func flacSide(assign int) int {
	switch assign {
	case codechdr.FLACLeftSide, codechdr.FLACMidSide:
		return 1
	case codechdr.FLACSideRight:
		return 0
	}
	return -1
}

// subframes decodes the subframes of a frame, one for each channel, as they
// are coded, and returns where they end in data: at the next whole byte.
func (d *FLACDecoder) subframes(h codechdr.FLACFrame, depth int, data []byte) (end int, ok bool) {
	n := h.BlockSize
	if cap(d.res) < n {
		d.res = make([]int32, n)
	}
	d.bits = flacBitReader{buf: data, pos: h.Size}
	// The channel that is a difference has a bit more (§4.2), and of 32-bit
	// samples that is one more than an int32 holds.
	side := flacSide(h.Assignment)
	for c := 0; c < h.Channels; c++ {
		if cap(d.ch[c]) < n {
			d.ch[c] = make([]int32, n)
		}
		d.ch[c] = d.ch[c][:n]
		switch {
		case c == side && depth == 32:
			if cap(d.wide) < n {
				d.wide = make([]int64, n)
			}
			d.wide = d.wide[:n]
			ok = flacDecodeSubframe(d, d.wide, depth+1)
		case c == side:
			ok = flacDecodeSubframe(d, d.ch[c], depth+1)
		default:
			ok = flacDecodeSubframe(d, d.ch[c], depth)
		}
		if !ok {
			return 0, false
		}
	}
	return d.bits.end(), !d.bits.over
}

// stereo turns the two channels of a frame that holds them as a sum and a
// difference, or as one of them and the difference, into left and right
// (§4.2).
func (d *FLACDecoder) stereo(h codechdr.FLACFrame, depth int) {
	if flacSide(h.Assignment) < 0 {
		return
	}
	a, b := d.ch[0], d.ch[1]
	if depth == 32 {
		for i, s := range d.wide {
			switch h.Assignment {
			case codechdr.FLACLeftSide:
				b[i] = int32(int64(a[i]) - s)
			case codechdr.FLACSideRight:
				a[i] = int32(int64(b[i]) + s)
			default:
				m := int64(a[i])<<1 | s&1
				a[i], b[i] = int32((m+s)>>1), int32((m-s)>>1)
			}
		}
		return
	}
	switch h.Assignment {
	case codechdr.FLACLeftSide:
		for i, s := range b {
			b[i] = a[i] - s
		}
	case codechdr.FLACSideRight:
		for i, s := range a {
			a[i] = s + b[i]
		}
	default:
		// The mid channel lost its lowest bit, which is the side's.
		for i, s := range b {
			m := a[i]<<1 | s&1
			a[i], b[i] = (m+s)>>1, (m-s)>>1
		}
	}
}

// flacDecodeSubframe decodes one channel of a block into x, whose samples have bps
// bits (§9.2). It reports whether the subframe is valid; one that ends
// before its samples do is caught by the reader, which the caller asks.
func flacDecodeSubframe[T int32 | int64](d *FLACDecoder, x []T, bps int) bool {
	b := &d.bits
	// A zero bit, six bits of type, and whether low bits were left out
	// because they are zero in every sample: their number less one follows
	// in unary (§9.2.2).
	head := b.u(8)
	if head&0x80 != 0 {
		return false
	}
	kind, wasted := int(head>>1&0x3f), uint(0)
	if head&1 != 0 {
		wasted = uint(b.unary()) + 1
		if b.over || int(wasted) >= bps {
			return false
		}
		bps -= int(wasted)
	}
	n := len(x)
	switch {
	case kind == 0: // constant
		v := T(b.s(bps))
		for i := range x {
			x[i] = v
		}
	case kind == 1: // verbatim
		for i := range x {
			x[i] = T(b.s(bps))
		}
	case kind >= 8 && kind <= 12: // a fixed predictor (§9.2.5)
		order := kind - 8
		if order > n {
			return false
		}
		for i := range x[:order] {
			x[i] = T(b.s(bps))
		}
		res := d.res[:n]
		if !b.residual(res, order) {
			return false
		}
		flacRestoreFixed(x, res, order)
	case kind >= 32: // a linear predictor (§9.2.6)
		order := kind - 31
		if order > n {
			return false
		}
		for i := range x[:order] {
			x[i] = T(b.s(bps))
		}
		// The size of the coefficients, 15 being forbidden, and how far
		// the sum of the products is shifted down: stored signed, and not
		// allowed to be negative.
		precision := int(b.u(4)) + 1
		shift := b.s(5)
		if precision == 16 || shift < 0 {
			return false
		}
		// The first coefficient goes with the sample just before; they are
		// kept the other way round, to run along the samples.
		coef := d.coef[:order]
		for i := range coef {
			coef[order-1-i] = int32(b.s(precision))
		}
		res := d.res[:n]
		if !b.residual(res, order) {
			return false
		}
		flacRestoreLPC(x, res, coef, uint(shift))
	default:
		return false // reserved
	}
	if wasted > 0 {
		for i := range x {
			x[i] <<= wasted
		}
	}
	return !b.over
}

// flacRestoreFixed restores x from the residual of the fixed predictor of the
// given order; x[:order] is there already. The arithmetic may wrap on its
// way to a result that fits.
func flacRestoreFixed[T int32 | int64](x []T, res []int32, order int) {
	res = res[:len(x)]
	switch order {
	case 0:
		for i := range x {
			x[i] = T(res[i])
		}
	case 1:
		for i := 1; i < len(x); i++ {
			x[i] = T(res[i]) + x[i-1]
		}
	case 2:
		for i := 2; i < len(x); i++ {
			x[i] = T(res[i]) + 2*x[i-1] - x[i-2]
		}
	case 3:
		for i := 3; i < len(x); i++ {
			x[i] = T(res[i]) + 3*x[i-1] - 3*x[i-2] + x[i-3]
		}
	case 4:
		for i := 4; i < len(x); i++ {
			x[i] = T(res[i]) + 4*x[i-1] - 6*x[i-2] + 4*x[i-3] - x[i-4]
		}
	}
}

// flacRestoreLPC restores x from the residual of a linear predictor: coef runs
// from the oldest of the len(coef) samples before the one predicted to the
// newest, and x[:len(coef)] is there already. The sum has 64 bits, which
// the largest samples and coefficients of the format cannot fill (§A.3).
func flacRestoreLPC[T int32 | int64](x []T, res []int32, coef []int32, shift uint) {
	order := len(coef)
	res = res[:len(x)]
	for i := order; i < len(x); i++ {
		past := x[i-order : i]
		past = past[:len(coef)]
		var sum int64
		for j, c := range coef {
			sum += int64(c) * int64(past[j])
		}
		x[i] = T(int64(res[i]) + sum>>shift)
	}
}

// flacBitReader reads the bits of a frame, most significant first. A read past
// the end gives 0 and sets over.
type flacBitReader struct {
	buf  []byte
	pos  int    // the next byte of buf to take
	acc  uint64 // the bits taken and not yet read, from the top; the rest is 0
	n    uint   // how many
	over bool
}

// fill takes bytes until there is no room for another.
func (b *flacBitReader) fill() {
	for b.n <= 56 && b.pos < len(b.buf) {
		b.acc |= uint64(b.buf[b.pos]) << (56 - b.n)
		b.n += 8
		b.pos++
	}
}

// fail records a read past the end.
func (b *flacBitReader) fail() {
	b.over, b.acc, b.n, b.pos = true, 0, 0, len(b.buf)
}

// u reads n bits, 0 to 32, as an unsigned number.
func (b *flacBitReader) u(n uint) uint32 {
	if b.n < n {
		if b.fill(); b.n < n {
			b.fail()
			return 0
		}
	}
	v := uint32(b.acc >> (64 - n))
	b.acc <<= n
	b.n -= n
	return v
}

// s reads n bits, 0 to 33, of two's complement.
func (b *flacBitReader) s(n int) int64 {
	if n <= 0 {
		return 0
	}
	var v uint64
	if n > 32 {
		v = uint64(b.u(uint(n-32))) << 32
		v |= uint64(b.u(32))
	} else {
		v = uint64(b.u(uint(n)))
	}
	return int64(v<<uint(64-n)) >> uint(64-n)
}

// unary reads zero bits up to a one and returns how many there were.
func (b *flacBitReader) unary() (q uint32) {
	for b.acc == 0 {
		q += uint32(b.n)
		b.n = 0
		if b.pos >= len(b.buf) {
			b.fail()
			return 0
		}
		b.fill()
	}
	z := uint(mathbits.LeadingZeros64(b.acc))
	b.acc = b.acc << z << 1
	b.n -= z + 1
	return q + uint32(z)
}

// end is the number of bytes read, counting one that was begun as read.
func (b *flacBitReader) end() int { return b.pos - int(b.n/8) }

// residual reads the coded residual of a block of len(res) samples whose
// first order are not predicted, into res[order:] (§9.2.7). It reports
// whether the coding is valid.
func (b *flacBitReader) residual(res []int32, order int) bool {
	// The method says how many bits a Rice parameter has, 4 or 5; the
	// block is cut in 2^order partitions of equal length, each with its own
	// parameter, and the first holds less by the samples not predicted.
	method := b.u(2)
	if method > 1 {
		return false
	}
	n := len(res)
	partOrder := uint(b.u(4))
	size := n >> partOrder
	if n&(1<<partOrder-1) != 0 || size < order {
		return false
	}
	paramBits := 4 + uint(method)
	escape := uint32(1)<<paramBits - 1
	i := order
	for end := size; end <= n; end += size {
		k := b.u(paramBits)
		if k == escape {
			// The samples as they are, in a number of bits that may be 0.
			raw := int(b.u(5))
			for ; i < end; i++ {
				res[i] = int32(b.s(raw))
			}
		} else if !b.rice(res[i:end], uint(k)) {
			return false
		}
		if i = end; b.over {
			return false
		}
	}
	return true
}

// rice reads len(dst) residual samples coded with Rice parameter k, at most
// 30: for each, the high part of its folded value in unary and the low k
// bits (§9.2.7.2). A value that does not fit 32 bits is not valid
// (§9.2.7.3).
func (b *flacBitReader) rice(dst []int32, k uint) bool {
	acc, n, pos, buf := b.acc, b.n, b.pos, b.buf
	most := ^uint32(0) >> k
	for i := range dst {
		var q uint32
		for acc == 0 {
			q += uint32(n)
			n = 0
			if pos >= len(buf) {
				b.fail()
				return false
			}
			for n <= 56 && pos < len(buf) {
				acc |= uint64(buf[pos]) << (56 - n)
				n += 8
				pos++
			}
		}
		z := uint(mathbits.LeadingZeros64(acc))
		q += uint32(z)
		acc = acc << z << 1
		n -= z + 1
		if n < k {
			for n <= 56 && pos < len(buf) {
				acc |= uint64(buf[pos]) << (56 - n)
				n += 8
				pos++
			}
			if n < k {
				b.fail()
				return false
			}
		}
		low := uint32(acc >> (64 - k))
		acc <<= k
		n -= k
		if q > most {
			b.fail()
			return false
		}
		u := q<<k | low
		dst[i] = int32(u>>1) ^ -int32(u&1)
	}
	b.acc, b.n, b.pos = acc, n, pos
	return true
}
