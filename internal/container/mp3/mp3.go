// Package mp3 reads bare MPEG audio files (.mp3, and layers I and II): the
// format of the stream, how many frames it holds, and the frames themselves.
// It decodes no audio.
//
// The frame count comes from the Xing/Info or VBRI header that most encoders
// put in the first frame. A file without one is measured by reading the
// header of every frame, since nothing else gives the length of a
// variable-rate stream; frame headers are four bytes, but the file is read
// from end to end.
package mp3

import (
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/gomcpgo/govideo/internal/codechdr"
)

// ErrNotMP3 reports that no MPEG audio frames were found where the file
// should start.
var ErrNotMP3 = errors.New("mp3: not an MPEG audio file")

// File describes an MPEG audio file.
type File struct {
	Codec        string // "mp3", "mp2" or "mp1"
	SampleRate   int
	Channels     int
	FrameSamples int // sample frames in each frame
	// Frames is the number of audio frames. The frame that carries a Xing,
	// Info or VBRI header holds no audio and is not counted.
	Frames int64
	// Counted reports that Frames was counted from the frame headers, the
	// file having no header that states it.
	Counted bool
	// Delay and Padding are the samples the encoder put before and after
	// the audio, from the LAME extension of the Xing/Info header, and Stated
	// reports that the header has that extension. Both are 0 in a file
	// without it, and may be in one with it: ffmpeg writes zeros around a
	// stream it copies.
	Delay, Padding int
	Stated         bool
	// BitRate is that of the first audio frame; VBR reports that the file
	// says its rate varies (a Xing or VBRI header) or that frames of
	// another rate were counted.
	BitRate    int
	VBR        bool
	DataOffset int64 // the first audio frame
	DataSize   int64 // bytes from there to the end of the audio

	layer int
}

// DecoderDelay is the delay of a layer III decoder, in samples: its output
// starts this much before the sound the encoder was given, after whatever
// delay the encoder has of its own.
const DecoderDelay = 529

// Lead returns how many samples a decoder gives before the audio starts: the
// encoder's delay and the decoder's, in a file that states the first. A file
// that states nothing is played as it decodes, delays and all, and Lead is 0.
// That is what ffmpeg does (measured: it goes by the extension when the
// encoder named in it is LAME, Lavc or Lavf, whatever the delay it states).
func (f *File) Lead() int {
	if !f.Stated || f.layer != 3 {
		return 0
	}
	return f.Delay + DecoderDelay
}

// Samples returns the length of the audio in sample frames: what the frames
// decode to, without Lead at the start and, at the end, without the padding
// the file states, less the decoder's delay, which the padding has to cover
// before any of it is left over.
func (f *File) Samples() int64 {
	n := f.Frames*int64(f.FrameSamples) - int64(f.Lead())
	if f.Lead() > 0 {
		n -= int64(max(f.Padding-DecoderDelay, 0))
	}
	return max(n, 0)
}

const (
	maxTags   = 8       // ID3v2 tags in a row at the start of a file
	maxJunk   = 1 << 16 // bytes searched for a frame: at the start, and after each break in the stream
	maxBreaks = 16      // breaks in the stream that a count resumes after
	minFrame  = 24      // the smallest frame: 8 kbit/s at 24 kHz, layer III
	windowLen = 1 << 16
)

// Sniff reports whether head looks like the start of an MPEG audio file: an
// ID3v2 tag or a frame header. Open decides.
func Sniff(head []byte) bool {
	if len(head) >= 3 && string(head[:3]) == "ID3" {
		return true
	}
	h, ok := codechdr.MPEGAudioFrameHeader(head)
	return ok && h.Size > 0
}

// SkipTags returns the offset of the first byte after the ID3v2 tags at the
// start of a file.
func SkipTags(r io.ReaderAt, size int64) int64 {
	var off int64
	for i := 0; i < maxTags; i++ {
		var h [10]byte
		if off+10 > size {
			break
		}
		if _, err := r.ReadAt(h[:], off); err != nil || string(h[:3]) != "ID3" || h[6]|h[7]|h[8]|h[9] >= 0x80 {
			break
		}
		// The size is 28 bits, seven to a byte; a footer repeats the header.
		n := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
		n += 10
		if h[5]&0x10 != 0 {
			n += 10
		}
		if off+n > size {
			break
		}
		off += n
	}
	return off
}

