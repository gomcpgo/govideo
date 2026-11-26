package native

import (
	"errors"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/codechdr"
)

// The MPEG audio layer III decoder is adapted from minimp3
// (https://github.com/lieff/minimp3, CC0; see NOTICE): its tables
// (mp3tables.go) and the arithmetic of its stages are kept, laid out for Go
// and with every index held within its array, since the input is not
// trusted. The standard is ISO/IEC 11172-3, with the lower sample rates of
// ISO/IEC 13818-3.

const (
	mp3Lines     = 576  // spectral lines, and samples, of one channel in a granule
	mp3Reservoir = 511  // bytes of earlier frames that a frame's data may start in
	mp3MaxFrame  = 1441 // the largest frame: 320 kbit/s at 32 kHz, padded

	mp3Short = 2 // block_type: three short blocks
	mp3Stop  = 3 // block_type: the long block after short ones
)

// ErrNotMP3 reports a packet that does not start with the header of a layer
// III frame.
var ErrNotMP3 = errors.New("native: not an MPEG audio layer III frame")

// MP3Decoder decodes MPEG audio layer III: MPEG-1 and the half and quarter
// sample rates of MPEG-2 and "MPEG-2.5", in mono, stereo and joint stereo.
// Free-format streams, whose frames do not state their size, are refused.
//
// A frame's audio data may start in the frames before it (the bit
// reservoir), and its samples overlap those of the frame before, so a
// decoder that starts inside a stream is not right at once: a frame whose
// data it does not have comes out silent, and the first frame after that
// still lacks what the one before left. Every frame gives its full count of
// samples all the same, so the position in the stream can be counted.
//
// The output has the delay every layer III decoder has, 529 samples; an
// encoder's own delay comes on top of it.
type MP3Decoder struct {
	rate, channels int

	// What one frame leaves for the next.
	overlap  [2][9 * 32]float32 // the second half of each subband's last transform
	qmf      [15 * 64]float32   // the last 15 slots of subband samples, for the synthesis filter
	reserve  [mp3Reservoir]byte // the end of the audio data not yet read
	reserved int                // bytes of it

	// Working memory of a frame, and what its header says.
	ist      [2][40]uint8 // a frame's scalefactors, for its second granule and as stereo positions
	main     [mp3Reservoir + mp3MaxFrame]byte
	bits     mp3Bits               // reads main
	gr       [4]mp3Granule         // side information: granule by granule, channel by channel
	spec     [2 * mp3Lines]float32 // a granule's spectrum, then its subband samples: left, right
	scf      [40]float32           // the gain of each scalefactor band
	lins     [(18 + 15) * 64]float32
	scratch  [mp3Lines]float32
	mpeg1    bool // not one of the lower rates
	row      int  // of the band tables, for the sample rate
	ms       bool // joint stereo: sum and difference
	iStereo  bool // joint stereo: intensity
	wideLong bool // 8 kHz: the long part of a mixed granule is four subbands, not two
}

// NewMP3Decoder returns a decoder at the start of a stream.
func NewMP3Decoder() *MP3Decoder { return &MP3Decoder{} }

// mp3Granule is the side information of one channel of a granule.
type mp3Granule struct {
	bands            []uint8 // widths of the scalefactor bands, ending in 0
	part23           int     // bits of scalefactors and spectrum
	bigValues        int     // pairs coded with the pair tables
	globalGain       int
	scalefacCompress int
	blockType        int
	mixed            bool
	nLong, nShort    int // scalefactor bands of each kind
	table            [3]int
	regionCount      [3]int
	subblockGain     [3]int
	preflag          bool
	scalefacScale    int
	count1Table      int
	scfsi            int // groups of scalefactors taken from the granule before: bit 3 is the first
}

// mp3Bits reads bits, most significant first. A read past the end gives 0
// and leaves pos beyond limit.
type mp3Bits struct {
	buf        []byte
	pos, limit int // in bits
}

func (b *mp3Bits) get(n int) int {
	p := b.pos
	b.pos += n
	if n <= 0 || b.pos > b.limit {
		return 0
	}
	i := p >> 3
	v := uint32(b.buf[i]) & (0xff >> uint(p&7))
	have := 8 - p&7
	for have < n {
		i++
		v = v<<8 | uint32(b.buf[i])
		have += 8
	}
	return int(v >> uint(have-n))
}

