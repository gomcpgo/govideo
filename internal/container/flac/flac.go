// Package flac reads bare FLAC files (.flac): the format of the stream, its
// length and tags, and the frames themselves. It decodes no audio.
//
// A FLAC frame does not state its size. Where one ends is found by its
// checksum: a frame ends before the next frame header that the two bytes in
// front of it, taken as the checksum of everything from the frame's start,
// agree with. So the frames of a file are read without decoding them, and a
// frame can be found from any place in the file, which is how a reader gets
// to a frame by its number without a seek table (RFC 9639 §9, §11).
package flac

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/mp3"
)

// ErrNotFLAC reports a file that does not start as a FLAC file does: with
// the "fLaC" marker and a STREAMINFO block, after any ID3v2 tags.
var ErrNotFLAC = errors.New("flac: not a FLAC file")

// File describes a FLAC file.
type File struct {
	SampleRate, Channels, BitDepth int
	// BlockSize is the number of sample frames in every frame but the last,
	// in a stream whose blocks are all of one size: its frames say so and
	// its header states one size. It is 0 for a stream of blocks of varying
	// size, and for a file with no frames.
	BlockSize int
	// Samples is the length of the audio in sample frames. Measured reports
	// that the file does not state it and it was taken from the last frame.
	Samples  int64
	Measured bool
	// StreamInfo is the STREAMINFO block, 34 bytes: what a decoder is
	// configured with.
	StreamInfo []byte
	// Tags are the fields of the Vorbis comment block, their names in lower
	// case. Fields of one name are joined with a semicolon.
	Tags       map[string]string
	DataOffset int64 // the first frame
	DataSize   int64 // bytes from there to the end of the audio

	variable   bool  // the blocking strategy of the frames
	firstBlock int   // the sample frames of the first frame
	start      int64 // the number its header gives the first frame's first sample
}

const (
	streamInfoSize = 34
	maxBlocks      = 4096    // metadata blocks
	maxComments    = 1 << 20 // bytes of the Vorbis comment block that are read
	maxFields      = 1024    // fields taken from it
	maxValue       = 1 << 12 // bytes of a field's value that are kept
	maxJunk        = 1 << 16 // bytes searched for a frame where one should start
	maxBreaks      = 16      // frames with no checksum to end them, before frames are taken as they come
	maxTries       = 8       // frame headers tried in a search for a frame from the middle of the file
	readAhead      = 1 << 18
)

// Sniff reports whether head is the start of a FLAC file.
func Sniff(head []byte) bool { return len(head) >= 4 && string(head[:4]) == "fLaC" }