// window reads a file through a buffer that follows the reads.
type window struct {
	r    io.ReaderAt
	size int64
	off  int64
	buf  []byte
	b    [windowLen]byte
}

// at returns the n bytes at pos, valid until the next call, or nil where the
// file ends before them.
func (w *window) at(pos int64, n int) []byte {
	if pos < 0 || n > windowLen || pos+int64(n) > w.size {
		return nil
	}
	if pos < w.off || pos+int64(n) > w.off+int64(len(w.buf)) {
		want := min(int64(windowLen), w.size-pos)
		got, err := w.r.ReadAt(w.b[:want], pos)
		if err != nil && err != io.EOF {
			got = 0
		}
		w.off, w.buf = pos, w.b[:got]
		if got < n {
			return nil
		}
	}
	return w.buf[pos-w.off : pos-w.off+int64(n)]
}

// stream is the format every frame of a file shares.
type stream struct {
	layer, rate int
}

// frame returns the header of the frame at pos if it belongs to the stream
// and lies within the file up to end.
func (w *window) frame(pos, end int64, s stream) (codechdr.MPEGAudioFrame, bool) {
	h, ok := codechdr.MPEGAudioFrameHeader(w.at(pos, 4))
	if !ok || h.Size == 0 || pos+int64(h.Size) > end {
		return h, false
	}
	if s.layer != 0 && (h.Layer != s.layer || h.SampleRate != s.rate) {
		return h, false
	}
	return h, true
}

// find returns the position of the first frame at or after pos, looking no
// further than maxJunk bytes. A frame is taken for one when another of the
// same stream follows it, or when it ends where the audio does: the twelve
// bits of a sync word turn up in other data too often to go by alone.
func (w *window) find(pos, end int64, s stream) (int64, codechdr.MPEGAudioFrame, bool) {
	for limit := min(pos+maxJunk, end-4); pos <= limit; pos++ {
		b := w.at(pos, 2)
		if b == nil {
			break
		}
		if b[0] != 0xff || b[1]&0xe0 != 0xe0 {
			continue
		}
		h, ok := w.frame(pos, end, s)
		if !ok {
			continue
		}
		next := pos + int64(h.Size)
		if _, ok := w.frame(next, end, stream{h.Layer, h.SampleRate}); ok || next == end {
			return pos, h, true
		}
	}
	return 0, codechdr.MPEGAudioFrame{}, false
}

// Open reads the layout of an MPEG audio file.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	w := &window{r: r, size: size}
	end := size
	// An ID3v1 tag is the last 128 bytes of the file.
	if t := w.at(size-128, 3); t != nil && string(t) == "TAG" {
		end = size - 128
	}
	start := SkipTags(r, size)
	pos, h, ok := w.find(start, end, stream{})
	if !ok {
		return nil, ErrNotMP3
	}
	f := &File{Codec: [...]string{"mp1", "mp2", "mp3"}[h.Layer-1], SampleRate: h.SampleRate,
		Channels: 2, FrameSamples: h.Samples, BitRate: h.BitRate, layer: h.Layer}
	if h.Mono {
		f.Channels = 1
	}
	st := stream{h.Layer, h.SampleRate}

	// A header in the first frame gives the number of frames that follow.
	if info, ok := parseInfo(w.at(pos, h.Size), h); ok {
		pos += int64(h.Size)
		f.VBR, f.Delay, f.Padding, f.Stated = info.vbr, info.delay, info.padding, info.stated
		if first, ok := w.frame(pos, end, st); ok {
			f.BitRate = first.BitRate
		}
		// A count that the bytes could not hold is not believed.
		if info.frames > 0 && info.frames <= (end-pos)/minFrame+1 {
			f.Frames = info.frames
		}
	}
	f.DataOffset, f.DataSize = pos, end-pos
	if f.Frames > 0 {
		return f, nil
	}

	// No count: read every frame header.
	f.Counted = true
	k := walker{w: w, pos: pos, end: end, st: st}
	for n := 0; ; n++ {
		if n&0x3ff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		_, fh, ok := k.next()
		if !ok {
			break
		}
		if fh.BitRate != f.BitRate {
			f.VBR = true
		}
		f.Frames++
	}
	if f.Frames == 0 {
		return nil, ErrNotMP3
	}
	return f, nil
}