// Decode decodes a packet: one frame, or several whole frames one after the
// other. Each gives 1152 samples of every channel, or 576 at the lower
// rates. A frame that cannot be decoded (damage, or audio data that lies in
// frames the decoder was not given) comes out as silence; the only error is
// a packet that does not start with a layer III frame.
func (d *MP3Decoder) Decode(packet []byte) (*codec.AudioFrame, error) {
	var out *codec.AudioFrame
	for {
		h, ok := codechdr.MPEGAudioFrameHeader(packet)
		if ok && (h.Layer != 3 || h.Size == 0) {
			ok = false
			if out == nil && h.Layer == 3 {
				return nil, errors.New("native: free-format MP3 streams are not supported")
			}
		}
		channels := 2
		if h.Mono {
			channels = 1
		}
		if out == nil {
			if !ok {
				return nil, ErrNotMP3
			}
			out = &codec.AudioFrame{SampleRate: h.SampleRate, Channels: channels}
		} else if !ok || h.SampleRate != out.SampleRate || channels != out.Channels {
			// What follows the frames is not looked at further.
			return out, nil
		}
		frame := packet[:min(h.Size, len(packet))]
		packet = packet[len(frame):]
		at := len(out.Samples)
		out.Samples = append(out.Samples, make([]float32, h.Samples*channels)...)
		d.frame(h, frame, out.Samples[at:])
		if len(packet) < 4 {
			return out, nil
		}
	}
}

// reset forgets what earlier frames left.
func (d *MP3Decoder) reset() {
	clear(d.overlap[0][:])
	clear(d.overlap[1][:])
	clear(d.qmf[:])
	d.reserved = 0
}

// frame decodes one frame into pcm, which is zeroed and holds the frame's
// samples, interleaved.
func (d *MP3Decoder) frame(h codechdr.MPEGAudioFrame, data []byte, pcm []float32) {
	channels := 2
	if h.Mono {
		channels = 1
	}
	if h.SampleRate != d.rate || channels != d.channels {
		d.reset()
		d.rate, d.channels = h.SampleRate, channels
	}
	d.mpeg1 = !h.LowRate
	d.row = mp3Row(h.SampleRate)
	d.wideLong = h.SampleRate == 8000
	// The mode extension says how the two channels of a joint stereo frame
	// are coded; in the other modes its bits mean nothing.
	joint := data[3]>>6 == 1
	d.ms = joint && data[3]&0x20 != 0
	d.iStereo = joint && data[3]&0x10 != 0

	body := data[4:]
	if h.Protected {
		body = body[min(2, len(body)):] // the CRC, which is not checked
	}
	side := mp3Bits{buf: body, limit: len(body) * 8}
	begin, ok := d.sideInfo(&side)
	if !ok || side.pos > side.limit {
		d.reset()
		return
	}
	d.ist = [2][40]uint8{}
	if d.restore(begin, body[side.pos/8:]) {
		granules := 1
		if d.mpeg1 {
			granules = 2
		}
		for g := 0; g < granules; g++ {
			clear(d.spec[:])
			d.granule(d.gr[g*channels : (g+1)*channels])
			d.synthGranule(pcm[g*mp3Lines*channels:(g+1)*mp3Lines*channels], channels)
		}
	}
	d.save()
}

// mp3Row is the row of the band tables for a sample rate.
func mp3Row(rate int) int {
	switch rate {
	case 8000:
		return 1
	case 22050:
		return 2
	case 24000:
		return 3
	case 16000:
		return 4
	case 44100:
		return 5
	case 48000:
		return 6
	case 32000:
		return 7
	}
	return 0 // 11025 and 12000
}

// sideInfo reads the side information of a frame into d.gr and returns
// main_data_begin: how many bytes before the frame its audio data starts.
func (d *MP3Decoder) sideInfo(b *mp3Bits) (begin int, ok bool) {
	var scfsi [2]int
	if d.mpeg1 {
		begin = b.get(9)
		b.get(7 - 2*d.channels) // private bits: 5 in mono, 3 in stereo
		for c := 0; c < d.channels; c++ {
			scfsi[c] = b.get(4)
		}
	} else {
		begin = b.get(8)
		b.get(d.channels) // private bits
	}
	n := d.channels
	if d.mpeg1 {
		n *= 2
	}
	sum := 0
	for i := 0; i < n; i++ {
		g := &d.gr[i]
		*g = mp3Granule{bands: mp3BandsLong[d.row][:], nLong: 22}
		g.part23 = b.get(12)
		sum += g.part23
		if g.bigValues = b.get(9); g.bigValues > 288 {
			return 0, false
		}
		g.globalGain = b.get(8)
		if d.mpeg1 {
			g.scalefacCompress = b.get(4)
		} else {
			g.scalefacCompress = b.get(9)
		}
		var tables int
		if b.get(1) != 0 { // window switching
			if g.blockType = b.get(2); g.blockType == 0 {
				return 0, false
			}
			g.mixed = b.get(1) != 0
			g.regionCount = [3]int{7, 255, 255}
			if g.blockType == mp3Short {
				if !g.mixed {
					g.regionCount[0] = 8
					g.bands, g.nLong, g.nShort = mp3BandsShort[d.row][:], 0, 39
				} else {
					g.bands, g.nLong, g.nShort = mp3BandsMixed[d.row][:], 6, 30
					if d.mpeg1 {
						g.nLong = 8
					}
				}
			}
			tables = b.get(10) << 5
			for k := range g.subblockGain {
				g.subblockGain[k] = b.get(3)
			}
		} else {
			tables = b.get(15)
			g.regionCount = [3]int{b.get(4), b.get(3), 255}
		}
		g.table = [3]int{tables >> 10, tables >> 5 & 31, tables & 31}
		if d.mpeg1 {
			g.preflag = b.get(1) != 0
		} else {
			g.preflag = g.scalefacCompress >= 500
		}
		g.scalefacScale = b.get(1)
		g.count1Table = b.get(1)
		// A second granule may take scalefactors from the first, unless its
		// blocks are short.
		if d.mpeg1 && i >= d.channels && g.blockType != mp3Short {
			g.scfsi = scfsi[i-d.channels]
		}
	}
	// The granules cannot hold more than the frame and the reservoir do.
	if sum+b.pos > b.limit+begin*8 {
		return 0, false
	}
	return begin, true
}

