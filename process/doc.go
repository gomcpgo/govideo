// Package process implements the pixel and sample processing stages of the
// pipeline (SPEC §7): image import and orientation, colour conversion to the
// BT.709 limited-range working format, tone mapping of high dynamic range
// pictures to it, scaling and fitting to the output canvas, and audio
// resampling, channel mapping, gain, fades and mixing.
//
// All hot loops live in internal/kernels so SIMD variants can be added
// without touching this package.
package process
