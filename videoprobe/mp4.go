package videoprobe

import (
	"fmt"
	"math"
	"time"

	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
)

var mp4VideoCodecs = map[string]string{
	"avc1": "h264", "avc3": "h264", "hvc1": "hevc", "hev1": "hevc", "dvh1": "hevc", "dvhe": "hevc",
	"av01": "av1", "vp08": "vp8", "vp09": "vp9", "mp4v": "mpeg4",
	"jpeg": "mjpeg", "mjpa": "mjpeg", "mjpb": "mjpegb", "s263": "h263", "h263": "h263",
	"apch": "prores", "apcn": "prores", "apcs": "prores", "apco": "prores", "ap4h": "prores", "ap4x": "prores",
	"vvc1": "vvc", "vvi1": "vvc", "apv1": "apv", "png ": "png",
}

var proresProfiles = map[string]string{
	"apch": "HQ", "apcn": "Standard", "apcs": "LT", "apco": "Proxy", "ap4h": "4444", "ap4x": "XQ",
}

var mp4AudioCodecs = map[string]string{
	"Opus": "opus", "fLaC": "flac", ".mp3": "mp3", "alac": "alac", "ac-3": "ac3", "ec-3": "eac3",
	"samr": "amr_nb", "sawb": "amr_wb", "ulaw": "pcm_mulaw", "alaw": "pcm_alaw", "ac-4": "ac4",
	"mha1": "mpegh_3d_audio", "mhm1": "mpegh_3d_audio",
}

var mp4SubtitleCodecs = map[string]string{
	"tx3g": "mov_text", "text": "mov_text", "wvtt": "webvtt", "stpp": "ttml", "c608": "eia_608", "c708": "eia_708",
}

func fromISOBMFF(f *isobmff.File) *Info {
	info := &Info{Format: "mp4", FastStart: f.FastStart, Fragmented: f.Fragmented, Tags: map[string]string{}}
	if f.QuickTime {
		info.Format = "mov"
	}
	for k, v := range f.Tags {
		info.Tags[k] = v
	}
	if f.MajorBrand != "" {
		info.Tags["major_brand"] = f.MajorBrand
	}
	if !f.CreationTime.IsZero() {
		info.Tags["creation_time"] = f.CreationTime.Format(time.RFC3339)
	}
	movieTS := int64(f.Timescale)
	if movieTS == 0 {
		movieTS = 1
	}
	info.Duration = ticks(int64(f.Duration), movieTS)
	first := true
	seenDefault := map[Kind]bool{}
	for _, t := range f.Tracks {
		s := mp4Stream(f, t, movieTS)
		if s.Kind != Data && !seenDefault[s.Kind] && t.Enabled {
			s.Default = true
			seenDefault[s.Kind] = true
		}
		if end := s.StartTime + s.Duration; (f.Fragmented || f.Duration == 0) && end > info.Duration {
			info.Duration = end
		}
		if s.Kind == Video || s.Kind == Audio {
			if first || s.StartTime < info.StartTime {
				info.StartTime = s.StartTime
			}
			first = false
		}
		info.Streams = append(info.Streams, s)
	}
	if d := ticks(int64(f.FragmentDuration), movieTS); f.Duration == 0 && d > info.Duration {
		info.Duration = d
	}
	return info
}

