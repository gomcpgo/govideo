// Package matroska is a read-only Matroska/WebM demuxer written from
// RFC 8794 (EBML) and the Matroska specification.
//
// Open parses the Segment metadata and scans every Cluster, reading element
// and block headers only (never frame payloads, except the first few bytes of
// each track's first frame), so files without Duration, Cues or known element
// sizes (browser MediaRecorder output) are fully described. PacketReader
// iterates frames for the editing pipeline.
//
// The parser is safe on hostile input: all sizes are checked against the file
// size, allocations and entry counts are capped, the element structure is
// walked without recursion, and truncated files yield whatever was parsed.
package matroska

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Track types.
const (
	TrackVideo    = 1
	TrackAudio    = 2
	TrackSubtitle = 17
)

const (
	maxTracks       = 1024
	maxCodecPrivate = 16 << 20
	maxString       = 4096
	maxTags         = 4096
	maxStatTracks   = 256
	maxVFRSamples   = 1 << 21 // timestamps kept for VFR detection, all tracks
	firstFrameLen   = 64
)

// ErrNotMatroska is returned by Open when the data does not start with an
// EBML header or contains no Segment.
var ErrNotMatroska = errors.New("matroska: not a Matroska/WebM file")

// Sniff reports whether head starts with the EBML magic number.
func Sniff(head []byte) bool {
	return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
}

// File is an opened Matroska or WebM file.
type File struct {
	DocType        string // "matroska" or "webm"
	TimestampScale uint64 // nanoseconds per tick (default 1_000_000)
	// Duration comes from Info.Duration. If that is absent it is the end of
	// the last block found by scanning (see DurationFromScan).
	Duration         time.Duration
	DurationFromScan bool
	Title            string
	MuxingApp        string
	WritingApp       string
	DateUTC          time.Time // zero if absent
	Tracks           []*Track
	Tags             map[string]string // global tags, lower-cased names

	r        io.ReaderAt
	size     int64
	segStart int64
	segEnd   int64
	byNum    map[uint64]*Track
}

// Colour is the video Colour element. Code points follow ITU-T H.273.
type Colour struct {
	MatrixCoefficients      int // 2 = unspecified
	BitsPerChannel          int
	ChromaSubsamplingHorz   int
	ChromaSubsamplingVert   int
	Range                   int // 0 unspecified, 1 limited, 2 full
	TransferCharacteristics int // 2 = unspecified
	Primaries               int // 2 = unspecified
	MaxCLL, MaxFALL         int
	HasMastering            bool
}

// Track is one TrackEntry plus statistics gathered by Open's header scan.
type Track struct {
	Number uint64
	UID    uint64
	Type   int // TrackVideo, TrackAudio, TrackSubtitle or other

	CodecID      string // "V_VP9", "A_OPUS", "V_MPEG4/ISO/AVC", ...
	CodecPrivate []byte
	Name         string
	Language     string // LanguageBCP47, else Language, else "eng"
	Default      bool
	Enabled      bool
	Forced       bool

	DefaultDuration time.Duration // per frame; 0 if absent
	CodecDelay      time.Duration
	SeekPreRoll     time.Duration

	// Video.
	PixelWidth, PixelHeight     int
	DisplayWidth, DisplayHeight int // default to the pixel size when DisplayUnit is 0
	DisplayUnit                 int
	Interlaced                  bool
	Colour                      *Colour
	ProjectionRoll              float64
	HasProjection               bool

	// Audio.
	SamplingFrequency       float64
	OutputSamplingFrequency float64
	Channels                int
	BitDepth                int

	// Statistics from the header scan.
	Frames         int64
	FirstTimestamp time.Duration // smallest block timestamp
	LastTimestamp  time.Duration // largest block timestamp
	HasTimestamps  bool
	TotalBytes     int64  // sum of block payload sizes
	FirstFrame     []byte // first ≤ 64 bytes of the first frame (nil if compressed)
	VFR            bool   // video timestamps do not fit a constant frame rate

	// Tags holds tags targeting this track (lower-cased names).
	Tags map[string]string
	// Err is non-nil when the track's frames use a ContentEncoding this
	// package cannot undo (encryption, unknown compression). PacketReader
	// skips the packets of such tracks.
	Err error

	strip []byte // header-stripping compression: bytes to prepend to frames
	zlib  bool   // frames are zlib-compressed
}

