package bits

// Writer writes bits MSB first, appending whole bytes to a slice as they
// fill. The zero value writes to a new slice; Reset makes it append to one
// the caller has.
type Writer struct {
	buf []byte
	acc uint64 // the low n bits are those not yet in buf
	n   uint   // 0..7 between calls
}

// Reset starts the writer afresh, appending to buf. Bits not yet aligned to
// a byte are dropped.
func (w *Writer) Reset(buf []byte) { *w = Writer{buf: buf} }

// U writes the low n bits (0..32) of v.
func (w *Writer) U(v uint32, n int) {
	if n <= 0 {
		return
	}
	w.acc = w.acc<<uint(n) | uint64(v)&(1<<uint(n)-1)
	w.n += uint(n)
	for w.n >= 8 {
		w.n -= 8
		w.buf = append(w.buf, byte(w.acc>>w.n))
	}
}

// S writes v in n bits (1..32) of two's complement.
func (w *Writer) S(v int32, n int) { w.U(uint32(v), n) }

// Unary writes q zero bits and then a one: the unary code of FLAC
// (RFC 9639 §5).
func (w *Writer) Unary(q uint32) {
	for ; q >= 32; q -= 32 {
		w.U(0, 32)
	}
	w.U(1, int(q)+1)
}

// Align pads with zero bits to the next byte boundary.
func (w *Writer) Align() {
	if w.n != 0 {
		w.U(0, int(8-w.n))
	}
}

// Len is the number of bits written after the first from bytes of the
// slice: pass the length the slice had at Reset for the bits written since.
func (w *Writer) Len(from int) int { return (len(w.buf)-from)*8 + int(w.n) }

// Bytes returns the slice with the whole bytes written so far appended.
// Bits after the last byte boundary are not in it: call Align first.
func (w *Writer) Bytes() []byte { return w.buf }
