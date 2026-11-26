package videoprobe

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/avi"
	"github.com/gomcpgo/govideo/internal/container/flac"
	"github.com/gomcpgo/govideo/internal/container/matroska"
	"github.com/gomcpgo/govideo/internal/container/mp3"
	"github.com/gomcpgo/govideo/internal/container/wav"
)

var mkvCodecs = map[string]string{
	"V_VP8": "vp8", "V_VP9": "vp9", "V_AV1": "av1", "V_MPEG4/ISO/AVC": "h264", "V_MPEGH/ISO/HEVC": "hevc",
	"V_MJPEG": "mjpeg", "V_MPEG2": "mpeg2video", "V_MPEG1": "mpeg1video", "V_PRORES": "prores",
	"V_MPEG4/ISO/ASP": "mpeg4", "V_MPEG4/ISO/SP": "mpeg4", "V_MPEG4/ISO/AP": "mpeg4", "V_THEORA": "theora",
	"A_OPUS": "opus", "A_VORBIS": "vorbis", "A_AAC": "aac", "A_FLAC": "flac", "A_MPEG/L3": "mp3",
	"A_MPEG/L2": "mp2", "A_AC3": "ac3", "A_EAC3": "eac3", "A_ALAC": "alac", "A_DTS": "dts", "A_TRUEHD": "truehd",
	"S_TEXT/UTF8": "subrip", "S_TEXT/WEBVTT": "webvtt", "S_TEXT/ASS": "ass", "S_TEXT/SSA": "ass",
	"S_HDMV/PGS": "hdmv_pgs_subtitle", "S_VOBSUB": "dvd_subtitle", "D_WEBVTT/SUBTITLES": "webvtt",
	"D_WEBVTT/CAPTIONS": "webvtt",
}

func fromMatroska(f *matroska.File) *Info {
	info := &Info{Format: "matroska", Duration: f.Duration, Tags: map[string]string{}}
	if f.DocType == "webm" {
		info.Format = "webm"
	}
	for k, v := range f.Tags {
		info.Tags[k] = v
	}
	if f.Title != "" {
		info.Tags["title"] = f.Title
	}
	if f.MuxingApp != "" {
		info.Tags["encoder"] = f.MuxingApp
	}
	if !f.DateUTC.IsZero() {
		info.Tags["creation_time"] = f.DateUTC.Format(time.RFC3339)
	}
	scale := int64(f.TimestampScale)
	if scale <= 0 {
		scale = 1_000_000
	}
	first := true
	for _, t := range f.Tracks {
		s := Stream{
			ID: int64(t.Number), Kind: Data, CodecTag: t.CodecID, Codec: mkvCodecs[t.CodecID],
			TimeBase: reduce(scale, 1_000_000_000), Language: t.Language, Default: t.Default,
			Frames: t.Frames, Duration: f.Duration,
		}
		if t.HasTimestamps {
			s.StartTime = t.FirstTimestamp
		}
		if d := f.Duration.Seconds(); d > 0 {
			s.BitRate = int64(float64(t.TotalBytes) * 8 / d)
		}
		switch t.Type {
		case 1:
			s.Kind = Video
			mkvVideo(&s, t)
		case 2:
			s.Kind = Audio
			mkvAudio(&s, t)
		case 17:
			s.Kind = Subtitle
		}
		if s.Kind == Video || s.Kind == Audio {
			if t.HasTimestamps && (first || s.StartTime < info.StartTime) {
				info.StartTime = s.StartTime
				first = false
			}
		}
		info.Streams = append(info.Streams, s)
	}
	return info
}