// restore lays the audio data of a frame out in d.main: the last begin
// bytes that earlier frames left, then the frame's own. It reports whether
// those bytes were there.
func (d *MP3Decoder) restore(begin int, own []byte) bool {
	have := min(d.reserved, begin)
	copy(d.main[:], d.reserve[d.reserved-have:d.reserved])
	n := copy(d.main[have:], own)
	d.bits = mp3Bits{buf: d.main[:have+n], limit: (have + n) * 8}
	return d.reserved >= begin
}

// save keeps the audio data that the frame did not read, for the frames
// after it.
func (d *MP3Decoder) save() {
	pos := (d.bits.pos + 7) / 8
	remains := d.bits.limit/8 - pos
	if remains > mp3Reservoir {
		pos += remains - mp3Reservoir
		remains = mp3Reservoir
	}
	if remains > 0 {
		copy(d.reserve[:], d.bits.buf[pos:pos+remains])
	}
	d.reserved = max(remains, 0)
}

// granule decodes the spectrum of a granule's channels from d.bits and
// turns it into subband samples in d.spec. gr is the side information of
// each channel.
func (d *MP3Decoder) granule(gr []mp3Granule) {
	for c := range gr {
		limit := d.bits.pos + gr[c].part23
		d.scalefactors(&gr[c], c)
		d.huffman(d.spec[c*mp3Lines:(c+1)*mp3Lines], &gr[c], limit)
	}
	if len(gr) == 2 {
		if d.iStereo {
			d.intensityStereo(gr)
		} else if d.ms {
			mp3MidSide(d.spec[:mp3Lines], d.spec[mp3Lines:])
		}
	}
	for c := range gr {
		g := &gr[c]
		buf := d.spec[c*mp3Lines : (c+1)*mp3Lines]
		aliased, long := 31, 0
		if g.mixed {
			long = 2
			if d.wideLong {
				long = 4
			}
		}
		if g.nShort != 0 {
			aliased = long - 1
			d.reorder(buf[long*18:], g.bands[g.nLong:])
		}
		mp3Antialias(buf, aliased)
		mp3IMDCT(buf, d.overlap[c][:], g.blockType, long)
		// The filter bank inverts the spectrum of every other subband.
		for b := 18; b < mp3Lines; b += 36 {
			for i := 1; i < 18; i += 2 {
				buf[b+i] = -buf[b+i]
			}
		}
	}
}

// mp3Ldexp returns y·2^(-e/4).
func mp3Ldexp(y float32, e int) float32 {
	for {
		step := min(30*4, e)
		y *= mp3ExpFrac[step&3] * float32(int(1)<<30>>uint(step>>2))
		if e -= step; e <= 0 {
			return y
		}
	}
}

