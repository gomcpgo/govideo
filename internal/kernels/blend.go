package kernels

// BlendShift is the number of fractional bits in a blend weight: a weight
// of 1<<BlendShift is all of the second picture.
const BlendShift = 14

// Blend writes the weighted mean of a and b to dst, len(dst) bytes of each:
// a + (b-a)*w/2^BlendShift, rounded to nearest, for a weight w from 0 (a)
// to 1<<BlendShift (b). The result never leaves the range a to b, so two
// equal samples stay as they are whatever the weight. dst may be a or b.
func Blend(dst, a, b []byte, w int32) {
	n := len(dst)
	a, b = a[:n], b[:n]
	const round = 1 << (BlendShift - 1)
	for i := range dst {
		av := int32(a[i])
		dst[i] = byte(av + ((int32(b[i])-av)*w+round)>>BlendShift)
	}
}

// BlendTo is Blend with a constant in place of b: it moves every sample of
// a towards the value c.
func BlendTo(dst, a []byte, c byte, w int32) {
	a = a[:len(dst)]
	const round = 1 << (BlendShift - 1)
	cv := int32(c)
	for i := range dst {
		av := int32(a[i])
		dst[i] = byte(av + ((cv-av)*w+round)>>BlendShift)
	}
}
