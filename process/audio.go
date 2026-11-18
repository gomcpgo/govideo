package process

import (
	"math"

	"github.com/gomcpgo/govideo/internal/kernels"
)

// ToStereo maps interleaved audio with the given channel count to
// interleaved stereo. Mono is duplicated; stereo is copied. Multichannel
// input is assumed to be in the usual order (L R C LFE Ls Rs [more
// surround pairs]) and is downmixed with the centre and surrounds at -3 dB,
// the LFE dropped, and the result normalised so it cannot clip. The result
// is always a new slice.
func ToStereo(in []float32, channels int) []float32 {
	if channels < 1 {
		channels = 1
	}
	frames := len(in) / channels
	out := make([]float32, frames*2)
	switch channels {
	case 1:
		for i := 0; i < frames; i++ {
			out[i*2], out[i*2+1] = in[i], in[i]
		}
		return out
	case 2:
		copy(out, in[:frames*2])
		return out
	}
	const a = float32(math.Sqrt2 / 2)
	// Per-input-channel gains into left and right.
	gl := make([]float32, channels)
	gr := make([]float32, channels)
	gl[0], gr[1] = 1, 1
	switch {
	case channels == 3: // L R C
		gl[2], gr[2] = a, a
	case channels == 4: // L R Ls Rs
		gl[2], gr[3] = a, a
	case channels == 5: // L R C Ls Rs
		gl[2], gr[2] = a, a
		gl[3], gr[4] = a, a
	default: // L R C LFE then alternating left/right surrounds
		gl[2], gr[2] = a, a
		for c := 4; c < channels; c++ {
			if c&1 == 0 {
				gl[c] = a
			} else {
				gr[c] = a
			}
		}
	}
	var sl, sr float32
	for c := 0; c < channels; c++ {
		sl += gl[c]
		sr += gr[c]
	}
	norm := 1 / float32(math.Max(float64(sl), float64(sr)))
	for c := range gl {
		gl[c] *= norm
		gr[c] *= norm
	}
	for i := 0; i < frames; i++ {
		fr := in[i*channels : (i+1)*channels]
		var l, r float32
		for c, v := range fr {
			l += v * gl[c]
			r += v * gr[c]
		}
		out[i*2], out[i*2+1] = l, r
	}
	return out
}

// ApplyGain multiplies every sample by gain.
func ApplyGain(s []float32, gain float32) {
	if gain == 1 {
		return
	}
	kernels.Gain(s, gain)
}

// ApplyFade applies linear fade-in and fade-out envelopes to a block of a
// longer track. startFrame is the absolute frame index of s[0] within a
// track of total frames; fadeIn and fadeOut are lengths in frames (0 = no
// fade). Blocks can be processed in any chunking with identical results.
func ApplyFade(s []float32, channels int, startFrame int64, fadeIn, fadeOut, total int64) {
	if channels < 1 || (fadeIn <= 0 && fadeOut <= 0) {
		return
	}
	frames := len(s) / channels
	for i := 0; i < frames; i++ {
		pos := startFrame + int64(i)
		g := float32(1)
		if fadeIn > 0 && pos < fadeIn {
			if pos < 0 {
				g = 0
			} else {
				g = float32(pos) / float32(fadeIn)
			}
		}
		if fadeOut > 0 {
			if rem := total - pos; rem < fadeOut {
				if rem <= 0 {
					g = 0
				} else {
					g *= float32(rem) / float32(fadeOut)
				}
			}
		}
		if g == 1 {
			continue
		}
		fr := s[i*channels : (i+1)*channels]
		for c := range fr {
			fr[c] *= g
		}
	}
}

// MixInto adds src into dst over the shorter of the two lengths.
func MixInto(dst, src []float32) { kernels.MixAdd(dst, src) }

// SoftClip is the mixer's clipping protection: samples below about ±0.9
// pass unchanged, louder ones are smoothly limited to ±1, NaN becomes 0.
func SoftClip(s []float32) { kernels.SoftClip(s) }