// walker steps through the frames of a stream.
type walker struct {
	w        *window
	pos, end int64
	st       stream
	breaks   int
}

// next returns where the next frame is and its header; ok is false at the
// end of the audio.
func (k *walker) next() (pos int64, h codechdr.MPEGAudioFrame, ok bool) {
	for k.pos+4 <= k.end {
		if h, ok = k.w.frame(k.pos, k.end, k.st); ok {
			pos = k.pos
			k.pos += int64(h.Size)
			return pos, h, true
		}
		// Not a frame: a tag (APE, lyrics) or damage. Go on from the next
		// frame, if one is near.
		if k.breaks++; k.breaks > maxBreaks {
			break
		}
		at, _, found := k.w.find(k.pos+1, k.end, k.st)
		if !found {
			break
		}
		k.pos = at
	}
	k.pos = k.end
	return 0, h, false
}

// Reader reads the audio frames of a file in order: the frames that Open
// counts where the file does not state their number.
type Reader struct {
	k walker
}

// NewReader returns a reader placed at the first audio frame of f. r is the
// file that Open read.
func (f *File) NewReader(r io.ReaderAt) *Reader {
	end := f.DataOffset + f.DataSize
	return &Reader{k: walker{w: &window{r: r, size: end}, pos: f.DataOffset, end: end,
		st: stream{f.layer, f.SampleRate}}}
}

// Next returns the next frame, header included. The bytes are valid until
// the next call. The error is io.EOF after the last frame.
func (r *Reader) Next() ([]byte, error) {
	pos, h, ok := r.k.next()
	if !ok {
		return nil, io.EOF
	}
	b := r.k.w.at(pos, h.Size)
	if b == nil {
		return nil, io.EOF
	}
	return b, nil
}

// Skip passes over up to n frames, reading their headers only, and returns
// how many it passed.
func (r *Reader) Skip(n int64) (passed int64) {
	for passed < n {
		if _, _, ok := r.k.next(); !ok {
			break
		}
		passed++
	}
	return passed
}

// info is the content of a Xing, Info or VBRI header.
type info struct {
	frames         int64
	vbr            bool
	delay, padding int
	stated         bool // the LAME extension is there
}

// parseInfo reads the header that an encoder may put in the first frame, a
// layer III frame that decodes to silence: "Xing" (variable rate) or "Info"
// (constant rate) after the frame's side information, with LAME's extension
// behind it, or Fraunhofer's "VBRI" at a fixed place.
func parseInfo(fr []byte, h codechdr.MPEGAudioFrame) (in info, ok bool) {
	if h.Layer != 3 || len(fr) < 4 {
		return in, false
	}
	be := binary.BigEndian
	if len(fr) >= 36+18 && string(fr[36:40]) == "VBRI" {
		return info{frames: int64(be.Uint32(fr[36+14:])), vbr: true}, true
	}
	// Side information: 32 bytes for MPEG-1 stereo, 17 for MPEG-1 mono and
	// for the low rates in stereo, 9 for the low rates in mono.
	p := 4 + 32
	switch {
	case h.LowRate && h.Mono:
		p = 4 + 9
	case h.LowRate || h.Mono:
		p = 4 + 17
	}
	if h.Protected {
		p += 2
	}
	if len(fr) < p+8 {
		return in, false
	}
	switch string(fr[p : p+4]) {
	case "Xing":
		in.vbr = true
	case "Info":
	default:
		return in, false
	}
	flags := be.Uint32(fr[p+4:])
	p += 8
	if flags&1 != 0 { // frames
		if len(fr) < p+4 {
			return in, true
		}
		in.frames = int64(be.Uint32(fr[p:]))
		p += 4
	}
	if flags&2 != 0 { // bytes
		p += 4
	}
	if flags&4 != 0 { // seek table
		p += 100
	}
	if flags&8 != 0 { // quality
		p += 4
	}
	// LAME's extension: a 9-byte encoder name, then 12 bytes of settings,
	// then the delay and the padding in 12 bits each.
	if len(fr) >= p+24 {
		switch string(fr[p : p+4]) {
		case "LAME", "Lavf", "Lavc":
			d := fr[p+21 : p+24]
			in.delay = int(d[0])<<4 | int(d[1])>>4
			in.padding = int(d[1]&15)<<8 | int(d[2])
			in.stated = true
		}
	}
	return in, true
}
