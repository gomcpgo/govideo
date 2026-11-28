//go:build darwin

package oscodec

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/internal/kernels"
)

var decodeCallback, encodeCallback uintptr

const (
	pixel420v = 0x34323076 // '420v': 8-bit 4:2:0 bi-planar, video range
	pixel420f = 0x34323066 // '420f': the same, full range
	pixelX420 = 0x78343230 // 'x420': 10-bit 4:2:0 bi-planar (P010), video range
	pixelXF20 = 0x78663230 // 'xf20': the same, full range
)

// --- decode ------------------------------------------------------------------

// vtDecoder is a VideoToolbox decompression session and the stream it is
// decoding. Decoding is synchronous: the output callback has run for a
// sample by the time VTDecompressionSessionDecodeFrame returns.
type vtDecoder struct {
	session, format, handle uintptr
	codec                   string // "h264" or "hevc"
	lenSize                 int    // bytes in the length prefix of each NAL unit
	deep                    bool   // the session gives 10-bit pictures (P010)

	// used says that the session has been given a sample; restarted, that
	// it is to go on with another stream and has not been given that
	// stream's first sample yet (see Restart); failed, that a sample could
	// not be decoded, after which the session is not reused.
	used, restarted, failed bool

	mu      sync.Mutex
	tag     uintptr
	pending map[uintptr]time.Duration
	out     []codec.TimedPicture
	err     error
	matrix  int                     // H.273 code of the decoded pictures; 0 until known
	live    map[*vtPicture]struct{} // pictures handed out and not yet released
}

// NewVideoDecoder returns a VideoToolbox decoder for an H.264 or HEVC stream.
// Pictures come back as 8-bit 4:2:0, 10-bit sources reduced to 8 bits by the
// system, unless c.Deep asks for 10 bits (see VideoConfig).
func NewVideoDecoder(c VideoConfig) (codec.VideoStreamDecoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	d := &vtDecoder{pending: map[uintptr]time.Duration{}, live: map[*vtPicture]struct{}{}}
	format, err := videoFormat(c)
	if err != nil {
		return nil, err
	}
	if err := d.open(c.Codec, format, c.Deep); err != nil {
		return nil, err
	}
	d.start(c)
	return d, nil
}

// videoFormat builds the format description of a stream from its decoder
// configuration record. The caller releases it.
func videoFormat(c VideoConfig) (uintptr, error) {
	var codecType, atom string
	switch c.Codec {
	case "h264":
		codecType, atom = "avc1", "avcC"
	case "hevc":
		codecType, atom = "hvc1", "hvcC"
	default:
		return 0, ErrUnsupported
	}
	if len(c.Record) == 0 || c.Width <= 0 || c.Height <= 0 {
		return 0, fmt.Errorf("oscodec: %s stream has no %s configuration record", c.Codec, atom)
	}
	data := cfDataCreate(0, unsafe.Pointer(&c.Record[0]), len(c.Record))
	name := cfStringCreateWithCString(0, atom, 0x08000100)
	atoms := cfDict(name, data)
	ext := cfDict(k.atoms, atoms)
	var format uintptr
	st := cmVideoFormatDescriptionCreate(0, fourCC(codecType), int32(c.Width), int32(c.Height), ext, &format)
	for _, o := range []uintptr{ext, atoms, name, data} {
		cfRelease(o)
	}
	if st != 0 {
		return 0, osErr("creating the "+c.Codec+" format description", st)
	}
	return format, nil
}

// nalLengthSize returns the size of the length prefix of the NAL units in a
// stream's samples, from its configuration record (ISO/IEC 14496-15 §5.3.3
// and §8.3.3), or 0 when the record is too short to say.
func nalLengthSize(c VideoConfig) int {
	at := map[string]int{"h264": 4, "hevc": 21}[c.Codec]
	if at == 0 || len(c.Record) <= at {
		return 0
	}
	return int(c.Record[at]&3) + 1
}

