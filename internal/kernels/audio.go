package kernels

import "math"

// Gain multiplies every sample by g.
func Gain(s []float32, g float32) {
	for i := range s {
		s[i] *= g
	}
}

// MixAdd adds src into dst over the shorter of the two lengths.
func MixAdd(dst, src []float32) {
	if len(src) < len(dst) {
		dst = dst[:len(src)]
	}
	for i := range dst {
		dst[i] += src[i]
	}
}

// SoftClipThreshold is the level below which SoftClip leaves samples
// untouched.
const SoftClipThreshold = 0.9

// SoftClip limits samples to [-1, 1]: transparent below the threshold, a
// smooth tanh knee above it. NaN becomes 0.
func SoftClip(s []float32) {
	const t = SoftClipThreshold
	for i, v := range s {
		if v >= -t && v <= t {
			continue
		}
		if v != v { // NaN
			s[i] = 0
			continue
		}
		a := float64(v)
		neg := a < 0
		if neg {
			a = -a
		}
		a = t + (1-t)*math.Tanh((a-t)/(1-t))
		if a > 1 {
			a = 1
		}
		if neg {
			a = -a
		}
		s[i] = float32(a)
	}
}

// DotInterleaved returns sum_k w[k] * x[k*channels+ch].
func DotInterleaved(x []float32, w []float32, channels, ch int) float32 {
	var acc float32
	x = x[ch:]
	for k, wk := range w {
		acc += wk * x[k*channels]
	}
	return acc
}
