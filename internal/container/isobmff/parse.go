// Package isobmff reads and writes ISO base media files (MP4, MOV, fragmented
// MP4). It is written from ISO/IEC 14496-12 and the QuickTime File Format
// specification; it parses container structure only and never decodes media.
package isobmff

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Limits that keep hostile input from exhausting memory.
const (
	maxMoovSize      = 1 << 30
	maxMoofSize      = 64 << 20
	maxSmallBox      = 1 << 20
	maxDepth         = 12
	maxTracks        = 256
	maxFragSamples   = 20_000_000
	maxTopLevelBoxes = 4_000_000
)

// Errors returned by Open.
var (
	ErrNotISOBMFF = errors.New("isobmff: not an ISO base media file")
	ErrNoMoov     = errors.New("isobmff: no moov box found")
)

// File is the parsed structure of an MP4/MOV file.
type File struct {
	MajorBrand       string
	MinorVersion     uint32
	CompatibleBrands []string
	QuickTime        bool // QuickTime brand, or no ftyp at all
	FastStart        bool // moov precedes the first mdat
	Fragmented       bool // has mvex or moof boxes
	Timescale        uint32
	Duration         uint64 // mvhd duration, movie timescale
	FragmentDuration uint64 // mehd, movie timescale (0 if absent)
	CreationTime     time.Time
	Tracks           []*Track
	Tags             map[string]string
	Gapless          *Gapless // iTunes gapless information, nil if absent
	MdatBytes        int64

	r         io.ReaderAt
	size      int64
	emptyFrag int
}

// Edit is one edit list entry.
type Edit struct {
	SegmentDuration uint64 // movie timescale
	MediaTime       int64  // media timescale; -1 = empty edit
	Rate            int32  // 16.16
}

// Track is one trak box.
type Track struct {
	ID           uint32
	Enabled      bool
	Handler      string // "vide", "soun", "sbtl", "text", "subt", "meta", ...
	HandlerName  string
	Timescale    uint32
	Duration     uint64 // mdhd duration, media timescale
	TkhdDuration uint64 // movie timescale
	Language     string // ISO 639-2, "und" if unset
	Matrix       [9]int32
	Width        uint32 // tkhd, 16.16
	Height       uint32
	Edits        []Edit
	Entries      []*SampleEntry
	CreationTime time.Time
	Name         string

	// Stats, valid after Open (sample tables and fragments combined).
	SampleCount   int64
	MediaDuration uint64 // sum of sample durations, media timescale
	TotalBytes    int64
	FirstCTS      int64  // composition time of the first sample
	MinCTS        int64  // smallest composition time seen in the first samples
	CommonDelta   uint32 // most frequent sample duration
	VariableRate  bool   // sample durations differ (ignoring the last sample)
	SyncCount     int64  // -1 if every sample is a sync sample

	stts, ctts, stsc, stsz, stco, stss []byte
	stz2, co64                         bool
	cttsV1                             bool
	trex                               trexDefaults
	frag                               []Sample
	fragNextDTS                        int64
	file                               *File
}

type trexDefaults struct {
	descIndex, duration, size, flags uint32
}

// SampleEntry is one stsd entry.
type SampleEntry struct {
	Format   string // fourcc, e.g. "avc1"; for encrypted entries the original format
	Boxes    map[string][]byte
	Width    int // visual
	Height   int
	Depth    int
	Encoder  string // compressor name
	Channels int    // audio
	Bits     int
	Rate     float64
	// QuickTime sound description extras.
	QTVersion     int
	BytesPerFrame int
	LPCMFlags     uint32 // version 2 formatSpecificFlags
}