func mp4Stream(f *isobmff.File, t *isobmff.Track, movieTS int64) Stream {
	ts := int64(t.Timescale)
	s := Stream{ID: int64(t.ID), Kind: Data, TimeBase: Rational{1, ts}, Language: t.Language, Frames: t.SampleCount}
	if s.Language == "und" {
		s.Language = ""
	}

	// Timing: edit lists shift and trim the media timeline.
	var empty, edited int64
	mediaStart := int64(0)
	haveEdit := false
	for _, e := range t.Edits {
		if e.MediaTime < 0 {
			if !haveEdit {
				empty += int64(e.SegmentDuration)
			}
			continue
		}
		if !haveEdit {
			mediaStart = e.MediaTime
		}
		haveEdit = true
		edited += int64(e.SegmentDuration)
	}
	start := t.MinCTS - mediaStart
	if haveEdit && start < 0 {
		start = 0
	}
	s.StartTime = ticks(empty, movieTS) + ticks(start, ts)
	media := ticks(int64(t.MediaDuration), ts)
	s.Duration = media
	if haveEdit && edited > 0 && !f.Fragmented {
		if d := ticks(edited, movieTS); d < media {
			s.Duration = d
		}
	}
	if media > 0 {
		s.BitRate = int64(float64(t.TotalBytes) * 8 / media.Seconds())
	}

	if len(t.Entries) == 0 {
		return s
	}
	e := t.Entries[0]
	s.CodecTag = e.Format
	switch t.Handler {
	case "vide":
		s.Kind = Video
		s.Codec = mp4VideoCodecs[e.Format]
		mp4Video(&s, t, e)
	case "soun":
		s.Kind = Audio
		mp4Audio(&s, f, e)
	case "sbtl", "subt", "text", "clcp":
		s.Kind = Subtitle
		s.Codec = mp4SubtitleCodecs[e.Format]
	default:
		if c, ok := mp4SubtitleCodecs[e.Format]; ok {
			s.Kind, s.Codec = Subtitle, c
		}
	}
	return s
}

