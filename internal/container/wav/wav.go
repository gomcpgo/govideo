// Package wav locates the PCM payload of RIFF/WAVE files, and writes the
// header of one.
package wav

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// File describes a WAVE file's format and data chunk.
type File struct {
	Float      bool
	Channels   int
	SampleRate int
	Bits       int
	BlockAlign int
	DataOffset int64
	DataSize   int64
}

// ErrNotWAV reports that the input is not a supported RIFF/WAVE file.
var ErrNotWAV = errors.New("wav: not a PCM RIFF/WAVE file")

// Sniff reports whether head looks like a RIFF/WAVE file.
func Sniff(head []byte) bool {
	return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE"
}

// Frames returns the number of sample frames in the data chunk.
func (f *File) Frames() int64 {
	if f.BlockAlign == 0 {
		return 0
	}
	return f.DataSize / int64(f.BlockAlign)
}

// Open parses the chunk list of a WAVE file.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	var hdr [12]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil || !Sniff(hdr[:]) {
		return nil, ErrNotWAV
	}
	f := &File{}
	le := binary.LittleEndian
	haveFmt := false
	off := int64(12)
	for i := 0; off+8 <= size && i < 4096; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var ch [8]byte
		if _, err := r.ReadAt(ch[:], off); err != nil {
			break
		}
		n := int64(le.Uint32(ch[4:]))
		body := off + 8
		switch string(ch[:4]) {
		case "fmt ":
			if n < 16 || n > 4096 || body+n > size {
				return nil, ErrNotWAV
			}
			b := make([]byte, n)
			if _, err := r.ReadAt(b, body); err != nil {
				return nil, err
			}
			tag := le.Uint16(b)
			f.Channels = int(le.Uint16(b[2:]))
			f.SampleRate = int(le.Uint32(b[4:]))
			f.BlockAlign = int(le.Uint16(b[12:]))
			f.Bits = int(le.Uint16(b[14:]))
			if tag == 0xfffe && n >= 40 {
				tag = le.Uint16(b[24:]) // first two bytes of the SubFormat GUID
			}
			switch tag {
			case 1:
			case 3:
				f.Float = true
			default:
				return nil, ErrNotWAV
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, ErrNotWAV
			}
			if n == 0xffffffff || body+n > size {
				n = size - body // streamed or truncated file
			}
			f.DataOffset, f.DataSize = body, n
			ok := f.Channels >= 1 && f.Channels <= 64 && f.SampleRate > 0 && f.BlockAlign == f.Channels*((f.Bits+7)/8)
			switch {
			case !ok:
				return nil, ErrNotWAV
			case f.Float && f.Bits != 32 && f.Bits != 64:
				return nil, ErrNotWAV
			case !f.Float && f.Bits != 8 && f.Bits != 16 && f.Bits != 24 && f.Bits != 32:
				return nil, ErrNotWAV
			}
			f.DataSize -= f.DataSize % int64(f.BlockAlign)
			return f, nil
		}
		off = body + n + n&1
	}
	return nil, ErrNotWAV
}

// HeaderSize is the length of the header PCMHeader returns; the samples
// follow it.
const HeaderSize = 44

// MaxDataSize is the most sample data, in bytes, a RIFF/WAVE file holds: its
// sizes are 32-bit, and the first of them counts the rest of the header too.
const MaxDataSize = 0xffffffff - (HeaderSize - 8)

// PCMHeader returns the header of a WAVE file holding frames sample frames
// of integer PCM, little-endian, bits wide (8, 16, 24 or 32): the canonical
// form, a 'fmt ' chunk of 16 bytes followed at once by the 'data' chunk,
// which is what the simplest readers expect to find.
func PCMHeader(channels, sampleRate, bits int, frames int64) ([]byte, error) {
	if channels < 1 || channels > 0xffff || sampleRate < 1 || int64(sampleRate) > 0xffffffff {
		return nil, fmt.Errorf("wav: cannot write %d channels at %d Hz", channels, sampleRate)
	}
	if bits != 8 && bits != 16 && bits != 24 && bits != 32 {
		return nil, fmt.Errorf("wav: cannot write %d-bit samples", bits)
	}
	align := int64(channels * bits / 8)
	if align > 0xffff || int64(sampleRate)*align > 0xffffffff {
		return nil, fmt.Errorf("wav: cannot write %d channels at %d Hz", channels, sampleRate)
	}
	if frames < 0 || frames > MaxDataSize/align {
		return nil, fmt.Errorf("wav: %d sample frames do not fit in a WAVE file (4 GB)", frames)
	}
	size := frames * align
	le := binary.LittleEndian
	h := make([]byte, 0, HeaderSize)
	h = append(h, "RIFF"...)
	h = le.AppendUint32(h, uint32(size+HeaderSize-8))
	h = append(h, "WAVEfmt "...)
	h = le.AppendUint32(h, 16)
	h = le.AppendUint16(h, 1) // WAVE_FORMAT_PCM
	h = le.AppendUint16(h, uint16(channels))
	h = le.AppendUint32(h, uint32(sampleRate))
	h = le.AppendUint32(h, uint32(int64(sampleRate)*align)) // bytes per second
	h = le.AppendUint16(h, uint16(align))
	h = le.AppendUint16(h, uint16(bits))
	h = append(h, "data"...)
	h = le.AppendUint32(h, uint32(size))
	return h, nil
}
