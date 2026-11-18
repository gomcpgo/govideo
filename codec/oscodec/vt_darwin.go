//go:build darwin

package oscodec

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/gomcpgo/govideo/codec"
)

var decodeCallback, encodeCallback uintptr

const (
	pixel420v = 0x34323076 // '420v': 8-bit 4:2:0 bi-planar, video range
	pixel420f = 0x34323066 // '420f': the same, full range
)

// --- decode ------------------------------------------------------------------

type vtDecoder struct {
	session, format, handle uintptr

	mu      sync.Mutex
	tag     uintptr
	pending map[uintptr]time.Duration
	out     []codec.TimedFrame
	err     error
	matrix  int // H.273 code of the decoded pictures; 0 until known
}

// NewVideoDecoder returns a VideoToolbox decoder for an H.264 or HEVC stream.
// Pictures come back as 8-bit 4:2:0; 10-bit sources are reduced to 8 bits by
// the system.
func NewVideoDecoder(c VideoConfig) (codec.VideoStreamDecoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var codecType, atom string
	switch c.Codec {
	case "h264":
		codecType, atom = "avc1", "avcC"
	case "hevc":
		codecType, atom = "hvc1", "hvcC"
	default:
		return nil, ErrUnsupported
	}
	if len(c.Record) == 0 || c.Width <= 0 || c.Height <= 0 {
		return nil, fmt.Errorf("oscodec: %s stream has no %s configuration record", c.Codec, atom)
	}
	data := cfDataCreate(0, unsafe.Pointer(&c.Record[0]), len(c.Record))
	name := cfStringCreateWithCString(0, atom, 0x08000100)
	atoms := cfDict(name, data)
	ext := cfDict(k.atoms, atoms)
	d := &vtDecoder{pending: map[uintptr]time.Duration{}, matrix: c.Matrix}
	st := cmVideoFormatDescriptionCreate(0, fourCC(codecType), int32(c.Width), int32(c.Height), ext, &d.format)
	for _, o := range []uintptr{ext, atoms, name, data} {
		cfRelease(o)
	}
	if st != 0 {
		return nil, osErr("creating the "+c.Codec+" format description", st)
	}
	pf := cfInt(pixel420v)
	attrs := cfDict(k.pixelFormatKey, pf)
	d.handle = newHandle(d)
	record := [2]uintptr{decodeCallback, d.handle}
	st = vtDecompressionSessionCreate(0, d.format, 0, attrs, unsafe.Pointer(&record), &d.session)
	cfRelease(attrs)
	cfRelease(pf)
	if st != 0 {
		dropHandle(d.handle)
		cfRelease(d.format)
		return nil, osErr("opening the "+c.Codec+" decoder", st)
	}
	return d, nil
}

// onDecodedFrame is the VTDecompressionOutputCallback. The two trailing
// CMTime arguments are not declared: the frame is identified by its tag.
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
	f, err := d.copyFrame(image)
	if err != nil {
		if d.err == nil {
			d.err = err
		}
		return
	}
	d.out = append(d.out, codec.TimedFrame{Frame: f, PTS: pts})
}

// copyFrame copies a bi-planar pixel buffer into a planar frame.
func (d *vtDecoder) copyFrame(image uintptr) (*codec.VideoFrame, error) {
	format := cvPixelBufferGetPixelFormatType(image)
	if (format != pixel420v && format != pixel420f) || cvPixelBufferGetPlaneCount(image) != 2 {
		return nil, fmt.Errorf("oscodec: decoder returned unexpected pixel format %#x", format)
	}
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
	const readOnly = 1
	if st := cvPixelBufferLockBaseAddress(image, readOnly); st != 0 {
		return nil, osErr("locking a decoded frame", st)
	}
	defer cvPixelBufferUnlockBaseAddress(image, readOnly)
	w, h := cvPixelBufferGetWidth(image), cvPixelBufferGetHeight(image)
	if w <= 0 || h <= 0 || w > 1<<15 || h > 1<<15 {
		return nil, fmt.Errorf("oscodec: decoder returned a %dx%d frame", w, h)
	}
	f := codec.NewVideoFrame(w, h)
	f.Matrix, f.FullRange = d.matrix, format == pixel420f
	cw, ch := f.ChromaSize()
	yp, ys := cvPixelBufferGetBaseAddressPlane(image, 0), cvPixelBufferGetBytesPerRowPlane(image, 0)
	cp, cs := cvPixelBufferGetBaseAddressPlane(image, 1), cvPixelBufferGetBytesPerRowPlane(image, 1)
	if yp == nil || cp == nil || ys < w || cs < 2*cw {
		return nil, errors.New("oscodec: decoded frame has no readable planes")
	}
	src := unsafe.Slice((*byte)(yp), ys*h)
	for y := range h {
		copy(f.Y[y*f.YStride:y*f.YStride+w], src[y*ys:])
	}
	src = unsafe.Slice((*byte)(cp), cs*ch)
	for y := range ch {
		row := src[y*cs : y*cs+2*cw]
		cb, cr := f.Cb[y*f.CStride:y*f.CStride+cw], f.Cr[y*f.CStride:y*f.CStride+cw]
		for x := range cw {
			cb[x], cr[x] = row[2*x], row[2*x+1]
		}
	}
	return f, nil
}

func (d *vtDecoder) take() ([]codec.TimedFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := d.out, d.err
	d.out, d.err = nil, nil
	return out, err
}

func (d *vtDecoder) Decode(p []byte, pts time.Duration) ([]codec.TimedFrame, error) {
	if d.session == 0 {
		return nil, errors.New("oscodec: decoder is closed")
	}
	if len(p) == 0 {
		return nil, nil
	}
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
	d.mu.Lock()
	delete(d.pending, tag)
	d.mu.Unlock()
	return out, err
}

func (d *vtDecoder) Flush() ([]codec.TimedFrame, error) {
	if d.session == 0 {
		return nil, nil
	}
	vtDecompressionSessionWait(d.session)
	return d.take()
}

func (d *vtDecoder) Close() error {
	if d.session != 0 {
		vtDecompressionSessionWait(d.session)
		vtDecompressionSessionInvalidate(d.session)
		cfRelease(d.session)
		cfRelease(d.format)
		dropHandle(d.handle)
		d.session = 0
	}
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
// must be BT.709 limited range with even dimensions. Frame reordering is off,
// so samples come out in presentation order and need no composition offsets.
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
	err := errors.Join(
		set("profile", k.profileLevel, k.highAuto, false),
		set("real-time mode", k.realTime, k.cfFalse, false),
		set("frame reordering", k.reorder, k.cfFalse, false),
		set("keyframe interval", k.keyInterval, cfInt(key), true),
		set("colour primaries", k.vtPrimaries, k.primaries709, false),
		set("transfer function", k.vtTransfer, k.transfer709, false),
		set("colour matrix", k.vtMatrix, k.matrix709, false),
	)
	if err == nil && c.Bitrate > 0 {
		err = set("bit rate", k.bitRate, cfInt(int32(min(c.Bitrate, 1<<30))), true)
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
