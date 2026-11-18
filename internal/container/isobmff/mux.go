package isobmff

import (
	"errors"
	"fmt"
	"io"
)

// Brand selects the flavour of file the Muxer writes.
type Brand int

const (
	BrandMP4 Brand = iota // ISO MP4 ("isom")
	BrandMOV              // QuickTime ("qt  ")
)

// TrackConfig describes one output track.
type TrackConfig struct {
	Handler   string // "vide", "soun" or "sbtl"
	Timescale uint32
	Entry     []byte // complete sample entry box for stsd
	Width     int    // presentation size (video and subtitle tracks)
	Height    int
	Language  string // ISO 639-2; "" = und
	Name      string // handler name
	// SampleSize > 0 declares a constant-size track (PCM): each sample is
	// SampleSize bytes lasting one timescale tick, and WriteSample may carry
	// any whole number of samples.
	SampleSize uint32
	AllSync    bool // every sample is a sync sample (no stss)
	Disabled   bool // clear the track_enabled flag
	AltGroup   int
}

type run struct{ count, value uint32 }

type muxTrack struct {
	cfg       TrackConfig
	count     uint64
	duration  uint64
	sizes     []uint32
	stts      []run
	ctts      []run
	hasCTS    bool
	sync      []uint32
	chunkOffs []int64
	stsc      []run // count = number of chunks with this samples-per-chunk; value = samples per chunk
	curChunk  uint32
}

// Muxer writes a faststart (moov-first) MP4 or MOV. Sample data is spooled
// to a scratch file; Finish assembles the final file. Memory use is limited
// to the sample tables.
type Muxer struct {
	brand   Brand
	scratch io.ReadWriteSeeker
	tracks  []*muxTrack
	last    int
	dataLen int64
	done    bool
}

// NewMuxer returns a Muxer that spools media data to scratch, which must be
// empty and positioned at its start.
func NewMuxer(brand Brand, scratch io.ReadWriteSeeker) *Muxer {
	return &Muxer{brand: brand, scratch: scratch, last: -1}
}

// AddTrack adds a track and returns its index. All tracks must be added
// before the first WriteSample.
func (m *Muxer) AddTrack(cfg TrackConfig) int {
	if cfg.Timescale == 0 {
		cfg.Timescale = 1000
	}
	m.tracks = append(m.tracks, &muxTrack{cfg: cfg})
	return len(m.tracks) - 1
}

func appendRun(r []run, v uint32, n uint32) []run {
	if k := len(r); k > 0 && r[k-1].value == v && uint64(r[k-1].count)+uint64(n) <= 0xffffffff {
		r[k-1].count += n
		return r
	}
	return append(r, run{n, v})
}

// WriteSample appends a sample to a track. dur is in the track timescale and
// ctsOffset is the composition offset. Samples should be written in
// interleaved order; consecutive samples of one track share a chunk. For
// constant-size tracks data may hold several samples and dur, sync and
// ctsOffset are ignored.
func (m *Muxer) WriteSample(track int, data []byte, dur uint32, sync bool, ctsOffset int32) error {
	if m.done {
		return errors.New("isobmff: muxer finished")
	}
	if track < 0 || track >= len(m.tracks) {
		return fmt.Errorf("isobmff: no track %d", track)
	}
	if len(data) == 0 {
		return nil
	}
	t := m.tracks[track]
	n := uint32(1)
	if ss := t.cfg.SampleSize; ss > 0 {
		if len(data)%int(ss) != 0 {
			return fmt.Errorf("isobmff: PCM write of %d bytes is not a multiple of %d", len(data), ss)
		}
		n = uint32(len(data) / int(ss))
		dur = 1
	}
	if m.last != track {
		m.flushChunk()
		t.chunkOffs = append(t.chunkOffs, m.dataLen)
		m.last = track
	}
	t.curChunk += n
	if _, err := m.scratch.Write(data); err != nil {
		return err
	}
	m.dataLen += int64(len(data))
	if t.cfg.SampleSize == 0 {
		t.sizes = append(t.sizes, uint32(len(data)))
		if !t.cfg.AllSync && sync {
			t.sync = append(t.sync, uint32(t.count+1))
		}
		if ctsOffset != 0 {
			t.hasCTS = true
		}
		t.ctts = appendRun(t.ctts, uint32(ctsOffset), 1)
	}
	t.stts = appendRun(t.stts, dur, n)
	t.count += uint64(n)
	t.duration += uint64(dur) * uint64(n)
	return nil
}