func mp4Video(s *Stream, t *isobmff.Track, e *isobmff.SampleEntry) {
	vi := &VideoInfo{Width: e.Width, Height: e.Height, CodedWidth: e.Width, CodedHeight: e.Height}
	s.Video = vi
	var hv *codechdr.Video
	switch s.Codec {
	case "h264":
		hv, _ = codechdr.ParseAVCC(e.Boxes["avcC"])
	case "hevc":
		hv, _ = codechdr.ParseHVCC(e.Boxes["hvcC"])
	case "av1":
		hv, _ = codechdr.ParseAV1C(e.Boxes["av1C"])
	case "vp9", "vp8":
		hv, _ = codechdr.ParseVPCC(e.Boxes["vpcC"])
		if hv != nil {
			hv.Width, hv.Height, hv.CodedWidth, hv.CodedHeight = 0, 0, 0, 0
		}
	case "prores":
		s.Profile = proresProfiles[e.Format]
		vi.BitDepth, vi.Chroma = 10, "4:2:2"
		if e.Format == "ap4h" || e.Format == "ap4x" {
			vi.BitDepth, vi.Chroma = 12, "4:4:4"
		}
	case "mjpeg":
		vi.BitDepth = 8
	}
	applyVideo(s, hv)

	// Aspect: container pasp wins, then the bitstream SAR.
	sarN, sarD := int64(0), int64(0)
	if hv != nil {
		sarN, sarD = int64(hv.SARNum), int64(hv.SARDen)
	}
	if p := e.Boxes["pasp"]; len(p) >= 8 {
		h, v := int64(be32(p)), int64(be32(p[4:]))
		if h > 0 && v > 0 {
			sarN, sarD = h, v
		}
	}
	vi.setAspect(sarN, sarD)

	// Frame rate from the sample tables.
	ts := int64(t.Timescale)
	if t.CommonDelta > 0 {
		vi.FrameRate = reduce(ts, int64(t.CommonDelta))
	}
	if t.MediaDuration > 0 && t.SampleCount > 0 {
		vi.AvgFrameRate = reduceBig(t.SampleCount, ts, int64(t.MediaDuration))
	}
	vi.VFR = t.VariableRate
	vi.Rotation = matrixRotation(t.Matrix)

	c := colourOf(hv)
	if prim, trc, mat, full, ok := codechdr.ParseColr(e.Boxes["colr"]); ok {
		isNCLX := len(e.Boxes["colr"]) >= 4 && string(e.Boxes["colr"][:4]) == "nclx"
		c.merge(prim, trc, mat, isNCLX, full)
	}
	c.apply(vi)

	for _, k := range []string{"dvcC", "dvvC", "dvwC"} {
		if b := e.Boxes[k]; b != nil {
			vi.DolbyVision = dolbyVision(b)
			break
		}
	}
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// reduceBig returns a*b/c as a reduced rational, guarding against overflow.
func reduceBig(a, b, c int64) Rational {
	if g := gcd(a, c); g > 1 {
		a, c = a/g, c/g
	}
	if g := gcd(b, c); g > 1 {
		b, c = b/g, c/g
	}
	if a != 0 && b > math.MaxInt64/a {
		return approxRate(float64(a) * float64(b) / float64(c))
	}
	return reduce(a*b, c)
}

// matrixRotation returns the clockwise display rotation encoded in a tkhd
// matrix, rounded to a multiple of 90 degrees.
func matrixRotation(m [9]int32) int {
	a, b := float64(m[0]), float64(m[1])
	if a == 0 && b == 0 {
		return 0
	}
	deg := math.Atan2(b, a) * 180 / math.Pi
	r := int(math.Round(deg/90)) * 90
	return ((r % 360) + 360) % 360
}

func mp4Audio(s *Stream, f *isobmff.File, e *isobmff.SampleEntry) {
	ai := &AudioInfo{SampleRate: int(e.Rate + 0.5), Channels: e.Channels}
	s.Audio = ai
	if c, ok := mp4AudioCodecs[e.Format]; ok {
		s.Codec = c
	}
	littleEnda := false
	if b := e.Boxes["enda"]; len(b) >= 2 && b[1] == 1 {
		littleEnda = true
	}
	pcm := func(kind string, bits int, little bool) {
		ai.BitDepth = bits
		if bits == 8 {
			s.Codec = "pcm_" + kind + "8"
			return
		}
		end := "be"
		if little {
			end = "le"
		}
		s.Codec = fmt.Sprintf("pcm_%s%d%s", kind, bits, end)
	}
	switch e.Format {
	case "mp4a":
		oti, asc, _, _, err := codechdr.ParseESDS(e.Boxes["esds"])
		if err != nil {
			s.Codec = "aac"
			break
		}
		switch oti {
		case 0x40, 0x66, 0x67, 0x68:
			s.Codec = "aac"
			if a, err := codechdr.ParseAudioSpecificConfig(asc); err == nil {
				applyAudio(s, a)
			}
		case 0x69, 0x6b:
			s.Codec = "mp3"
		case 0xa5:
			s.Codec = "ac3"
		case 0xa6:
			s.Codec = "eac3"
		case 0xdd:
			s.Codec = "vorbis"
		case 0xad:
			s.Codec = "opus"
		}
	case "Opus":
		if a, err := codechdr.ParseDOps(e.Boxes["dOps"]); err == nil {
			applyAudio(s, a)
		}
	case "fLaC":
		if a, err := codechdr.ParseFLACStreamInfo(e.Boxes["dfLa"]); err == nil {
			applyAudio(s, a)
		}
	case "alac":
		if b := e.Boxes["alac"]; len(b) >= 28 {
			ai.BitDepth = int(b[9])
			ai.Channels = int(b[13])
			ai.SampleRate = int(be32(b[24:]))
		}
	case "sowt":
		pcm("s", 16, true)
	case "twos":
		pcm("s", max(e.Bits, 8)/8*8, false)
	case "raw ", "NONE":
		if e.Bits <= 8 {
			pcm("u", 8, false)
		} else {
			pcm("s", 16, false)
		}
	case "in24":
		pcm("s", 24, littleEnda)
	case "in32":
		pcm("s", 32, littleEnda)
	case "fl32":
		pcm("f", 32, littleEnda)
	case "fl64":
		pcm("f", 64, littleEnda)
	case "lpcm":
		kind := "s"
		if e.LPCMFlags&1 != 0 {
			kind = "f"
		} else if e.LPCMFlags&4 == 0 && e.Bits == 8 {
			kind = "u"
		}
		pcm(kind, e.Bits, e.LPCMFlags&2 == 0)
	case "ipcm", "fpcm":
		kind := "s"
		if e.Format == "fpcm" {
			kind = "f"
		}
		bits, little := e.Bits, false
		if b := e.Boxes["pcmC"]; len(b) >= 6 {
			little, bits = b[4]&1 != 0, int(b[5])
		}
		pcm(kind, bits, little)
	}
	if s.Codec == "pcm_s8" && e.Format == "twos" {
		s.Codec = "pcm_s8"
	}
	if ai.ChannelLayout == "" {
		ai.ChannelLayout = defaultLayout(ai.Channels)
	}
	_ = f
}