// open creates the session for a stream of the given format, which it takes
// over. The session converts every picture to the pixel format asked for
// here, whatever the depth of the stream: 10 bits with deep, else 8.
func (d *vtDecoder) open(codecName string, format uintptr, deep bool) error {
	pf := cfInt(pixel420v)
	if deep {
		cfRelease(pf)
		pf = cfInt(pixelX420)
	}
	attrs := cfDict(k.pixelFormatKey, pf)
	handle := newHandle(d)
	record := [2]uintptr{decodeCallback, handle}
	var session uintptr
	st := vtDecompressionSessionCreate(0, format, 0, attrs, unsafe.Pointer(&record), &session)
	cfRelease(attrs)
	cfRelease(pf)
	if st != 0 {
		dropHandle(handle)
		cfRelease(format)
		return osErr("opening the "+codecName+" decoder", st)
	}
	stats.sessions.Add(1)
	d.session, d.format, d.handle, d.used, d.deep = session, format, handle, false, deep
	return nil
}

// start readies the decoder's state for the first sample of stream c.
func (d *vtDecoder) start(c VideoConfig) {
	d.codec, d.lenSize = c.Codec, nalLengthSize(c)
	d.restarted, d.failed = false, false
	d.mu.Lock()
	clear(d.pending)
	d.out, d.err, d.matrix = d.out[:0], nil, c.Matrix
	d.mu.Unlock()
}

// shut ends the session, if there is one. Pictures already handed out stay
// valid: each holds its own reference to its pixel buffer.
func (d *vtDecoder) shut() {
	if d.session == 0 {
		return
	}
	vtDecompressionSessionWait(d.session)
	vtDecompressionSessionInvalidate(d.session)
	cfRelease(d.session)
	cfRelease(d.format)
	dropHandle(d.handle)
	d.session, d.format, d.handle = 0, 0, 0
}

// Restart makes the decoder ready for another stream, as a decoder newly
// opened for c would be, and reports whether it could. It can when the
// session is in good order and accepts the stream's format: the same codec
// and depth of output, and parameter sets the system decoder is willing to
// switch to. The pictures of the stream before are released.
//
// A session that has decoded something is kept only if the new stream then
// begins with a picture that clears the decoder's reference pictures (see
// cleanStart); otherwise the first Decode opens a new one in its place.
// Either way the caller has a working decoder.
func (d *vtDecoder) Restart(c VideoConfig) bool {
	if d.session == 0 || d.failed || c.Codec != d.codec || c.Deep != d.deep {
		return false
	}
	format, err := videoFormat(c)
	if err != nil {
		return false
	}
	if !cfEqual(format, d.format) && !vtDecompressionSessionCanAccept(d.session, format) {
		cfRelease(format)
		return false
	}
	vtDecompressionSessionWait(d.session)
	d.releaseAll()
	cfRelease(d.format)
	d.format = format
	d.start(c)
	d.restarted = true
	return true
}

// cleanStart reports whether a sample holds a picture at which a decoder
// drops every reference picture it has: an IDR picture in H.264 (NAL unit
// type 5, H.264 §7.4.1.2.4), an IDR or BLA picture in HEVC (types 16 to 20,
// H.265 §8.1.3). From there on a session that has decoded something else
// behaves like a new one. A stream entered at any other sync sample (a
// recovery point, a CRA picture) leaves the old references in place, which
// a new session would not have had.
func cleanStart(codecName string, lenSize int, sample []byte) bool {
	if lenSize == 0 {
		return false
	}
	// A sample has a handful of NAL units; the bound is against bad input.
	for range 256 {
		if len(sample) < lenSize+2 {
			return false
		}
		n := 0
		for _, b := range sample[:lenSize] {
			n = n<<8 | int(b)
		}
		sample = sample[lenSize:]
		if n < 1 || n > len(sample) {
			return false
		}
		switch codecName {
		case "h264":
			if sample[0]&0x1f == 5 {
				return true
			}
		case "hevc":
			if t := sample[0] >> 1 & 0x3f; t >= 16 && t <= 20 {
				return true
			}
		}
		sample = sample[n:]
	}
	return false
}

