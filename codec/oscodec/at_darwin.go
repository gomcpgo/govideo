//go:build darwin

package oscodec

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/gomcpgo/govideo/codec"
)

var audioInputCallback uintptr

// asbd mirrors AudioStreamBasicDescription.
type asbd struct {
	SampleRate       float64
	FormatID         uint32
	FormatFlags      uint32
	BytesPerPacket   uint32
	FramesPerPacket  uint32
	BytesPerFrame    uint32
	ChannelsPerFrame uint32
	BitsPerChannel   uint32
	Reserved         uint32
}

// audioBufferList mirrors an AudioBufferList holding one AudioBuffer.
type audioBufferList struct {
	NumberBuffers  uint32
	_              uint32
	NumberChannels uint32
	DataByteSize   uint32
	Data           unsafe.Pointer
}

// packetDesc mirrors AudioStreamPacketDescription.
type packetDesc struct {
	StartOffset   int64
	VariableCount uint32
	DataByteSize  uint32
}

const (
	propBitRate       = 0x62726174 // 'brat'
	propBitRates      = 0x61656272 // 'aebr': the bit rates the encoder has for its formats
	propDelayMode     = 0x646d6f64 // 'dmod': how the encoder accounts for its delay
	propMaxOutputSize = 0x786f7073 // 'xops'
	propPrimeInfo     = 0x7072696d // 'prim'
	propDecoderCookie = 0x646d6763 // 'dmgc'

	// noMoreInput is returned by the input callback when it has nothing to
	// give yet; AudioConverterFillComplexBuffer passes it through.
	noMoreInput = 0x676f7631

	maxAudioPacket = 1 << 20
)

var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// pcmFormat is interleaved native-endian float32.
func pcmFormat(rate, channels int) asbd {
	const isFloat, isPacked = 1, 8
	return asbd{
		SampleRate: float64(rate), FormatID: fourCC("lpcm"), FormatFlags: isFloat | isPacked,
		BytesPerPacket: uint32(4 * channels), FramesPerPacket: 1, BytesPerFrame: uint32(4 * channels),
		ChannelsPerFrame: uint32(channels), BitsPerChannel: 32,
	}
}

// audioSource feeds an AudioConverter from its input callback.
type audioSource interface {
	supply(packets *uint32, data *audioBufferList, desc *unsafe.Pointer) uintptr
}

// onAudioInput is the AudioConverterComplexInputDataProc.
func onAudioInput(conv uintptr, packets *uint32, data *audioBufferList, desc *unsafe.Pointer, user uintptr) uintptr {
	src, _ := lookupHandle(user).(audioSource)
	if src == nil || packets == nil || data == nil {
		return noMoreInput
	}
	return src.supply(packets, data, desc)
}

// cbuf is a block of C memory. The converter keeps pointers to its input
// between calls, so input must not live in Go memory.
type cbuf struct {
	p unsafe.Pointer
	n int
}

func (b *cbuf) grow(n int) []byte {
	if n > b.n {
		if b.p != nil {
			cFree(b.p)
		}
		b.n = n + n/2
		b.p = cMalloc(b.n)
	}
	return unsafe.Slice((*byte)(b.p), b.n)
}

func (b *cbuf) free() {
	if b.p != nil {
		cFree(b.p)
		b.p, b.n = nil, 0
	}
}

// --- decode ------------------------------------------------------------------

type atDecoder struct {
	conv, handle   uintptr
	rate, channels int
	maxFrames      int
	in, out, desc  cbuf
	pending        int   // bytes of the packet waiting in d.in
	order          []int // channel reordering, nil for none
}