type trackStats struct {
	frames     int64
	minTS      int64
	maxTS      int64
	maxTSDur   int64 // BlockDuration of the block at maxTS, -1 if none
	bytes      int64
	first      []byte
	haveFirst  bool
	timestamps []int64
}

type pendingTag struct {
	uids []uint64 // TagTrackUIDs; empty = global
	skip bool     // targets an edition/chapter/attachment
	kv   [][2]string
}

// Open parses the file's metadata and scans its clusters. A truncated file is
// not an error: Open returns what could be parsed.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*File, error) {
	if r == nil || size < 4 {
		return nil, ErrNotMatroska
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rd := &reader{r: r, size: size}
	f := &File{DocType: "matroska", TimestampScale: 1_000_000, r: r, size: size,
		Tags: map[string]string{}, byNum: map[uint64]*Track{}}

	id, hsize, h, ok := rd.header(0)
	if !ok || id != idEBML || hsize < 0 {
		if rd.err != nil {
			return nil, rd.err
		}
		return nil, ErrNotMatroska
	}
	pos := int64(h)
	hend := size
	if hsize < size-pos {
		hend = pos + hsize
	}
	if err := rd.each(ctx, pos, hend, func(id uint32, off, sz int64) {
		if id == idDocType {
			f.DocType = rd.str(off, sz, 64)
		}
	}); err != nil {
		return nil, err
	}

	// Find the Segment, skipping Void and other top-level elements.
	pos = hend
	found := false
	for n := 0; pos < size && n < 1024; n++ {
		id, sz, h, ok := rd.header(pos)
		if !ok {
			break
		}
		data := pos + int64(h)
		if id == idSegment {
			f.segStart, f.segEnd = data, size
			if sz >= 0 && sz < size-data {
				f.segEnd = data + sz
			}
			found = true
			break
		}
		if sz < 0 || sz > size-data {
			break
		}
		pos = data + sz
	}
	if rd.err != nil {
		return nil, rd.err
	}
	if !found {
		return nil, ErrNotMatroska
	}

	var (
		haveInfo, haveTracks bool
		infoDur              float64
		tags                 []pendingTag
		nTags                int
		stats                = map[uint64]*trackStats{}
		vfrBudget            = maxVFRSamples
		perr                 error
	)
	w := &walker{rd: rd, ctx: ctx, pos: f.segStart, segEnd: f.segEnd}
	w.onTop = func(id uint32, off, sz int64) {
		if perr != nil {
			return
		}
		switch id {
		case idInfo:
			if !haveInfo {
				haveInfo = true
				infoDur, perr = f.parseInfo(ctx, rd, off, sz)
			}
		case idTracks:
			if !haveTracks {
				haveTracks = true
				perr = f.parseTracks(ctx, rd, off, sz)
			}
		case idTags:
			perr = rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
				if id != idTag || nTags >= maxTags {
					return
				}
				t := parseTag(ctx, rd, off, sz, &nTags)
				tags = append(tags, t)
			})
		}
	}
	for {
		b, ok := w.next()
		if !ok || perr != nil {
			break
		}
		st := stats[b.track]
		if st == nil {
			if len(stats) >= maxStatTracks {
				continue
			}
			st = &trackStats{minTS: b.ts, maxTS: b.ts, maxTSDur: b.dur}
			stats[b.track] = st
		}
		st.frames += int64(b.laced) + 1
		st.bytes += b.size
		if b.ts < st.minTS {
			st.minTS = b.ts
		}
		if b.ts >= st.maxTS {
			st.maxTS, st.maxTSDur = b.ts, b.dur
		}
		if t := f.byNum[b.track]; (t == nil || t.Type == TrackVideo) && vfrBudget > 0 {
			st.timestamps = append(st.timestamps, b.ts)
			vfrBudget--
		}
		if !st.haveFirst {
			st.haveFirst = true
			st.first = firstFrame(rd, b)
		}
	}
	if perr != nil {
		return nil, perr
	}
	if w.err != nil {
		return nil, w.err
	}
	if rd.err != nil {
		return nil, rd.err
	}

	// Attach statistics and derive the duration.
	var scanEnd int64
	anyBlocks := false
	for _, t := range f.Tracks {
		st := stats[t.Number]
		if st == nil || st.frames == 0 {
			continue
		}
		anyBlocks = true
		t.Frames = st.frames
		t.TotalBytes = st.bytes
		t.HasTimestamps = true
		t.FirstTimestamp = f.ticks(st.minTS)
		t.LastTimestamp = f.ticks(st.maxTS)
		if t.Err == nil && !t.zlib && st.first != nil {
			ff := append(append([]byte(nil), t.strip...), st.first...)
			if len(ff) > firstFrameLen {
				ff = ff[:firstFrameLen]
			}
			t.FirstFrame = ff
		}
		if t.Type == TrackVideo {
			t.VFR = isVFR(st.timestamps)
		}
		dur := st.maxTSDur
		if dur < 0 {
			switch {
			case t.DefaultDuration > 0:
				dur = int64(uint64(t.DefaultDuration) / f.TimestampScale)
			case st.frames > 1:
				dur = (st.maxTS - st.minTS) / (st.frames - 1)
			default:
				dur = 0
			}
		}
		if end := st.maxTS + dur; end > scanEnd {
			scanEnd = end
		}
	}
	switch {
	case infoDur > 0 && !math.IsInf(infoDur, 0) && !math.IsNaN(infoDur):
		ns := infoDur * float64(f.TimestampScale)
		if ns >= math.MaxInt64 {
			f.Duration = math.MaxInt64
		} else {
			f.Duration = time.Duration(ns)
		}
	case anyBlocks:
		f.Duration = f.ticks(scanEnd)
		f.DurationFromScan = true
	}

	for _, tg := range tags {
		if tg.skip {
			continue
		}
		if len(tg.uids) == 0 {
			for _, kv := range tg.kv {
				f.Tags[kv[0]] = kv[1]
			}
			continue
		}
		for _, t := range f.Tracks {
			if t.UID == 0 || !slices.Contains(tg.uids, t.UID) {
				continue
			}
			if t.Tags == nil {
				t.Tags = map[string]string{}
			}
			for _, kv := range tg.kv {
				t.Tags[kv[0]] = kv[1]
			}
		}
	}
	return f, nil
}