// onDecodedFrame is the VTDecompressionOutputCallback. The two trailing
// CMTime arguments are not declared: the frame is identified by its tag.
//
// The pixel buffer is kept, not copied: the picture holds a reference to it
// until it is released, and it is copied out only if it is shown.
func onDecodedFrame(refCon, tag, status, flags, image uintptr) {
	d, _ := lookupHandle(refCon).(*vtDecoder)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	pts, ok := d.pending[tag]
	delete(d.pending, tag)
	if !ok {
		return
	}
	if int32(status) != 0 || image == 0 {
		if int32(status) != 0 && d.err == nil {
			d.err = osErr("decoding a video frame", int32(status))
		}
		return // a dropped frame
	}
	p := &vtPicture{d: d, image: cfRetain(image)}
	d.live[p] = struct{}{}
	stats.pictures.Add(1)
	d.out = append(d.out, codec.TimedPicture{Picture: p, PTS: pts})
}

// vtPicture is a decoded picture: a reference to the pixel buffer the
// session produced.
type vtPicture struct {
	d     *vtDecoder
	image uintptr // 0 once released
}

func (p *vtPicture) Release() {
	p.d.mu.Lock()
	defer p.d.mu.Unlock()
	p.release()
}

// release drops the pixel buffer; the decoder's mutex is held.
func (p *vtPicture) release() {
	if p.image == 0 {
		return
	}
	cfRelease(p.image)
	p.image = 0
	delete(p.d.live, p)
	stats.pictures.Add(-1)
}

// releaseAll releases the pictures that are still out.
func (d *vtDecoder) releaseAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for p := range d.live {
		p.release()
	}
	d.out = d.out[:0]
}

// planes is a decoded pixel buffer, locked for reading: its two planes and
// what it says of itself.
type planes struct {
	image     uintptr
	format    uint32
	w, h      int
	matrix    int
	y, c      unsafe.Pointer
	ys, cs    int // bytes in a row of each plane
	fullRange bool
}

// lock locks the picture's pixel buffer for reading, having checked that it
// is of one of the two given pixel formats (limited and full range); unlock
// lets it go.
func (p *vtPicture) lock(limited, full uint32, sample int) (planes, error) {
	d := p.d
	d.mu.Lock()
	image := p.image
	d.mu.Unlock()
	if image == 0 {
		return planes{}, errors.New("oscodec: a decoded picture was used after its release")
	}
	format := cvPixelBufferGetPixelFormatType(image)
	if (format != limited && format != full) || cvPixelBufferGetPlaneCount(image) != 2 {
		return planes{}, fmt.Errorf("oscodec: decoder returned unexpected pixel format %#x", format)
	}
	d.mu.Lock()
	if d.matrix == 0 {
		d.matrix = 2 // unspecified
		if v := cvBufferCopyAttachment(image, k.matrixKey, nil); v != 0 {
			switch {
			case cfEqual(v, k.matrix709):
				d.matrix = 1
			case cfEqual(v, k.matrix601):
				d.matrix = 6
			case cfEqual(v, k.matrix2020):
				d.matrix = 9
			}
			cfRelease(v)
		}
	}
	matrix := d.matrix
	d.mu.Unlock()
	const readOnly = 1
	if st := cvPixelBufferLockBaseAddress(image, readOnly); st != 0 {
		return planes{}, osErr("locking a decoded frame", st)
	}
	b := planes{image: image, format: format, matrix: matrix, fullRange: format == full,
		w: cvPixelBufferGetWidth(image), h: cvPixelBufferGetHeight(image)}
	b.y, b.ys = cvPixelBufferGetBaseAddressPlane(image, 0), cvPixelBufferGetBytesPerRowPlane(image, 0)
	b.c, b.cs = cvPixelBufferGetBaseAddressPlane(image, 1), cvPixelBufferGetBytesPerRowPlane(image, 1)
	var err error
	switch {
	case b.w <= 0 || b.h <= 0 || b.w > 1<<15 || b.h > 1<<15:
		err = fmt.Errorf("oscodec: decoder returned a %dx%d frame", b.w, b.h)
	case b.y == nil || b.c == nil || b.ys < b.w*sample || b.cs < 2*((b.w+1)/2)*sample:
		err = errors.New("oscodec: decoded frame has no readable planes")
	}
	if err != nil {
		b.unlock()
		return planes{}, err
	}
	return b, nil
}