func (m *Muxer) flushChunk() {
	if m.last < 0 {
		return
	}
	t := m.tracks[m.last]
	if t.curChunk > 0 {
		t.stsc = appendRun(t.stsc, t.curChunk, 1)
		t.curChunk = 0
	}
}

// Finish writes the complete file (ftyp, moov, mdat) to w.
func (m *Muxer) Finish(w io.Writer) error {
	if m.done {
		return errors.New("isobmff: muxer finished")
	}
	m.flushChunk()
	m.done = true

	ftyp := &buf{}
	ftyp.box("ftyp", func() {
		if m.brand == BrandMOV {
			ftyp.str("qt  ")
			ftyp.u32(0x200)
			ftyp.str("qt  ")
		} else {
			ftyp.str("isom")
			ftyp.u32(0x200)
			ftyp.str("isomiso2mp41")
		}
	})
	mdatHdr := int64(8)
	if m.dataLen+8 > 0xffffffff {
		mdatHdr = 16
	}
	co64 := false
	var moov []byte
	for pass := 0; pass < 2; pass++ {
		moov = m.buildMoov(0, co64)
		base := int64(len(ftyp.b)+len(moov)) + mdatHdr
		if !co64 && base+m.dataLen > 0xffffffff {
			co64 = true
			continue
		}
		moov = m.buildMoov(base, co64)
		break
	}
	if _, err := w.Write(ftyp.b); err != nil {
		return err
	}
	if _, err := w.Write(moov); err != nil {
		return err
	}
	h := &buf{}
	if mdatHdr == 16 {
		h.u32(1)
		h.str("mdat")
		h.u64(uint64(m.dataLen + 16))
	} else {
		h.u32(uint32(m.dataLen + 8))
		h.str("mdat")
	}
	if _, err := w.Write(h.b); err != nil {
		return err
	}
	if _, err := m.scratch.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(m.scratch, m.dataLen))
	if err != nil {
		return err
	}
	if n != m.dataLen {
		return io.ErrUnexpectedEOF
	}
	return nil
}

const movieTimescale = 1000

func scaleDur(d uint64, from uint32) uint64 {
	if from == 0 {
		return 0
	}
	return (d*movieTimescale + uint64(from)/2) / uint64(from)
}

func (m *Muxer) buildMoov(base int64, co64 bool) []byte {
	b := &buf{}
	var movieDur uint64
	for _, t := range m.tracks {
		if d := scaleDur(t.duration, t.cfg.Timescale); d > movieDur {
			movieDur = d
		}
	}
	identity := func() {
		for _, v := range []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
			b.u32(v)
		}
	}
	b.box("moov", func() {
		b.box("mvhd", func() {
			b.u32(0) // version 0
			b.u32(0)
			b.u32(0)
			b.u32(movieTimescale)
			b.u32(uint32(movieDur))
			b.u32(0x10000) // rate
			b.u16(0x100)   // volume
			b.zero(10)
			identity()
			b.zero(24)
			b.u32(uint32(len(m.tracks) + 1))
		})
		for i, t := range m.tracks {
			cfg := t.cfg
			b.box("trak", func() {
				b.box("tkhd", func() {
					flags := uint32(0x2) // in movie
					if !cfg.Disabled {
						flags |= 0x1
					}
					b.u32(flags)
					b.u32(0)
					b.u32(0)
					b.u32(uint32(i + 1))
					b.u32(0)
					b.u32(uint32(scaleDur(t.duration, cfg.Timescale)))
					b.zero(8)
					b.u16(0) // layer
					b.u16(uint16(cfg.AltGroup))
					if cfg.Handler == "soun" {
						b.u16(0x100)
					} else {
						b.u16(0)
					}
					b.u16(0)
					identity()
					b.u32(uint32(cfg.Width) << 16)
					b.u32(uint32(cfg.Height) << 16)
				})
				b.box("mdia", func() {
					if t.duration > 0xffffffff {
						b.box("mdhd", func() {
							b.u32(1 << 24)
							b.u64(0)
							b.u64(0)
							b.u32(cfg.Timescale)
							b.u64(t.duration)
							b.u16(encodeLang(cfg.Language))
							b.u16(0)
						})
					} else {
						b.box("mdhd", func() {
							b.u32(0)
							b.u32(0)
							b.u32(0)
							b.u32(cfg.Timescale)
							b.u32(uint32(t.duration))
							b.u16(encodeLang(cfg.Language))
							b.u16(0)
						})
					}
					name := cfg.Name
					if name == "" {
						name = map[string]string{"vide": "VideoHandler", "soun": "SoundHandler", "sbtl": "SubtitleHandler"}[cfg.Handler]
					}
					b.box("hdlr", func() {
						b.u32(0)
						if m.brand == BrandMOV {
							b.str("mhlr")
						} else {
							b.u32(0)
						}
						b.str(cfg.Handler)
						b.zero(12)
						if m.brand == BrandMOV {
							b.u8(uint8(len(name)))
							b.str(name)
						} else {
							b.str(name)
							b.u8(0)
						}
					})
					b.box("minf", func() {
						switch cfg.Handler {
						case "vide":
							b.box("vmhd", func() { b.u32(1); b.zero(8) })
						case "soun":
							b.box("smhd", func() { b.u32(0); b.zero(4) })
						default:
							b.box("nmhd", func() { b.u32(0) })
						}
						if m.brand == BrandMOV {
							b.box("hdlr", func() {
								b.u32(0)
								b.str("dhlr")
								b.str("url ")
								b.zero(12)
								b.u8(11)
								b.str("DataHandler")
							})
						}
						b.box("dinf", func() {
							b.box("dref", func() {
								b.u32(0)
								b.u32(1)
								b.box("url ", func() { b.u32(1) })
							})
						})
						b.box("stbl", func() { m.buildStbl(b, t, base, co64) })
					})
				})
			})
		}
	})
	return b.b
}

