// Package avi parses AVI (RIFF) files: the stream headers of the hdrl list
// and a per-stream index of the data chunks in every movi list, including
// the extra "RIFF AVIX" segments of OpenDML files.
//
// The layout follows Microsoft's "AVI RIFF File Reference" and the OpenDML
// AVI file format extensions. The sample index is built by walking the chunk
// headers of the movi lists rather than by trusting idx1 or the OpenDML
// indexes, so files that were never finalised or were cut short still open:
// the index simply ends at the last whole chunk.
//
// Input is untrusted. Every read is bounds-checked against the file size,
// header allocations and the index length are capped, and Open observes
// context cancellation.
package avi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"time"
)

// ErrNotAVI reports that the input is not a RIFF/AVI file.
var ErrNotAVI = errors.New("avi: not a RIFF/AVI file")

// MaxChunkSize is the largest chunk ReadChunk will load into memory.
const MaxChunkSize = 1 << 28

const (
	maxStreams      = 100     // two decimal digits in a chunk id
	maxIndexEntries = 4 << 20 // chunks indexed across all streams
	maxScanSteps    = 16 << 20
	maxHeaderChunks = 4096
	maxExtra        = 1 << 16 // codec data kept from a strf chunk
)

// StreamType is the kind of an AVI stream.
type StreamType int

// Stream kinds. Text, MIDI and interleaved DV streams are StreamOther.
const (
	StreamOther StreamType = iota
	StreamVideo
	StreamAudio
)

func (t StreamType) String() string {
	switch t {
	case StreamVideo:
		return "video"
	case StreamAudio:
		return "audio"
	}
	return "other"
}

// WAVEFORMATEX format tags of the audio codecs callers commonly meet.
const (
	FormatPCM        = 0x0001
	FormatIEEEFloat  = 0x0003
	FormatMP3        = 0x0055
	FormatAAC        = 0x00ff
	FormatAC3        = 0x2000
	FormatExtensible = 0xfffe
)

// Chunk locates the payload of one data chunk of a stream.
type Chunk struct {
	Offset int64  // file offset of the payload, after the 8-byte chunk header
	Size   uint32 // payload length in bytes; 0 for a dropped (repeated) frame
}

// Stream describes one stream: its strh and strf headers and the chunks that
// carry its data, in file order.
type Stream struct {
	Index int        // stream number, as used in chunk ids ("00dc" is stream 0)
	Type  StreamType // from the strh fccType: "vids", "auds" or anything else

	FccType string // strh fccType, e.g. "vids"
	Handler string // strh fccHandler, e.g. "mjpg"; may be empty or all zero bytes

	// Scale and Rate are the strh time base: one unit lasts Scale/Rate
	// seconds. For video a unit is a frame, so the frame rate is Rate/Scale.
	// If a video stream leaves either at zero, Open substitutes the avih
	// frame period (Scale = dwMicroSecPerFrame, Rate = 1000000).
	Scale, Rate uint32
	Start       uint32 // strh dwStart: presentation delay in units of Scale/Rate
	Length      uint32 // strh dwLength as written by the muxer (unverified)
	SampleSize  uint32 // strh dwSampleSize: bytes per unit, 0 if chunks vary

	// Video (BITMAPINFOHEADER).
	Compression string // biCompression fourcc, e.g. "MJPG"; "" for BI_RGB (0)
	Width       int
	Height      int  // absolute value of biHeight
	TopDown     bool // biHeight was negative: uncompressed rows run top to bottom
	BitCount    int  // biBitCount

	// Audio (WAVEFORMATEX). For WAVE_FORMAT_EXTENSIBLE, FormatTag is the
	// format code taken from the SubFormat GUID and Extensible is set.
	FormatTag      uint16
	Extensible     bool
	Channels       int
	SampleRate     int
	AvgBytesPerSec int
	BlockAlign     int
	BitsPerSample  int
	ChannelMask    uint32 // WAVE_FORMAT_EXTENSIBLE dwChannelMask, else 0

	// Extra is the codec-specific data following the fixed part of strf:
	// the bytes after the 40-byte BITMAPINFOHEADER (palette or codec
	// configuration) or after the 18-byte WAVEFORMATEX. At most 64 KiB kept.
	Extra []byte

	// Chunks lists the stream's data chunks in file order. For video each
	// chunk is one frame; a zero-length chunk is a dropped frame, meaning
	// "show the previous frame again". Such chunks stay in the list so that
	// chunk i is always presented at unit Start+i.
	Chunks []Chunk
	// TotalBytes is the sum of the chunk sizes.
	TotalBytes int64

	before []int64 // audio: payload bytes preceding each chunk
}