// ticks converts segment ticks to a duration, saturating on overflow.
func (f *File) ticks(t int64) time.Duration {
	if t <= 0 {
		return 0
	}
	if uint64(t) > math.MaxInt64/f.TimestampScale {
		return math.MaxInt64
	}
	return time.Duration(uint64(t) * f.TimestampScale)
}

func nsDuration(v uint64) time.Duration {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(v)
}

func capInt(v uint64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

func (f *File) parseInfo(ctx context.Context, rd *reader, off, size int64) (dur float64, err error) {
	err = rd.each(ctx, off, off+size, func(id uint32, off, sz int64) {
		switch id {
		case idTimestampScale:
			if v := rd.uint(off, sz); v > 0 {
				f.TimestampScale = v
			}
		case idDuration:
			dur = rd.float(off, sz)
		case idDateUTC:
			if sz == 8 {
				epoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
				f.DateUTC = epoch.Add(time.Duration(rd.sint(off, sz)))
			}
		case idTitle:
			f.Title = rd.str(off, sz, maxString)
		case idMuxingApp:
			f.MuxingApp = rd.str(off, sz, maxString)
		case idWritingApp:
			f.WritingApp = rd.str(off, sz, maxString)
		}
	})
	return dur, err
}

func (f *File) parseTracks(ctx context.Context, rd *reader, off, size int64) error {
	var ierr error
	err := rd.each(ctx, off, off+size, func(id uint32, off, sz int64) {
		if id != idTrackEntry || len(f.Tracks) >= maxTracks || ierr != nil {
			return
		}
		t := &Track{Default: true, Enabled: true}
		var lang, bcp string
		haveDW, haveDH, haveOut := false, false, false
		ierr = rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
			switch id {
			case idTrackNumber:
				t.Number = rd.uint(off, sz)
			case idTrackUID:
				t.UID = rd.uint(off, sz)
			case idTrackType:
				t.Type = capInt(rd.uint(off, sz))
			case idFlagEnabled:
				t.Enabled = rd.uint(off, sz) != 0
			case idFlagDefault:
				t.Default = rd.uint(off, sz) != 0
			case idFlagForced:
				t.Forced = rd.uint(off, sz) != 0
			case idDefaultDuration:
				t.DefaultDuration = nsDuration(rd.uint(off, sz))
			case idName:
				t.Name = rd.str(off, sz, maxString)
			case idLanguage:
				lang = rd.str(off, sz, 64)
			case idLanguageBCP47:
				bcp = rd.str(off, sz, 64)
			case idCodecID:
				t.CodecID = rd.str(off, sz, 256)
			case idCodecPrivate:
				t.CodecPrivate = rd.bytes(off, sz, maxCodecPrivate)
			case idCodecDelay:
				t.CodecDelay = nsDuration(rd.uint(off, sz))
			case idSeekPreRoll:
				t.SeekPreRoll = nsDuration(rd.uint(off, sz))
			case idVideo:
				rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
					switch id {
					case idFlagInterlaced:
						t.Interlaced = rd.uint(off, sz) == 1
					case idPixelWidth:
						t.PixelWidth = capInt(rd.uint(off, sz))
					case idPixelHeight:
						t.PixelHeight = capInt(rd.uint(off, sz))
					case idDisplayWidth:
						t.DisplayWidth, haveDW = capInt(rd.uint(off, sz)), true
					case idDisplayHeight:
						t.DisplayHeight, haveDH = capInt(rd.uint(off, sz)), true
					case idDisplayUnit:
						t.DisplayUnit = capInt(rd.uint(off, sz))
					case idColour:
						t.Colour = parseColour(ctx, rd, off, sz)
					case idProjection:
						t.HasProjection = true
						rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
							if id == idProjectionPoseRoll {
								if v := rd.float(off, sz); !math.IsNaN(v) && !math.IsInf(v, 0) {
									t.ProjectionRoll = v
								}
							}
						})
					}
				})
			case idAudio:
				t.SamplingFrequency, t.Channels = 8000, 1
				rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
					switch id {
					case idSamplingFrequency:
						t.SamplingFrequency = rd.float(off, sz)
					case idOutputSamplingFrequency:
						t.OutputSamplingFrequency, haveOut = rd.float(off, sz), true
					case idChannels:
						t.Channels = capInt(rd.uint(off, sz))
					case idBitDepth:
						t.BitDepth = capInt(rd.uint(off, sz))
					}
				})
			case idContentEncs:
				parseEncodings(ctx, rd, off, sz, t)
			}
		})
		switch {
		case bcp != "":
			t.Language = bcp
		case lang != "":
			t.Language = lang
		default:
			t.Language = "eng"
		}
		if t.DisplayUnit == 0 {
			if !haveDW {
				t.DisplayWidth = t.PixelWidth
			}
			if !haveDH {
				t.DisplayHeight = t.PixelHeight
			}
		}
		if t.Type == TrackAudio && !haveOut {
			t.OutputSamplingFrequency = t.SamplingFrequency
		}
		f.Tracks = append(f.Tracks, t)
		if _, dup := f.byNum[t.Number]; !dup {
			f.byNum[t.Number] = t
		}
	})
	if err == nil {
		err = ierr
	}
	return err
}