// NewAudioDecoder returns an AudioToolbox decoder for an AAC stream (AAC-LC,
// and HE-AAC when the configuration signals it explicitly).
func NewAudioDecoder(c AudioConfig) (codec.AudioDecoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	if c.Codec != "aac" {
		return nil, ErrUnsupported
	}
	if c.Channels < 1 || c.Channels > 8 || c.SampleRate < 1 {
		return nil, fmt.Errorf("oscodec: unsupported AAC layout (%d channels, %d Hz)", c.Channels, c.SampleRate)
	}
	in := asbd{SampleRate: float64(c.SampleRate), FormatID: fourCC("aac "), FormatFlags: 2,
		FramesPerPacket: 1024, ChannelsPerFrame: uint32(c.Channels)}
	if len(c.Config) >= 2 {
		// AudioSpecificConfig (ISO/IEC 14496-3 §1.6.2.1): object type, then
		// the sampling frequency index of the core coder.
		aot := int(c.Config[0] >> 3)
		if aot == 31 { // escape: the type is 32 plus the next six bits
			aot = 32 + int(c.Config[0]&7)<<3 | int(c.Config[1]>>5)
		}
		switch aot {
		case 42: // USAC (xHE-AAC)
			in.FormatID, in.FormatFlags = fourCC("usac"), 0
		case 5:
			in.FormatID, in.FormatFlags, in.FramesPerPacket = fourCC("aach"), 0, 2048
		case 29:
			in.FormatID, in.FormatFlags, in.FramesPerPacket = fourCC("aacp"), 0, 2048
		default:
			in.FormatFlags = uint32(aot)
			if idx := int(c.Config[0]&7)<<1 | int(c.Config[1]>>7); idx < len(aacRates) {
				in.SampleRate = float64(aacRates[idx])
			}
		}
	}
	d := &atDecoder{rate: int(in.SampleRate), channels: c.Channels, maxFrames: 2 * int(in.FramesPerPacket),
		order: aacChannelOrder[c.Channels]}
	out := pcmFormat(d.rate, d.channels)
	if st := audioConverterNew(&in, &out, &d.conv); st != 0 {
		return nil, osErr("opening the AAC decoder", st)
	}
	if len(c.Config) > 0 && len(c.Config) <= 4096 {
		cookie := esDescriptor(c.Config)
		audioConverterSetProperty(d.conv, propDecoderCookie, uint32(len(cookie)), unsafe.Pointer(&cookie[0]))
	}
	d.handle = newHandle(d)
	d.desc.grow(int(unsafe.Sizeof(packetDesc{})))
	d.out.grow(d.maxFrames * d.channels * 4)
	return d, nil
}