// File is a parsed AVI file.
type File struct {
	Streams []*Stream

	// Main header (avih) fields, as written by the muxer.
	MicroSecPerFrame uint32
	HeaderFrames     uint32 // dwTotalFrames; counts the first RIFF segment only in OpenDML files
	Width, Height    int

	// Duration is the latest end time over the video and audio streams.
	Duration time.Duration

	// Truncated reports that a movi list ended inside a chunk: the file was
	// cut short and the index stops at the last whole chunk.
	Truncated bool
	// IndexCapped reports that the chunk index hit its size cap and the
	// rest of the file was not indexed.
	IndexCapped bool

	r    io.ReaderAt
	size int64
}

// Sniff reports whether head looks like the start of a RIFF/AVI file.
func Sniff(head []byte) bool {
	return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI "
}

// Video returns the first video stream, or nil.
func (f *File) Video() *Stream { return f.first(StreamVideo) }

// Audio returns the first audio stream, or nil.
func (f *File) Audio() *Stream { return f.first(StreamAudio) }

func (f *File) first(t StreamType) *Stream {
	for _, s := range f.Streams {
		if s.Type == t {
			return s
		}
	}
	return nil
}

// Frames returns the number of frames of the first video stream, counted
// from the index (dropped frames included), or 0 if there is no video.
func (f *File) Frames() int64 {
	if v := f.Video(); v != nil {
		return v.Frames()
	}
	return 0
}

// ReadChunk reads the payload of c. The result reuses buf's storage when it
// is large enough. Chunks larger than MaxChunkSize or lying outside the file
// are refused, so a hostile index cannot force a large allocation.
func (f *File) ReadChunk(c Chunk, buf []byte) ([]byte, error) {
	n := int64(c.Size)
	if n > MaxChunkSize {
		return nil, fmt.Errorf("avi: chunk of %d bytes exceeds the %d byte limit", n, MaxChunkSize)
	}
	if c.Offset < 0 || c.Offset > f.size || n > f.size-c.Offset {
		return nil, errors.New("avi: chunk lies outside the file")
	}
	if int64(cap(buf)) >= n {
		buf = buf[:n]
	} else {
		buf = make([]byte, n)
	}
	if n == 0 {
		return buf, nil
	}
	if m, err := f.r.ReadAt(buf, c.Offset); m < len(buf) {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("avi: reading chunk: %w", err)
	}
	return buf, nil
}

// FourCC returns the codec fourcc of a video stream: biCompression, or the
// strh handler when biCompression is absent.
func (s *Stream) FourCC() string {
	if s.Compression != "" {
		return s.Compression
	}
	return s.Handler
}

// IsPCM reports whether an audio stream carries uncompressed integer or
// IEEE float PCM with a usable block size.
func (s *Stream) IsPCM() bool {
	return s.Type == StreamAudio && (s.FormatTag == FormatPCM || s.FormatTag == FormatIEEEFloat) &&
		s.BlockAlign > 0 && s.SampleRate > 0
}

// Frames returns the number of presentation units in the stream: frames for
// video (one per chunk, dropped frames included), sample frames for PCM
// audio, and chunks otherwise.
func (s *Stream) Frames() int64 {
	if s.IsPCM() {
		return s.TotalBytes / int64(s.BlockAlign)
	}
	return int64(len(s.Chunks))
}

// FrameRate returns the stream rate Rate/Scale as a fraction; den is 0 when
// the header carries no usable rate.
func (s *Stream) FrameRate() (num, den uint32) {
	if s.Rate == 0 || s.Scale == 0 {
		return 0, 0
	}
	return s.Rate, s.Scale
}

// StartTime returns the stream's presentation delay, Start*Scale/Rate.
func (s *Stream) StartTime() time.Duration {
	return scaleDur(uint64(s.Start), uint64(s.Scale), uint64(s.Rate))
}

