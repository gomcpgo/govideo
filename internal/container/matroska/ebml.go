package matroska

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	mbits "math/bits"
	"strings"
)

// Element IDs (Matroska specification; IDs include the EBML length marker).
const (
	idEBML    = 0x1A45DFA3
	idDocType = 0x4282
	idVoid    = 0xEC
	idCRC32   = 0xBF

	idSegment     = 0x18538067
	idSeekHead    = 0x114D9B74
	idInfo        = 0x1549A966
	idTracks      = 0x1654AE6B
	idCluster     = 0x1F43B675
	idCues        = 0x1C53BB6B
	idTags        = 0x1254C367
	idAttachments = 0x1941A469
	idChapters    = 0x1043A770

	idTimestampScale = 0x2AD7B1
	idDuration       = 0x4489
	idDateUTC        = 0x4461
	idTitle          = 0x7BA9
	idMuxingApp      = 0x4D80
	idWritingApp     = 0x5741

	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackUID        = 0x73C5
	idTrackType       = 0x83
	idFlagEnabled     = 0xB9
	idFlagDefault     = 0x88
	idFlagForced      = 0x55AA
	idDefaultDuration = 0x23E383
	idName            = 0x536E
	idLanguage        = 0x22B59C
	idLanguageBCP47   = 0x22B59D
	idCodecID         = 0x86
	idCodecPrivate    = 0x63A2
	idCodecDelay      = 0x56AA
	idSeekPreRoll     = 0x56BB
	idVideo           = 0xE0
	idAudio           = 0xE1
	idContentEncs     = 0x6D80

	idFlagInterlaced = 0x9A
	idPixelWidth     = 0xB0
	idPixelHeight    = 0xBA
	idDisplayWidth   = 0x54B0
	idDisplayHeight  = 0x54BA
	idDisplayUnit    = 0x54B2
	idColour         = 0x55B0
	idProjection     = 0x7670

	idMatrixCoefficients = 0x55B1
	idBitsPerChannel     = 0x55B2
	idChromaSubHorz      = 0x55B3
	idChromaSubVert      = 0x55B4
	idRange              = 0x55B9
	idTransfer           = 0x55BA
	idPrimaries          = 0x55BB
	idMaxCLL             = 0x55BC
	idMaxFALL            = 0x55BD
	idMasteringMetadata  = 0x55D0

	idProjectionPoseRoll = 0x7675

	idSamplingFrequency       = 0xB5
	idOutputSamplingFrequency = 0x78B5
	idChannels                = 0x9F
	idBitDepth                = 0x6264

	idContentEncoding      = 0x6240
	idContentEncodingScope = 0x5032
	idContentEncodingType  = 0x5033
	idContentCompression   = 0x5034
	idContentCompAlgo      = 0x4254
	idContentCompSettings  = 0x4255

	idTimestamp      = 0xE7
	idSimpleBlock    = 0xA3
	idBlockGroup     = 0xA0
	idBlock          = 0xA1
	idBlockDuration  = 0x9B
	idReferenceBlock = 0xFB

	idTag              = 0x7373
	idTargets          = 0x63C0
	idTagTrackUID      = 0x63C5
	idTagEditionUID    = 0x63C9
	idTagChapterUID    = 0x63C4
	idTagAttachmentUID = 0x63C6
	idSimpleTag        = 0x67C8
	idTagName          = 0x45A3
	idTagString        = 0x4487
)

// isTopLevel reports whether id is a Segment-level (or higher) element. An
// unknown-size Cluster ends at the first such element (RFC 8794 §6.2).
func isTopLevel(id uint32) bool {
	switch id {
	case idEBML, idSegment, idSeekHead, idInfo, idTracks, idCluster, idCues,
		idTags, idAttachments, idChapters:
		return true
	}
	return false
}

const (
	readWindow = 8192 // sequential read-ahead
	readProbe  = 512  // read size after a seek (skipped payload)
)

// reader serves small bounds-checked reads from an io.ReaderAt through a
// sliding window, so that element and block headers cost few system calls
// while skipped payloads are never read.
type reader struct {
	r    io.ReaderAt
	size int64
	buf  []byte
	off  int64
	err  error // first real I/O error (not EOF)
}

