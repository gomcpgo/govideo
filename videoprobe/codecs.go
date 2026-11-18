package videoprobe

import "github.com/gomcpgo/govideo/internal/codechdr"

// applyVideo copies parsed codec-header fields into the stream.
func applyVideo(s *Stream, v *codechdr.Video) {
	if v == nil {
		return
	}
	vi := s.Video
	if v.Profile != "" {
		s.Profile = v.Profile
	}
	if v.Level != codechdr.LevelUnknown {
		s.level = v.Level
		s.Level = levelString(s.Codec, v.Level)
	}
	if v.Width > 0 && v.Height > 0 {
		vi.Width, vi.Height = v.Width, v.Height
	}
	if v.CodedWidth > 0 && v.CodedHeight > 0 {
		vi.CodedWidth, vi.CodedHeight = v.CodedWidth, v.CodedHeight
	}
	if v.BitDepth > 0 {
		vi.BitDepth = v.BitDepth
	}
	if v.Chroma != "" {
		vi.Chroma = v.Chroma
	}
	vi.Interlaced = v.Interlaced
}

// colour merges codec-level and container-level colour information; the
// codec bitstream wins where it specifies a value.
type colour struct {
	prim, trc, matrix int
	rangeKnown, full  bool
}

func (c *colour) merge(prim, trc, matrix int, rangeKnown, full bool) {
	if c.prim == 0 || c.prim == 2 {
		c.prim = prim
	}
	if c.trc == 0 || c.trc == 2 {
		c.trc = trc
	}
	if c.matrix == 2 || c.matrix == 0 && !c.rangeKnown && matrix != 0 {
		c.matrix = matrix
	}
	if !c.rangeKnown && rangeKnown {
		c.rangeKnown, c.full = true, full
	}
}

func (c *colour) apply(vi *VideoInfo) {
	vi.ColorPrimaries = codechdr.PrimariesName(c.prim)
	vi.ColorTransfer = codechdr.TransferName(c.trc)
	vi.ColorMatrix = codechdr.MatrixName(c.matrix)
	if c.matrix == 0 && c.prim != 0 && c.prim != 2 {
		vi.ColorMatrix = "gbr"
	}
	if c.rangeKnown {
		vi.ColorRange = "tv"
		if c.full {
			vi.ColorRange = "pc"
		}
	}
	vi.HDR = hdrKind(c.trc)
}

func colourOf(v *codechdr.Video) colour {
	if v == nil {
		return colour{prim: 2, trc: 2, matrix: 2}
	}
	return colour{v.ColorPrimaries, v.ColorTransfer, v.ColorMatrix, v.ColorRangeKnown, v.FullRange}
}

func applyAudio(s *Stream, a *codechdr.Audio) {
	if a == nil {
		return
	}
	ai := s.Audio
	if a.Profile != "" {
		s.Profile = a.Profile
	}
	if a.SampleRate > 0 {
		ai.SampleRate = a.SampleRate
	}
	if a.Channels > 0 {
		ai.Channels = a.Channels
	}
	if a.BitDepth > 0 {
		ai.BitDepth = a.BitDepth
	}
	if a.ChannelLayout != "" {
		ai.ChannelLayout = a.ChannelLayout
	}
}

func dolbyVision(b []byte) *DolbyVisionInfo {
	d, err := codechdr.ParseDOVIConfig(b)
	if err != nil {
		return nil
	}
	return &DolbyVisionInfo{
		Profile: d.Profile, Level: d.Level, Compatibility: d.BLSignalCompatibilityID,
		RPU: d.RPUPresent, EL: d.ELPresent, BL: d.BLPresent,
		VersionMajor: d.VersionMajor, VersionMinor: d.VersionMinor,
	}
}