// ChunkTime returns the presentation time of chunk i, including the
// stream's start delay. Video chunk i is shown at (Start+i)*Scale/Rate. A
// PCM audio chunk starts at the number of payload bytes before it divided by
// BlockAlign and the sample rate. Other audio uses the strh time base:
// bytes/SampleSize units when SampleSize is set, else one unit per chunk.
// i may equal len(Chunks), which gives the end time of the stream.
func (s *Stream) ChunkTime(i int) time.Duration {
	if i < 0 {
		i = 0
	}
	if i > len(s.Chunks) {
		i = len(s.Chunks)
	}
	if s.Type != StreamAudio {
		return scaleDur(uint64(s.Start)+uint64(i), uint64(s.Scale), uint64(s.Rate))
	}
	bytes := s.TotalBytes
	if i < len(s.before) {
		bytes = s.before[i]
	}
	switch {
	case s.IsPCM():
		return s.StartTime() + scaleDur(uint64(bytes)/uint64(s.BlockAlign), 1, uint64(s.SampleRate))
	case s.SampleSize > 0:
		return scaleDur(uint64(s.Start)+uint64(bytes)/uint64(s.SampleSize), uint64(s.Scale), uint64(s.Rate))
	}
	return scaleDur(uint64(s.Start)+uint64(i), uint64(s.Scale), uint64(s.Rate))
}

// Duration returns the length of the stream's media, excluding its start
// delay.
func (s *Stream) Duration() time.Duration {
	return s.ChunkTime(len(s.Chunks)) - s.StartTime()
}

// scaleDur returns n*num/den seconds as a Duration, 0 if den is 0, and the
// largest representable value if the result is out of range.
func scaleDur(n, num, den uint64) time.Duration {
	const maxDur = time.Duration(1<<63 - 1)
	if den == 0 || n == 0 || num == 0 {
		return 0
	}
	hi, lo := bits.Mul64(n, num)
	if hi >= den {
		return maxDur
	}
	sec, rem := bits.Div64(hi, lo, den)
	if sec > 1<<33 {
		return maxDur
	}
	// rem < den, so rem*1e9/den < 1e9 and the division cannot overflow.
	hi, lo = bits.Mul64(rem, 1_000_000_000)
	ns, r2 := bits.Div64(hi, lo, den)
	if r2 >= den-r2 { // round to nearest
		ns++
	}
	return time.Duration(sec)*time.Second + time.Duration(ns)
}

// Open parses the headers of an AVI file of the given size and indexes the
// data chunks of each stream. A file cut short while recording opens with
// the chunks that are wholly present; a file without stream headers is an
// error.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	var hdr [12]byte
	if size < 12 {
		return nil, ErrNotAVI
	}
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, 12), hdr[:]); err != nil || !Sniff(hdr[:]) {
		return nil, ErrNotAVI
	}
	p := &parser{ctx: ctx, f: &File{r: r, size: size}}
	// Top level: "RIFF size 'AVI '" followed, in OpenDML files, by any
	// number of "RIFF size 'AVIX'" segments that hold further movi lists.
	off := int64(0)
	for seg := 0; seg < maxHeaderChunks && off+12 <= size; seg++ {
		if !p.read(hdr[:], off) {
			break
		}
		form := string(hdr[8:12])
		if string(hdr[:4]) != "RIFF" || (form != "AVI " && form != "AVIX") || (seg == 0) != (form == "AVI ") {
			break
		}
		end := off + 8 + int64(le.Uint32(hdr[4:]))
		// A zero or overlong size means the file was not finalised.
		if end <= off+12 || end > size {
			end = size
		}
		if err := p.riff(off+12, end, seg == 0); err != nil {
			return nil, err
		}
		if p.stop {
			break
		}
		off = end + end&1
	}
	f := p.f
	if len(f.Streams) == 0 {
		return nil, errors.New("avi: no stream headers")
	}
	for _, s := range f.Streams {
		if s.Type != StreamVideo && s.Type != StreamAudio {
			continue
		}
		if end := s.ChunkTime(len(s.Chunks)); end > f.Duration {
			f.Duration = end
		}
	}
	return f, nil
}

var le = binary.LittleEndian

type parser struct {
	ctx   context.Context
	f     *File
	steps int
	total int  // index entries so far
	stop  bool // stop scanning: truncation, cap reached or unreadable data
}

func (p *parser) tick() error {
	p.steps++
	if p.steps&1023 == 0 {
		if err := p.ctx.Err(); err != nil {
			return err
		}
	}
	if p.steps > maxScanSteps {
		p.f.IndexCapped = true
		p.stop = true
	}
	return nil
}