// at returns up to n bytes at off; fewer (possibly none) at end of file or on
// error. The slice is only valid until the next call.
func (rd *reader) at(off int64, n int) []byte {
	if rd.err != nil || off < 0 || off >= rd.size || n <= 0 {
		return nil
	}
	if int64(n) > rd.size-off {
		n = int(rd.size - off)
	}
	end := rd.off + int64(len(rd.buf))
	if off >= rd.off && off+int64(n) <= end {
		return rd.buf[off-rd.off:][:n]
	}
	want := readProbe
	if len(rd.buf) > 0 && off >= rd.off && off <= end+256 {
		want = readWindow
	}
	if want < n {
		want = n
	}
	if int64(want) > rd.size-off {
		want = int(rd.size - off)
	}
	if cap(rd.buf) < want {
		rd.buf = make([]byte, want)
	}
	rd.buf = rd.buf[:want]
	m, err := rd.r.ReadAt(rd.buf, off)
	if m < 0 {
		m = 0
	}
	if m > want {
		m = want
	}
	rd.buf, rd.off = rd.buf[:m], off
	if m < n {
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			rd.err = err
		}
		n = m
	}
	return rd.buf[:n]
}

// header parses an element header at pos. size is -1 for the EBML
// "unknown size" encoding. ok is false on truncation or invalid coding.
func (rd *reader) header(pos int64) (id uint32, size int64, hdrLen int, ok bool) {
	b := rd.at(pos, 12)
	if len(b) < 2 {
		return
	}
	l := mbits.LeadingZeros8(b[0]) + 1
	if l > 4 || len(b) < l+1 {
		return
	}
	for i := 0; i < l; i++ {
		id = id<<8 | uint32(b[i])
	}
	v, sl, unknown, vok := vint(b[l:])
	if !vok {
		return
	}
	if unknown {
		return id, -1, l + sl, true
	}
	return id, int64(v), l + sl, true
}

// vint decodes an EBML variable-size integer (RFC 8794 §4) with the length
// marker removed. unknown reports the all-ones reserved value.
func vint(b []byte) (v uint64, n int, unknown, ok bool) {
	if len(b) == 0 {
		return
	}
	n = mbits.LeadingZeros8(b[0]) + 1
	if n > 8 || len(b) < n {
		return 0, 0, false, false
	}
	v = uint64(b[0]) & (0xFF >> uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	unknown = v == 1<<(7*uint(n))-1
	return v, n, unknown, true
}

func (rd *reader) uint(off, size int64) uint64 {
	if size <= 0 || size > 8 {
		return 0
	}
	b := rd.at(off, int(size))
	if int64(len(b)) < size {
		return 0
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// sint reads a signed big-endian integer of 0..8 bytes.
func (rd *reader) sint(off, size int64) int64 {
	if size <= 0 || size > 8 {
		return 0
	}
	v := rd.uint(off, size)
	shift := uint(64 - 8*size)
	return int64(v<<shift) >> shift
}

func (rd *reader) float(off, size int64) float64 {
	switch size {
	case 4:
		if b := rd.at(off, 4); len(b) == 4 {
			return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
		}
	case 8:
		if b := rd.at(off, 8); len(b) == 8 {
			return math.Float64frombits(binary.BigEndian.Uint64(b))
		}
	}
	return 0
}

// str reads a string element, truncated to max bytes, without trailing NULs.
func (rd *reader) str(off, size int64, max int) string {
	if size > int64(max) {
		size = int64(max)
	}
	b := rd.at(off, int(size))
	s := string(b)
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return s
}

// bytes returns a copy of a binary element, or nil if it exceeds max or is
// truncated.
func (rd *reader) bytes(off, size int64, max int) []byte {
	if size <= 0 || size > int64(max) {
		return nil
	}
	b := rd.at(off, int(size))
	if int64(len(b)) < size {
		return nil
	}
	return append([]byte(nil), b...)
}

const maxChildren = 1 << 20

// each calls fn for every child element in [start, end). Iteration stops
// quietly at a truncated, overlong or unknown-size child: callers keep what
// was parsed so far.
func (rd *reader) each(ctx context.Context, start, end int64, fn func(id uint32, off, size int64)) error {
	pos := start
	for n := 0; pos < end && n < maxChildren; n++ {
		if n&255 == 255 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		id, size, h, ok := rd.header(pos)
		if !ok || size < 0 {
			break
		}
		data := pos + int64(h)
		if size > end-data {
			break
		}
		fn(id, data, size)
		pos = data + size
	}
	return rd.err
}