func (m *Muxer) buildStbl(b *buf, t *muxTrack, base int64, co64 bool) {
	b.box("stsd", func() {
		b.u32(0)
		b.u32(1)
		b.bytes(t.cfg.Entry)
	})
	b.box("stts", func() {
		b.u32(0)
		b.u32(uint32(len(t.stts)))
		for _, r := range t.stts {
			b.u32(r.count)
			b.u32(r.value)
		}
	})
	if t.hasCTS {
		b.box("ctts", func() {
			b.u32(1 << 24) // version 1: signed offsets
			b.u32(uint32(len(t.ctts)))
			for _, r := range t.ctts {
				b.u32(r.count)
				b.u32(r.value)
			}
		})
	}
	if !t.cfg.AllSync && t.cfg.SampleSize == 0 {
		b.box("stss", func() {
			b.u32(0)
			b.u32(uint32(len(t.sync)))
			for _, s := range t.sync {
				b.u32(s)
			}
		})
	}
	b.box("stsc", func() {
		b.u32(0)
		b.u32(uint32(len(t.stsc)))
		first := uint32(1)
		for _, r := range t.stsc {
			b.u32(first)
			b.u32(r.value)
			b.u32(1)
			first += r.count
		}
	})
	b.box("stsz", func() {
		b.u32(0)
		if t.cfg.SampleSize > 0 {
			b.u32(t.cfg.SampleSize)
			b.u32(uint32(t.count))
			return
		}
		b.u32(0)
		b.u32(uint32(len(t.sizes)))
		for _, s := range t.sizes {
			b.u32(s)
		}
	})
	if co64 {
		b.box("co64", func() {
			b.u32(0)
			b.u32(uint32(len(t.chunkOffs)))
			for _, o := range t.chunkOffs {
				b.u64(uint64(o + base))
			}
		})
	} else {
		b.box("stco", func() {
			b.u32(0)
			b.u32(uint32(len(t.chunkOffs)))
			for _, o := range t.chunkOffs {
				b.u32(uint32(o + base))
			}
		})
	}
}

func encodeLang(s string) uint16 {
	if len(s) != 3 {
		s = "und"
	}
	var v uint16
	for i := 0; i < 3; i++ {
		c := s[i]
		if c < 'a' || c > 'z' {
			return 0x55c4
		}
		v = v<<5 | uint16(c-0x60)
	}
	return v
}

// --- box building helpers ----------------------------------------------------

type buf struct{ b []byte }

func (w *buf) u8(v uint8)     { w.b = append(w.b, v) }
func (w *buf) u16(v uint16)   { w.b = be.AppendUint16(w.b, v) }
func (w *buf) u32(v uint32)   { w.b = be.AppendUint32(w.b, v) }
func (w *buf) u64(v uint64)   { w.b = be.AppendUint64(w.b, v) }
func (w *buf) str(s string)   { w.b = append(w.b, s...) }
func (w *buf) bytes(p []byte) { w.b = append(w.b, p...) }
func (w *buf) zero(n int)     { w.b = append(w.b, make([]byte, n)...) }
func (w *buf) box(typ string, fn func()) {
	start := len(w.b)
	w.u32(0)
	w.str(typ)
	fn()
	be.PutUint32(w.b[start:], uint32(len(w.b)-start))
}

// Box returns a box of the given type wrapping the concatenated payloads.
func Box(typ string, payload ...[]byte) []byte {
	w := &buf{}
	w.box(typ, func() {
		for _, p := range payload {
			w.bytes(p)
		}
	})
	return w.b
}