// riff walks the chunks of one RIFF segment in [off, end).
func (p *parser) riff(off, end int64, first bool) error {
	r := p.f.r
	var ch [12]byte
	for !p.stop && off+8 <= end {
		if err := p.tick(); err != nil {
			return err
		}
		if n, _ := r.ReadAt(ch[:], off); n < 8 {
			p.stop = true
			return nil
		}
		id, n := string(ch[:4]), int64(le.Uint32(ch[4:]))
		body := off + 8
		if id != "LIST" {
			if n > end-body {
				p.stop = true
				return nil
			}
			off = body + n + n&1
			continue
		}
		if body+4 > end {
			p.stop = true
			return nil
		}
		form := string(ch[8:12])
		lend := body + n
		switch form {
		case "movi":
			// An unfinalised file leaves the movi size at zero or beyond
			// the end; the data then runs to the end of the segment.
			if n < 4 || lend > end {
				lend = end
				if end == p.f.size && n >= 4 {
					p.f.Truncated = true
				}
			}
			if err := p.movi(body+4, lend); err != nil {
				return err
			}
		case "hdrl":
			if lend > end || n < 4 {
				p.stop = true
				return nil
			}
			if first && len(p.f.Streams) == 0 {
				if err := p.hdrl(body+4, lend); err != nil {
					return err
				}
			}
		default:
			if lend > end || n < 4 {
				p.stop = true
				return nil
			}
		}
		off = lend + lend&1
	}
	return nil
}

// hdrl parses the avih chunk and the strl lists in [off, end).
func (p *parser) hdrl(off, end int64) error {
	f := p.f
	var ch [12]byte
	for i := 0; i < maxHeaderChunks && off+8 <= end; i++ {
		if err := p.tick(); err != nil {
			return err
		}
		if n, _ := f.r.ReadAt(ch[:], off); n < 8 {
			break
		}
		id, n := string(ch[:4]), int64(le.Uint32(ch[4:]))
		body := off + 8
		if n > end-body {
			break
		}
		switch {
		case id == "avih":
			var b [40]byte
			if n >= 40 && p.read(b[:], body) {
				f.MicroSecPerFrame = le.Uint32(b[0:])
				f.HeaderFrames = le.Uint32(b[16:])
				f.Width = clampDim(int64(le.Uint32(b[32:])))
				f.Height = clampDim(int64(le.Uint32(b[36:])))
			}
		case id == "LIST" && n >= 4 && string(ch[8:12]) == "strl":
			if len(f.Streams) >= maxStreams {
				break
			}
			s := &Stream{Index: len(f.Streams)}
			if err := p.strl(s, body+4, body+n); err != nil {
				return err
			}
			if s.Type == StreamVideo && (s.Rate == 0 || s.Scale == 0) && f.MicroSecPerFrame > 0 {
				s.Scale, s.Rate = f.MicroSecPerFrame, 1_000_000
			}
			f.Streams = append(f.Streams, s)
		}
		off = body + n + n&1
	}
	return nil
}

// strl parses the strh and strf chunks of one stream list in [off, end).
func (p *parser) strl(s *Stream, off, end int64) error {
	r := p.f.r
	var ch [8]byte
	haveH, haveF := false, false
	for i := 0; i < maxHeaderChunks && off+8 <= end; i++ {
		if err := p.tick(); err != nil {
			return err
		}
		if n, _ := r.ReadAt(ch[:], off); n < 8 {
			break
		}
		id, n := string(ch[:4]), int64(le.Uint32(ch[4:]))
		body := off + 8
		if n > end-body {
			break
		}
		switch {
		case id == "strh" && !haveH:
			var b [48]byte
			if n < 48 || !p.read(b[:], body) {
				break
			}
			haveH = true
			s.FccType = string(b[0:4])
			s.Handler = fourcc(b[4:8])
			switch s.FccType {
			case "vids":
				s.Type = StreamVideo
			case "auds":
				s.Type = StreamAudio
			}
			s.Scale = le.Uint32(b[20:])
			s.Rate = le.Uint32(b[24:])
			s.Start = le.Uint32(b[28:])
			s.Length = le.Uint32(b[32:])
			s.SampleSize = le.Uint32(b[44:])
		case id == "strf" && haveH && !haveF:
			m := n
			if m > 40+maxExtra {
				m = 40 + maxExtra
			}
			b := make([]byte, m)
			if !p.read(b, body) {
				break
			}
			haveF = true
			switch s.Type {
			case StreamVideo:
				s.bitmapInfo(b)
			case StreamAudio:
				s.waveFormat(b)
			}
		}
		off = body + n + n&1
	}
	if !haveF {
		// Without a format the stream cannot be interpreted; keep its slot
		// so chunk ids still address the right stream.
		s.Type = StreamOther
	}
	return nil
}

