package kernels

// FilterShift is the number of fractional bits in resampling filter weights.
// The weights of each output sample must sum to exactly 1<<FilterShift.
const FilterShift = 14

const filterRound = 1 << (FilterShift - 1)

// ResampleH filters one row: dst[i] = sum_k src[start[i]+k] * w[i*taps+k].
func ResampleH(dst, src []byte, start []int32, w []int16, taps int) {
	for i := range dst {
		s := src[start[i]:]
		s = s[:taps]
		ww := w[i*taps : i*taps+taps]
		var acc int32 = filterRound
		for k, v := range s {
			acc += int32(v) * int32(ww[k])
		}
		dst[i] = clip8(acc >> FilterShift)
	}
}

// ResampleV filters one output row vertically from len(w) consecutive source
// rows starting at row `start`. acc is scratch of at least width entries.
func ResampleV(dst, src []byte, stride, width, start int, w []int16, acc []int32) {
	acc = acc[:width]
	for i := range acc {
		acc[i] = filterRound
	}
	for k, wk := range w {
		if wk == 0 {
			continue
		}
		row := src[(start+k)*stride:]
		row = row[:width]
		c := int32(wk)
		for i, v := range row {
			acc[i] += int32(v) * c
		}
	}
	dst = dst[:width]
	for i := range dst {
		dst[i] = clip8(acc[i] >> FilterShift)
	}
}