func mkvVideo(s *Stream, t *matroska.Track) {
	vi := &VideoInfo{Width: t.PixelWidth, Height: t.PixelHeight, CodedWidth: t.PixelWidth, CodedHeight: t.PixelHeight}
	s.Video = vi
	var hv *codechdr.Video
	switch s.Codec {
	case "h264":
		hv, _ = codechdr.ParseAVCC(t.CodecPrivate)
	case "hevc":
		hv, _ = codechdr.ParseHVCC(t.CodecPrivate)
	case "av1":
		hv, _ = codechdr.ParseAV1C(t.CodecPrivate)
	case "vp9":
		hv, _ = codechdr.ParseVP9Frame(t.FirstFrame)
	case "vp8":
		hv, _ = codechdr.ParseVP8Frame(t.FirstFrame)
		vi.BitDepth, vi.Chroma = 8, "4:2:0"
	case "mjpeg":
		vi.BitDepth = 8
	}
	if hv != nil && (s.Codec == "vp9" || s.Codec == "vp8") {
		// The container's pixel size is authoritative for display.
		hv.Width, hv.Height = 0, 0
		hv.CodedWidth, hv.CodedHeight = 0, 0
	}
	applyVideo(s, hv)
	vi.Interlaced = vi.Interlaced || t.Interlaced

	sarN, sarD := int64(0), int64(0)
	if t.DisplayUnit == 0 && t.DisplayWidth > 0 && t.DisplayHeight > 0 && vi.Width > 0 && vi.Height > 0 {
		sarN = int64(t.DisplayWidth) * int64(vi.Height)
		sarD = int64(t.DisplayHeight) * int64(vi.Width)
	} else if hv != nil {
		sarN, sarD = int64(hv.SARNum), int64(hv.SARDen)
	}
	vi.setAspect(sarN, sarD)

	span := (t.LastTimestamp - t.FirstTimestamp).Seconds()
	if t.Frames > 1 && span > 0 {
		vi.AvgFrameRate = approxRate(float64(t.Frames-1) / span)
	}
	if t.DefaultDuration > 0 {
		vi.FrameRate = approxRate(1 / t.DefaultDuration.Seconds())
	} else {
		vi.FrameRate = vi.AvgFrameRate
	}
	if vi.AvgFrameRate.Den == 0 {
		vi.AvgFrameRate = vi.FrameRate
	}
	vi.VFR = t.VFR
	if t.HasProjection {
		r := int(math.Round(-t.ProjectionRoll/90)) * 90
		vi.Rotation = ((r % 360) + 360) % 360
	}

	c := colourOf(hv)
	if mc := t.Colour; mc != nil {
		known, full := mc.Range == 1 || mc.Range == 2, mc.Range == 2
		prim, trc, mat := mc.Primaries, mc.TransferCharacteristics, mc.MatrixCoefficients
		c.merge(orUnspec(prim), orUnspec(trc), orUnspec(mat), known, full)
		if vi.BitDepth == 0 && mc.BitsPerChannel > 0 {
			vi.BitDepth = mc.BitsPerChannel
		}
	}
	c.apply(vi)
}

func orUnspec(v int) int {
	if v == 0 {
		return 2
	}
	return v
}

func mkvAudio(s *Stream, t *matroska.Track) {
	ai := &AudioInfo{SampleRate: int(t.SamplingFrequency + 0.5), Channels: t.Channels, BitDepth: t.BitDepth}
	if t.OutputSamplingFrequency > 0 {
		ai.SampleRate = int(t.OutputSamplingFrequency + 0.5)
	}
	s.Audio = ai
	var a *codechdr.Audio
	switch {
	case s.Codec == "aac" || strings.HasPrefix(t.CodecID, "A_AAC"):
		s.Codec = "aac"
		a, _ = codechdr.ParseAudioSpecificConfig(t.CodecPrivate)
	case s.Codec == "opus":
		a, _ = codechdr.ParseOpusHead(t.CodecPrivate)
	case s.Codec == "flac":
		a, _ = codechdr.ParseFLACStreamInfo(t.CodecPrivate)
	case s.Codec == "mp3" || s.Codec == "mp2":
		if h, err := codechdr.ParseMPEGAudioHeader(t.FirstFrame); err == nil {
			a = &codechdr.Audio{ChannelLayout: h.ChannelLayout}
		}
	case t.CodecID == "A_PCM/INT/LIT":
		s.Codec = pcmName("s", t.BitDepth, true)
	case t.CodecID == "A_PCM/INT/BIG":
		s.Codec = pcmName("s", t.BitDepth, false)
	case t.CodecID == "A_PCM/FLOAT/IEEE":
		s.Codec = pcmName("f", t.BitDepth, true)
	}
	applyAudio(s, a)
	if ai.ChannelLayout == "" {
		ai.ChannelLayout = defaultLayout(ai.Channels)
	}
}