func parseColour(ctx context.Context, rd *reader, off, size int64) *Colour {
	c := &Colour{MatrixCoefficients: 2, TransferCharacteristics: 2, Primaries: 2}
	rd.each(ctx, off, off+size, func(id uint32, off, sz int64) {
		v := 0
		if sz <= 8 {
			v = capInt(rd.uint(off, sz))
		}
		switch id {
		case idMatrixCoefficients:
			c.MatrixCoefficients = v
		case idBitsPerChannel:
			c.BitsPerChannel = v
		case idChromaSubHorz:
			c.ChromaSubsamplingHorz = v
		case idChromaSubVert:
			c.ChromaSubsamplingVert = v
		case idRange:
			c.Range = v
		case idTransfer:
			c.TransferCharacteristics = v
		case idPrimaries:
			c.Primaries = v
		case idMaxCLL:
			c.MaxCLL = v
		case idMaxFALL:
			c.MaxFALL = v
		case idMasteringMetadata:
			c.HasMastering = true
		}
	})
	return c
}

// parseEncodings handles ContentEncodings. Supported for frames: a single
// compression encoding with algorithm 0 (zlib) or 3 (header stripping).
func parseEncodings(ctx context.Context, rd *reader, off, size int64, t *Track) {
	frameEncs := 0
	rd.each(ctx, off, off+size, func(id uint32, off, sz int64) {
		if id != idContentEncoding {
			return
		}
		scope, typ, algo := uint64(1), uint64(0), uint64(0)
		var settings []byte
		rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
			switch id {
			case idContentEncodingScope:
				scope = rd.uint(off, sz)
			case idContentEncodingType:
				typ = rd.uint(off, sz)
			case idContentCompression:
				rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
					switch id {
					case idContentCompAlgo:
						algo = rd.uint(off, sz)
					case idContentCompSettings:
						settings = rd.bytes(off, sz, maxString)
						if settings == nil && sz > 0 {
							algo = math.MaxUint64 // oversized settings: unsupported
						}
					}
				})
			}
		})
		if scope&1 == 0 {
			return // does not apply to frame data
		}
		frameEncs++
		switch {
		case frameEncs > 1:
			t.Err = errors.New("matroska: multiple content encodings are not supported")
		case typ != 0:
			t.Err = errors.New("matroska: encrypted track")
		case algo == 0:
			t.zlib = true
		case algo == 3:
			t.strip = settings
		default:
			t.Err = fmt.Errorf("matroska: unsupported content compression algorithm %d", algo)
		}
	})
}