func (b planes) unlock() {
	const readOnly = 1
	cvPixelBufferUnlockBaseAddress(b.image, readOnly)
}

// Frame copies the bi-planar pixel buffer into a planar frame. A picture of
// a session that gives 10 bits is reduced to 8 by dropping the low bits:
// the tone mapping of such a picture reads it with DeepFrame instead.
func (p *vtPicture) Frame(into *codec.VideoFrame) (*codec.VideoFrame, error) {
	if p.d.deep {
		deep, err := p.DeepFrame(nil)
		if err != nil {
			return nil, err
		}
		f := into.Reuse(deep.Width, deep.Height)
		f.Matrix, f.FullRange = deep.Matrix, deep.FullRange
		for i, v := range deep.Y {
			f.Y[i] = byte(v >> 8)
		}
		for i := range deep.Cb {
			f.Cb[i], f.Cr[i] = byte(deep.Cb[i]>>8), byte(deep.Cr[i]>>8)
		}
		return f, nil
	}
	b, err := p.lock(pixel420v, pixel420f, 1)
	if err != nil {
		return nil, err
	}
	defer b.unlock()
	w, h := b.w, b.h
	cw, ch := (w+1)/2, (h+1)/2
	f := into.Reuse(w, h)
	f.Matrix, f.FullRange = b.matrix, b.fullRange
	src := unsafe.Slice((*byte)(b.y), b.ys*h)
	for y := range h {
		copy(f.Y[y*w:y*w+w], src[y*b.ys:])
	}
	src = unsafe.Slice((*byte)(b.c), b.cs*ch)
	for y := range ch {
		kernels.SplitPairs(f.Cb[y*cw:y*cw+cw], f.Cr[y*cw:y*cw+cw], src[y*b.cs:])
	}
	return f, nil
}

// DeepFrame copies the 10-bit bi-planar pixel buffer (P010) of a session
// opened with VideoConfig.Deep into a planar deep frame. P010 holds each
// sample in the top bits of 16, as a DeepFrame does.
func (p *vtPicture) DeepFrame(into *codec.DeepFrame) (*codec.DeepFrame, error) {
	if !p.d.deep {
		return nil, errors.New("oscodec: the decoder was not opened for pictures of more than 8 bits")
	}
	b, err := p.lock(pixelX420, pixelXF20, 2)
	if err != nil {
		return nil, err
	}
	defer b.unlock()
	w, h := b.w, b.h
	cw, ch := (w+1)/2, (h+1)/2
	f := into.Reuse(w, h)
	f.Depth, f.Matrix, f.FullRange = 10, b.matrix, b.fullRange
	for y := range h {
		copy(f.Y[y*w:y*w+w], unsafe.Slice((*uint16)(unsafe.Add(b.y, y*b.ys)), w))
	}
	for y := range ch {
		kernels.SplitPairs16(f.Cb[y*cw:y*cw+cw], f.Cr[y*cw:y*cw+cw], unsafe.Slice((*uint16)(unsafe.Add(b.c, y*b.cs)), 2*cw))
	}
	return f, nil
}

func (d *vtDecoder) take() ([]codec.TimedPicture, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := d.out, d.err
	d.out, d.err = d.out[:0], nil
	return out, err
}