func pcmName(kind string, bits int, little bool) string {
	if bits == 8 {
		return "pcm_u8"
	}
	end := "be"
	if little {
		end = "le"
	}
	return fmt.Sprintf("pcm_%s%d%s", kind, bits, end)
}

func fromWAV(f *wav.File) *Info {
	kind := "s"
	if f.Float {
		kind = "f"
	}
	s := Stream{
		Kind: Audio, Codec: pcmName(kind, f.Bits, true), CodecTag: "wav",
		TimeBase: Rational{1, int64(f.SampleRate)}, Frames: f.Frames(), Default: true,
		Duration: ticks(f.Frames(), int64(f.SampleRate)),
		BitRate:  int64(f.SampleRate) * int64(f.BlockAlign) * 8,
		Audio: &AudioInfo{SampleRate: f.SampleRate, Channels: f.Channels, BitDepth: f.Bits,
			ChannelLayout: defaultLayout(f.Channels)},
	}
	return &Info{Format: "wav", Duration: s.Duration, Tags: map[string]string{}, Streams: []Stream{s}}
}

// fromMP3 describes a bare MPEG audio file. The duration is that of the
// audio: the samples of every frame, less the encoder's delay and padding
// where the file states them.
func fromMP3(f *mp3.File) *Info {
	rate := int64(f.SampleRate)
	s := Stream{
		Kind: Audio, Codec: f.Codec, TimeBase: Rational{1, rate}, Frames: f.Frames, Default: true,
		Duration: ticks(f.Samples(), rate), BitRate: int64(f.BitRate),
		Audio: &AudioInfo{SampleRate: f.SampleRate, Channels: f.Channels, ChannelLayout: defaultLayout(f.Channels)},
	}
	if total := f.Frames * int64(f.FrameSamples); f.VBR && total > 0 {
		s.BitRate = int64(float64(f.DataSize) * 8 * float64(rate) / float64(total)) // the mean
	}
	return &Info{Format: "mp3", Duration: s.Duration, Tags: map[string]string{}, Streams: []Stream{s}}
}

// fromFLAC describes a bare FLAC file. The length is the one its header
// states, or where it states none, the one its last frame gives.
func fromFLAC(f *flac.File) *Info {
	rate := int64(f.SampleRate)
	s := Stream{
		Kind: Audio, Codec: "flac", TimeBase: Rational{1, rate}, Default: true, Duration: ticks(f.Samples, rate),
		Audio: &AudioInfo{SampleRate: f.SampleRate, Channels: f.Channels, BitDepth: f.BitDepth},
	}
	if a, err := codechdr.ParseFLACStreamInfo(f.StreamInfo); err == nil {
		applyAudio(&s, a)
	}
	if f.BlockSize > 0 {
		s.Frames = (f.Samples + int64(f.BlockSize) - 1) / int64(f.BlockSize)
	}
	if d := s.Duration.Seconds(); d > 0 {
		s.BitRate = int64(float64(f.DataSize) * 8 / d)
	}
	info := &Info{Format: "flac", Duration: s.Duration, Tags: map[string]string{}, Streams: []Stream{s}}
	for k, v := range f.Tags {
		info.Tags[k] = v
	}
	return info
}

