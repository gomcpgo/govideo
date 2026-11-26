package govideo

import (
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/gomcpgo/govideo/codec/oscodec"
)

// CodecID names a codec, using ffprobe's vocabulary ("h264", "hevc", "av1",
// "vp9", "opus", "aac", "pcm", "mjpeg", ...).
type CodecID string

// MediaKind is the kind of media a codec carries.
type MediaKind int

const (
	Video MediaKind = iota
	Audio
	Image
	Subtitle
)

func (k MediaKind) String() string {
	return [...]string{"video", "audio", "image", "subtitle"}[k]
}

// Backend says who implements a codec.
type Backend int

const (
	Native Backend = iota // pure Go, in this module
	OS                    // operating-system codec framework
)

func (b Backend) String() string { return [...]string{"native", "os"}[b] }

// PatentClass is the patent status of a codec (SPEC §4.1).
type PatentClass int

const (
	Expired              PatentClass = iota // all known essential patents expired
	RoyaltyFree                             // royalty-free, no known pool claims
	RoyaltyFreeContested                    // royalty-free grant, but pools or litigation exist
	OSOnly                                  // patent-pooled; never implemented natively
)

func (p PatentClass) String() string {
	return [...]string{"expired", "royalty-free", "royalty-free-contested", "os-only"}[p]
}

// Op is a codec operation.
type Op int

const (
	Decode Op = iota
	Encode
)

func (o Op) String() string { return [...]string{"decode", "encode"}[o] }

// Chroma is a chroma subsampling format.
type Chroma int

const (
	Chroma420 Chroma = iota
	Chroma422
	Chroma444
)

// CodecInfo describes one registered codec implementation.
type CodecInfo struct {
	ID          CodecID
	Kind        MediaKind
	Backend     Backend
	PatentClass PatentClass
	CanDecode   bool
	CanEncode   bool
	Profiles    []string
	MaxBitDepth int
	Chroma      []Chroma
}

var (
	regMu    sync.RWMutex
	registry = map[CodecID]CodecInfo{}
	disabled = map[CodecID]bool{}
)

// register adds a codec implementation. It is called from init functions.
func register(ci CodecInfo) {
	regMu.Lock()
	registry[ci.ID] = ci
	regMu.Unlock()
}

// policy records the planned backend and patent class of every codec in the
// SPEC §4.2 matrix, so errors can explain why a codec is missing.
var policy = map[CodecID]struct {
	class   PatentClass
	backend Backend
	phase   int
}{
	"h264": {OSOnly, OS, 1}, "hevc": {OSOnly, OS, 1}, "aac": {OSOnly, OS, 1},
	"prores": {OSOnly, OS, 3}, "eac3": {OSOnly, OS, 3},
	"opus": {RoyaltyFreeContested, Native, 1}, "vp8": {RoyaltyFreeContested, Native, 1},
	"vp9": {RoyaltyFreeContested, Native, 1}, "av1": {RoyaltyFreeContested, Native, 2},
	"mp3": {Expired, Native, 1}, "flac": {RoyaltyFree, Native, 1},
	"ac3": {Expired, Native, 3}, "mpeg2video": {Expired, Native, 3}, "mpeg4": {Expired, Native, 3},
	"alac": {RoyaltyFree, Native, 3}, "vorbis": {RoyaltyFree, Native, 3},
}