// Open reads the layout of a FLAC file: the metadata blocks and where the
// frames lie. ID3v2 tags before the marker, which some programs write, are
// stepped over, and an ID3v1 tag at the end is left out of the audio.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	pos := mp3.SkipTags(r, size)
	var head [8 + streamInfoSize]byte
	if pos+int64(len(head)) > size {
		return nil, ErrNotFLAC
	}
	if _, err := r.ReadAt(head[:], pos); err != nil || !Sniff(head[:]) {
		return nil, ErrNotFLAC
	}
	// The first block is STREAMINFO (RFC 9639 §8.2).
	if head[4]&0x7f != 0 || head[5] != 0 || head[6] != 0 || head[7] != streamInfoSize {
		return nil, errors.New("flac: the first metadata block is not STREAMINFO")
	}
	info := head[8:]
	v := binary.BigEndian.Uint64(info[10:])
	f := &File{
		SampleRate: int(v >> 44), Channels: int(v>>41&7) + 1, BitDepth: int(v>>36&31) + 1,
		Samples: int64(v & (1<<36 - 1)), StreamInfo: append([]byte(nil), info...), Tags: map[string]string{},
	}
	if f.SampleRate == 0 || f.BitDepth < 4 {
		return nil, errors.New("flac: a stream with no sample rate, or with samples of fewer than 4 bits")
	}
	minBlock, maxBlock := int(binary.BigEndian.Uint16(info)), int(binary.BigEndian.Uint16(info[2:]))

	// The other blocks, up to the one flagged as the last.
	last := head[4]&0x80 != 0
	pos += int64(len(head))
	for n := 0; !last; n++ {
		if n == maxBlocks {
			return nil, errors.New("flac: too many metadata blocks")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var h [4]byte
		if pos+4 > size {
			return nil, errors.New("flac: the metadata runs past the end of the file")
		}
		if _, err := r.ReadAt(h[:], pos); err != nil {
			return nil, err
		}
		kind, length := h[0]&0x7f, int64(h[1])<<16|int64(h[2])<<8|int64(h[3])
		last = h[0]&0x80 != 0
		pos += 4
		if kind == 127 || pos+length > size {
			return nil, errors.New("flac: a metadata block that is not valid, or runs past the end of the file")
		}
		if kind == 4 && len(f.Tags) == 0 {
			b := make([]byte, min(length, maxComments))
			if _, err := r.ReadAt(b, pos); err != nil {
				return nil, err
			}
			parseComments(b, f.Tags)
		}
		pos += length
	}
	end := size
	var tag [3]byte
	if size-128 >= pos {
		if _, err := r.ReadAt(tag[:], size-128); err == nil && string(tag[:]) == "TAG" {
			end = size - 128
		}
	}
	f.DataOffset, f.DataSize = pos, end-pos

	// The first frame says whether the blocks are of one size, and then
	// STREAMINFO says which. (Before the frames could say it, a stream of
	// varying blocks was known by the range of sizes stated there.)
	rd := f.NewReader(r)
	at, h, ok := rd.find(pos, min(end, pos+maxJunk), false)
	if !ok {
		// A file may hold no sound. One that states a length and has no
		// frame of the format it states is not what it says: the frames are
		// of another format, or are not there.
		if f.Samples > 0 {
			return nil, errors.New("flac: no frame of the stream that the file describes")
		}
		return f, nil
	}
	// What lies before the first frame is not audio.
	f.variable, f.firstBlock = h.Variable, h.BlockSize
	f.DataOffset, f.DataSize, rd.pos = at, end-at, at
	if !h.Variable && minBlock == maxBlock && maxBlock >= 16 {
		f.BlockSize = maxBlock
	}
	f.start = f.firstSample(h)
	if f.Samples == 0 {
		if err := f.measure(ctx, rd); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// parseComments reads the fields of a Vorbis comment block into tags
// (RFC 9639 §8.6). Lengths are little-endian here, alone in the format.
func parseComments(b []byte, tags map[string]string) {
	next := func() ([]byte, bool) {
		if len(b) < 4 {
			return nil, false
		}
		n := binary.LittleEndian.Uint32(b)
		if uint64(n) > uint64(len(b)-4) {
			return nil, false
		}
		field := b[4 : 4+n]
		b = b[4+n:]
		return field, true
	}
	if _, ok := next(); !ok { // the vendor string
		return
	}
	if len(b) < 4 {
		return
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	for i := uint32(0); i < count && i < maxFields; i++ {
		field, ok := next()
		if !ok {
			return
		}
		name, value, ok := bytes.Cut(field, []byte{'='})
		if !ok || len(name) == 0 || len(name) > 64 {
			continue
		}
		valid := true
		for _, c := range name {
			valid = valid && c >= 0x20 && c <= 0x7e
		}
		if len(value) > maxValue {
			value = value[:maxValue]
			for len(value) > 0 && !utf8.Valid(value) && len(value) > maxValue-utf8.UTFMax {
				value = value[:len(value)-1]
			}
		}
		if !valid || !utf8.Valid(value) {
			continue
		}
		key := strings.ToLower(string(name))
		if old, ok := tags[key]; ok {
			tags[key] = old + ";" + string(value)
		} else {
			tags[key] = string(value)
		}
	}
}

// measure sets the length of a file that does not state it, from its last
// frame: the number of that frame's first sample and the samples it holds.
// rd is at the first frame.
func (f *File) measure(ctx context.Context, rd *Reader) error {
	first, end := rd.pos, rd.end
	// A frame is looked for ever further from the end, and from the one
	// found the frames are stepped through to the last.
	for back := int64(maxJunk); ; back *= 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		from := max(first, end-back)
		if at, _, ok := rd.sync(from, end); ok || from == first {
			rd.pos = max(at, first)
			var last codechdr.FLACFrame
			for n := 0; ; n++ {
				if n&0xff == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				data, err := rd.Next()
				if err != nil {
					break
				}
				last, _ = codechdr.FLACFrameHeader(data)
			}
			rd.pos, rd.breaks = first, 0
			if n := f.firstSample(last) - f.start + int64(last.BlockSize); last.BlockSize > 0 && n > 0 {
				f.Samples, f.Measured = n, true
			}
			return nil
		}
	}
}

// firstSample is the number the header h gives its frame's first sample.
// A frame that is numbered as a frame, in a stream that states no one block
// size, is taken to follow frames of the size of the first.
func (f *File) firstSample(h codechdr.FLACFrame) int64 {
	switch {
	case h.Variable:
		return int64(h.Number)
	case f.BlockSize > 0:
		return int64(h.Number) * int64(f.BlockSize)
	}
	return int64(h.Number) * int64(f.firstBlock)
}

// FirstSample returns where in the file's sound a frame that Next returned
// starts, in sample frames. The file's first frame starts at 0 whatever its
// number: a stream cut out of a longer one goes on from the numbers it had.
func (f *File) FirstSample(frame []byte) int64 {
	h, _ := codechdr.FLACFrameHeader(frame)
	return f.firstSample(h) - f.start
}

// Reader reads the frames of a file in order.
type Reader struct {
	r        io.ReaderAt
	f        *File
	pos, end int64
	buf      []byte // bytes of the file from off
	off      int64
	breaks   int // frames that no checksum ended
}

// NewReader returns a reader placed at the first frame of f. r is the file
// that Open read.
func (f *File) NewReader(r io.ReaderAt) *Reader {
	return &Reader{r: r, f: f, pos: f.DataOffset, end: f.DataOffset + f.DataSize}
}

// view returns the n bytes of the file at pos, or as many as there are up
// to the end of the audio. They are valid until the next call.
func (rd *Reader) view(pos int64, n int) []byte {
	if pos < 0 || pos >= rd.end || n <= 0 {
		return nil
	}
	n = int(min(int64(n), rd.end-pos))
	if pos < rd.off || pos+int64(n) > rd.off+int64(len(rd.buf)) {
		want := int(min(int64(max(n, readAhead)), rd.end-pos))
		if cap(rd.buf) < want {
			rd.buf = make([]byte, want)
		}
		got, err := rd.r.ReadAt(rd.buf[:want], pos)
		if err != nil && err != io.EOF {
			got = 0
		}
		rd.buf, rd.off = rd.buf[:got], pos
		n = min(n, got)
	}
	return rd.buf[pos-rd.off : pos-rd.off+int64(n)]
}

// match reads the frame header at the start of b, and reports whether it is
// one of this stream: the format the file states and, where strict, the
// blocking strategy of its first frame.
func (rd *Reader) match(b []byte, strict bool) (codechdr.FLACFrame, bool) {
	h, ok := codechdr.FLACFrameHeader(b)
	f := rd.f
	if !ok || h.Channels != f.Channels || h.SampleRate != 0 && h.SampleRate != f.SampleRate ||
		h.BitDepth != 0 && h.BitDepth != f.BitDepth || strict && h.Variable != f.variable {
		return h, false
	}
	return h, true
}

// find returns the position of the first frame header of the stream that
// starts in [from, to), and the header. A header is a sync code and a
// checksum of eight bits, so now and then other data passes for one.
func (rd *Reader) find(from, to int64, strict bool) (int64, codechdr.FLACFrame, bool) {
	to = min(to, rd.end)
	for from < to {
		b := rd.view(from, int(min(to-from+codechdr.FLACMaxHeader, readAhead)))
		for i := 0; i < len(b); i++ {
			j := bytes.IndexByte(b[i:], 0xff)
			if j < 0 || from+int64(i+j) >= to {
				break
			}
			i += j
			if i+1 < len(b) && b[i+1]&0xfe == 0xf8 {
				if h, ok := rd.match(b[i:], strict); ok {
					return from + int64(i), h, true
				}
			}
		}
		// A header may lie across the end of what was looked at.
		if len(b) <= codechdr.FLACMaxHeader {
			break
		}
		from += int64(len(b) - codechdr.FLACMaxHeader + 1)
	}
	return 0, codechdr.FLACFrame{}, false
}

// bound is the most bytes a frame with header h is taken to have: its
// samples stored as they are, and room for the headers. An encoder that
// finds no smaller coding stores them so.
func (rd *Reader) bound(h codechdr.FLACFrame) int64 {
	return 64 + int64(h.Channels)*(int64(h.BlockSize)*int64(rd.f.BitDepth+1)/8+16)
}

// frame returns the frame that starts at pos, whose header is h. sure
// reports that the frame's checksum holds: it ends before another frame
// header, or at the end of the audio, and the two bytes there are the
// checksum of the rest. A frame that no checksum ends is damaged, or is not
// a frame: it is taken to end at the next header, or where the largest
// frame would.
func (rd *Reader) frame(pos int64, h codechdr.FLACFrame) (data []byte, sure bool) {
	limit := int(min(rd.bound(h), rd.end-pos))
	var (
		crc   uint16
		crcAt int      // crc is that of the frame's first crcAt bytes
		guess = -1     // the first header after the frame's own
		i     = h.Size // where the search goes on
	)
	check := func(b []byte, end int) bool {
		if end-2 < crcAt {
			return false
		}
		crc = codechdr.FLACCRC16(crc, b[crcAt:end-2])
		crcAt = end - 2
		return crc == binary.BigEndian.Uint16(b[end-2:])
	}
	for want := min(limit, 1<<15); ; want = min(limit, want*4) {
		b := rd.view(pos, want)
		for i < len(b) {
			j := bytes.IndexByte(b[i:], 0xff)
			if j < 0 {
				i = len(b)
				break
			}
			i += j
			// A header is looked at once all of it is in view.
			if i+codechdr.FLACMaxHeader > len(b) && len(b) < limit {
				break
			}
			if i+1 < len(b) && b[i+1]&0xfe == 0xf8 {
				if _, ok := rd.match(b[i:], true); ok {
					if check(b, i) {
						return b[:i], true
					}
					if guess < 0 {
						guess = i
					}
				}
			}
			i++
		}
		if len(b) >= limit || len(b) < want {
			// The last frame of the file ends where the audio does.
			if pos+int64(len(b)) == rd.end && check(b, len(b)) {
				return b, true
			}
			if guess > 0 {
				return b[:guess], false
			}
			return b, false
		}
	}
}

// sync returns the first frame that starts in [from, to) and whose checksum
// holds, with its header: a place to start reading from, found without
// reading what comes before.
func (rd *Reader) sync(from, to int64) (int64, codechdr.FLACFrame, bool) {
	for try := 0; try < maxTries; try++ {
		at, h, ok := rd.find(from, to, true)
		if !ok {
			break
		}
		if _, sure := rd.frame(at, h); sure {
			return at, h, true
		}
		from = at + 1
	}
	return 0, codechdr.FLACFrame{}, false
}

// Next returns the next frame, from its sync code to its checksum. The
// bytes are valid until the next call. The error is io.EOF after the last
// frame.
//
// What is not a frame (damage, or other data among the audio) is stepped
// over, up to the next frame header. A damaged frame is returned as far as
// the next header, for the decoder to make silence of.
func (rd *Reader) Next() ([]byte, error) {
	for rd.pos < rd.end {
		h, ok := rd.match(rd.view(rd.pos, codechdr.FLACMaxHeader), true)
		if !ok {
			at, _, found := rd.find(rd.pos+1, rd.pos+maxJunk, true)
			if !found {
				break
			}
			rd.pos = at
			continue
		}
		// A file with many frames that no checksum ends is searched for
		// each of them as far as a frame can reach: after a few, frames
		// are taken to end at the next header, checksum or not.
		var data []byte
		if rd.breaks < maxBreaks {
			var sure bool
			if data, sure = rd.frame(rd.pos, h); !sure {
				rd.breaks++
			}
		} else {
			to := rd.pos + min(rd.bound(h), rd.end-rd.pos)
			if at, _, found := rd.find(rd.pos+int64(h.Size), to, true); found {
				to = at
			}
			data = rd.view(rd.pos, int(to-rd.pos))
		}
		if len(data) == 0 {
			break
		}
		rd.pos += int64(len(data))
		return data, nil
	}
	rd.pos = rd.end
	return nil, io.EOF
}

// Skip moves a reader that is at the first frame to the frame n frames on,
// or as near before it as it gets, and returns how many frames it passed.
// It reads a few places in the file, not the frames on the way: a frame
// states its number, so the file is halved until the frame is near.
//
// It passes none, and the reader stays where it is, in a stream of blocks of
// varying size, whose frames are numbered by sample: there a count of
// frames says nothing of where the sound is.
func (rd *Reader) Skip(n int64) (passed int64) {
	h, ok := rd.match(rd.view(rd.pos, codechdr.FLACMaxHeader), true)
	if !ok || rd.f.BlockSize == 0 || n <= 0 {
		return 0
	}
	// Frames count from the stream's first, whatever number that has.
	first := int64(h.Number)
	lo, hi, at, want := rd.pos, rd.end, first, first+n
	// The frame at lo has number at, which is not past the one wanted. A
	// frame found further on takes its place if its number lies between:
	// numbers that do not rise with the place in the file are not gone by
	// (RFC 9639 §11).
	for hi-lo > readAhead/2 && at < want {
		mid := lo + (hi-lo)/2
		p, h, ok := rd.sync(mid, hi)
		if num := int64(h.Number); !ok || num > want || num <= at {
			hi = mid
			continue
		}
		lo, at = p, int64(h.Number)
	}
	// The rest of the way frame by frame, as long as they are whole and
	// numbered one after the other.
	for at < want {
		h, ok := rd.match(rd.view(lo, codechdr.FLACMaxHeader), true)
		if !ok {
			break
		}
		data, sure := rd.frame(lo, h)
		next := lo + int64(len(data))
		h, ok = rd.match(rd.view(next, codechdr.FLACMaxHeader), true)
		if !sure || !ok || int64(h.Number) != at+1 {
			break
		}
		lo, at = next, at+1
	}
	rd.pos = lo
	return at - first
}