func (d *vtDecoder) Decode(p []byte, pts time.Duration) ([]codec.TimedPicture, error) {
	if d.session == 0 {
		return nil, errors.New("oscodec: decoder is closed")
	}
	if len(p) == 0 {
		return nil, nil
	}
	if d.restarted && d.used {
		if cleanStart(d.codec, d.lenSize, p) {
			stats.reused.Add(1)
		} else {
			// The session still refers to pictures of the stream before.
			format := cfRetain(d.format)
			d.shut()
			if err := d.open(d.codec, format, d.deep); err != nil {
				return nil, err
			}
		}
	}
	d.restarted, d.used = false, true
	const assureMemoryNow = 1
	var block, sbuf uintptr
	if st := cmBlockBufferCreateWithMemoryBlock(0, 0, len(p), 0, 0, 0, len(p), assureMemoryNow, &block); st != 0 {
		return nil, osErr("allocating a sample", st)
	}
	defer cfRelease(block)
	if st := cmBlockBufferReplaceDataBytes(unsafe.Pointer(&p[0]), block, 0, len(p)); st != 0 {
		return nil, osErr("copying a sample", st)
	}
	size := uintptr(len(p))
	if st := cmSampleBufferCreateReady(0, block, d.format, 1, 0, 0, 1, &size, &sbuf); st != 0 {
		return nil, osErr("wrapping a sample", st)
	}
	defer cfRelease(sbuf)
	d.mu.Lock()
	d.tag++
	tag := d.tag
	d.pending[tag] = pts
	d.mu.Unlock()
	// Flags 0: decode synchronously, so the callback has run on return.
	st := vtDecompressionSessionDecode(d.session, sbuf, 0, tag, nil)
	out, err := d.take()
	if st != 0 && err == nil {
		err = osErr("decoding a video frame", st)
	}
	if err != nil {
		d.failed = true
		stats.failures.Add(1)
	}
	d.mu.Lock()
	delete(d.pending, tag)
	d.mu.Unlock()
	return out, err
}

func (d *vtDecoder) Flush() ([]codec.TimedPicture, error) {
	if d.session == 0 {
		return nil, nil
	}
	vtDecompressionSessionWait(d.session)
	return d.take()
}

func (d *vtDecoder) Close() error {
	d.shut()
	d.releaseAll()
	return nil
}

// --- encode ------------------------------------------------------------------

type vtEncoder struct {
	session, handle, attrs uintptr
	cfg                    EncodeConfig
	next                   int64

	mu     sync.Mutex
	out    []codec.EncodedFrame
	err    error
	config []byte
}