// Sniff reports whether head (the first bytes of a file) looks like ISOBMFF.
func Sniff(head []byte) bool {
	if len(head) < 8 {
		return false
	}
	switch string(head[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "styp", "pnot", "moof":
		return true
	}
	return false
}

var be = binary.BigEndian

// Open parses the box structure of an MP4/MOV file. It reads the moov and
// moof boxes but never the media data.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	f := &File{r: r, size: size, Tags: map[string]string{}}
	var (
		off       int64
		hdr       [16]byte
		sawFtyp   bool
		sawMoov   bool
		sawMdat   bool
		recognise bool
	)
	for n := 0; off+8 <= size; n++ {
		if n > maxTopLevelBoxes {
			break
		}
		if n&0xff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		want := 16
		if size-off < 16 {
			want = int(size - off)
		}
		if _, err := r.ReadAt(hdr[:want], off); err != nil && err != io.EOF {
			return nil, err
		}
		bsize := int64(be.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hlen := int64(8)
		switch bsize {
		case 0:
			bsize = size - off
		case 1:
			if want < 16 {
				bsize = -1
				break
			}
			u := be.Uint64(hdr[8:16])
			if u > uint64(size) {
				bsize = size - off // truncated; clamp
			} else {
				bsize = int64(u)
			}
			hlen = 16
		}
		if bsize < hlen {
			break
		}
		truncated := false
		if off+bsize > size {
			bsize = size - off
			truncated = true
		}
		payload := bsize - hlen
		switch typ {
		case "ftyp", "styp":
			recognise = true
			if typ == "ftyp" && !sawFtyp && payload >= 8 && payload <= maxSmallBox {
				b, err := readFull(r, off+hlen, payload)
				if err != nil {
					return nil, err
				}
				sawFtyp = true
				f.MajorBrand = string(b[:4])
				f.MinorVersion = be.Uint32(b[4:8])
				for i := 8; i+4 <= len(b); i += 4 {
					f.CompatibleBrands = append(f.CompatibleBrands, string(b[i:i+4]))
				}
			}
		case "moov":
			recognise = true
			if sawMoov {
				break
			}
			if payload > maxMoovSize {
				return nil, fmt.Errorf("isobmff: moov box too large (%d bytes)", payload)
			}
			b, err := readFull(r, off+hlen, payload)
			if err != nil {
				return nil, err
			}
			sawMoov = true
			f.FastStart = !sawMdat
			f.QuickTime = !sawFtyp || f.MajorBrand == "qt  "
			if err := f.parseMoov(b); err != nil {
				return nil, err
			}
		case "moof":
			recognise = true
			f.Fragmented = true
			if !sawMoov || payload > maxMoofSize {
				break
			}
			b, err := readFull(r, off+hlen, payload)
			if err != nil {
				return nil, err
			}
			if err := f.parseMoof(b, off); err != nil {
				return nil, err
			}
		case "mdat":
			recognise = true
			sawMdat = true
			f.MdatBytes += payload
		case "free", "skip", "wide", "pnot", "sidx", "mfra", "uuid", "meta", "prft", "emsg":
			recognise = true
		}
		if n == 0 && !recognise {
			return nil, ErrNotISOBMFF
		}
		if truncated {
			break
		}
		off += bsize
	}
	if !sawMoov {
		if !recognise {
			return nil, ErrNotISOBMFF
		}
		return nil, ErrNoMoov
	}
	for _, t := range f.Tracks {
		t.finishStats()
	}
	return f, nil
}

func readFull(r io.ReaderAt, off, n int64) ([]byte, error) {
	b := make([]byte, n)
	got, err := r.ReadAt(b, off)
	if int64(got) < n {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// walk calls fn for each child box in b. Malformed trailing data ends the
// walk without error, so truncated files keep what was parsed.
func walk(b []byte, fn func(typ string, payload []byte) error) error {
	for len(b) >= 8 {
		size := uint64(be.Uint32(b[:4]))
		typ := string(b[4:8])
		hlen := uint64(8)
		switch size {
		case 0:
			size = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return nil
			}
			size = be.Uint64(b[8:16])
			hlen = 16
		}
		if size < hlen || size > uint64(len(b)) {
			return nil
		}
		if err := fn(typ, b[hlen:size]); err != nil {
			return err
		}
		b = b[size:]
	}
	return nil
}

// mp4Time converts seconds since 1904-01-01 to a time.Time.
func mp4Time(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	const epochDiff = 2082844800
	return time.Unix(int64(v)-epochDiff, 0).UTC()
}

func (f *File) parseMoov(b []byte) error {
	return walk(b, func(typ string, p []byte) error {
		switch typ {
		case "mvhd":
			if len(p) < 4 {
				return nil
			}
			if p[0] == 1 && len(p) >= 32 {
				f.CreationTime = mp4Time(be.Uint64(p[4:]))
				f.Timescale = be.Uint32(p[20:])
				f.Duration = be.Uint64(p[24:])
			} else if p[0] == 0 && len(p) >= 20 {
				f.CreationTime = mp4Time(uint64(be.Uint32(p[4:])))
				f.Timescale = be.Uint32(p[12:])
				f.Duration = uint64(be.Uint32(p[16:]))
			}
		case "trak":
			if len(f.Tracks) >= maxTracks {
				return nil
			}
			t := &Track{file: f, Language: "und", SyncCount: -1}
			t.Matrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
			if err := t.parse(p, 0); err != nil {
				return err
			}
			if t.Timescale == 0 {
				t.Timescale = 1
			}
			f.Tracks = append(f.Tracks, t)
		case "mvex":
			f.Fragmented = true
			return walk(p, func(typ string, p []byte) error {
				switch typ {
				case "mehd":
					if len(p) >= 12 && p[0] == 1 {
						f.FragmentDuration = be.Uint64(p[4:])
					} else if len(p) >= 8 {
						f.FragmentDuration = uint64(be.Uint32(p[4:]))
					}
				case "trex":
					if len(p) >= 24 {
						id := be.Uint32(p[4:])
						for _, t := range f.Tracks {
							if t.ID == id {
								t.trex = trexDefaults{be.Uint32(p[8:]), be.Uint32(p[12:]), be.Uint32(p[16:]), be.Uint32(p[20:])}
							}
						}
					}
				}
				return nil
			})
		case "udta":
			f.parseUdta(p)
		case "meta":
			f.parseMeta(p)
		}
		return nil
	})
}

func (t *Track) parse(b []byte, depth int) error {
	if depth > maxDepth {
		return nil
	}
	return walk(b, func(typ string, p []byte) error {
		switch typ {
		case "tkhd":
			if len(p) < 4 {
				return nil
			}
			flags := be.Uint32(p[:4]) & 0xffffff
			t.Enabled = flags&1 != 0
			var rest []byte
			if p[0] == 1 && len(p) >= 36 {
				t.CreationTime = mp4Time(be.Uint64(p[4:]))
				t.ID = be.Uint32(p[20:])
				t.TkhdDuration = be.Uint64(p[28:])
				rest = p[36:]
			} else if p[0] == 0 && len(p) >= 24 {
				t.CreationTime = mp4Time(uint64(be.Uint32(p[4:])))
				t.ID = be.Uint32(p[12:])
				t.TkhdDuration = uint64(be.Uint32(p[20:]))
				rest = p[24:]
			}
			// reserved(8) layer(2) altgroup(2) volume(2) reserved(2) matrix(36) width(4) height(4)
			if len(rest) >= 60 {
				for i := range t.Matrix {
					t.Matrix[i] = int32(be.Uint32(rest[16+4*i:]))
				}
				t.Width = be.Uint32(rest[52:])
				t.Height = be.Uint32(rest[56:])
			}
		case "edts":
			return walk(p, func(typ string, p []byte) error {
				if typ != "elst" || len(p) < 8 {
					return nil
				}
				n := int(be.Uint32(p[4:]))
				q := p[8:]
				esz := 12
				if p[0] == 1 {
					esz = 20
				}
				if n > len(q)/esz {
					n = len(q) / esz
				}
				for i := 0; i < n; i++ {
					e := q[i*esz:]
					var ed Edit
					if p[0] == 1 {
						ed = Edit{be.Uint64(e), int64(be.Uint64(e[8:])), int32(be.Uint32(e[16:]))}
					} else {
						ed = Edit{uint64(be.Uint32(e)), int64(int32(be.Uint32(e[4:]))), int32(be.Uint32(e[8:]))}
					}
					t.Edits = append(t.Edits, ed)
				}
				return nil
			})
		case "mdia", "minf", "stbl":
			return t.parse(p, depth+1)
		case "mdhd":
			if len(p) < 4 {
				return nil
			}
			var lang uint16
			if p[0] == 1 && len(p) >= 34 {
				t.Timescale = be.Uint32(p[20:])
				t.Duration = be.Uint64(p[24:])
				lang = be.Uint16(p[32:])
			} else if p[0] == 0 && len(p) >= 22 {
				t.Timescale = be.Uint32(p[12:])
				t.Duration = uint64(be.Uint32(p[16:]))
				lang = be.Uint16(p[20:])
			}
			t.Language = decodeLang(lang)
		case "hdlr":
			// The mdia-level hdlr names the media type; minf may hold a
			// QuickTime "dhlr" data handler that must not override it.
			if len(p) >= 12 && t.Handler == "" {
				t.Handler = string(p[8:12])
				if len(p) > 24 {
					name := p[24:]
					if len(name) > 0 && int(name[0]) == len(name)-1 { // Pascal string (QuickTime)
						name = name[1:]
					}
					for len(name) > 0 && name[len(name)-1] == 0 {
						name = name[:len(name)-1]
					}
					t.HandlerName = string(name)
				}
			}
		case "stsd":
			t.parseStsd(p)
		case "stts":
			t.stts = table(p, 8, 8)
		case "ctts":
			t.ctts = table(p, 8, 8)
			t.cttsV1 = len(p) > 0 && p[0] == 1
		case "stsc":
			t.stsc = table(p, 8, 12)
		case "stss":
			t.stss = table(p, 8, 4)
			t.SyncCount = int64(len(t.stss) / 4)
		case "stsz":
			if len(p) >= 12 {
				t.stsz = p
			}
		case "stz2":
			if len(p) >= 12 {
				t.stsz, t.stz2 = p, true
			}
		case "stco":
			t.stco = table(p, 8, 4)
		case "co64":
			t.stco, t.co64 = table(p, 8, 8), true
		case "udta":
			walk(p, func(typ string, p []byte) error {
				if typ == "name" {
					t.Name = string(p)
				}
				return nil
			})
		}
		return nil
	})
}

// table returns the entry bytes of a FullBox table whose entry count is the
// uint32 at hdr-4, clamped to the bytes actually present.
func table(p []byte, hdr, entry int) []byte {
	if len(p) < hdr {
		return nil
	}
	n := uint64(be.Uint32(p[hdr-4:]))
	body := p[hdr:]
	if n*uint64(entry) > uint64(len(body)) {
		n = uint64(len(body) / entry)
	}
	return body[:n*uint64(entry)]
}

func decodeLang(v uint16) string {
	if v == 0 || v == 0x7fff || v == 0x55c4 { // 0x55c4 = "und"
		return "und"
	}
	if v < 0x400 { // QuickTime Macintosh language code
		if v == 0 {
			return "eng"
		}
		return "und"
	}
	b := []byte{byte(v>>10&31) + 0x60, byte(v>>5&31) + 0x60, byte(v&31) + 0x60}
	for _, c := range b {
		if c < 'a' || c > 'z' {
			return "und"
		}
	}
	return string(b)
}

func (t *Track) parseStsd(p []byte) {
	if len(p) < 8 {
		return
	}
	n := int(be.Uint32(p[4:]))
	b := p[8:]
	for i := 0; i < n && i < 64 && len(b) >= 16; i++ {
		size := int(be.Uint32(b))
		if size < 16 || size > len(b) {
			size = len(b)
		}
		e := &SampleEntry{Format: string(b[4:8]), Boxes: map[string][]byte{}}
		body := b[16:size] // after size, format, reserved(6), data_reference_index(2)
		qt := t.file.QuickTime
		switch t.Handler {
		case "vide":
			if len(body) >= 70 {
				e.Width = int(be.Uint16(body[16:]))
				e.Height = int(be.Uint16(body[18:]))
				nl := int(body[34])
				if nl > 31 {
					nl = 31
				}
				e.Encoder = string(body[35 : 35+nl])
				e.Depth = int(be.Uint16(body[66:]))
				e.collect(body[70:], 0)
			}
		case "soun":
			if len(body) >= 20 {
				ver := int(be.Uint16(body))
				e.QTVersion = ver
				e.Channels = int(be.Uint16(body[8:]))
				e.Bits = int(be.Uint16(body[10:]))
				e.Rate = float64(be.Uint32(body[16:])) / 65536
				rest := body[20:]
				switch {
				case ver == 1 && qt && len(rest) >= 16:
					e.BytesPerFrame = int(be.Uint32(rest[8:]))
					rest = rest[16:]
				case ver == 2 && len(rest) >= 36:
					e.Rate = float64frombits(be.Uint64(rest[4:]))
					e.Channels = int(be.Uint32(rest[12:]))
					e.Bits = int(be.Uint32(rest[20:]))
					e.LPCMFlags = be.Uint32(rest[24:])
					e.BytesPerFrame = 0
					rest = rest[36:]
				}
				e.collect(rest, 0)
				if srat := e.Boxes["srat"]; len(srat) >= 8 {
					e.Rate = float64(be.Uint32(srat[4:]))
				}
			}
		default:
			e.Boxes[""] = body
			switch e.Format {
			case "wvtt", "stpp", "sbtt", "stxt":
				e.collect(body, 0)
			}
		}
		// Encrypted entries: recover the original format from sinf/frma.
		if sinf := e.Boxes["sinf"]; sinf != nil {
			walk(sinf, func(typ string, p []byte) error {
				if typ == "frma" && len(p) >= 4 {
					e.Format = string(p[:4])
				}
				return nil
			})
		}
		t.Entries = append(t.Entries, e)
		b = b[size:]
	}
}

// collect records child boxes by type (first occurrence wins) and descends
// into QuickTime 'wave' atoms.
func (e *SampleEntry) collect(b []byte, depth int) {
	walk(b, func(typ string, p []byte) error {
		if typ == "wave" && depth < 2 {
			e.collect(p, depth+1)
			return nil
		}
		if _, dup := e.Boxes[typ]; !dup && len(e.Boxes) < 64 {
			e.Boxes[typ] = p
		}
		return nil
	})
}

// --- fragments --------------------------------------------------------------

func (f *File) track(id uint32) *Track {
	for _, t := range f.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (f *File) parseMoof(b []byte, moofOff int64) error {
	var total int
	for _, t := range f.Tracks {
		total += len(t.frag)
	}
	var prevEnd int64 = -1 // end of the previous traf's data, for the legacy default base
	return walk(b, func(typ string, p []byte) error {
		if typ != "traf" {
			return nil
		}
		var (
			t        *Track
			flags    uint32
			base     = moofOff
			defDur   uint32
			defSize  uint32
			defFlags uint32
			dataOff  int64
		)
		return walk(p, func(typ string, p []byte) error {
			switch typ {
			case "tfhd":
				if len(p) < 8 {
					return nil
				}
				flags = be.Uint32(p) & 0xffffff
				t = f.track(be.Uint32(p[4:]))
				if t == nil {
					return nil
				}
				defDur, defSize, defFlags = t.trex.duration, t.trex.size, t.trex.flags
				q := p[8:]
				next := func(n int) uint64 {
					if len(q) < n {
						q = nil
						return 0
					}
					var v uint64
					if n == 8 {
						v = be.Uint64(q)
					} else {
						v = uint64(be.Uint32(q))
					}
					q = q[n:]
					return v
				}
				if flags&0x1 != 0 {
					base = int64(next(8))
				} else if flags&0x20000 == 0 && prevEnd >= 0 {
					base = prevEnd
				}
				if flags&0x2 != 0 {
					next(4)
				}
				if flags&0x8 != 0 {
					defDur = uint32(next(4))
				}
				if flags&0x10 != 0 {
					defSize = uint32(next(4))
				}
				if flags&0x20 != 0 {
					defFlags = uint32(next(4))
				}
				dataOff = base
			case "tfdt":
				if t == nil || len(p) < 8 {
					return nil
				}
				if p[0] == 1 && len(p) >= 12 {
					t.fragNextDTS = int64(be.Uint64(p[4:]))
				} else {
					t.fragNextDTS = int64(be.Uint32(p[4:]))
				}
			case "trun":
				if t == nil || len(p) < 8 {
					return nil
				}
				tf := be.Uint32(p) & 0xffffff
				n := int(be.Uint32(p[4:]))
				q := p[8:]
				if tf&0x1 != 0 {
					if len(q) < 4 {
						return nil
					}
					dataOff = base + int64(int32(be.Uint32(q)))
					q = q[4:]
				}
				firstFlags, hasFirst := uint32(0), false
				if tf&0x4 != 0 {
					if len(q) < 4 {
						return nil
					}
					firstFlags, hasFirst = be.Uint32(q), true
					q = q[4:]
				}
				per := 0
				for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
					if tf&bit != 0 {
						per += 4
					}
				}
				if per > 0 && n > len(q)/per {
					n = len(q) / per
				}
				if per == 0 && n > 1<<20 {
					n = 1 << 20
				}
				if total+n > maxFragSamples {
					return fmt.Errorf("isobmff: too many fragment samples")
				}
				total += n
				for i := 0; i < n; i++ {
					s := Sample{Offset: dataOff, Size: defSize, Dur: defDur, DTS: t.fragNextDTS}
					sf := defFlags
					if tf&0x100 != 0 {
						s.Dur = be.Uint32(q)
						q = q[4:]
					}
					if tf&0x200 != 0 {
						s.Size = be.Uint32(q)
						q = q[4:]
					}
					if tf&0x400 != 0 {
						sf = be.Uint32(q)
						q = q[4:]
					} else if i == 0 && hasFirst {
						sf = firstFlags
					}
					if tf&0x800 != 0 {
						s.CTSOffset = int32(be.Uint32(q)) // unsigned in version 0, but int32 is ample
						q = q[4:]
					}
					s.Sync = sf&0x10000 == 0 // sample_is_non_sync_sample
					// Samples must lie inside the file, and only a bounded
					// number may be empty; otherwise a tiny file could claim
					// millions of samples.
					if s.Size == 0 {
						if f.emptyFrag++; f.emptyFrag > 1<<16 {
							return nil
						}
					} else if dataOff < 0 || dataOff+int64(s.Size) > f.size {
						return nil
					}
					t.frag = append(t.frag, s)
					dataOff += int64(s.Size)
					t.fragNextDTS += int64(s.Dur)
				}
				prevEnd = dataOff
			}
			return nil
		})
	})
}