// bitmapInfo reads a BITMAPINFOHEADER.
func (s *Stream) bitmapInfo(b []byte) {
	if len(b) < 20 {
		return
	}
	w, h := int64(int32(le.Uint32(b[4:]))), int64(int32(le.Uint32(b[8:])))
	if h < 0 {
		h, s.TopDown = -h, true
	}
	if w < 0 {
		w = -w
	}
	s.Width, s.Height = clampDim(w), clampDim(h)
	s.BitCount = int(le.Uint16(b[14:]))
	if le.Uint32(b[16:]) != 0 {
		s.Compression = fourcc(b[16:20])
	}
	if len(b) > 40 {
		s.Extra = append([]byte(nil), b[40:]...)
	}
}

// waveFormat reads a PCMWAVEFORMAT, WAVEFORMATEX or WAVEFORMATEXTENSIBLE.
func (s *Stream) waveFormat(b []byte) {
	if len(b) < 14 {
		return
	}
	s.FormatTag = le.Uint16(b)
	s.Channels = int(le.Uint16(b[2:]))
	s.SampleRate = int(le.Uint32(b[4:]) & 0x7fffffff)
	s.AvgBytesPerSec = int(le.Uint32(b[8:]) & 0x7fffffff)
	s.BlockAlign = int(le.Uint16(b[12:]))
	if len(b) >= 16 {
		s.BitsPerSample = int(le.Uint16(b[14:]))
	}
	if s.FormatTag == FormatExtensible && len(b) >= 40 {
		s.Extensible = true
		s.ChannelMask = le.Uint32(b[20:])
		s.FormatTag = le.Uint16(b[24:]) // first two bytes of the SubFormat GUID
		if len(b) > 40 {
			s.Extra = append([]byte(nil), b[40:]...)
		}
		return
	}
	if len(b) > 18 {
		s.Extra = append([]byte(nil), b[18:]...)
	}
}

// movi indexes the data chunks in [off, end). "rec " lists only group
// chunks, so they are entered by stepping over their 12-byte header.
func (p *parser) movi(off, end int64) error {
	f := p.f
	var ch [8]byte
	for !p.stop && off+8 <= end {
		if err := p.tick(); err != nil {
			return err
		}
		if n, _ := f.r.ReadAt(ch[:], off); n < 8 {
			p.stop = true
			break
		}
		n := int64(le.Uint32(ch[4:]))
		body := off + 8
		if string(ch[:4]) == "LIST" {
			if n < 4 || body+4 > end {
				p.stop, f.Truncated = true, true
				break
			}
			off = body + 4
			continue
		}
		if n > end-body {
			// The last chunk is incomplete: recording stopped mid-chunk.
			p.stop, f.Truncated = true, true
			break
		}
		if idx, ok := streamOf(ch[:4]); ok && idx < len(f.Streams) {
			if p.total >= maxIndexEntries {
				p.stop, f.IndexCapped = true, true
				break
			}
			p.total++
			s := f.Streams[idx]
			if s.Type == StreamAudio {
				s.before = append(s.before, s.TotalBytes)
			}
			s.Chunks = append(s.Chunks, Chunk{Offset: body, Size: uint32(n)})
			s.TotalBytes += n
		}
		off = body + n + n&1
	}
	return nil
}

// streamOf decodes a data chunk id such as "00dc" or "01wb": two digits of
// stream number and a two-character type. Palette changes ("pc") carry no
// samples, and index chunks ("ix00") do not start with a digit.
func streamOf(id []byte) (int, bool) {
	a, b := digit(id[0]), digit(id[1])
	if a < 0 || b < 0 || (id[2] == 'p' && id[3] == 'c') {
		return 0, false
	}
	if a > 9 || b > 9 {
		return a<<4 | b, true // hexadecimal numbering
	}
	return a*10 + b, true
}

func digit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// read fills b from offset off, reporting whether all of it was available.
func (p *parser) read(b []byte, off int64) bool {
	n, _ := p.f.r.ReadAt(b, off)
	return n == len(b)
}

// fourcc renders a four-character code, or "" when it is all zero bytes.
func fourcc(b []byte) string {
	if b[0]|b[1]|b[2]|b[3] == 0 {
		return ""
	}
	return string(b[:4])
}

func clampDim(v int64) int {
	if v < 0 || v > 1<<20 {
		return 0
	}
	return int(v)
}
