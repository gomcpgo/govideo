package govideo

import (
	"bufio"
	"errors"
	"fmt"
	"os"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/codec/native"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/internal/container/wav"
	"github.com/gomcpgo/govideo/process"
)

// videoSink encodes the output pictures of an export and writes them to the
// video track. write is called once per output frame; a nil picture repeats
// the previous one.
type videoSink interface {
	write(pic *codec.VideoFrame) error
	finish() error
	close()
}

// audioSink encodes one output audio track, or the file of an audio-only
// export. Samples are interleaved, at the sink's sample rate and channel
// count: 48 kHz stereo for the tracks of a video.
type audioSink interface {
	write(samples []float32) error
	// finish ends the track, which carries total sample frames of programme.
	finish(total int64) error
	close()
}

// Test hooks, nil outside tests. tuneH264 may change the encoder settings an
// export has chosen; tapH264 sees every picture handed to the encoder. The
// encoder-settings measurement uses them to compare rate-control modes
// against the exact pictures that were encoded.
var (
	tuneH264 func(*oscodec.EncodeConfig)
	tapH264  func(*codec.VideoFrame)
)

// mjpegSink writes Motion JPEG: every frame is a sync sample, and a repeated
// picture reuses the encoded bytes.
type mjpegSink struct {
	mux   *isobmff.Muxer
	track int
	dur   uint32
	enc   *native.MJPEGEncoder
	last  []byte
}

func (s *mjpegSink) write(pic *codec.VideoFrame) (err error) {
	if pic != nil {
		if s.last, err = s.enc.Encode(pic); err != nil {
			return err
		}
	}
	return s.mux.WriteSample(s.track, s.last, s.dur, true, 0)
}

func (s *mjpegSink) finish() error { return nil }
func (s *mjpegSink) close()        {}

// h264Sink writes H.264 from the operating-system encoder. The sample entry
// is set once the encoder has produced its parameter sets. With B-frames the
// encoder returns samples in decode order; each is written with the distance
// between its presentation and decode positions as its composition offset.
type h264Sink struct {
	mux           *isobmff.Muxer
	track         int
	dur           uint32
	width, height int
	enc           codec.VideoStreamEncoder
	last          *codec.VideoFrame
	described     bool
	written       int64 // samples written so far: the next decode position
}

func (s *h264Sink) write(pic *codec.VideoFrame) error {
	if pic == nil {
		pic = s.last
	}
	if pic == nil {
		return errors.New("govideo: no picture to encode")
	}
	pic = process.ConvertTo709Limited(pic)
	s.last = pic
	if tapH264 != nil {
		tapH264(pic)
	}
	out, err := s.enc.Encode(pic)
	if err != nil {
		return err
	}
	return s.emit(out)
}

func (s *h264Sink) emit(out []codec.EncodedFrame) error {
	for _, f := range out {
		if !s.described {
			cfg := s.enc.Config()
			if cfg == nil {
				return errors.New("govideo: the H.264 encoder returned no configuration")
			}
			// BT.709 primaries, transfer and matrix, limited range (SPEC §8.1).
			s.mux.SetEntry(s.track, isobmff.VisualEntry("avc1", s.width, s.height, "",
				isobmff.Box("avcC", cfg), isobmff.ColrNCLX(1, 1, 1, false)))
			s.described = true
		}
		lead := f.Index - s.written
		if lead < -64 || lead > 64 {
			return fmt.Errorf("govideo: the H.264 encoder returned frame %d at decode position %d", f.Index, s.written)
		}
		if err := s.mux.WriteSample(s.track, f.Data, s.dur, f.Sync, int32(lead)*int32(s.dur)); err != nil {
			return err
		}
		s.written++
	}
	return nil
}

func (s *h264Sink) finish() error {
	out, err := s.enc.Flush()
	if err != nil {
		return err
	}
	return s.emit(out)
}

func (s *h264Sink) close() { s.enc.Close() }

// pcmSink writes 16-bit little-endian LPCM.
type pcmSink struct {
	mux   *isobmff.Muxer
	track int
	buf   []byte
}

func (s *pcmSink) write(samples []float32) error {
	s.buf = native.AppendS16LE(s.buf[:0], samples)
	return s.mux.WriteSample(s.track, s.buf, 0, true, 0)
}

func (s *pcmSink) finish(int64) error { return nil }
func (s *pcmSink) close()             {}

// aacSink writes AAC-LC from the operating-system encoder.
type aacSink struct {
	mux   *isobmff.Muxer
	track int
	enc   codec.AudioStreamEncoder
}