// scalefactors reads the scalefactors of a granule's channel and sets
// d.scf, the gain of each of its bands.
func (d *MP3Decoder) scalefactors(g *mp3Granule, ch int) {
	kind := 0 // long, mixed or short blocks
	if g.nShort != 0 {
		kind = 1
		if g.nLong == 0 {
			kind = 2
		}
	}
	counts := mp3ScfPartitions[kind][:]
	var (
		size  [4]int // bits of a scalefactor in each group
		iscf  [40]uint8
		scfsi = g.scfsi
	)
	if d.mpeg1 {
		part := int(mp3ScfBits[g.scalefacCompress&15])
		size = [4]int{part >> 2, part >> 2, part & 3, part & 3}
	} else {
		// At the lower rates scalefac_compress holds the four sizes as the
		// digits of a number, in radices that depend on its range; the right
		// channel of intensity stereo has ranges of its own.
		sfc, k := g.scalefacCompress, 0
		if d.iStereo && ch == 1 {
			sfc, k = sfc>>1, 12
		}
		for ; sfc >= 0 && k+4 <= len(mp3ScfMod); k += 4 {
			prod := 1
			for i := 3; i >= 0; i-- {
				size[i] = sfc / prod % mp3ScfMod[k+i]
				prod *= mp3ScfMod[k+i]
			}
			sfc -= prod
		}
		counts = counts[k:]
		scfsi = -16 // none shared, and the largest value marks a band without a stereo position
	}
	ist := d.ist[ch][:]
	at := 0
	for i := 0; i < 4 && counts[i] != 0; i++ {
		n := int(counts[i])
		switch bits := size[i]; {
		case scfsi&8 != 0:
			copy(iscf[at:at+n], ist[at:])
		case bits == 0:
			clear(iscf[at : at+n])
			clear(ist[at : at+n])
		default:
			illegal := -1
			if scfsi < 0 {
				illegal = 1<<uint(bits) - 1
			}
			for k := at; k < at+n; k++ {
				s := d.bits.get(bits)
				iscf[k] = uint8(s)
				if s == illegal {
					s = 255
				}
				ist[k] = uint8(s)
			}
		}
		at += n
		scfsi *= 2
	}

	shift := uint(g.scalefacScale + 1)
	if g.nShort != 0 {
		sh := 3 - shift
		for i := g.nLong; i+2 < g.nLong+g.nShort; i += 3 {
			iscf[i] += uint8(g.subblockGain[0] << sh)
			iscf[i+1] += uint8(g.subblockGain[1] << sh)
			iscf[i+2] += uint8(g.subblockGain[2] << sh)
		}
	} else if g.preflag {
		for i, p := range mp3Preamp {
			iscf[11+i] += p
		}
	}
	// The gain of a line is 2^((global_gain-210)/4), less the band's
	// scalefactor; the synthesis window is scaled to 16 bits, and the sum
	// and difference of joint stereo are halved here.
	const maxGain = 44 // above any global_gain-214, and a multiple of 4
	exp := g.globalGain - 214
	if d.ms {
		exp -= 2
	}
	gain := mp3Ldexp(1<<(maxGain/4), maxGain-exp)
	for i := 0; i < g.nLong+g.nShort; i++ {
		d.scf[i] = mp3Ldexp(gain, int(iscf[i])<<shift)
	}
}

// mp3Dequant returns x^(4/3) for the values the pair tables cannot hold
// (15 and up, to 8206).
func mp3Dequant(x int) float32 {
	if x < 129 {
		return mp3Pow43[16+x]
	}
	// From the table entry of a nearby multiple of 64 (of 8, under 1024),
	// by the first terms of the series of (1+f)^(4/3).
	mult := float32(256)
	if x < 1024 {
		mult = 16
		x <<= 3
	}
	sign := 2 * x & 64
	frac := float32(x&63-sign) / float32(x&^63+sign)
	i := min(16+(x+sign)>>6, len(mp3Pow43)-1)
	return mp3Pow43[i] * (1 + frac*(4.0/3+frac*(2.0/9))) * mult
}

// mp3Cache is a 32-bit window on the bits of the spectrum, refilled a byte
// at a time: at least 25 bits are ahead after fill. Past the end of the
// data the bits are zeros.
type mp3Cache struct {
	buf   []byte
	next  int    // the byte to take in next
	cache uint32 // the bits ahead, from the top
	sh    int    // where the next byte goes, less 8: negative while the cache is full
}

func (c *mp3Cache) fill() {
	for c.sh >= 0 {
		if c.next < len(c.buf) {
			c.cache |= uint32(c.buf[c.next]) << uint(c.sh)
		}
		c.next++
		c.sh -= 8
	}
}

func (c *mp3Cache) peek(n int) int { return int(c.cache >> uint(32-n)) }

func (c *mp3Cache) skip(n int) {
	c.cache <<= uint(n)
	c.sh += n
}

// pos is the position, in bits, of the first bit of the cache.
func (c *mp3Cache) pos() int { return c.next*8 - 24 + c.sh }