func parseTag(ctx context.Context, rd *reader, off, size int64, n *int) pendingTag {
	var t pendingTag
	rd.each(ctx, off, off+size, func(id uint32, off, sz int64) {
		switch id {
		case idTargets:
			rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
				v := rd.uint(off, sz)
				switch id {
				case idTagTrackUID:
					if v != 0 && len(t.uids) < 64 {
						t.uids = append(t.uids, v)
					}
				case idTagEditionUID, idTagChapterUID, idTagAttachmentUID:
					if v != 0 {
						t.skip = true
					}
				}
			})
		case idSimpleTag:
			if *n >= maxTags {
				return
			}
			var name, val string
			rd.each(ctx, off, off+sz, func(id uint32, off, sz int64) {
				switch id {
				case idTagName:
					name = rd.str(off, sz, 256)
				case idTagString:
					val = rd.str(off, sz, maxString)
				}
			})
			if name != "" {
				*n++
				t.kv = append(t.kv, [2]string{strings.ToLower(name), val})
			}
		}
	})
	return t
}

// firstFrame reads the first bytes of the first frame in b.
func firstFrame(rd *reader, b block) []byte {
	off, size := b.off, b.size
	if b.lacing != laceNone {
		n := int64(4096)
		if size < n {
			n = size
		}
		sizes, hdr, ok := laceSizes(rd.at(off, int(n)), b.lacing, size)
		if !ok {
			return nil
		}
		off, size = off+int64(hdr), sizes[0]
	}
	if size > firstFrameLen {
		size = firstFrameLen
	}
	if size <= 0 {
		return nil
	}
	return append([]byte(nil), rd.at(off, int(size))...)
}

// isVFR reports whether timestamps (in ticks, any order) deviate from a
// constant-rate model. Timestamps are usually rounded to milliseconds, so a
// constant-rate stream stays within one tick of the model; anything beyond
// 1.5 ticks (or 2% of the frame period, if larger) counts as variable.
func isVFR(ts []int64) bool {
	n := len(ts)
	if n < 3 {
		return false
	}
	ts = slices.Clone(ts)
	slices.Sort(ts)
	period := float64(ts[n-1]-ts[0]) / float64(n-1)
	if period <= 0 {
		return false
	}
	tol := math.Max(1.5, period*0.02)
	for i, t := range ts {
		if math.Abs(float64(t-ts[0])-float64(i)*period) > tol {
			return true
		}
	}
	return false
}