// NewH264Encoder returns a VideoToolbox H.264 High profile encoder. Frames
// must be BT.709 limited range with even dimensions. Unless c.Reorder is set
// samples come out in presentation order and need no composition offsets.
func NewH264Encoder(c EncodeConfig) (codec.VideoStreamEncoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	if c.Width <= 0 || c.Height <= 0 || c.Width%2 != 0 || c.Height%2 != 0 {
		return nil, fmt.Errorf("oscodec: H.264 needs even picture dimensions, not %dx%d", c.Width, c.Height)
	}
	if c.FrameNum <= 0 || c.FrameDen <= 0 || c.FrameNum > 1<<30 {
		return nil, fmt.Errorf("oscodec: unsupported frame rate %d/%d", c.FrameNum, c.FrameDen)
	}
	e := &vtEncoder{cfg: c}
	e.handle = newHandle(e)
	if st := vtCompressionSessionCreate(0, int32(c.Width), int32(c.Height), fourCC("avc1"), 0, 0, 0, encodeCallback, e.handle, &e.session); st != 0 {
		dropHandle(e.handle)
		return nil, osErr("opening the H.264 encoder", st)
	}
	set := func(what string, key, value uintptr, owned bool) error {
		st := vtSessionSetProperty(e.session, key, value)
		if owned {
			cfRelease(value)
		}
		if st != 0 {
			return osErr("setting the encoder's "+what, st)
		}
		return nil
	}
	fps := int32((c.FrameNum + c.FrameDen/2) / c.FrameDen)
	key := int32(c.KeyInterval)
	if key <= 0 {
		key = 2 * max(fps, 1)
	}
	if c.Quality < 0 || c.Quality > 1 {
		e.Close()
		return nil, fmt.Errorf("oscodec: encoder quality %v is outside 0..1", c.Quality)
	}
	reorder := k.cfFalse
	if c.Reorder {
		reorder = k.cfTrue
	}
	err := errors.Join(
		set("profile", k.profileLevel, k.highAuto, false),
		set("real-time mode", k.realTime, k.cfFalse, false),
		set("frame reordering", k.reorder, reorder, false),
		set("keyframe interval", k.keyInterval, cfInt(key), true),
		set("colour primaries", k.vtPrimaries, k.primaries709, false),
		set("transfer function", k.vtTransfer, k.transfer709, false),
		set("colour matrix", k.vtMatrix, k.matrix709, false),
	)
	if err == nil && c.Quality > 0 {
		if qerr := set("quality", k.quality, cfFloat(c.Quality), true); qerr != nil {
			err = fmt.Errorf("%w (%v)", ErrNoQualityMode, qerr)
		}
	}
	if err == nil && c.Bitrate > 0 {
		err = set("bit rate", k.bitRate, cfInt(int32(min(c.Bitrate, 1<<30))), true)
	}
	if err == nil && c.MaxBitrate > 0 {
		// One limit: this many bytes in any one second (of decode time).
		bytes, seconds := cfInt(int32(min(c.MaxBitrate, 1<<30)/8)), cfInt(1)
		err = set("bit rate ceiling", k.rateLimits, cfArray(bytes, seconds), true)
		cfRelease(bytes)
		cfRelease(seconds)
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	set("frame rate", k.expectedFPS, cfInt(max(fps, 1)), true) // a hint; failure is harmless
	surface := cfDict()
	e.attrs = cfDict(k.ioSurfaceKey, surface)
	cfRelease(surface)
	return e, nil
}

// onEncodedFrame is the VTCompressionOutputCallback.
func onEncodedFrame(refCon, tag, status, flags, sbuf uintptr) {
	e, _ := lookupHandle(refCon).(*vtEncoder)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	fail := func(err error) {
		if e.err == nil {
			e.err = err
		}
	}
	const frameDropped = 2
	switch {
	case int32(status) != 0:
		fail(osErr("encoding a video frame", int32(status)))
		return
	case sbuf == 0 || flags&frameDropped != 0:
		fail(errors.New("oscodec: the H.264 encoder dropped a frame"))
		return
	}
	block := cmSampleBufferGetDataBuffer(sbuf)
	n := cmBlockBufferGetDataLength(block)
	if block == 0 || n <= 0 {
		fail(errors.New("oscodec: the H.264 encoder returned an empty sample"))
		return
	}
	data := make([]byte, n)
	if st := cmBlockBufferCopyDataBytes(block, 0, n, unsafe.Pointer(&data[0])); st != 0 {
		fail(osErr("reading an encoded sample", st))
		return
	}
	sync := true
	if arr := cmSampleBufferGetAttachments(sbuf, false); arr != 0 {
		if dict := cfArrayGetValueAtIndex(arr, 0); dict != 0 {
			sync = cfDictionaryGetValue(dict, k.notSync) != k.cfTrue
		}
	}
	if e.config == nil {
		cfg, err := avcConfig(cmSampleBufferGetFormatDescription(sbuf))
		if err != nil {
			fail(err)
			return
		}
		e.config = cfg
	}
	e.out = append(e.out, codec.EncodedFrame{Data: data, Index: int64(tag) - 1, Sync: sync})
}

// avcConfig builds an AVCDecoderConfigurationRecord (ISO/IEC 14496-15 §5.3.3)
// from the parameter sets of an H.264 format description.
func avcConfig(desc uintptr) ([]byte, error) {
	if desc == 0 {
		return nil, errors.New("oscodec: encoded sample has no format description")
	}
	var sps, pps [][]byte
	var count int
	for i := 0; i == 0 || i < count; i++ {
		var p unsafe.Pointer
		var size int
		var nalLen int32
		if st := cmH264ParameterSetAtIndex(desc, i, &p, &size, &count, &nalLen); st != 0 {
			return nil, osErr("reading the H.264 parameter sets", st)
		}
		if nalLen != 4 || p == nil || size < 4 || size > 0xffff || count > 64 {
			return nil, errors.New("oscodec: unexpected H.264 parameter set layout")
		}
		ps := append([]byte(nil), unsafe.Slice((*byte)(p), size)...)
		if ps[0]&0x1f == 7 {
			sps = append(sps, ps)
		} else {
			pps = append(pps, ps)
		}
	}
	if len(sps) == 0 || len(pps) == 0 || len(sps) > 31 {
		return nil, errors.New("oscodec: H.264 encoder produced no parameter sets")
	}
	c := []byte{1, sps[0][1], sps[0][2], sps[0][3], 0xff, 0xe0 | byte(len(sps))}
	for _, s := range sps {
		c = append(c, byte(len(s)>>8), byte(len(s)))
		c = append(c, s...)
	}
	c = append(c, byte(len(pps)))
	for _, s := range pps {
		c = append(c, byte(len(s)>>8), byte(len(s)))
		c = append(c, s...)
	}
	// High profile extension: 4:2:0, 8-bit luma and chroma, no SPS extensions.
	return append(c, 0xfd, 0xf8, 0xf8, 0), nil
}

func (e *vtEncoder) take() ([]codec.EncodedFrame, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out, err := e.out, e.err
	e.out, e.err = nil, nil
	return out, err
}

func (e *vtEncoder) Encode(f *codec.VideoFrame) ([]codec.EncodedFrame, error) {
	if e.session == 0 {
		return nil, errors.New("oscodec: encoder is closed")
	}
	w, h := e.cfg.Width, e.cfg.Height
	if f.Width != w || f.Height != h {
		return nil, fmt.Errorf("oscodec: frame is %dx%d, encoder is %dx%d", f.Width, f.Height, w, h)
	}
	var image uintptr
	if st := cvPixelBufferCreate(0, w, h, pixel420v, e.attrs, &image); st != 0 {
		return nil, osErr("allocating an input frame", st)
	}
	defer cfRelease(image)
	if st := cvPixelBufferLockBaseAddress(image, 0); st != 0 {
		return nil, osErr("locking an input frame", st)
	}
	yp, ys := cvPixelBufferGetBaseAddressPlane(image, 0), cvPixelBufferGetBytesPerRowPlane(image, 0)
	cp, cs := cvPixelBufferGetBaseAddressPlane(image, 1), cvPixelBufferGetBytesPerRowPlane(image, 1)
	if yp == nil || cp == nil || ys < w || cs < w {
		cvPixelBufferUnlockBaseAddress(image, 0)
		return nil, errors.New("oscodec: input frame has no writable planes")
	}
	dst := unsafe.Slice((*byte)(yp), ys*h)
	for y := range h {
		copy(dst[y*ys:y*ys+w], f.Y[y*f.YStride:])
	}
	dst = unsafe.Slice((*byte)(cp), cs*(h/2))
	for y := range h / 2 {
		row := dst[y*cs : y*cs+w]
		cb, cr := f.Cb[y*f.CStride:], f.Cr[y*f.CStride:]
		for x := range w / 2 {
			row[2*x], row[2*x+1] = cb[x], cr[x]
		}
	}
	cvPixelBufferUnlockBaseAddress(image, 0)
	const propagate = 1
	cvBufferSetAttachment(image, k.primariesKey, k.primaries709, propagate)
	cvBufferSetAttachment(image, k.transferKey, k.transfer709, propagate)
	cvBufferSetAttachment(image, k.matrixKey, k.matrix709, propagate)

	scale := int32(e.cfg.FrameNum)
	pts, dur := makeTime(e.next*e.cfg.FrameDen, scale), makeTime(e.cfg.FrameDen, scale)
	e.next++
	if st := vtCompressionSessionEncode(e.session, image, pts, dur, 0, uintptr(e.next), nil); st != 0 {
		return nil, osErr("encoding a video frame", st)
	}
	return e.take()
}

func (e *vtEncoder) Flush() ([]codec.EncodedFrame, error) {
	if e.session == 0 {
		return nil, nil
	}
	if st := vtCompressionSessionComplete(e.session, cmTime{}); st != 0 {
		return nil, osErr("finishing the H.264 stream", st)
	}
	return e.take()
}

func (e *vtEncoder) Config() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

func (e *vtEncoder) Close() error {
	if e.session != 0 {
		vtCompressionSessionInvalidate(e.session)
		cfRelease(e.session)
		if e.attrs != 0 {
			cfRelease(e.attrs)
		}
		dropHandle(e.handle)
		e.session = 0
	}
	return nil
}