// huffman decodes the spectrum of a granule's channel, which ends at bit
// limit of d.bits, into dst, each line scaled by the gain of its band.
func (d *MP3Decoder) huffman(dst []float32, g *mp3Granule, limit int) {
	c := mp3Cache{buf: d.bits.buf, next: d.bits.pos >> 3, sh: 24}
	c.fill() // four bytes
	c.skip(d.bits.pos & 7)

	var (
		bands = g.bands
		band  int     // the next entry of bands
		one   float32 // the gain of the band being read
		np    int     // pairs in that band
		at    int     // the next line
		big   = g.bigValues
	)
	// The big values: pairs of lines, in up to three regions that each have
	// a table.
	for region := 0; big > 0 && region < 3; region++ {
		table := g.table[region]
		tree := mp3Huff[mp3HuffStart[table]:]
		linbits := int(mp3LinBits[table])
		for left := g.regionCount[region]; big > 0 && left >= 0; left-- {
			if np = int(bands[band]) / 2; np == 0 {
				big = 0 // past the last band
				break
			}
			one = d.scf[band]
			band++
			for pairs := min(big, np); pairs > 0; pairs-- {
				w := 5
				leaf := int(tree[c.peek(w)])
				for leaf < 0 {
					c.skip(w)
					w = leaf & 7
					leaf = int(tree[c.peek(w)-leaf>>3])
				}
				c.skip(leaf >> 8)
				for j := 0; j < 2; j++ {
					v := leaf & 15
					switch {
					case v == 15 && linbits != 0:
						v += c.peek(linbits)
						c.skip(linbits)
						c.fill()
						x := one * mp3Dequant(v)
						if int32(c.cache) < 0 {
							x = -x
						}
						dst[at] = x
					default:
						dst[at] = mp3Pow43[16+v-16*int(c.cache>>31)] * one
					}
					if v != 0 {
						c.skip(1) // the sign
					}
					at++
					leaf >>= 4
				}
				c.fill()
			}
			big -= np
		}
	}

	// The rest, up to the end of the granule's bits: quadruples of lines
	// that are 0 or ±1.
	quad := mp3Quad0[:]
	if g.count1Table != 0 {
		quad = mp3Quad1[:]
	}
	// reload moves on to the next band when the pairs of this one are used
	// up, and reports whether there was one.
	reload := func() bool {
		if np--; np != 0 {
			return true
		}
		if np = int(bands[band]) / 2; np == 0 {
			return false
		}
		one = d.scf[band]
		band++
		return true
	}
	line := func(i int, set bool) {
		if !set {
			return
		}
		if int32(c.cache) < 0 {
			dst[i] = -one
		} else {
			dst[i] = one
		}
		c.skip(1)
	}
	for np = 1 - big; ; at += 4 {
		leaf := int(quad[c.peek(4)])
		if leaf&8 == 0 {
			leaf = int(quad[leaf>>3+int(c.cache<<4>>uint(32-leaf&3))])
		}
		c.skip(leaf & 7)
		if c.pos() > limit || !reload() {
			break
		}
		line(at, leaf&128 != 0)
		line(at+1, leaf&64 != 0)
		if !reload() {
			break
		}
		line(at+2, leaf&32 != 0)
		line(at+3, leaf&16 != 0)
		c.fill()
	}
	d.bits.pos = limit
}

// mp3MidSide turns the sum and difference of a band into its two channels.
func mp3MidSide(left, right []float32) {
	for i, a := range left {
		b := right[i]
		left[i], right[i] = a+b, a-b
	}
}

// intensityStereo makes the two channels of a joint stereo granule in
// which the upper bands of the right channel are not coded: such a band is
// the left channel's, shared out by the position that stands where the right
// channel's scalefactor would. The bands below are coded as two channels, or
// as sum and difference.
func (d *MP3Decoder) intensityStereo(gr []mp3Granule) {
	g := &gr[0]
	left, right := d.spec[:mp3Lines], d.spec[mp3Lines:]
	n := g.nLong + g.nShort
	windows := 1
	if g.nShort != 0 {
		windows = 3
	}
	// top is the last band in which the right channel has a line that is
	// not zero: of each window, where there are short blocks.
	top := [3]int{-1, -1, -1}
	at := 0
	for i := 0; i < n; i++ {
		w := int(g.bands[i])
		for k := at; k < at+w; k++ {
			if right[k] != 0 {
				top[i%3] = i
				break
			}
		}
		at += w
	}
	if g.nLong != 0 {
		m := max(top[0], top[1], top[2])
		top = [3]int{m, m, m}
	}
	// The last band has no scalefactor: it takes the position of the band
	// before it.
	ist := d.ist[1][:]
	for i := 0; i < windows; i++ {
		last := n - windows + i
		before := last - windows
		switch {
		case top[i] < before:
			ist[last] = ist[before]
		case d.mpeg1:
			ist[last] = 3
		default:
			ist[last] = 0
		}
	}

	positions := 64
	if d.mpeg1 {
		positions = 7
	}
	scale := float32(1)
	if d.ms {
		scale = 1.41421356
	}
	at = 0
	for i := 0; g.bands[i] != 0; i++ {
		w := int(g.bands[i])
		l, r := left[at:at+w], right[at:at+w]
		at += w
		pos := int(ist[i])
		switch {
		case i > top[i%3] && pos < positions:
			var kl, kr float32
			if d.mpeg1 {
				kl, kr = mp3Pan[2*pos], mp3Pan[2*pos+1]
			} else {
				// One channel is left as it is and the other is lowered,
				// by steps of 2^(-1/4) or 2^(-1/2).
				kl, kr = 1, mp3Ldexp(1, (pos+1)>>1<<uint(gr[1].scalefacCompress&1))
				if pos&1 != 0 {
					kl, kr = kr, 1
				}
			}
			kl, kr = kl*scale, kr*scale
			for k, v := range l {
				l[k], r[k] = v*kl, v*kr
			}
		case d.ms:
			mp3MidSide(l, r)
		}
	}
}

