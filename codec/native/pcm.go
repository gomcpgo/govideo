// Package native holds the pure-Go implementations of royalty-free and
// patent-expired codecs (SPEC §4.1).
package native

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/gomcpgo/govideo/codec"
)

// PCMDecoder converts linear PCM bytes to float32 samples.
type PCMDecoder struct {
	kind     byte // 's', 'u' or 'f'
	bits     int
	little   bool
	channels int
	rate     int
}

// NewPCMDecoder builds a decoder from an ffprobe-style codec name such as
// "pcm_s16le", "pcm_s24be", "pcm_f32le" or "pcm_u8".
func NewPCMDecoder(name string, channels, rate int) (*PCMDecoder, error) {
	d := &PCMDecoder{channels: channels, rate: rate}
	spec, ok := strings.CutPrefix(name, "pcm_")
	if !ok || len(spec) < 2 || channels < 1 || rate < 1 {
		return nil, fmt.Errorf("native: unsupported PCM format %q", name)
	}
	d.kind = spec[0]
	spec = spec[1:]
	switch {
	case strings.HasSuffix(spec, "le"):
		d.little, spec = true, spec[:len(spec)-2]
	case strings.HasSuffix(spec, "be"):
		spec = spec[:len(spec)-2]
	}
	fmt.Sscanf(spec, "%d", &d.bits)
	ok = false
	switch d.kind {
	case 's':
		ok = d.bits == 8 || d.bits == 16 || d.bits == 24 || d.bits == 32
	case 'u':
		ok = d.bits == 8
	case 'f':
		ok = d.bits == 32 || d.bits == 64
	}
	if !ok {
		return nil, fmt.Errorf("native: unsupported PCM format %q", name)
	}
	return d, nil
}

// FrameSize returns the size in bytes of one sample frame.
func (d *PCMDecoder) FrameSize() int { return d.channels * d.bits / 8 }

// Decode converts a block of PCM. Trailing bytes that do not form a whole
// sample frame are ignored.
func (d *PCMDecoder) Decode(p []byte) (*codec.AudioFrame, error) {
	bps := d.bits / 8
	n := len(p) / (bps * d.channels) * d.channels
	out := make([]float32, n)
	var bo binary.ByteOrder = binary.BigEndian
	if d.little {
		bo = binary.LittleEndian
	}
	for i := range out {
		b := p[i*bps:]
		switch {
		case d.kind == 'u':
			out[i] = (float32(b[0]) - 128) / 128
		case d.kind == 'f' && d.bits == 32:
			out[i] = math.Float32frombits(bo.Uint32(b))
		case d.kind == 'f':
			out[i] = float32(math.Float64frombits(bo.Uint64(b)))
		case d.bits == 8:
			out[i] = float32(int8(b[0])) / 128
		case d.bits == 16:
			out[i] = float32(int16(bo.Uint16(b))) / 32768
		case d.bits == 24:
			var v int32
			if d.little {
				v = int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
			} else {
				v = int32(int8(b[0]))<<16 | int32(b[1])<<8 | int32(b[2])
			}
			out[i] = float32(v) / 8388608
		default:
			out[i] = float32(int32(bo.Uint32(b))) / 2147483648
		}
	}
	return &codec.AudioFrame{SampleRate: d.rate, Channels: d.channels, Samples: out}, nil
}

// AppendS16LE appends samples to dst as signed 16-bit little-endian PCM,
// rounding to nearest and clamping to the valid range.
func AppendS16LE(dst []byte, samples []float32) []byte {
	for _, s := range samples {
		v := float64(s) * 32767
		switch {
		case v != v:
			v = 0
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		dst = binary.LittleEndian.AppendUint16(dst, uint16(int16(math.RoundToEven(v))))
	}
	return dst
}