// Codecs lists the codecs usable on this host, sorted by ID. Codecs removed
// with WithoutCodecs are not listed.
func Codecs() []CodecInfo {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]CodecInfo, 0, len(registry))
	for id, ci := range registry {
		if !disabled[id] {
			out = append(out, ci)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// canonical folds codec name variants ("pcm_s16le" → "pcm") to registry IDs.
func canonical(id CodecID) CodecID {
	s := strings.ToLower(string(id))
	if strings.HasPrefix(s, "pcm_") {
		return "pcm"
	}
	return CodecID(s)
}

// Available reports whether a codec can be used for op on this host. When it
// cannot, the error is an *ErrCodecUnavailable.
func Available(id CodecID, op Op) (CodecInfo, error) {
	id = canonical(id)
	regMu.RLock()
	ci, ok := registry[id]
	off := disabled[id]
	regMu.RUnlock()
	if off {
		return CodecInfo{}, &ErrCodecUnavailable{Codec: id, Op: op, Reason: "disabled with WithoutCodecs"}
	}
	if ok && (op == Decode && ci.CanDecode || op == Encode && ci.CanEncode) {
		return ci, nil
	}
	e := &ErrCodecUnavailable{Codec: id, Op: op}
	p, known := policy[id]
	switch {
	case ok:
		e.Reason = "this codec supports only " + map[bool]string{true: "decoding", false: "encoding"}[ci.CanDecode]
		if op == Decode && known && p.backend == Native {
			e.Hint = "a decoder is planned for phase " + string(rune('0'+p.phase))
		}
	case !known:
		e.Reason = "unsupported codec"
	case p.backend == OS && runtime.GOOS != "darwin" && runtime.GOOS != "windows":
		e.Reason = "patent-pooled codec, available only through an operating-system codec, and " + runtime.GOOS + " has none"
		e.Hint = "run on macOS or Windows, or use royalty-free formats"
	case p.backend == OS && runtime.GOOS == "darwin" && (id == "h264" || id == "hevc" || id == "aac"):
		e.Reason = "the macOS codec frameworks could not be loaded"
		if err := oscodec.Load(); err != nil {
			e.Reason += ": " + err.Error()
		}
	case p.backend == OS:
		e.Reason = "the operating-system codec backend for this codec is not implemented yet"
		e.Hint = "planned for phase " + string(rune('0'+p.phase))
	default:
		e.Reason = "the native implementation is not written yet"
		e.Hint = "planned for phase " + string(rune('0'+p.phase))
	}
	return CodecInfo{}, e
}

// WithoutCodecs removes codecs from use for the whole process, letting an
// integrator enforce a stricter policy (for example "expired patents only").
// The set can be restricted but never expanded.
func WithoutCodecs(ids ...CodecID) {
	regMu.Lock()
	for _, id := range ids {
		disabled[canonical(id)] = true
	}
	regMu.Unlock()
}

// BuildInfoT describes how the package was built.
type BuildInfoT struct {
	GoVersion string
	GOOS      string
	GOARCH    string
	SIMD      bool // built with GOEXPERIMENT=simd and SIMD kernels compiled in
	OSBackend bool // an operating-system codec backend is present
}

// BuildInfo reports the Go version, SIMD state and OS backend presence.
func BuildInfo() BuildInfoT {
	return BuildInfoT{GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		OSBackend: oscodec.Load() == nil}
}

func init() {
	register(CodecInfo{ID: "pcm", Kind: Audio, Backend: Native, PatentClass: RoyaltyFree,
		CanDecode: true, CanEncode: true, MaxBitDepth: 32})
	register(CodecInfo{ID: "mjpeg", Kind: Video, Backend: Native, PatentClass: Expired,
		CanDecode: true, CanEncode: true, Profiles: []string{"baseline"}, MaxBitDepth: 8,
		Chroma: []Chroma{Chroma420, Chroma422, Chroma444}})
	// FLAC is written (audio-only export) but not yet read.
	register(CodecInfo{ID: "flac", Kind: Audio, Backend: Native, PatentClass: RoyaltyFree,
		CanEncode: true, MaxBitDepth: 16})
	// Patent-pooled codecs exist only where the operating system has them
	// (SPEC §4.1, §6). 10-bit HEVC is decoded to 8 bits by the system.
	if oscodec.Load() == nil {
		register(CodecInfo{ID: "h264", Kind: Video, Backend: OS, PatentClass: OSOnly,
			CanDecode: true, CanEncode: true, Profiles: []string{"baseline", "main", "high"}, MaxBitDepth: 8,
			Chroma: []Chroma{Chroma420}})
		register(CodecInfo{ID: "hevc", Kind: Video, Backend: OS, PatentClass: OSOnly,
			CanDecode: true, Profiles: []string{"main", "main10"}, MaxBitDepth: 10, Chroma: []Chroma{Chroma420}})
		register(CodecInfo{ID: "aac", Kind: Audio, Backend: OS, PatentClass: OSOnly,
			CanDecode: true, CanEncode: true, Profiles: []string{"lc"}, MaxBitDepth: 16})
	}
}