// reorder puts the lines of short blocks, which are coded band by band and
// window by window, in the order of the transform: the three windows of a
// line together.
func (d *MP3Decoder) reorder(buf []float32, bands []uint8) {
	src, dst := 0, 0
	for i := 0; i < len(bands) && bands[i] != 0; i += 3 {
		n := int(bands[i])
		if src+3*n > len(buf) {
			break
		}
		for k := 0; k < n; k++ {
			d.scratch[dst] = buf[src]
			d.scratch[dst+1] = buf[src+n]
			d.scratch[dst+2] = buf[src+2*n]
			dst += 3
			src++
		}
		src += 2 * n
	}
	copy(buf, d.scratch[:dst])
}

// mp3Antialias undoes the aliasing that the analysis filter left between a
// subband and the next, for the first n joins.
func mp3Antialias(buf []float32, n int) {
	for ; n > 0; n-- {
		for i := 0; i < 8; i++ {
			u, dn := buf[18+i], buf[17-i]
			buf[18+i] = u*mp3Alias[0][i] - dn*mp3Alias[1][i]
			buf[17-i] = u*mp3Alias[1][i] + dn*mp3Alias[0][i]
		}
		buf = buf[18:]
	}
}

// mp3DCT9 is a 9-point DCT-III, in place.
func mp3DCT9(y *[9]float32) {
	s0, s2, s4, s6, s8 := y[0], y[2], y[4], y[6], y[8]
	t0 := s0 + s6*0.5
	s0 -= s6
	t4 := (s4 + s2) * 0.93969262
	t2 := (s8 + s2) * 0.76604444
	s6 = (s4 - s8) * 0.17364818
	s4 += s8 - s2

	s2 = s0 - s4*0.5
	y[4] = s4 + s0
	s8 = t0 - t2 + s6
	s0 = t0 - t4 + t2
	s4 = t0 + t4 - s6

	s1, s3, s5, s7 := y[1], y[3], y[5], y[7]

	s3 *= 0.86602540
	t0 = (s5 + s1) * 0.98480775
	t4 = (s5 - s7) * 0.34202014
	t2 = (s1 + s7) * 0.64278761
	s1 = (s1 - s5 - s7) * 0.86602540

	s5 = t0 - s3 - t2
	s7 = t4 - s3 - t0
	s3 = t4 + s3 - t2

	y[0] = s4 - s7
	y[1] = s2 + s1
	y[2] = s0 - s3
	y[3] = s8 + s5
	y[5] = s8 - s5
	y[6] = s0 + s3
	y[7] = s2 - s1
	y[8] = s4 + s7
}

// mp3IMDCT36 is the inverse transform of n subbands of long blocks: buf
// holds 18 lines for each and is given 18 samples, the first half of the
// transform's output added to the second half of the one before, which
// overlap holds (folded to 9 values). window is laid over the join.
func mp3IMDCT36(buf, overlap []float32, window *[18]float32, n int) {
	for ; n > 0; n-- {
		var co, si [9]float32
		co[0] = -buf[0]
		si[0] = buf[17]
		for i := 0; i < 4; i++ {
			si[8-2*i] = buf[4*i+1] - buf[4*i+2]
			co[1+2*i] = buf[4*i+1] + buf[4*i+2]
			si[7-2*i] = buf[4*i+4] - buf[4*i+3]
			co[2+2*i] = -(buf[4*i+3] + buf[4*i+4])
		}
		mp3DCT9(&co)
		mp3DCT9(&si)
		si[1], si[3], si[5], si[7] = -si[1], -si[3], -si[5], -si[7]
		for i := 0; i < 9; i++ {
			ovl := overlap[i]
			sum := co[i]*mp3Twid9[9+i] + si[i]*mp3Twid9[i]
			overlap[i] = co[i]*mp3Twid9[i] - si[i]*mp3Twid9[9+i]
			buf[i] = ovl*window[i] - sum*window[9+i]
			buf[17-i] = ovl*window[9+i] + sum*window[i]
		}
		buf, overlap = buf[18:], overlap[9:]
	}
}