// esDescriptor wraps an AudioSpecificConfig in an MPEG-4 ES_Descriptor
// (ISO/IEC 14496-1 §7.2.6.5), the form AudioToolbox takes as a magic cookie.
func esDescriptor(asc []byte) []byte {
	// Descriptor sizes use the four-byte form, so any length fits.
	desc := func(tag byte, body ...[]byte) []byte {
		n := 0
		for _, b := range body {
			n += len(b)
		}
		out := []byte{tag, byte(n>>21) | 0x80, byte(n>>14) | 0x80, byte(n>>7) | 0x80, byte(n & 0x7f)}
		for _, b := range body {
			out = append(out, b...)
		}
		return out
	}
	// DecoderConfigDescriptor: MPEG-4 audio (0x40), audio stream (0x15),
	// buffer size and bit rates unknown.
	dcd := desc(4, []byte{0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, desc(5, asc))
	return desc(3, []byte{0, 0, 0}, dcd, desc(6, []byte{2}))
}

// aacChannelOrder maps, per channel count, each output position in the
// conventional order (L R C LFE Ls Rs, then extra surround pairs) to its
// position in the order AAC codes the channels (centre first, LFE last:
// ISO/IEC 14496-3 Table 1.19), which is the order the decoder delivers.
//
// Eight channels are read as 7.1 with side and back surrounds, the front
// pair coded straight after the centre. That is channelConfiguration 12,
// and it is also what files signalling configuration 7 carry in practice
// (the table defines 7 as five front channels, L and R coded after Lc and
// Rc, but real files and other decoders do not use it that way). Reading
// them the other way sent the front pair through the downmix at -3 dB and
// the side surrounds at unity (govideo#2).
var aacChannelOrder = map[int][]int{
	3: {1, 2, 0},                // C L R
	4: {1, 2, 0, 3},             // C L R Cs
	5: {1, 2, 0, 3, 4},          // C L R Ls Rs
	6: {1, 2, 0, 5, 3, 4},       // C L R Ls Rs LFE
	8: {1, 2, 0, 7, 3, 4, 5, 6}, // C L R Ls Rs Lb Rb LFE
}

func (d *atDecoder) supply(packets *uint32, data *audioBufferList, desc *unsafe.Pointer) uintptr {
	if d.pending == 0 {
		*packets = 0
		data.DataByteSize = 0
		return noMoreInput
	}
	data.NumberBuffers, data.NumberChannels = 1, uint32(d.channels)
	data.Data, data.DataByteSize = d.in.p, uint32(d.pending)
	if desc != nil {
		*(*packetDesc)(d.desc.p) = packetDesc{DataByteSize: uint32(d.pending)}
		*desc = d.desc.p
	}
	*packets = 1
	d.pending = 0
	return 0
}

// Decode decodes one AAC access unit.
func (d *atDecoder) Decode(p []byte) (*codec.AudioFrame, error) {
	if d.conv == 0 {
		return nil, errors.New("oscodec: decoder is closed")
	}
	fr := &codec.AudioFrame{SampleRate: d.rate, Channels: d.channels}
	if len(p) == 0 {
		return fr, nil
	}
	if len(p) > maxAudioPacket {
		return nil, fmt.Errorf("oscodec: AAC packet of %d bytes is too large", len(p))
	}
	copy(d.in.grow(len(p)), p)
	d.pending = len(p)
	for {
		frames := uint32(d.maxFrames)
		abl := audioBufferList{NumberBuffers: 1, NumberChannels: uint32(d.channels),
			DataByteSize: uint32(d.maxFrames * d.channels * 4), Data: d.out.p}
		st := audioConverterFill(d.conv, audioInputCallback, d.handle, &frames, &abl, nil)
		if st != 0 && st != noMoreInput {
			return nil, osErr("decoding an AAC packet", st)
		}
		if n := int(frames) * d.channels; n > 0 {
			src := unsafe.Slice((*float32)(d.out.p), n)
			if d.order == nil {
				fr.Samples = append(fr.Samples, src...)
			} else {
				for i := 0; i < n; i += d.channels {
					for _, from := range d.order {
						fr.Samples = append(fr.Samples, src[i+from])
					}
				}
			}
		}
		if st == noMoreInput || frames == 0 {
			return fr, nil
		}
	}
}

// Close releases the converter.
func (d *atDecoder) Close() error {
	if d.conv != 0 {
		audioConverterDispose(d.conv)
		dropHandle(d.handle)
		d.in.free()
		d.out.free()
		d.desc.free()
		d.conv = 0
	}
	return nil
}

// --- encode ------------------------------------------------------------------

type atEncoder struct {
	conv, handle   uintptr
	rate, channels int
	bitrate        int
	priming        int
	config         []byte
	in, out, desc  cbuf
	queue          []float32 // samples not yet handed to the converter
	loaded, used   int       // frames in e.in, and how many the converter took
	flushing       bool
}

// aacBitrates returns the bit rates an open AAC encoder has for its sample
// rate and channel count, as ranges of bits per second. AudioToolbox has a
// short list for each layout (12 to 48 kbit/s for 16 kHz mono, 64 to
// 320 kbit/s for 48 kHz stereo) and refuses any other.
func aacBitrates(conv uintptr) [][2]float64 {
	var size uint32
	if st := audioConverterPropertyInfo(conv, propBitRates, &size, nil); st != 0 || size < 16 || size > 1<<16 {
		return nil
	}
	ranges := make([][2]float64, size/16) // AudioValueRange: minimum, maximum
	size = uint32(len(ranges) * 16)
	if st := audioConverterGetProperty(conv, propBitRates, &size, unsafe.Pointer(&ranges[0])); st != 0 {
		return nil
	}
	return ranges[:min(int(size/16), len(ranges))]
}

// nearestBitrate returns the bit rate within ranges that is closest to want,
// the higher of two that are equally close; want itself if there are none.
func nearestBitrate(want int, ranges [][2]float64) int {
	best, found := want, false
	for _, r := range ranges {
		lo, hi := int(r[0]), int(r[1])
		if lo <= 0 || hi < lo {
			continue
		}
		v := min(max(want, lo), hi)
		if d, bd := abs(v-want), abs(best-want); !found || d < bd || d == bd && v > best {
			best, found = v, true
		}
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// NewAACEncoder returns an AudioToolbox AAC-LC encoder.
func NewAACEncoder(c AudioEncodeConfig) (codec.AudioStreamEncoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	idx := -1
	for i, r := range aacRates {
		if r == c.SampleRate {
			idx = i
		}
	}
	if idx < 0 || c.Channels < 1 || c.Channels > 2 {
		return nil, fmt.Errorf("oscodec: unsupported AAC layout (%d channels, %d Hz)", c.Channels, c.SampleRate)
	}
	e := &atEncoder{rate: c.SampleRate, channels: c.Channels}
	in := pcmFormat(c.SampleRate, c.Channels)
	out := asbd{SampleRate: float64(c.SampleRate), FormatID: fourCC("aac "), FormatFlags: 2,
		FramesPerPacket: 1024, ChannelsPerFrame: uint32(c.Channels)}
	if st := audioConverterNew(&in, &out, &e.conv); st != 0 {
		return nil, osErr("opening the AAC encoder", st)
	}
	// At the lowest bit rates of each layout (up to 24 kbit/s for 16 kHz
	// mono, 64 kbit/s for 48 kHz stereo) the encoder's delay is not what
	// it is at the others. In its default delay mode, "compatibility", it
	// still reports the usual 2112 priming frames while the sound in fact
	// comes 1088 frames late, so a file made by the reported figure plays
	// 1024 samples early. In "minimum" mode (1) the figure it reports is
	// the delay it has (2127 to 2145 at those rates), and at every other
	// rate it is 2112 and the packets are the same, byte for byte. Measured
	// for every layout and bit rate (SPEC §14.1); TestAACPriming repeats
	// it. An encoder that refuses the mode is left as it is.
	mode := uint32(1)
	audioConverterSetProperty(e.conv, propDelayMode, 4, unsafe.Pointer(&mode))
	if c.Bitrate > 0 {
		rate := uint32(nearestBitrate(c.Bitrate, aacBitrates(e.conv)))
		if st := audioConverterSetProperty(e.conv, propBitRate, 4, unsafe.Pointer(&rate)); st != 0 {
			audioConverterDispose(e.conv)
			return nil, osErr(fmt.Sprintf("setting the AAC bit rate to %d", rate), st)
		}
	}
	// What the encoder aims at: the rate just set, or its own choice for
	// the layout.
	rate, size := uint32(0), uint32(4)
	if st := audioConverterGetProperty(e.conv, propBitRate, &size, unsafe.Pointer(&rate)); st == 0 {
		e.bitrate = int(rate)
	}
	maxOut, size := uint32(0), uint32(4)
	if st := audioConverterGetProperty(e.conv, propMaxOutputSize, &size, unsafe.Pointer(&maxOut)); st != 0 || maxOut == 0 || maxOut > maxAudioPacket {
		maxOut = 8192
	}
	// Read once the bit rate is set: the priming depends on it.
	var prime [2]uint32 // leading and trailing frames
	size = 8
	if st := audioConverterGetProperty(e.conv, propPrimeInfo, &size, unsafe.Pointer(&prime)); st == 0 {
		e.priming = int(prime[0])
	} else {
		e.priming = 2112
	}
	// AudioSpecificConfig: AAC-LC (object type 2), rate index, channel count.
	e.config = []byte{2<<3 | byte(idx>>1), byte(idx&1)<<7 | byte(c.Channels)<<3}
	e.out.grow(int(maxOut))
	e.desc.grow(int(unsafe.Sizeof(packetDesc{})))
	e.handle = newHandle(e)
	return e, nil
}

func (e *atEncoder) supply(packets *uint32, data *audioBufferList, desc *unsafe.Pointer) uintptr {
	avail := e.loaded - e.used
	if avail <= 0 {
		*packets = 0
		data.DataByteSize = 0
		if e.flushing {
			return 0 // end of stream
		}
		return noMoreInput
	}
	n := min(int(*packets), avail)
	bpf := 4 * e.channels
	data.NumberBuffers, data.NumberChannels = 1, uint32(e.channels)
	data.Data, data.DataByteSize = unsafe.Add(e.in.p, e.used*bpf), uint32(n*bpf)
	*packets = uint32(n)
	e.used += n
	return 0
}

// run pulls packets while at least one packet's worth of input is queued
// (or, when flushing, until the converter is drained).
func (e *atEncoder) run() ([][]byte, error) {
	var out [][]byte
	for {
		frames := len(e.queue) / e.channels
		if !e.flushing && frames < 1024 {
			return out, nil
		}
		frames = min(frames, 4096)
		if n := frames * e.channels; n > 0 {
			copy(unsafe.Slice((*float32)(unsafe.Pointer(&e.in.grow(n * 4)[0])), n), e.queue)
		}
		e.loaded, e.used = frames, 0
		packets := uint32(1)
		abl := audioBufferList{NumberBuffers: 1, NumberChannels: uint32(e.channels), DataByteSize: uint32(e.out.n), Data: e.out.p}
		st := audioConverterFill(e.conv, audioInputCallback, e.handle, &packets, &abl, e.desc.p)
		e.queue = e.queue[:copy(e.queue, e.queue[e.used*e.channels:])]
		e.loaded, e.used = 0, 0
		if st != 0 && st != noMoreInput {
			return out, osErr("encoding AAC", st)
		}
		if packets > 0 && abl.DataByteSize > 0 {
			out = append(out, append([]byte(nil), unsafe.Slice((*byte)(e.out.p), abl.DataByteSize)...))
		} else if e.flushing || st == noMoreInput {
			return out, nil
		}
	}
}

func (e *atEncoder) Encode(samples []float32) ([][]byte, error) {
	if e.conv == 0 || e.flushing {
		return nil, errors.New("oscodec: encoder is closed")
	}
	e.queue = append(e.queue, samples...)
	return e.run()
}

func (e *atEncoder) Flush() ([][]byte, error) {
	if e.conv == 0 || e.flushing {
		return nil, nil
	}
	e.flushing = true
	return e.run()
}

func (e *atEncoder) Config() []byte { return e.config }
func (e *atEncoder) FrameSize() int { return 1024 }
func (e *atEncoder) Bitrate() int   { return e.bitrate }
func (e *atEncoder) Priming() int   { return e.priming }

func (e *atEncoder) Close() error {
	if e.conv != 0 {
		audioConverterDispose(e.conv)
		dropHandle(e.handle)
		e.in.free()
		e.out.free()
		e.desc.free()
		e.conv = 0
	}
	return nil
}
