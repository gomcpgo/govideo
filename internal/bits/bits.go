// Package bits provides a bounds-checked MSB-first bit reader, used by the
// codec configuration header parsers, and a bit writer, used by the FLAC
// encoder.
package bits

import "errors"

// ErrShort is reported when a read runs past the end of the data.
var ErrShort = errors.New("bits: read past end of data")

// Reader reads bits MSB first. Errors are sticky: after a read past the end
// every read returns zero and Err reports ErrShort.
type Reader struct {
	data []byte
	pos  int // bit position
	err  error
}

// NewReader returns a Reader over b. The slice is not copied.
func NewReader(b []byte) *Reader { return &Reader{data: b} }

// Err returns the first error hit by the reader, if any.
func (r *Reader) Err() error { return r.err }

// Pos returns the current bit position.
func (r *Reader) Pos() int { return r.pos }

// BitsLeft returns the number of unread bits.
func (r *Reader) BitsLeft() int {
	if r.err != nil {
		return 0
	}
	return len(r.data)*8 - r.pos
}

// U reads n bits (0..32) as an unsigned value.
func (r *Reader) U(n int) uint32 { return uint32(r.U64(n)) }

// U64 reads n bits (0..64) as an unsigned value.
func (r *Reader) U64(n int) uint64 {
	if r.err != nil || n <= 0 {
		return 0
	}
	if n > 64 || n > len(r.data)*8-r.pos {
		r.err = ErrShort
		return 0
	}
	var v uint64
	for n > 0 {
		b := r.data[r.pos>>3]
		avail := 8 - r.pos&7
		take := avail
		if take > n {
			take = n
		}
		v = v<<uint(take) | uint64(b>>(uint(avail-take))&(1<<uint(take)-1))
		r.pos += take
		n -= take
	}
	return v
}

// Flag reads one bit as a bool.
func (r *Reader) Flag() bool { return r.U(1) == 1 }

// Skip advances n bits.
func (r *Reader) Skip(n int) {
	if r.err != nil || n <= 0 {
		return
	}
	if n > len(r.data)*8-r.pos {
		r.err = ErrShort
		return
	}
	r.pos += n
}

// AlignByte advances to the next byte boundary.
func (r *Reader) AlignByte() {
	if m := r.pos & 7; m != 0 {
		r.Skip(8 - m)
	}
}

// UE reads an unsigned Exp-Golomb code (ue(v)).
func (r *Reader) UE() uint32 {
	zeros := 0
	for r.err == nil && r.U(1) == 0 {
		zeros++
		if zeros > 32 {
			r.err = ErrShort
			return 0
		}
	}
	if r.err != nil {
		return 0
	}
	if zeros == 0 {
		return 0
	}
	if zeros == 32 {
		r.U(32)
		return 0xFFFFFFFF
	}
	return 1<<uint(zeros) - 1 + r.U(zeros)
}

// SE reads a signed Exp-Golomb code (se(v)).
func (r *Reader) SE() int32 {
	k := r.UE()
	if k&1 == 1 {
		return int32(k/2 + 1)
	}
	return -int32(k / 2)
}

// RemoveEmulationPrevention strips the 0x03 emulation prevention bytes from
// an H.264/HEVC NAL unit payload, returning the RBSP.
func RemoveEmulationPrevention(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}