// mp3IMDCT12 is the inverse transform of one short block: every third line
// of x, from the first.
func mp3IMDCT12(x []float32, dst, overlap []float32) {
	idct3 := func(x0, x1, x2 float32) [3]float32 {
		m1 := x1 * 0.86602540
		a1 := x0 - x2*0.5
		return [3]float32{a1 + m1, x0 + x2, a1 - m1}
	}
	co := idct3(-x[0], x[6]+x[3], x[12]+x[9])
	si := idct3(x[15], x[12]-x[9], x[6]-x[3])
	si[1] = -si[1]
	for i := 0; i < 3; i++ {
		ovl := overlap[i]
		sum := co[i]*mp3Twid3[3+i] + si[i]*mp3Twid3[i]
		overlap[i] = co[i]*mp3Twid3[i] - si[i]*mp3Twid3[3+i]
		dst[i] = ovl*mp3Twid3[2-i] - sum*mp3Twid3[5-i]
		dst[5-i] = ovl*mp3Twid3[5-i] + sum*mp3Twid3[2-i]
	}
}

// mp3IMDCT is the inverse transform of a granule's channel, whose first
// long subbands are long blocks whatever the block type says.
func mp3IMDCT(buf, overlap []float32, blockType, long int) {
	if long > 0 {
		mp3IMDCT36(buf, overlap, &mp3Window[0], long)
		buf, overlap = buf[18*long:], overlap[9*long:]
	}
	switch blockType {
	case mp3Short:
		for n := 32 - long; n > 0; n-- {
			var tmp [18]float32
			copy(tmp[:], buf)
			copy(buf[:6], overlap)
			mp3IMDCT12(tmp[:], buf[6:12], overlap[6:9])
			mp3IMDCT12(tmp[1:], buf[12:18], overlap[6:9])
			mp3IMDCT12(tmp[2:], overlap[:6], overlap[6:9])
			buf, overlap = buf[18:], overlap[9:]
		}
	case mp3Stop:
		mp3IMDCT36(buf, overlap, &mp3Window[1], 32-long)
	default:
		mp3IMDCT36(buf, overlap, &mp3Window[0], 32-long)
	}
}

// mp3DCT32 is the 32-point DCT-II that starts the synthesis filter, over
// each of the 18 slots of a granule: buf holds 18 samples of each of 32
// subbands, and slot k is every 18th value from k.
func mp3DCT32(buf []float32) {
	for k := 0; k < 18; k++ {
		var t [4][8]float32
		y := buf[k:]
		for i := 0; i < 8; i++ {
			x0, x1, x2, x3 := y[i*18], y[(15-i)*18], y[(16+i)*18], y[(31-i)*18]
			t0, t1 := x0+x3, x1+x2
			t2 := (x1 - x2) * mp3Sec[3*i]
			t3 := (x0 - x3) * mp3Sec[3*i+1]
			t[0][i] = t0 + t1
			t[1][i] = (t0 - t1) * mp3Sec[3*i+2]
			t[2][i] = t3 + t2
			t[3][i] = (t3 - t2) * mp3Sec[3*i+2]
		}
		for i := range t {
			x := &t[i]
			x0, x1, x2, x3, x4, x5, x6, x7 := x[0], x[1], x[2], x[3], x[4], x[5], x[6], x[7]
			xt := x0 - x7
			x0 += x7
			x7 = x1 - x6
			x1 += x6
			x6 = x2 - x5
			x2 += x5
			x5 = x3 - x4
			x3 += x4
			x4 = x0 - x3
			x0 += x3
			x3 = x1 - x2
			x1 += x2
			x[0] = x0 + x1
			x[4] = (x0 - x1) * 0.70710677
			x5 = x5 + x6
			x6 = (x6 + x7) * 0.70710677
			x7 = x7 + xt
			x3 = (x3 + x4) * 0.70710677
			x5 -= x7 * 0.198912367 // a rotation by π/8
			x7 += x5 * 0.382683432
			x5 -= x7 * 0.198912367
			x0 = xt - x6
			xt += x6
			x[1] = (xt + x7) * 0.50979561
			x[2] = (x4 + x3) * 0.54119611
			x[3] = (x0 - x5) * 0.60134488
			x[5] = (x0 + x5) * 0.89997619
			x[6] = (x4 - x3) * 1.30656302
			x[7] = (xt - x7) * 2.56291556
		}
		for i := 0; i < 7; i++ {
			y[0] = t[0][i]
			y[18] = t[2][i] + t[3][i] + t[3][i+1]
			y[2*18] = t[1][i] + t[1][i+1]
			y[3*18] = t[2][i+1] + t[3][i] + t[3][i+1]
			y = y[4*18:]
		}
		y[0] = t[0][7]
		y[18] = t[2][7] + t[3][7]
		y[2*18] = t[1][7]
		y[3*18] = t[3][7]
	}
}