func (s *aacSink) emit(packets [][]byte, err error) error {
	if err != nil {
		return err
	}
	for _, p := range packets {
		if err := s.mux.WriteSample(s.track, p, uint32(s.enc.FrameSize()), true, 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *aacSink) write(samples []float32) error { return s.emit(s.enc.Encode(samples)) }

func (s *aacSink) finish(total int64) error {
	if err := s.emit(s.enc.Flush()); err != nil {
		return err
	}
	// The encoder's priming samples are cut by an edit, which also ends the
	// track exactly where the programme ends.
	s.mux.SetEdit(s.track, int64(s.enc.Priming()), uint64(total))
	return nil
}

func (s *aacSink) close() { s.enc.Close() }

// newAACSink adds an AAC-LC track to mux and opens its encoder. cfg carries
// what the caller decides about the track (language, flags); the sample
// entry and the timescale, which is the sample rate, are set here. bitrate 0
// leaves the rate to the encoder.
func newAACSink(mux *isobmff.Muxer, brand isobmff.Brand, cfg isobmff.TrackConfig, rate, channels, bitrate int) (*aacSink, error) {
	enc, err := oscodec.NewAACEncoder(oscodec.AudioEncodeConfig{SampleRate: rate, Channels: channels, Bitrate: bitrate})
	if err != nil {
		return nil, err
	}
	aim := uint32(enc.Bitrate())
	esds := isobmff.ESDS(0x40, 0x05, enc.Config(), 0, aim, aim)
	cfg.Handler, cfg.Timescale = "soun", uint32(rate)
	if brand == isobmff.BrandMOV {
		cfg.Entry = isobmff.AudioEntryQT("mp4a", channels, rate, enc.FrameSize(), esds)
	} else {
		cfg.Entry = isobmff.AudioEntry("mp4a", channels, 16, rate, esds)
	}
	return &aacSink{mux: mux, track: mux.AddTrack(cfg), enc: enc}, nil
}

// wavSink writes an audio-only export as a WAVE file of 16-bit PCM. The
// header goes in last, when the length is known.
type wavSink struct {
	f              *os.File
	w              *bufio.Writer
	rate, channels int
	frames         int64
	buf            []byte
}

// newWAVSink starts a WAVE file in f, which must be empty.
func newWAVSink(f *os.File, rate, channels int) (*wavSink, error) {
	s := &wavSink{f: f, w: bufio.NewWriterSize(f, 1<<18), rate: rate, channels: channels}
	if _, err := s.w.Write(make([]byte, wav.HeaderSize)); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *wavSink) write(samples []float32) error {
	s.buf = native.AppendS16LE(s.buf[:0], samples)
	s.frames += int64(len(samples) / s.channels)
	_, err := s.w.Write(s.buf)
	return err
}

func (s *wavSink) finish(int64) error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	h, err := wav.PCMHeader(s.channels, s.rate, 16, s.frames)
	if err != nil {
		return fmt.Errorf("govideo: %w", err)
	}
	_, err = s.f.WriteAt(h, 0)
	return err
}

func (s *wavSink) close() {}

// flacSink writes an audio-only export as a FLAC file: 16-bit samples,
// compressed without loss by the module's own encoder. The header goes in
// again at the end, when the length and the checksum of the sound are known.
type flacSink struct {
	f   *os.File
	w   *bufio.Writer
	enc *native.FLACEncoder
}

// newFLACSink starts a FLAC file in f, which must be empty.
func newFLACSink(f *os.File, rate, channels int) (*flacSink, error) {
	enc, err := native.NewFLACEncoder(rate, channels)
	if err != nil {
		return nil, err
	}
	s := &flacSink{f: f, w: bufio.NewWriterSize(f, 1<<18), enc: enc}
	if _, err := s.w.Write(enc.Header()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *flacSink) emit(frames [][]byte, err error) error {
	if err != nil {
		return err
	}
	for _, f := range frames {
		if _, err := s.w.Write(f); err != nil {
			return err
		}
	}
	return nil
}

func (s *flacSink) write(samples []float32) error { return s.emit(s.enc.Encode(samples)) }

func (s *flacSink) finish(int64) error {
	if err := s.emit(s.enc.Flush()); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	_, err := s.f.WriteAt(s.enc.Header(), 0)
	return err
}

func (s *flacSink) close() {}

// outputs creates the tracks of an export for one profile.
type outputs struct {
	profile Profile
	brand   isobmff.Brand
	mux     *isobmff.Muxer
	canvas  Canvas
	opts    ExportOptions
	sinks   []interface{ close() }
}

func (o *outputs) close() {
	for _, s := range o.sinks {
		s.close()
	}
}

// h264Quality is the encoder quality setting for each Quality preset. The
// values were chosen on a corpus of real footage to give the same median
// picture quality as the average-bit-rate targets they replaced (SPEC §8.1).
func (o *outputs) h264Quality() float64 {
	switch o.opts.Quality {
	case QualityLow:
		return 0.55
	case QualityMedium:
		return 0.60
	}
	return 0.65
}

// fallbackBitrate picks an H.264 bit rate from the picture size, frame rate
// and quality, for an encoder that has no quality-targeted mode: about
// 8 Mbit/s for 1080p30 at high quality.
func (o *outputs) fallbackBitrate() int {
	bpp := map[Quality]float64{QualityLow: 0.045, QualityMedium: 0.08}[o.opts.Quality]
	if bpp == 0 {
		bpp = 0.13
	}
	rate := bpp * float64(o.canvas.Width) * float64(o.canvas.Height) * min(o.canvas.FrameRate.Float(), 60)
	return int(max(rate, 150_000))
}

// h264Encoder opens the encoder. Unless the caller fixed a bit rate, it asks
// for a constant picture quality, so that the size follows the content: a
// slide show comes out small and a busy scene keeps its detail.
func (o *outputs) h264Encoder() (codec.VideoStreamEncoder, error) {
	cv := o.canvas
	ec := oscodec.EncodeConfig{Width: cv.Width, Height: cv.Height, FrameNum: cv.FrameRate.Num, FrameDen: cv.FrameRate.Den}
	if o.opts.VideoBitrate > 0 {
		ec.Bitrate = o.opts.VideoBitrate
	} else {
		ec.Quality = o.h264Quality()
	}
	if tuneH264 != nil {
		tuneH264(&ec)
	}
	enc, err := oscodec.NewH264Encoder(ec)
	if errors.Is(err, oscodec.ErrNoQualityMode) {
		// Not every encoder has a quality mode; aim for a bit rate instead.
		ec.Quality, ec.Bitrate = 0, o.fallbackBitrate()
		enc, err = oscodec.NewH264Encoder(ec)
	}
	return enc, err
}

func (o *outputs) audioBitrate() int {
	if o.opts.AudioBitrate > 0 {
		return o.opts.AudioBitrate
	}
	if r := map[Quality]int{QualityLow: 128_000, QualityMedium: 160_000}[o.opts.Quality]; r != 0 {
		return r
	}
	return 192_000
}

// video adds the video track. dur is the frame duration in timescale ticks.
func (o *outputs) video(timescale, dur uint32) (videoSink, error) {
	cv := o.canvas
	cfg := isobmff.TrackConfig{Handler: "vide", Timescale: timescale, Width: cv.Width, Height: cv.Height}
	if o.profile == ProfileIntermediate {
		cfg.AllSync = true
		if o.brand == isobmff.BrandMOV {
			cfg.Entry = isobmff.VisualEntry("jpeg", cv.Width, cv.Height, "Photo - JPEG")
		} else {
			cfg.Entry = isobmff.VisualEntry("mp4v", cv.Width, cv.Height, "", isobmff.ESDS(0x6c, 0x04, nil, 0, 0, 0))
		}
		s := &mjpegSink{mux: o.mux, track: o.mux.AddTrack(cfg), dur: dur,
			enc: &native.MJPEGEncoder{Quality: map[Quality]int{QualityLow: 70, QualityMedium: 82}[o.opts.Quality]}}
		return s, nil
	}
	enc, err := o.h264Encoder()
	if err != nil {
		return nil, fmt.Errorf("govideo: %w", err)
	}
	s := &h264Sink{mux: o.mux, track: o.mux.AddTrack(cfg), dur: dur, width: cv.Width, height: cv.Height, enc: enc}
	o.sinks = append(o.sinks, s)
	return s, nil
}

// audio adds an audio track and returns its sink.
func (o *outputs) audio(language string, disabled bool) (audioSink, error) {
	cfg := isobmff.TrackConfig{Handler: "soun", Timescale: outRate, AltGroup: 1, Language: language, Disabled: disabled}
	if o.profile == ProfileIntermediate {
		cfg.SampleSize = 4
		if o.brand == isobmff.BrandMOV {
			cfg.Entry = isobmff.AudioEntry("sowt", 2, 16, outRate)
		} else {
			cfg.Entry = isobmff.AudioEntry("ipcm", 2, 16, outRate, isobmff.PcmC(true, 16))
		}
		return &pcmSink{mux: o.mux, track: o.mux.AddTrack(cfg)}, nil
	}
	s, err := newAACSink(o.mux, o.brand, cfg, outRate, 2, o.audioBitrate())
	if err != nil {
		return nil, fmt.Errorf("govideo: %w", err)
	}
	o.sinks = append(o.sinks, s)
	return s, nil
}
