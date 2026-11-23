//go:build darwin

package oscodec

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Function pointers into the system frameworks, bound by Load. CoreFoundation
// objects are carried as uintptr handles: they are never dereferenced in Go.
var (
	cfRelease                 func(obj uintptr)
	cfRetain                  func(obj uintptr) uintptr
	cfNumberCreate            func(alloc uintptr, typ int, value unsafe.Pointer) uintptr
	cfDictionaryCreate        func(alloc uintptr, keys, values unsafe.Pointer, n int, keyCB, valueCB uintptr) uintptr
	cfDictionaryGetValue      func(dict, key uintptr) uintptr
	cfDataCreate              func(alloc uintptr, bytes unsafe.Pointer, n int) uintptr
	cfStringCreateWithCString func(alloc uintptr, s string, encoding uint32) uintptr
	cfEqual                   func(a, b uintptr) bool
	cfArrayGetValueAtIndex    func(array uintptr, i int) uintptr
	cfArrayCreate             func(alloc uintptr, values unsafe.Pointer, n int, cb uintptr) uintptr

	cmVideoFormatDescriptionCreate     func(alloc uintptr, codec uint32, width, height int32, extensions uintptr, out *uintptr) int32
	cmBlockBufferCreateWithMemoryBlock func(alloc, block uintptr, blockLen int, blockAlloc, source uintptr, offset, dataLen int, flags uint32, out *uintptr) int32
	cmBlockBufferReplaceDataBytes      func(src unsafe.Pointer, block uintptr, offset, n int) int32
	cmBlockBufferGetDataLength         func(block uintptr) int
	cmBlockBufferCopyDataBytes         func(block uintptr, offset, n int, dst unsafe.Pointer) int32
	cmSampleBufferCreateReady          func(alloc, data, format uintptr, samples, timings int, timing uintptr, sizes int, size *uintptr, out *uintptr) int32
	cmSampleBufferGetDataBuffer        func(sbuf uintptr) uintptr
	cmSampleBufferGetAttachments       func(sbuf uintptr, create bool) uintptr
	cmSampleBufferGetFormatDescription func(sbuf uintptr) uintptr
	cmH264ParameterSetAtIndex          func(desc uintptr, i int, ptr *unsafe.Pointer, size, count *int, nalLen *int32) int32

	cvPixelBufferCreate              func(alloc uintptr, width, height int, format uint32, attrs uintptr, out *uintptr) int32
	cvPixelBufferLockBaseAddress     func(buf uintptr, flags uint64) int32
	cvPixelBufferUnlockBaseAddress   func(buf uintptr, flags uint64) int32
	cvPixelBufferGetBaseAddressPlane func(buf uintptr, plane int) unsafe.Pointer
	cvPixelBufferGetBytesPerRowPlane func(buf uintptr, plane int) int
	cvPixelBufferGetWidth            func(buf uintptr) int
	cvPixelBufferGetHeight           func(buf uintptr) int
	cvPixelBufferGetPixelFormatType  func(buf uintptr) uint32
	cvPixelBufferGetPlaneCount       func(buf uintptr) int
	cvBufferCopyAttachment           func(buf, key uintptr, mode *uint32) uintptr
	cvBufferSetAttachment            func(buf, key, value uintptr, mode uint32)

	vtDecompressionSessionCreate     func(alloc, format, spec, attrs uintptr, callback unsafe.Pointer, out *uintptr) int32
	vtDecompressionSessionDecode     func(session, sbuf uintptr, flags uint32, frameRefCon uintptr, info *uint32) int32
	vtDecompressionSessionWait       func(session uintptr) int32
	vtDecompressionSessionInvalidate func(session uintptr)
	vtDecompressionSessionCanAccept  func(session, format uintptr) bool
	vtCompressionSessionCreate       func(alloc uintptr, width, height int32, codec uint32, spec, attrs, dataAlloc, callback, refCon uintptr, out *uintptr) int32
	vtCompressionSessionEncode       func(session, image uintptr, pts, dur cmTime, props, frameRefCon uintptr, info *uint32) int32
	vtCompressionSessionComplete     func(session uintptr, until cmTime) int32
	vtCompressionSessionInvalidate   func(session uintptr)
	vtSessionSetProperty             func(session, key, value uintptr) int32

	audioConverterNew         func(in, out *asbd, conv *uintptr) int32
	audioConverterDispose     func(conv uintptr) int32
	audioConverterSetProperty func(conv uintptr, id uint32, size uint32, data unsafe.Pointer) int32
	audioConverterGetProperty func(conv uintptr, id uint32, size *uint32, data unsafe.Pointer) int32
	audioConverterFill        func(conv, proc, user uintptr, packets *uint32, out *audioBufferList, desc unsafe.Pointer) int32

	cMalloc func(n int) unsafe.Pointer
	cFree   func(p unsafe.Pointer)
)