// synthGranule runs the synthesis filter over a granule's subband samples
// in d.spec and writes its 576 samples of each channel to pcm.
func (d *MP3Decoder) synthGranule(pcm []float32, channels int) {
	for c := 0; c < channels; c++ {
		mp3DCT32(d.spec[c*mp3Lines : (c+1)*mp3Lines])
	}
	// lins is the filter's history, 64 values to a slot (four lanes: the two
	// channels of each of the two slots that are worked together): the 15
	// slots the last granule left, then this granule's 18.
	copy(d.lins[:], d.qmf[:])
	clear(d.lins[len(d.qmf):])
	for i := 0; i < 18; i += 2 {
		d.synth(i, pcm[32*channels*i:], channels, d.lins[i*64:])
	}
	copy(d.qmf[:], d.lins[18*64:])
}

// mp3SynthPair gives the two output samples of a pair of slots that the
// window meets at its centre: one of each slot, 16 samples apart.
func mp3SynthPair(pcm []float32, channels int, z []float32) {
	a := (z[14*64] - z[0]) * 29
	a += (z[1*64] + z[13*64]) * 213
	a += (z[12*64] - z[2*64]) * 459
	a += (z[3*64] + z[11*64]) * 2037
	a += (z[10*64] - z[4*64]) * 5153
	a += (z[5*64] + z[9*64]) * 6574
	a += (z[8*64] - z[6*64]) * 37489
	a += z[7*64] * 75038
	pcm[0] = a * (1.0 / 32768)

	z = z[2:]
	a = z[14*64] * 104
	a += z[12*64] * 1567
	a += z[10*64] * 9727
	a += z[8*64] * 64019
	a += z[6*64] * -9975
	a += z[4*64] * -45
	a += z[2*64] * 146
	a += z[0*64] * -5
	pcm[16*channels] = a * (1.0 / 32768)
}

// synth makes the 64 output samples of each channel that slots i and i+1
// of the granule give. lins is the filter's history from 15 slots back.
func (d *MP3Decoder) synth(i int, pcm []float32, channels int, lins []float32) {
	xl := d.spec[i:]
	xr := d.spec[i+mp3Lines*(channels-1):]
	dl, dr := pcm, pcm[channels-1:]
	const z = 15 * 64 // the slot being made

	lins[z+4*15] = xl[18*16]
	lins[z+4*15+1] = xr[18*16]
	lins[z+4*15+2] = xl[0]
	lins[z+4*15+3] = xr[0]

	lins[z+4*31] = xl[1+18*16]
	lins[z+4*31+1] = xr[1+18*16]
	lins[z+4*31+2] = xl[1]
	lins[z+4*31+3] = xr[1]

	mp3SynthPair(dr, channels, lins[4*15+1:])
	mp3SynthPair(dr[32*channels:], channels, lins[4*15+64+1:])
	mp3SynthPair(dl, channels, lins[4*15:])
	mp3SynthPair(dl[32*channels:], channels, lins[4*15+64:])

	win := mp3Synth[:]
	for k := 14; k >= 0; k-- {
		lins[z+4*k] = xl[18*(31-k)]
		lins[z+4*k+1] = xr[18*(31-k)]
		lins[z+4*k+2] = xl[1+18*(31-k)]
		lins[z+4*k+3] = xr[1+18*(31-k)]
		lins[z+4*(k+16)] = xl[1+18*(1+k)]
		lins[z+4*(k+16)+1] = xr[1+18*(1+k)]
		lins[z+4*(k-16)+2] = xl[18*(1+k)]
		lins[z+4*(k-16)+3] = xr[18*(1+k)]

		var a, b [4]float32
		for tap := 0; tap < 8; tap++ {
			w0, w1 := win[0], win[1]
			win = win[2:]
			vz := lins[z+4*k-tap*64:]
			vy := lins[z+4*k-(15-tap)*64:]
			for j := 0; j < 4; j++ {
				b[j] += vz[j]*w1 + vy[j]*w0
				if tap&1 != 0 {
					a[j] += vy[j]*w1 - vz[j]*w0
				} else {
					a[j] += vz[j]*w0 - vy[j]*w1
				}
			}
		}
		dr[(15-k)*channels] = a[1] * (1.0 / 32768)
		dr[(17+k)*channels] = b[1] * (1.0 / 32768)
		dl[(15-k)*channels] = a[0] * (1.0 / 32768)
		dl[(17+k)*channels] = b[0] * (1.0 / 32768)
		dr[(47-k)*channels] = a[3] * (1.0 / 32768)
		dr[(49+k)*channels] = b[3] * (1.0 / 32768)
		dl[(47-k)*channels] = a[2] * (1.0 / 32768)
		dl[(49+k)*channels] = b[2] * (1.0 / 32768)
	}
}
