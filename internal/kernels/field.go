package kernels

// FieldMidRow fills dst with the row that lies midway between rows b and c
// of a field, a and d being the field rows beyond them. It is the
// half-sample point of a Catmull-Rom cubic, (-a + 9b + 9c - d) / 16: the
// row a deinterlacer puts in place of the other field's.
func FieldMidRow(dst, a, b, c, d []byte) {
	a, b, c, d = a[:len(dst)], b[:len(dst)], c[:len(dst)], d[:len(dst)]
	for i := range dst {
		dst[i] = clip8((9*(int32(b[i])+int32(c[i])) - int32(a[i]) - int32(d[i]) + 8) >> 4)
	}
}
