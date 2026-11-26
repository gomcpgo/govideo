package bits

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// TestWriter writes fields of every width, signed and unsigned, with unary
// codes among them, and reads them back with the Reader.
func TestWriter(t *testing.T) {
	type field struct {
		kind byte // 'u', 's' or 'q' (unary)
		v    uint32
		n    int
	}
	rng := rand.New(rand.NewPCG(3, 4))
	fields := []field{{'u', 1, 1}, {'u', 0, 0}, {'u', 0xffffffff, 32}, {'q', 0, 0}, {'q', 31, 0}, {'q', 32, 0}, {'q', 100, 0}, {'u', 5, 3}}
	for range 5000 {
		switch n := rng.IntN(33); rng.IntN(5) {
		case 0:
			fields = append(fields, field{'q', uint32(rng.IntN(70)), 0})
		case 1:
			fields = append(fields, field{'s', rng.Uint32(), max(n, 1)})
		default:
			// More bits than the field has: the excess is not written.
			fields = append(fields, field{'u', rng.Uint32(), n})
		}
	}
	prefix := []byte{0xab, 0xcd}
	var w Writer
	w.Reset(append([]byte(nil), prefix...))
	want := 0
	for _, f := range fields {
		switch f.kind {
		case 'u':
			w.U(f.v, f.n)
			want += f.n
		case 's':
			w.S(int32(f.v), f.n)
			want += f.n
		case 'q':
			w.Unary(f.v)
			want += int(f.v) + 1
		}
		if w.Len(len(prefix)) != want {
			t.Fatalf("%d bits written, want %d", w.Len(len(prefix)), want)
		}
	}
	w.Align()
	out := w.Bytes()
	if !bytes.HasPrefix(out, prefix) || len(out) != len(prefix)+(want+7)/8 {
		t.Fatalf("%d bytes for %d bits after a prefix of %d", len(out), want, len(prefix))
	}
	r := NewReader(out[len(prefix):])
	for i, f := range fields {
		switch f.kind {
		case 'u', 's':
			mask := uint32(uint64(1)<<uint(f.n) - 1)
			if got := r.U(f.n); got != f.v&mask {
				t.Fatalf("field %d: %d bits read back as %#x, want %#x", i, f.n, got, f.v&mask)
			}
		case 'q':
			q := uint32(0)
			for r.U(1) == 0 && r.Err() == nil {
				q++
			}
			if q != f.v {
				t.Fatalf("field %d: unary %d read back as %d", i, f.v, q)
			}
		}
	}
	if r.Err() != nil || r.BitsLeft() >= 8 || r.U(r.BitsLeft()) != 0 {
		t.Errorf("after the fields: %v, %d bits left; want zero padding to the byte", r.Err(), r.BitsLeft())
	}

	// A zero Writer starts a slice of its own, and Reset drops what was
	// not yet a whole byte.
	var z Writer
	z.U(0b101, 3)
	if len(z.Bytes()) != 0 || z.Len(0) != 3 {
		t.Errorf("3 bits: %d bytes, %d bits", len(z.Bytes()), z.Len(0))
	}
	z.Align()
	if !bytes.Equal(z.Bytes(), []byte{0b10100000}) {
		t.Errorf("101 aligned is % x", z.Bytes())
	}
	z.U(1, 1)
	z.Reset(nil)
	if z.Align(); len(z.Bytes()) != 0 {
		t.Errorf("Reset kept %d bytes", len(z.Bytes()))
	}
}