// VisualEntry builds a VisualSampleEntry with the given child boxes.
func VisualEntry(format string, width, height int, compressor string, children ...[]byte) []byte {
	w := &buf{}
	w.box(format, func() {
		w.zero(6)
		w.u16(1) // data reference index
		w.zero(16)
		w.u16(uint16(width))
		w.u16(uint16(height))
		w.u32(0x480000) // 72 dpi
		w.u32(0x480000)
		w.u32(0)
		w.u16(1) // frame count
		if len(compressor) > 31 {
			compressor = compressor[:31]
		}
		w.u8(uint8(len(compressor)))
		w.str(compressor)
		w.zero(31 - len(compressor))
		w.u16(24) // depth
		w.u16(0xffff)
		for _, c := range children {
			w.bytes(c)
		}
	})
	return w.b
}

// AudioEntry builds a version 0 AudioSampleEntry with the given child boxes.
func AudioEntry(format string, channels, bits, sampleRate int, children ...[]byte) []byte {
	w := &buf{}
	w.box(format, func() {
		w.zero(6)
		w.u16(1)
		w.zero(8)
		w.u16(uint16(channels))
		w.u16(uint16(bits))
		w.u32(0)
		w.u32(uint32(sampleRate) << 16)
		for _, c := range children {
			w.bytes(c)
		}
	})
	return w.b
}

// PcmC builds the pcmC box for 'ipcm'/'fpcm' entries (ISO/IEC 23003-5).
func PcmC(littleEndian bool, bits int) []byte {
	flags := byte(0)
	if littleEndian {
		flags = 1
	}
	return Box("pcmC", []byte{0, 0, 0, 0, flags, byte(bits)})
}

// ColrNCLX builds an ISO 'colr' box with nclx colour information.
func ColrNCLX(primaries, transfer, matrix int, fullRange bool) []byte {
	w := &buf{}
	w.str("nclx")
	w.u16(uint16(primaries))
	w.u16(uint16(transfer))
	w.u16(uint16(matrix))
	if fullRange {
		w.u8(0x80)
	} else {
		w.u8(0)
	}
	return Box("colr", w.b)
}

// ColrNCLC builds a QuickTime 'colr' atom with nclc colour information.
func ColrNCLC(primaries, transfer, matrix int) []byte {
	w := &buf{}
	w.str("nclc")
	w.u16(uint16(primaries))
	w.u16(uint16(transfer))
	w.u16(uint16(matrix))
	return Box("colr", w.b)
}

// ESDS builds an esds box for the given object type indication and stream
// type (0x04 visual, 0x05 audio), with optional decoder-specific info.
func ESDS(oti, streamType byte, decSpecific []byte, bufferSize, maxBitrate, avgBitrate uint32) []byte {
	desc := func(tag byte, payload []byte) []byte {
		out := []byte{tag}
		n := len(payload)
		out = append(out, byte(n>>21)|0x80, byte(n>>14)|0x80, byte(n>>7)|0x80, byte(n&0x7f))
		return append(out, payload...)
	}
	dc := &buf{}
	dc.u8(oti)
	dc.u8(streamType<<2 | 1)
	dc.u8(byte(bufferSize >> 16))
	dc.u16(uint16(bufferSize))
	dc.u32(maxBitrate)
	dc.u32(avgBitrate)
	if len(decSpecific) > 0 {
		dc.bytes(desc(0x05, decSpecific))
	}
	es := &buf{}
	es.u16(0) // ES_ID
	es.u8(0)
	es.bytes(desc(0x04, dc.b))
	es.bytes(desc(0x06, []byte{2}))
	return Box("esds", []byte{0, 0, 0, 0}, desc(0x03, es.b))
}

// Tx3gEntry builds a 3GPP timed text sample entry (3GPP TS 26.245).
func Tx3gEntry() []byte {
	w := &buf{}
	w.box("tx3g", func() {
		w.zero(6)
		w.u16(1)
		w.u32(0)          // display flags
		w.u8(1)           // horizontal justification: centre
		w.u8(0xff)        // vertical justification: bottom
		w.u32(0)          // background colour
		w.zero(8)         // default text box
		w.u16(0)          // style: start char
		w.u16(0)          // end char
		w.u16(1)          // font ID
		w.u8(0)           // face
		w.u8(18)          // size
		w.u32(0xffffffff) // text colour
		w.box("ftab", func() {
			w.u16(1)
			w.u16(1)
			w.u8(5)
			w.str("Serif")
		})
	})
	return w.b
}