// Constants read from the frameworks (CFString and CFBoolean objects).
var k struct {
	dictKeyCB, dictValueCB uintptr
	arrayCB                uintptr
	cfTrue, cfFalse        uintptr

	atoms, notSync                             uintptr
	pixelFormatKey, ioSurfaceKey               uintptr
	matrixKey, primariesKey, transferKey       uintptr
	matrix709, matrix601, matrix2020           uintptr
	primaries709, transfer709                  uintptr
	realTime, profileLevel, highAuto           uintptr
	reorder, keyInterval, bitRate, expectedFPS uintptr
	vtPrimaries, vtTransfer, vtMatrix          uintptr
	quality, rateLimits                        uintptr
}

// cmTime mirrors CMTime.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

func makeTime(value int64, scale int32) cmTime { return cmTime{value, scale, 1, 0} }

var (
	loadOnce sync.Once
	loadErr  error
)

// Load binds the macOS codec frameworks. It reports an error when they
// cannot be used; the result is cached.
func Load() error {
	loadOnce.Do(func() { loadErr = load() })
	return loadErr
}

func load() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("oscodec: %v", r)
		}
	}()
	open := func(name string) uintptr {
		h, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			panic(err)
		}
		return h
	}
	cf, cm, cv, vt, at := open("CoreFoundation"), open("CoreMedia"), open("CoreVideo"), open("VideoToolbox"), open("AudioToolbox")
	sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	bind := purego.RegisterLibFunc // panics on a missing symbol
	bind(&cfRelease, cf, "CFRelease")
	bind(&cfRetain, cf, "CFRetain")
	bind(&cfNumberCreate, cf, "CFNumberCreate")
	bind(&cfDictionaryCreate, cf, "CFDictionaryCreate")
	bind(&cfDictionaryGetValue, cf, "CFDictionaryGetValue")
	bind(&cfDataCreate, cf, "CFDataCreate")
	bind(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	bind(&cfEqual, cf, "CFEqual")
	bind(&cfArrayGetValueAtIndex, cf, "CFArrayGetValueAtIndex")
	bind(&cfArrayCreate, cf, "CFArrayCreate")

	bind(&cmVideoFormatDescriptionCreate, cm, "CMVideoFormatDescriptionCreate")
	bind(&cmBlockBufferCreateWithMemoryBlock, cm, "CMBlockBufferCreateWithMemoryBlock")
	bind(&cmBlockBufferReplaceDataBytes, cm, "CMBlockBufferReplaceDataBytes")
	bind(&cmBlockBufferGetDataLength, cm, "CMBlockBufferGetDataLength")
	bind(&cmBlockBufferCopyDataBytes, cm, "CMBlockBufferCopyDataBytes")
	bind(&cmSampleBufferCreateReady, cm, "CMSampleBufferCreateReady")
	bind(&cmSampleBufferGetDataBuffer, cm, "CMSampleBufferGetDataBuffer")
	bind(&cmSampleBufferGetAttachments, cm, "CMSampleBufferGetSampleAttachmentsArray")
	bind(&cmSampleBufferGetFormatDescription, cm, "CMSampleBufferGetFormatDescription")
	bind(&cmH264ParameterSetAtIndex, cm, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex")

	bind(&cvPixelBufferCreate, cv, "CVPixelBufferCreate")
	bind(&cvPixelBufferLockBaseAddress, cv, "CVPixelBufferLockBaseAddress")
	bind(&cvPixelBufferUnlockBaseAddress, cv, "CVPixelBufferUnlockBaseAddress")
	bind(&cvPixelBufferGetBaseAddressPlane, cv, "CVPixelBufferGetBaseAddressOfPlane")
	bind(&cvPixelBufferGetBytesPerRowPlane, cv, "CVPixelBufferGetBytesPerRowOfPlane")
	bind(&cvPixelBufferGetWidth, cv, "CVPixelBufferGetWidth")
	bind(&cvPixelBufferGetHeight, cv, "CVPixelBufferGetHeight")
	bind(&cvPixelBufferGetPixelFormatType, cv, "CVPixelBufferGetPixelFormatType")
	bind(&cvPixelBufferGetPlaneCount, cv, "CVPixelBufferGetPlaneCount")
	bind(&cvBufferCopyAttachment, cv, "CVBufferCopyAttachment")
	bind(&cvBufferSetAttachment, cv, "CVBufferSetAttachment")

	bind(&vtDecompressionSessionCreate, vt, "VTDecompressionSessionCreate")
	bind(&vtDecompressionSessionDecode, vt, "VTDecompressionSessionDecodeFrame")
	bind(&vtDecompressionSessionWait, vt, "VTDecompressionSessionWaitForAsynchronousFrames")
	bind(&vtDecompressionSessionInvalidate, vt, "VTDecompressionSessionInvalidate")
	bind(&vtDecompressionSessionCanAccept, vt, "VTDecompressionSessionCanAcceptFormatDescription")
	bind(&vtCompressionSessionCreate, vt, "VTCompressionSessionCreate")
	bind(&vtCompressionSessionEncode, vt, "VTCompressionSessionEncodeFrame")
	bind(&vtCompressionSessionComplete, vt, "VTCompressionSessionCompleteFrames")
	bind(&vtCompressionSessionInvalidate, vt, "VTCompressionSessionInvalidate")
	bind(&vtSessionSetProperty, vt, "VTSessionSetProperty")

	bind(&audioConverterNew, at, "AudioConverterNew")
	bind(&audioConverterDispose, at, "AudioConverterDispose")
	bind(&audioConverterSetProperty, at, "AudioConverterSetProperty")
	bind(&audioConverterGetProperty, at, "AudioConverterGetProperty")
	bind(&audioConverterFill, at, "AudioConverterFillComplexBuffer")

	bind(&cMalloc, sys, "malloc")
	bind(&cFree, sys, "free")

	addr := func(lib uintptr, name string) uintptr {
		p, err := purego.Dlsym(lib, name)
		if err != nil {
			panic(err)
		}
		return p
	}
	// value reads the object pointer stored in an exported constant.
	value := func(lib uintptr, name string) uintptr {
		p := addr(lib, name)
		return **(**uintptr)(unsafe.Pointer(&p))
	}
	k.dictKeyCB = addr(cf, "kCFTypeDictionaryKeyCallBacks")
	k.dictValueCB = addr(cf, "kCFTypeDictionaryValueCallBacks")
	k.arrayCB = addr(cf, "kCFTypeArrayCallBacks")
	k.cfTrue = value(cf, "kCFBooleanTrue")
	k.cfFalse = value(cf, "kCFBooleanFalse")
	k.atoms = value(cm, "kCMFormatDescriptionExtension_SampleDescriptionExtensionAtoms")
	k.notSync = value(cm, "kCMSampleAttachmentKey_NotSync")
	k.pixelFormatKey = value(cv, "kCVPixelBufferPixelFormatTypeKey")
	k.ioSurfaceKey = value(cv, "kCVPixelBufferIOSurfacePropertiesKey")
	k.matrixKey = value(cv, "kCVImageBufferYCbCrMatrixKey")
	k.primariesKey = value(cv, "kCVImageBufferColorPrimariesKey")
	k.transferKey = value(cv, "kCVImageBufferTransferFunctionKey")
	k.matrix709 = value(cv, "kCVImageBufferYCbCrMatrix_ITU_R_709_2")
	k.matrix601 = value(cv, "kCVImageBufferYCbCrMatrix_ITU_R_601_4")
	k.matrix2020 = value(cv, "kCVImageBufferYCbCrMatrix_ITU_R_2020")
	k.primaries709 = value(cv, "kCVImageBufferColorPrimaries_ITU_R_709_2")
	k.transfer709 = value(cv, "kCVImageBufferTransferFunction_ITU_R_709_2")
	k.realTime = value(vt, "kVTCompressionPropertyKey_RealTime")
	k.profileLevel = value(vt, "kVTCompressionPropertyKey_ProfileLevel")
	k.highAuto = value(vt, "kVTProfileLevel_H264_High_AutoLevel")
	k.reorder = value(vt, "kVTCompressionPropertyKey_AllowFrameReordering")
	k.keyInterval = value(vt, "kVTCompressionPropertyKey_MaxKeyFrameInterval")
	k.bitRate = value(vt, "kVTCompressionPropertyKey_AverageBitRate")
	k.quality = value(vt, "kVTCompressionPropertyKey_Quality")
	k.rateLimits = value(vt, "kVTCompressionPropertyKey_DataRateLimits")
	k.expectedFPS = value(vt, "kVTCompressionPropertyKey_ExpectedFrameRate")
	k.vtPrimaries = value(vt, "kVTCompressionPropertyKey_ColorPrimaries")
	k.vtTransfer = value(vt, "kVTCompressionPropertyKey_TransferFunction")
	k.vtMatrix = value(vt, "kVTCompressionPropertyKey_YCbCrMatrix")

	decodeCallback = purego.NewCallback(onDecodedFrame)
	encodeCallback = purego.NewCallback(onEncodedFrame)
	audioInputCallback = purego.NewCallback(onAudioInput)
	return nil
}

// cfInt returns a new CFNumber holding v.
func cfInt(v int32) uintptr {
	const kCFNumberSInt32Type = 3
	return cfNumberCreate(0, kCFNumberSInt32Type, unsafe.Pointer(&v))
}

// cfFloat returns a new CFNumber holding v.
func cfFloat(v float64) uintptr {
	const kCFNumberFloat64Type = 6
	return cfNumberCreate(0, kCFNumberFloat64Type, unsafe.Pointer(&v))
}

// cfArray returns a new CFArray of the given objects, retaining them.
func cfArray(values ...uintptr) uintptr {
	return cfArrayCreate(0, unsafe.Pointer(&values[0]), len(values), k.arrayCB)
}

// cfDict returns a new CFDictionary of the given key/value pairs, retaining
// both.
func cfDict(kv ...uintptr) uintptr {
	n := len(kv) / 2
	if n == 0 {
		return cfDictionaryCreate(0, nil, nil, 0, k.dictKeyCB, k.dictValueCB)
	}
	keys, values := make([]uintptr, n), make([]uintptr, n)
	for i := range n {
		keys[i], values[i] = kv[2*i], kv[2*i+1]
	}
	return cfDictionaryCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&values[0]), n, k.dictKeyCB, k.dictValueCB)
}

// fourCC packs a four-character code.
func fourCC(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

// osErr formats a framework status code.
func osErr(what string, status int32) error {
	return fmt.Errorf("oscodec: %s failed (OSStatus %d)", what, status)
}

// handles maps the reference constants passed through C callbacks back to Go
// objects, so no Go pointer is ever stored in framework memory.
var (
	handleMu   sync.Mutex
	handles    = map[uintptr]any{}
	nextHandle uintptr
)

func newHandle(v any) uintptr {
	handleMu.Lock()
	defer handleMu.Unlock()
	nextHandle++
	handles[nextHandle] = v
	return nextHandle
}

func lookupHandle(h uintptr) any {
	handleMu.Lock()
	defer handleMu.Unlock()
	return handles[h]
}

func dropHandle(h uintptr) {
	handleMu.Lock()
	delete(handles, h)
	handleMu.Unlock()
}
