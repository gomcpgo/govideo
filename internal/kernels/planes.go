package kernels

import "encoding/binary"

// SplitPairs writes the even bytes of src to a and the odd bytes to b:
// len(a) bytes each, from 2*len(a) bytes of src. It takes apart the
// interleaved chroma of a bi-planar picture (NV12), which is how the
// operating-system decoders deliver it.
//
// Sixteen source bytes are handled at a time as two 64-bit words, each
// squeezed to its even or odd bytes by shifts and masks: nearly twice the
// speed of a byte loop, with the same result on any byte order. A vector
// unzip would be several times faster again (SPEC §9.2).
func SplitPairs(a, b, src []byte) {
	n := len(a)
	b, src = b[:n], src[:2*n]
	const (
		m8  = 0x00ff00ff00ff00ff
		m16 = 0x0000ffff0000ffff
		m32 = 0x00000000ffffffff
	)
	// squeeze gathers bytes 0, 2, 4 and 6 of x in its low half.
	squeeze := func(x uint64) uint64 {
		x &= m8
		x = (x | x>>8) & m16
		return (x | x>>16) & m32
	}
	i := 0
	for ; i+8 <= n; i += 8 {
		s := src[2*i : 2*i+16 : 2*i+16]
		lo, hi := binary.LittleEndian.Uint64(s), binary.LittleEndian.Uint64(s[8:])
		binary.LittleEndian.PutUint64(a[i:i+8:i+8], squeeze(lo)|squeeze(hi)<<32)
		binary.LittleEndian.PutUint64(b[i:i+8:i+8], squeeze(lo>>8)|squeeze(hi>>8)<<32)
	}
	for ; i < n; i++ {
		a[i], b[i] = src[2*i], src[2*i+1]
	}
}

// SplitPairs16 is SplitPairs for 16-bit samples: the even samples of src go
// to a and the odd ones to b. It takes apart the chroma of a bi-planar
// picture of more than 8 bits (P010).
func SplitPairs16(a, b, src []uint16) {
	n := len(a)
	b, src = b[:n], src[:2*n]
	for i := range a {
		a[i], b[i] = src[2*i], src[2*i+1]
	}
}