// aviVideoCodecs maps upper-cased video fourccs (biCompression, else the
// strh handler) to ffprobe codec names.
var aviVideoCodecs = map[string]string{
	"MJPG": "mjpeg", "AVRN": "mjpeg", "DMB1": "mjpeg", "JPEG": "mjpeg", "AVDJ": "mjpeg",
	"H264": "h264", "X264": "h264", "AVC1": "h264",
	"HEVC": "hevc", "H265": "hevc", "X265": "hevc", "HVC1": "hevc", "HEV1": "hevc",
	"XVID": "mpeg4", "DIVX": "mpeg4", "DX50": "mpeg4", "FMP4": "mpeg4", "MP4V": "mpeg4",
	"MPG1": "mpeg1video", "MPG2": "mpeg2video",
	"VP80": "vp8", "VP90": "vp9", "AV01": "av1",
	"DVSD": "dvvideo", "DVHD": "dvvideo", "DVSL": "dvvideo",
}

// aviAudioCodecs maps WAVEFORMATEX format tags of compressed audio to
// ffprobe codec names; PCM is named from the sample size.
var aviAudioCodecs = map[uint16]string{
	0x0006: "pcm_alaw", 0x0007: "pcm_mulaw", 0x0050: "mp2", avi.FormatMP3: "mp3",
	avi.FormatAAC: "aac", avi.FormatAC3: "ac3", 0x2001: "dts",
}

func fromAVI(f *avi.File) *Info {
	info := &Info{Format: "avi", Duration: f.Duration, Tags: map[string]string{}}
	first := true
	for _, t := range f.Streams {
		s := Stream{
			ID: int64(t.Index), Kind: Data, TimeBase: reduce(int64(t.Scale), int64(t.Rate)),
			StartTime: t.StartTime(), Duration: t.Duration(), Frames: t.Frames(),
		}
		if d := s.Duration.Seconds(); d > 0 {
			s.BitRate = int64(float64(t.TotalBytes) * 8 / d)
		}
		switch t.Type {
		case avi.StreamVideo:
			s.Kind = Video
			s.CodecTag = t.FourCC()
			s.Codec = aviVideoCodecs[strings.ToUpper(s.CodecTag)]
			vi := &VideoInfo{Width: t.Width, Height: t.Height, CodedWidth: t.Width, CodedHeight: t.Height}
			s.Video = vi
			if s.Codec == "mjpeg" {
				vi.BitDepth = 8
			}
			if num, den := t.FrameRate(); den > 0 {
				vi.FrameRate = reduce(int64(num), int64(den))
				vi.AvgFrameRate = vi.FrameRate
			}
			vi.setAspect(0, 0)
			c := colourOf(nil)
			c.apply(vi)
		case avi.StreamAudio:
			s.Kind = Audio
			s.CodecTag = fmt.Sprintf("0x%04x", t.FormatTag)
			switch {
			case t.FormatTag == avi.FormatPCM && t.BitsPerSample%8 == 0 && t.BitsPerSample >= 8 && t.BitsPerSample <= 32:
				s.Codec = pcmName("s", t.BitsPerSample, true)
			case t.FormatTag == avi.FormatIEEEFloat && (t.BitsPerSample == 32 || t.BitsPerSample == 64):
				s.Codec = pcmName("f", t.BitsPerSample, true)
			default:
				s.Codec = aviAudioCodecs[t.FormatTag]
			}
			if t.IsPCM() {
				s.TimeBase = Rational{1, int64(t.SampleRate)}
				s.BitRate = int64(t.SampleRate) * int64(t.BlockAlign) * 8
			}
			s.Audio = &AudioInfo{SampleRate: t.SampleRate, Channels: t.Channels, BitDepth: t.BitsPerSample,
				ChannelLayout: defaultLayout(t.Channels)}
		}
		if s.Kind == Video || s.Kind == Audio {
			if first || s.StartTime < info.StartTime {
				info.StartTime = s.StartTime
				first = false
			}
		}
		info.Streams = append(info.Streams, s)
	}
	return info
}
