package videoprobe

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func secs(d time.Duration) string { return fmt.Sprintf("%.6f", d.Seconds()) }

// JSON renders the Info with field names close to
// `ffprobe -show_format -show_streams -of json`, so the two can be compared
// field by field. Fields ffprobe lacks (vfr, rotation, hdr, fast_start,
// fragmented) are added alongside.
func (in *Info) JSON() ([]byte, error) {
	type m = map[string]any
	streams := make([]m, 0, len(in.Streams))
	for _, s := range in.Streams {
		o := m{
			"index": s.Index, "id": s.ID, "codec_type": s.Kind.String(),
			"time_base":  fmt.Sprintf("%d/%d", s.TimeBase.Num, s.TimeBase.Den),
			"start_time": secs(s.StartTime), "duration": secs(s.Duration),
			"disposition": m{"default": b2i(s.Default)},
		}
		set := func(k, v string) {
			if v != "" {
				o[k] = v
			}
		}
		set("codec_name", s.Codec)
		set("codec_tag_string", s.CodecTag)
		set("profile", s.Profile)
		set("level_name", s.Level)
		if s.Level != "" {
			o["level"] = s.level
		}
		if s.BitRate > 0 {
			o["bit_rate"] = fmt.Sprint(s.BitRate)
		}
		if s.Frames > 0 {
			o["nb_frames"] = fmt.Sprint(s.Frames)
		}
		if s.Language != "" {
			o["tags"] = m{"language": s.Language}
		}
		if v := s.Video; v != nil {
			o["width"], o["height"] = v.Width, v.Height
			o["coded_width"], o["coded_height"] = v.CodedWidth, v.CodedHeight
			o["sample_aspect_ratio"] = fmt.Sprintf("%d:%d", v.SAR.Num, v.SAR.Den)
			o["display_aspect_ratio"] = fmt.Sprintf("%d:%d", v.DAR.Num, v.DAR.Den)
			o["r_frame_rate"] = v.FrameRate.String()
			o["avg_frame_rate"] = v.AvgFrameRate.String()
			o["vfr"] = v.VFR
			o["rotation"] = v.Rotation
			if v.BitDepth > 0 {
				o["bits_per_raw_sample"] = fmt.Sprint(v.BitDepth)
			}
			set("chroma", v.Chroma)
			if v.Interlaced {
				o["field_order"] = "interlaced"
			} else {
				o["field_order"] = "progressive"
			}
			set("color_range", v.ColorRange)
			set("color_space", v.ColorMatrix)
			set("color_transfer", v.ColorTransfer)
			set("color_primaries", v.ColorPrimaries)
			set("hdr", v.HDR)
			if d := v.DolbyVision; d != nil {
				o["dolby_vision"] = m{
					"dv_version_major": d.VersionMajor, "dv_version_minor": d.VersionMinor,
					"dv_profile": d.Profile, "dv_level": d.Level,
					"rpu_present_flag": b2i(d.RPU), "el_present_flag": b2i(d.EL), "bl_present_flag": b2i(d.BL),
					"dv_bl_signal_compatibility_id": d.Compatibility,
				}
			}
		}
		if a := s.Audio; a != nil {
			o["sample_rate"] = fmt.Sprint(a.SampleRate)
			o["channels"] = a.Channels
			set("channel_layout", a.ChannelLayout)
			if a.BitDepth > 0 {
				o["bits_per_sample"] = a.BitDepth
			}
		}
		streams = append(streams, o)
	}
	name := in.Format
	switch name {
	case "mp4", "mov":
		name = "mov,mp4,m4a,3gp,3g2,mj2"
	case "matroska", "webm":
		name = "matroska,webm"
	}
	format := m{
		"format_name": name, "container": in.Format, "nb_streams": len(in.Streams),
		"start_time": secs(in.StartTime), "duration": secs(in.Duration),
		"size": fmt.Sprint(in.Size), "bit_rate": fmt.Sprint(in.BitRate),
		"fast_start": in.FastStart, "fragmented": in.Fragmented,
	}
	if len(in.Tags) > 0 {
		tags := m{}
		for k, v := range in.Tags {
			tags[strings.ToLower(k)] = v
		}
		format["tags"] = tags
	}
	return json.MarshalIndent(m{"streams": streams, "format": format}, "", "  ")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
