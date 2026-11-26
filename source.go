package govideo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // still image formats
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/codec/native"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/avi"
	"github.com/gomcpgo/govideo/internal/container/flac"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/internal/container/matroska"
	"github.com/gomcpgo/govideo/internal/container/mp3"
	"github.com/gomcpgo/govideo/internal/container/wav"
	"github.com/gomcpgo/govideo/process"
	"github.com/gomcpgo/govideo/videoprobe"
)

const outRate = 48000 // programme audio sample rate (SPEC §7.4)

// packet is one compressed sample with its presentation time.
type packet struct {
	pts  time.Duration
	data []byte
	sync bool // a decoder can start here
}

type packetReader interface {
	next() (packet, error) // io.EOF at end of stream
}

// media is an opened source file.
type media struct {
	path string
	f    *os.File
	info *videoprobe.Info
	iso  *isobmff.File
	mkv  *matroska.File
	wv   *wav.File
	avi  *avi.File
	mp3  *mp3.File
	flac *flac.File
}

func openMedia(ctx context.Context, path string) (*media, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	m := &media{path: path, f: f}
	fail := func(err error) (*media, error) {
		f.Close()
		return nil, fmt.Errorf("govideo: %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if m.info, err = videoprobe.Probe(ctx, f, st.Size()); err != nil {
		// A bare audio file of a codec with no decoder is a missing codec,
		// and is reported as one.
		var uf *videoprobe.UnsupportedFormatError
		if errors.As(err, &uf) {
			if id, ok := map[string]CodecID{"ac3": "ac3"}[uf.Format]; ok {
				if _, cerr := Available(id, Decode); cerr != nil {
					return fail(cerr)
				}
			}
		}
		return fail(err)
	}
	switch m.info.Format {
	case "mp4", "mov":
		m.iso, err = isobmff.Open(ctx, f, st.Size())
	case "matroska", "webm":
		m.mkv, err = matroska.Open(ctx, f, st.Size())
	case "wav":
		m.wv, err = wav.Open(ctx, f, st.Size())
	case "avi":
		m.avi, err = avi.Open(ctx, f, st.Size())
	case "mp3":
		// videoprobe measures layers I and II too, which have no decoder.
		if _, err = Available(CodecID(m.info.Streams[0].Codec), Decode); err == nil {
			m.mp3, err = mp3.Open(ctx, f, st.Size())
		}
	case "flac":
		if _, err = Available("flac", Decode); err == nil {
			m.flac, err = flac.Open(ctx, f, st.Size())
		}
	default:
		err = fmt.Errorf("unsupported container %q", m.info.Format)
	}
	if err != nil {
		return fail(err)
	}
	return m, nil
}

func (m *media) close() { m.f.Close() }

// stream returns the first stream of a kind, preferring the default one.
func (m *media) stream(kind videoprobe.Kind) *videoprobe.Stream {
	var first *videoprobe.Stream
	for i := range m.info.Streams {
		s := &m.info.Streams[i]
		if s.Kind != kind {
			continue
		}
		if s.Default {
			return s
		}
		if first == nil {
			first = s
		}
	}
	return first
}

func (m *media) reader(ctx context.Context, s *videoprobe.Stream) (packetReader, error) {
	pcmFrame := 0
	if s.Audio != nil && strings.HasPrefix(s.Codec, "pcm_") {
		pcmFrame = s.Audio.Channels * s.Audio.BitDepth / 8
	}
	switch {
	case m.iso != nil:
		for _, t := range m.iso.Tracks {
			if int64(t.ID) != s.ID {
				continue
			}
			r := &isoReader{f: m.iso, tr: t, it: t.Iter(), ts: int64(t.Timescale), pcm: pcmFrame > 0, pcmFrame: pcmFrame}
			if pcmFrame > 0 {
				r.it.SetUniformSize(uint32(pcmFrame))
			}
			// The first edit positions the media on the presentation
			// timeline: empty edits delay it, media_time trims its start.
			movieTS := int64(m.iso.Timescale)
			for _, e := range t.Edits {
				if e.MediaTime < 0 {
					if movieTS > 0 {
						r.shift += time.Duration(float64(e.SegmentDuration) / float64(movieTS) * 1e9)
					}
					continue
				}
				r.mediaStart = e.MediaTime
				break
			}
			// Audio files from iTunes-style encoders mark the encoder's
			// priming samples in a tag instead of an edit list.
			if g := m.iso.Gapless; g != nil && len(t.Edits) == 0 && s.Audio != nil && s.Audio.SampleRate > 0 {
				r.mediaStart = int64(g.Priming) * int64(t.Timescale) / int64(s.Audio.SampleRate)
			}
			return r, nil
		}
	case m.mkv != nil:
		for _, t := range m.mkv.Tracks {
			if int64(t.Number) == s.ID {
				if t.Err != nil {
					return nil, t.Err
				}
				return &mkvReader{f: m.mkv, ctx: ctx, pr: m.mkv.NewReaderContext(ctx), track: t.Number, delay: t.CodecDelay, pcm: pcmFrame > 0}, nil
			}
		}
	case m.wv != nil:
		return &wavReader{f: m.f, w: m.wv}, nil
	case m.mp3 != nil:
		return newMP3Reader(m.f, m.mp3), nil
	case m.flac != nil:
		return &flacReader{f: m.flac, rd: m.flac.NewReader(m.f)}, nil
	case m.avi != nil:
		for _, t := range m.avi.Streams {
			if int64(t.Index) == s.ID {
				return &aviReader{f: m.avi, s: t}, nil
			}
		}
	}
	return nil, fmt.Errorf("govideo: %s: stream %d not found", m.path, s.Index)
}

// config returns the codec configuration of stream s: the named child box of
// its MP4 sample entry, the Matroska CodecPrivate, or the STREAMINFO block of
// a FLAC file.
func (m *media) config(s *videoprobe.Stream, box string) []byte {
	switch {
	case m.flac != nil:
		return m.flac.StreamInfo
	case m.iso != nil:
		for _, t := range m.iso.Tracks {
			if int64(t.ID) == s.ID && len(t.Entries) > 0 {
				return t.Entries[0].Boxes[box]
			}
		}
	case m.mkv != nil:
		for _, t := range m.mkv.Tracks {
			if int64(t.Number) == s.ID {
				return t.CodecPrivate
			}
		}
	}
	return nil
}

// videoSpan returns how long the video stream s plays. Matroska records only
// a file duration, which also covers audio that outlasts the picture, so
// there the span is measured from the frame timestamps.
func (m *media) videoSpan(s *videoprobe.Stream) time.Duration {
	if m.mkv != nil {
		for _, t := range m.mkv.Tracks {
			if int64(t.Number) != s.ID || !t.HasTimestamps || t.LastTimestamp <= t.FirstTimestamp {
				continue
			}
			span := t.LastTimestamp - t.FirstTimestamp
			if r := s.Video.FrameRate.Float(); r > 0 {
				span += time.Duration(1e9 / r)
			} else if t.Frames > 1 {
				span += span / time.Duration(t.Frames-1)
			}
			return min(span, s.Duration)
		}
	}
	return s.Duration
}

type isoReader struct {
	f          *isobmff.File
	tr         *isobmff.Track
	it         *isobmff.SampleIter
	ts         int64
	mediaStart int64
	shift      time.Duration
	pcm        bool
	pcmFrame   int // bytes in a PCM sample frame
}

func (r *isoReader) next() (packet, error) {
	var (
		s   isobmff.Sample
		n   = 1
		err error
	)
	if r.pcm {
		s, n, err = r.it.NextBlock(4096)
	} else {
		s, err = r.it.Next()
	}
	if err != nil {
		return packet{}, err
	}
	data, err := r.f.ReadSample(s, n, nil)
	if errors.Is(err, isobmff.ErrTruncated) {
		// The file was cut short: play what is there. The picture then
		// holds on the last complete frame and the audio falls silent.
		return packet{}, io.EOF
	}
	if err != nil {
		return packet{}, err
	}
	return packet{pts: r.time(s.PTS()), data: data, sync: s.Sync}, nil
}

// time converts a media time to a presentation time.
func (r *isoReader) time(t int64) time.Duration {
	return r.shift + time.Duration(float64(t-r.mediaStart)/float64(r.ts)*1e9)
}

// syncSeeker is a packetReader that can start at a sync sample.
type syncSeeker interface {
	// seekSync positions the reader on the last sync sample presented at
	// or before t, so a decoder need not start from the beginning of the
	// file. It must be called before the first next.
	seekSync(t time.Duration)
}

// audioSeeker is a packetReader of an audio stream that can start partway
// through it.
type audioSeeker interface {
	// seekAudio positions the reader where a decoder can start, as far
	// into the stream as it can with no more than n passed over, and
	// returns how much that is. Both are counts: of sample frames for PCM,
	// of packets for any other codec. It returns 0, and the reader stays at
	// the start, when the reader cannot place itself by count. It must be
	// called before the first next.
	//
	// Audio is sought by count and not by time because that is how it is
	// played: the samples of a stream follow one another from the time of
	// its first packet, whatever the later timestamps say. Those are no
	// guide to the sample. Matroska rounds them to the millisecond, and MP4
	// files give packets lengths their samples do not have (a first packet
	// that lasts 1871 samples and decodes to 1024, in a phone's recording).
	seekAudio(n int64) (passed int64)
}

// seek moves the reader to the last sync sample that fits. The samples are
// offered in decode order, each with the count of what comes before it
// (sample frames of PCM, else samples), until one is past the place sought.
// It returns the count before the sample the reader is left at.
func (r *isoReader) seek(past, fits func(s isobmff.Sample, before int64) bool) (passed int64) {
	// A copy of the iterator runs ahead through the sample table, which
	// reads nothing from the file; the reader then takes up the state it had
	// before the last sample a decoder can start at.
	ahead, at := *r.it, *r.it
	var count int64
	for {
		here, before := ahead, count
		var (
			s   isobmff.Sample
			n   = 1
			err error
		)
		if r.pcm {
			s, n, err = ahead.NextBlock(4096)
		} else {
			s, err = ahead.Next()
		}
		// The index of a file cut short lists samples that were never
		// written; reading has to start where there is something to read.
		if err != nil || past(s, before) || !r.f.Present(s) {
			break
		}
		if r.pcm {
			count += int64(s.Size) * int64(n) / int64(r.pcmFrame)
		} else {
			count++
		}
		if s.Sync && fits(s, before) {
			at, passed = here, before
		}
	}
	*r.it = at
	return passed
}

func (r *isoReader) seekSync(t time.Duration) {
	r.seek(func(s isobmff.Sample, _ int64) bool { return r.time(s.DTS) > t },
		func(s isobmff.Sample, _ int64) bool { return r.time(s.PTS()) <= t })
}

func (r *isoReader) seekAudio(n int64) (passed int64) {
	return r.seek(func(_ isobmff.Sample, before int64) bool { return before > n },
		func(isobmff.Sample, int64) bool { return true })
}

type mkvReader struct {
	f     *matroska.File
	ctx   context.Context
	pr    *matroska.PacketReader
	track uint64
	// delay is the track's CodecDelay: block timestamps are that much later
	// than presentation time, so the encoder's priming samples land before
	// time zero and are cut like any other leading audio.
	delay time.Duration
	pcm   bool
}

func (r *mkvReader) next() (packet, error) {
	for {
		p, err := r.pr.Next()
		if err != nil {
			return packet{}, err
		}
		if p.Track == r.track {
			return packet{pts: p.Timestamp - r.delay, data: p.Data, sync: p.Keyframe}, nil
		}
	}
}

func (r *mkvReader) seekSync(t time.Duration) {
	if pr, ok := r.f.NewReaderAt(r.ctx, r.track, t+r.delay); ok {
		r.pr = pr
	}
}

func (r *mkvReader) seekAudio(n int64) (passed int64) {
	if r.pcm {
		// The index counts blocks, and a block of PCM is as many samples
		// as the muxer chose to put in it.
		return 0
	}
	if pr, ok := r.f.NewReaderAtFrame(r.ctx, r.track, n); ok {
		r.pr = pr
	}
	return r.pr.Passed()
}

// aviReader reads the chunks of one AVI stream.
type aviReader struct {
	f *avi.File
	s *avi.Stream
	i int
}

func (r *aviReader) next() (packet, error) {
	for r.i < len(r.s.Chunks) {
		i := r.i
		r.i++
		c := r.s.Chunks[i]
		if c.Size == 0 {
			continue // a dropped frame: the previous picture stays up
		}
		data, err := r.f.ReadChunk(c, nil)
		if err != nil {
			return packet{}, err
		}
		return packet{pts: r.s.ChunkTime(i), data: data, sync: true}, nil
	}
	return packet{}, io.EOF
}

// seekAudio starts at the last chunk of PCM with no more than n sample
// frames before it.
func (r *aviReader) seekAudio(n int64) (passed int64) {
	align := int64(r.s.BlockAlign)
	if !r.s.IsPCM() || align == 0 {
		return 0
	}
	i := sort.Search(len(r.s.Chunks), func(i int) bool { return r.s.BytesBefore(i)/align > n })
	r.i = max(i-1, 0)
	return r.s.BytesBefore(r.i) / align
}

type wavReader struct {
	f   io.ReaderAt
	w   *wav.File
	pos int64 // frames read
}

func (r *wavReader) next() (packet, error) {
	left := r.w.Frames() - r.pos
	if left <= 0 {
		return packet{}, io.EOF
	}
	n := min(left, 4096)
	buf := make([]byte, n*int64(r.w.BlockAlign))
	if _, err := r.f.ReadAt(buf, r.w.DataOffset+r.pos*int64(r.w.BlockAlign)); err != nil {
		return packet{}, err
	}
	pts := time.Duration(float64(r.pos) / float64(r.w.SampleRate) * 1e9)
	r.pos += n
	return packet{pts: pts, data: buf}, nil
}

func (r *wavReader) seekAudio(n int64) (passed int64) {
	r.pos = min(max(n, 0), r.w.Frames())
	return r.pos
}

// mp3Reader reads the frames of a bare MP3 file.
type mp3Reader struct {
	f     *mp3.File
	rd    *mp3.Reader
	n     int64         // frames read or passed over
	first time.Duration // the time of the first frame
}

func newMP3Reader(f io.ReaderAt, m *mp3.File) *mp3Reader {
	r := &mp3Reader{f: m, rd: m.NewReader(f)}
	// A file that states its encoder's delay starts where its audio does:
	// what the encoder and the decoder put before it lies ahead of time
	// zero and is cut. The end is cut by the stream's duration, which
	// leaves the padding out. A file that states nothing is played as it
	// decodes, delays and all, which is what ffmpeg does with it and what
	// videoprobe gives as its length.
	r.first = -time.Duration(float64(m.Lead()) / float64(m.SampleRate) * 1e9)
	return r
}

func (r *mp3Reader) next() (packet, error) {
	data, err := r.rd.Next()
	if err != nil {
		return packet{}, err
	}
	pts := r.first + time.Duration(float64(r.n*int64(r.f.FrameSamples))/float64(r.f.SampleRate)*1e9)
	r.n++
	return packet{pts: pts, data: data, sync: true}, nil
}

func (r *mp3Reader) seekAudio(n int64) (passed int64) {
	r.n = r.rd.Skip(n)
	return r.n
}

// flacReader reads the frames of a bare FLAC file.
type flacReader struct {
	f  *flac.File
	rd *flac.Reader
}

func (r *flacReader) next() (packet, error) {
	data, err := r.rd.Next()
	if err != nil {
		return packet{}, err
	}
	// A frame says where in the stream its samples start.
	pts := time.Duration(float64(r.f.FirstSample(data)) / float64(r.f.SampleRate) * 1e9)
	return packet{pts: pts, data: data, sync: true}, nil
}

// seekAudio goes to a frame by its number: it reads a few places in the
// file, not the frames before.
func (r *flacReader) seekAudio(n int64) (passed int64) { return r.rd.Skip(n) }

// --- audio -------------------------------------------------------------------

// audioStream delivers a source audio stream as 48 kHz stereo float32.
//
// A stream that starts well into its source does not decode what comes
// before: reading starts a run-in ahead of the cut (see audioRunIn). The
// samples still come out where decoding from the start of the file puts
// them, to the sample: what was passed over is counted, not timed, the
// number of samples to drop is still taken from the time of the stream's
// first packet, and the resampler keeps to the output grid that starts
// there. So two clips cut from one file at the same point join without a
// seam, as they did when both were decoded from the start.
type audioStream struct {
	rd      packetReader
	dec     codec.AudioDecoder
	rs      *process.Resampler // created from the first decoded block
	buf     []float32
	skip    int64 // output frames still to drop (trim, negative start)
	started bool
	eof     bool
	decoded int    // packets decoded so far
	codec   string // codec and profile, for error messages
	profile string

	// After a seek: the presentation time of the stream's first packet,
	// which reading did not start at, and how many sample frames, at the
	// decoder's rate, were passed over.
	seeked bool
	first  time.Duration
	passed int64
}

func audioDecoder(m *media, s *videoprobe.Stream) (codec.AudioDecoder, error) {
	ci, err := Available(CodecID(s.Codec), Decode)
	if err != nil {
		return nil, err
	}
	switch ci.ID {
	case "pcm":
		return native.NewPCMDecoder(s.Codec, s.Audio.Channels, s.Audio.SampleRate)
	case "mp3":
		return native.NewMP3Decoder(), nil
	case "flac":
		return native.NewFLACDecoder(m.config(s, "dfLa"))
	}
	cfg := m.config(s, "esds")
	if m.iso != nil {
		if _, cfg, _, _, err = codechdr.ParseESDS(cfg); err != nil {
			return nil, fmt.Errorf("govideo: %s audio configuration: %w", ci.ID, err)
		}
	}
	return oscodec.NewAudioDecoder(oscodec.AudioConfig{Codec: string(ci.ID),
		SampleRate: s.Audio.SampleRate, Channels: s.Audio.Channels, Config: cfg})
}

// audioRunIn is how many packets ahead of a cut the decoding of compressed
// audio starts. A decoder that starts inside a stream has none of the state
// the earlier packets left: an AAC frame overlaps the one before it, and SBR
// needs three packets before its output is what a decoder that read the
// whole stream gives (measured on the system decoder, SPEC §14.1). The
// output of the run-in is dropped.
const audioRunIn = 8

// mp3RunIn is the run-in of MP3, in frames, for a stream's sample rate and
// channels. A frame's audio data may start in the frames before it, up to
// 511 bytes back (255 at the lower rates), so decoding has to start that
// many bytes of audio data ahead: the count is for frames of the lowest bit
// rate there is, which hold the least. Two more frames give the overlap of
// the transform and the history of the synthesis filter. That is 10 frames
// at 44.1 kHz in stereo, 31 at 24 kHz in mono and 87 at 22.05 kHz in stereo.
func mp3RunIn(rate, channels int) int64 {
	// The frame at 32 kbit/s (8 at the lower rates), less its header, a
	// checksum and the side information.
	reach, frame, side := 511, 144*32000/rate, [2]int{17, 32}
	if rate < 32000 {
		reach, frame, side = 255, 72*8000/rate, [2]int{9, 17}
	}
	data := max(frame-4-2-side[min(max(channels, 1), 2)-1], 1)
	return int64((reach+data-1)/data + 2)
}

// pcmRunIn is the run-in of PCM: there is no decoder state, but the
// resampler's filter reaches some tens of samples back (4 ms at 8 kHz).
const pcmRunIn = 20 * time.Millisecond

// openAudio starts reading stream s of m, positioned trim into the stream.
func openAudio(ctx context.Context, m *media, s *videoprobe.Stream, trim time.Duration) (*audioStream, error) {
	dec, err := audioDecoder(m, s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	rd, err := m.reader(ctx, s)
	if err != nil {
		closeDecoder(dec)
		return nil, err
	}
	a := &audioStream{rd: rd, dec: dec, skip: durToFrames(trim), codec: s.Codec, profile: s.Profile}
	a.seek(ctx, m, s, trim)
	return a, nil
}

// seek moves the reader to a run-in before the place trim into the stream,
// if the reader can do that and there is anything to gain. Whatever stops
// it, the stream is left to be decoded from its start.
func (a *audioStream) seek(ctx context.Context, m *media, s *videoprobe.Stream, trim time.Duration) {
	sk, ok := a.rd.(audioSeeker)
	if !ok || s.Audio == nil || s.Audio.SampleRate < 1 {
		return
	}
	pcm := strings.HasPrefix(s.Codec, "pcm_")
	// No packet of compressed audio is shorter than 960 samples at the
	// container's rate (576 of MP3): a cut inside the run-in is not looked
	// at further.
	runIn, shortest := int64(audioRunIn), int64(960)
	var block int64 // FLAC: the sample frames of every packet but the last
	switch s.Codec {
	case "mp3":
		runIn, shortest = mp3RunIn(s.Audio.SampleRate, s.Audio.Channels), 576
	case "flac":
		// A FLAC frame stands alone, so there is no decoder to run in, only
		// the resampler, as for PCM. Its packets are counted, which says
		// where the sound is when they all hold the same number of samples;
		// a stream of blocks of varying size is decoded from its start.
		a, err := codechdr.ParseFLACStreamInfo(m.config(s, "dfLa"))
		if err != nil || a.FrameSize < 1 {
			return
		}
		block = int64(a.FrameSize)
		runIn = (int64(pcmRunIn.Seconds()*float64(s.Audio.SampleRate))+block-1)/block + 1
		shortest = block
	}
	if !pcm && trim < time.Duration(runIn*shortest)*time.Second/time.Duration(s.Audio.SampleRate) {
		return
	}
	// The stream's first packet, from a reader and a decoder of its own,
	// gives what a stream read from its start goes by: the time its samples
	// are counted from, the rate they are decoded at (not the container's,
	// for HE-AAC) and how many a packet holds.
	head, err := m.reader(ctx, s)
	if err != nil {
		return
	}
	p, err := head.next()
	if err != nil {
		return
	}
	dec, err := audioDecoder(m, s)
	if err != nil {
		return
	}
	defer closeDecoder(dec)
	fr, err := dec.Decode(p.data)
	if err != nil || fr.SampleRate < 1 || fr.Channels < 1 || len(fr.Samples) < fr.Channels {
		return
	}
	n := int64((trim - p.pts).Seconds() * float64(fr.SampleRate)) // samples before the cut
	per := int64(1)                                               // samples in what the reader counts
	if pcm {
		n -= int64(pcmRunIn.Seconds() * float64(fr.SampleRate))
	} else {
		// Every packet of the codecs decoded here holds as many samples
		// as the first. One with packets of several lengths (Opus, Vorbis)
		// will need its reader to count samples.
		per = int64(len(fr.Samples) / fr.Channels)
		if block > 0 && per != block {
			return // the only frame of the stream, or not the stream STREAMINFO describes
		}
		if m.mkv != nil {
			for _, t := range m.mkv.Tracks {
				if int64(t.Number) == s.ID {
					runIn = max(runIn, int64(t.SeekPreRoll.Seconds()*float64(fr.SampleRate))/per+1)
				}
			}
		}
		n = n/per - runIn
	}
	if n <= 0 {
		return
	}
	if passed := sk.seekAudio(n); passed > 0 {
		a.seeked, a.first, a.passed = true, p.pts, passed*per
	}
}

// closeDecoder releases a decoder that holds operating-system resources.
func closeDecoder(d any) {
	if c, ok := d.(io.Closer); ok {
		c.Close()
	}
}

func (a *audioStream) close() { closeDecoder(a.dec) }

func durToFrames(d time.Duration) int64 {
	return int64(float64(d)/1e9*outRate + 0.5)
}

// lead accounts for n output frames that precede what is decoded next: they
// are taken from the frames still to drop, and any that are left over are
// silence.
func (a *audioStream) lead(n int64) {
	if n > a.skip {
		a.buf = append(a.buf, make([]float32, 2*(n-a.skip))...)
		a.skip = 0
	} else {
		a.skip -= n
	}
}

// read fills dst (interleaved stereo) and returns the number of frames
// delivered; fewer than requested means the stream has ended.
func (a *audioStream) read(dst []float32) (int, error) {
	for len(a.buf) < len(dst) && !a.eof {
		var out []float32
		p, err := a.rd.next()
		switch {
		case err == io.EOF:
			a.eof = true
			if a.rs != nil {
				out = a.rs.Flush()
			}
		case err != nil:
			return 0, err
		default:
			fr, err := a.dec.Decode(p.data)
			if err != nil && a.decoded < 4 {
				// The decoder opened but fails from the outset: a profile
				// or layout the system codec does not handle.
				cu := &ErrCodecUnavailable{Codec: canonical(CodecID(a.codec)), Op: Decode,
					Reason: "the decoder rejected the stream: " + err.Error()}
				if a.profile != "" {
					cu.Reason = "the decoder does not handle this " + a.profile + " stream: " + err.Error()
				}
				return 0, cu
			}
			if err != nil {
				return 0, err
			}
			a.decoded++
			if !a.started {
				a.started = true
				// The stream's first packet places the samples: a positive
				// first timestamp is leading silence; a negative one
				// (encoder priming) is trimmed.
				first := p.pts
				if a.seeked {
					first = a.first
				}
				if lead := durToFrames(first); first > 0 {
					a.lead(lead)
				} else if first < 0 {
					a.skip += durToFrames(-first)
				}
			}
			if a.rs == nil {
				// The decoder knows the real rate (it can differ from the
				// container's for HE-AAC).
				if fr.SampleRate < 1 || fr.Channels < 1 {
					return 0, errors.New("govideo: audio decoder returned no format")
				}
				a.rs = process.NewResampler(fr.SampleRate, outRate, 2)
				// After a seek the decoder's first sample is not the
				// stream's: the resampler is told which it is, and says
				// which output frame it will start with.
				a.lead(a.rs.StartAt(a.passed))
			}
			out = a.rs.Process(process.ToStereo(fr.Samples, fr.Channels))
		}
		if a.skip > 0 {
			drop := min(a.skip, int64(len(out)/2))
			out = out[drop*2:]
			a.skip -= drop
		}
		a.buf = append(a.buf, out...)
	}
	n := copy(dst, a.buf)
	n -= n % 2
	a.buf = a.buf[:copy(a.buf, a.buf[n:])]
	return n / 2, nil
}

// --- video -------------------------------------------------------------------

// reorderWindow is how many packets ahead of the decoder the stream reads.
// A decoded frame is released once no packet in the window is presented
// before it; H.264 and HEVC bound reordering to 16 frames.
const reorderWindow = 17

// vframe is a source picture: of an intra-only codec, a packet (data) that
// is decoded when the picture is shown (frame); of an inter-frame codec, a
// picture still in the decoder (pic), copied out when it is shown.
type vframe struct {
	pts   time.Duration
	data  []byte
	frame *codec.VideoFrame
	pic   codec.Picture
	// order says whether the frame is a pair of fields. Such a frame is
	// delivered as two vframes, one per field (bob), which share the decoded
	// picture: twin is the second field's, for the first to pass a lazily
	// decoded frame on to. The second field is the later of the two, so it
	// is the one that gives the picture back to the decoder.
	order codechdr.FieldOrder
	bob   bool
	field process.Field
	twin  *vframe
}

// videoStream selects, for a rising sequence of times, the source frame to
// show. Intra-only codecs are decoded lazily, only when a frame is selected.
// Inter-frame codecs are decoded in full, in decode order, and the pictures
// are put back into presentation order here.
//
// An interlaced frame counts as two pictures, its fields, the second half a
// frame after the first (SPEC §7.1). Which of them an output frame shows is
// chosen by time like any other picture, so an output at the field rate
// shows every field and one at the frame rate shows one field of each
// frame.
//
// The decoder of an inter-frame codec is taken from the export's decoders
// when the first frame is asked for and given back by close, so that a
// timeline of many clips has one decoder at work, not one per clip. Its
// pictures stay in the decoder until one is shown: most of what is decoded
// ahead of a cut, and every frame a lower output rate passes over, is
// never copied.
type videoStream struct {
	rd       packetReader
	intra    codec.VideoDecoder       // intra-only codecs
	dec      codec.VideoStreamDecoder // inter-frame codecs; nil until needed
	decs     *videoDecoders           // where dec comes from and goes back to
	cfg      oscodec.VideoConfig      // what dec is opened for
	name     string                   // the file, for error messages
	broken   bool                     // dec has failed on a packet
	closed   bool
	rotation int
	mirror   bool          // flip left to right after rotating
	start    time.Duration // first time that will be asked for
	slack    time.Duration // how far past the asked time a frame may start

	look    []packet             // read but not yet decoded
	held    []codec.TimedPicture // decoded, waiting for their turn
	flushed bool
	eof     bool

	// After a seek, reading starts at a sync sample. When that is not an
	// IDR picture, the packets that follow it but are presented before it
	// (the leading pictures of an open group of pictures) refer to pictures
	// the seek skipped. They are dropped: none is shown, as the sync sample
	// is at or before the start, and a system decoder that is fed them can
	// fail on every frame after.
	seeked   bool          // the next packet is the one a seek landed on
	dropLead bool          // drop the packets presented before leadEnd
	leadEnd  time.Duration // presentation time of the sync sample

	// Deinterlacing. force is the field order to assume for every frame, if
	// the clip names one. Otherwise pics, when set, reads the structure of
	// each H.264 picture on its way into the decoder, and orders holds the
	// field order of the interlaced ones until they come out, by
	// presentation time.
	force    codechdr.FieldOrder
	pics     *codechdr.H264Pictures
	orders   map[time.Duration]codechdr.FieldOrder
	second   *vframe       // the second field of the frame last split
	ahead    *vframe       // the frame after it, read to learn its length
	frameDur time.Duration // nominal frame length; 0 when unknown
	lastDur  time.Duration // length of the frame last split

	cur, peek *vframe
	seq       int64
	shown     *codec.VideoFrame
	shownSeq  int64

	// The frames a shown picture is written to on its way out: as copied
	// from the decoder (raw, holding the picture rawPic), deinterlaced and
	// rotated. Each is reused for the next picture, so a stream does not
	// allocate once it is running.
	raw, bobbed, turned *codec.VideoFrame
	rawPic              codec.Picture
}

// maxIdleDecoders is how many decoders of finished clips an export keeps
// for later clips: enough for a timeline that goes round four kinds of
// source which cannot share one. An idle session costs little memory in
// this process (its picture buffers are let go within seconds), but the
// system has only so many hardware decoders to lend.
const maxIdleDecoders = 4

// videoDecoders hands out the video decoders of one export and takes them
// back, so that a clip can continue on the decoder of an earlier clip of
// the same format instead of setting up its own (SPEC §6). It also passes
// on the frames a finished stream copied its pictures into.
type videoDecoders struct {
	idle    []codec.VideoStreamDecoder // least recently used first
	frames  []*codec.VideoFrame
	checked map[string]bool // formats check has found a decoder for
}

// check reports whether the stream cfg describes can be decoded here, by
// getting a decoder for it, which is then kept for the clip. A format is
// tried once however many clips have it.
func (c *videoDecoders) check(cfg oscodec.VideoConfig) error {
	key := fmt.Sprintf("%s %dx%d %s", cfg.Codec, cfg.Width, cfg.Height, cfg.Record)
	if c.checked[key] {
		return nil
	}
	d, err := c.open(cfg)
	if err != nil {
		return err
	}
	c.put(d)
	if c.checked == nil {
		c.checked = map[string]bool{}
	}
	c.checked[key] = true
	return nil
}

// open returns a decoder for the stream cfg describes: an idle one that can
// take the stream over, else a new one.
func (c *videoDecoders) open(cfg oscodec.VideoConfig) (codec.VideoStreamDecoder, error) {
	for i := len(c.idle) - 1; i >= 0; i-- {
		d := c.idle[i]
		if r, ok := d.(oscodec.Restarter); ok && r.Restart(cfg) {
			c.idle = append(c.idle[:i], c.idle[i+1:]...)
			return d, nil
		}
	}
	return oscodec.NewVideoDecoder(cfg)
}

// put takes back a decoder that is in good order and whose pictures have
// all been released.
func (c *videoDecoders) put(d codec.VideoStreamDecoder) {
	if _, ok := d.(oscodec.Restarter); !ok {
		d.Close()
		return
	}
	c.idle = append(c.idle, d)
	if len(c.idle) > maxIdleDecoders {
		c.idle[0].Close()
		c.idle = append(c.idle[:0], c.idle[1:]...)
	}
}

// frame returns a frame to reuse, or nil.
func (c *videoDecoders) frame() *codec.VideoFrame {
	n := len(c.frames)
	if n == 0 {
		return nil
	}
	f := c.frames[n-1]
	c.frames = c.frames[:n-1]
	return f
}

func (c *videoDecoders) close() {
	for _, d := range c.idle {
		d.Close()
	}
	c.idle, c.frames = nil, nil
}

// openVideo starts reading stream s of m; the first frame asked for will be
// the one shown at start. slack is the tolerance of frame selection (see
// frameAt). A stream that needs a system decoder is checked against one
// here, so that a clip that cannot be decoded fails before anything is
// rendered, but holds none until its first frame is asked for.
func openVideo(ctx context.Context, m *media, s *videoprobe.Stream, start, slack time.Duration, deint DeinterlaceMode, decs *videoDecoders) (*videoStream, error) {
	ci, err := Available(CodecID(s.Codec), Decode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	rd, err := m.reader(ctx, s)
	if err != nil {
		return nil, err
	}
	v := &videoStream{rd: rd, decs: decs, name: m.path,
		rotation: s.Video.Rotation, mirror: s.Video.Mirrored, start: start, slack: slack, shownSeq: -1}
	if r := s.Video.FrameRate.Float(); r > 0 {
		v.frameDur = time.Duration(1e9 / r)
	}
	switch deint {
	case DeinterlaceTopFirst:
		v.force = codechdr.TopFieldFirst
	case DeinterlaceBottomFirst:
		v.force = codechdr.BottomFieldFirst
	case DeinterlaceAuto:
		// The sequence parameter set says whether fields can occur at all;
		// only then is every picture looked at.
		if ci.ID == "h264" && s.Video.Interlaced {
			if v.pics, err = codechdr.NewH264Pictures(m.config(s, "avcC")); err != nil {
				v.pics = nil // no usable configuration: shown as decoded
			}
			v.orders = map[time.Duration]codechdr.FieldOrder{}
		}
	}
	if ci.ID == "mjpeg" {
		v.intra = native.MJPEGDecoder{}
		return v, nil
	}
	box := map[CodecID]string{"h264": "avcC", "hevc": "hvcC"}[ci.ID]
	// The display size, not the coded size: the decoder applies the
	// bitstream's cropping and returns pictures of this size.
	w, h := s.Video.Width, s.Video.Height
	// The matrix comes from the stream's own signalling; the system decoder
	// does not always pass it on with the pictures.
	matrix := map[string]int{"bt709": 1, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "bt2020nc": 9, "bt2020c": 10}[s.Video.ColorMatrix]
	v.cfg = oscodec.VideoConfig{Codec: string(ci.ID), Width: w, Height: h, Record: m.config(s, box), Matrix: matrix}
	if err := decs.check(v.cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	if sk, ok := rd.(syncSeeker); ok && start > 0 {
		sk.seekSync(start)
		v.seeked = true
	}
	return v, nil
}

// close ends the stream: its pictures go back to the decoder, and the
// decoder and the frames go back to the export for the next clip. The frame
// frameAt returned last is no longer valid.
func (v *videoStream) close() {
	if v.closed {
		return
	}
	v.closed = true
	for _, f := range []*vframe{v.cur, v.peek, v.second, v.ahead} {
		v.drop(f)
	}
	for _, f := range v.held {
		f.Picture.Release()
	}
	v.cur, v.peek, v.second, v.ahead, v.held, v.look = nil, nil, nil, nil, nil, nil
	v.shown, v.rawPic = nil, nil
	if v.dec != nil {
		if v.broken {
			v.dec.Close() // not one to hand to the next clip
		} else {
			v.decs.put(v.dec)
		}
		v.dec = nil
	}
	for _, f := range []*codec.VideoFrame{v.raw, v.bobbed, v.turned} {
		if f != nil {
			v.decs.frames = append(v.decs.frames, f)
		}
	}
	v.raw, v.bobbed, v.turned = nil, nil, nil
}

// drop gives the picture of a frame that will not be shown again back to
// the decoder. The first field of a frame leaves that to the second.
func (v *videoStream) drop(f *vframe) {
	if f != nil && f.pic != nil && f.twin == nil {
		f.pic.Release()
		f.pic = nil
	}
}

// decode passes a packet, or with flush the end of the stream, to the
// decoder, which is taken from the export's decoders on first use.
func (v *videoStream) decode(p packet, flush bool) ([]codec.TimedPicture, error) {
	if v.closed {
		return nil, errors.New("govideo: video stream is closed")
	}
	if v.dec == nil {
		dec, err := v.decs.open(v.cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.name, err)
		}
		v.dec = dec
	}
	if flush {
		return v.dec.Flush()
	}
	out, err := v.dec.Decode(p.data, p.pts)
	if err != nil {
		v.broken = true
	}
	return out, err
}

func (v *videoStream) pull() (*packet, error) {
	for {
		if v.eof {
			return nil, nil
		}
		p, err := v.rd.next()
		if err == io.EOF {
			v.eof = true
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if v.seeked {
			// The first packet after a seek is the sync sample.
			v.seeked = false
			if p.sync && p.pts <= v.start {
				v.dropLead, v.leadEnd = true, p.pts
			}
		} else if v.dropLead && p.pts < v.leadEnd {
			continue
		}
		return &p, nil
	}
}

// next returns the following picture in presentation order, or nil at the
// end: a frame, or one field of an interlaced frame.
func (v *videoStream) next() (*vframe, error) {
	if f := v.second; f != nil {
		v.second = nil
		return f, nil
	}
	f := v.ahead
	v.ahead = nil
	if f == nil {
		var err error
		if f, err = v.nextFrame(); f == nil {
			return nil, err
		}
	}
	if f.order != codechdr.TopFieldFirst && f.order != codechdr.BottomFieldFirst {
		return f, nil
	}
	// The second field is shown midway to the next frame, which is read
	// ahead for its time. A much longer wait than a frame is a gap in the
	// stream, not a slower field rate; the last frame has the length of the
	// one before it.
	var err error
	if v.ahead, err = v.nextFrame(); err != nil {
		return nil, err
	}
	dur := v.lastDur
	if v.ahead != nil && v.ahead.pts > f.pts {
		dur = v.ahead.pts - f.pts
	}
	if dur <= 0 || (v.frameDur > 0 && dur > 3*v.frameDur) {
		dur = v.frameDur
	}
	v.lastDur = dur
	second := *f
	second.pts += dur / 2
	f.field, second.field = process.TopField, process.BottomField
	if f.order == codechdr.BottomFieldFirst {
		f.field, second.field = process.BottomField, process.TopField
	}
	f.bob, second.bob = true, true
	f.twin, v.second = &second, &second
	return f, nil
}

// fieldOrder returns the field order of the frame presented at pts, which
// has left the decoder, and forgets the frames before it.
func (v *videoStream) fieldOrder(pts time.Duration) codechdr.FieldOrder {
	if v.force != codechdr.Progressive {
		return v.force
	}
	o := v.orders[pts]
	for t := range v.orders {
		if t <= pts {
			delete(v.orders, t)
		}
	}
	return o
}

// nextFrame returns the following frame in presentation order, or nil at the
// end.
func (v *videoStream) nextFrame() (*vframe, error) {
	if v.intra != nil {
		p, err := v.pull()
		if p == nil {
			return nil, err
		}
		return &vframe{pts: p.pts, data: p.data, order: v.force}, nil
	}
	for {
		for !v.eof && len(v.look) < reorderWindow {
			p, err := v.pull()
			if err != nil {
				return nil, err
			}
			if p != nil {
				v.look = append(v.look, *p)
			}
		}
		if len(v.held) > 0 {
			first := 0
			for i, f := range v.held {
				if f.PTS < v.held[first].PTS {
					first = i
				}
			}
			ready := v.flushed
			if !ready && len(v.look) > 0 {
				ready = true
				for _, p := range v.look {
					if p.pts < v.held[first].PTS {
						ready = false
						break
					}
				}
			}
			if ready {
				f := v.held[first]
				v.held = append(v.held[:first], v.held[first+1:]...)
				return &vframe{pts: f.PTS, pic: f.Picture, order: v.fieldOrder(f.PTS)}, nil
			}
		}
		var (
			frames []codec.TimedPicture
			err    error
		)
		switch {
		case len(v.look) > 0:
			p := v.look[0]
			v.look = v.look[1:]
			if v.pics != nil {
				// Every packet is read, in decode order: parameter sets can
				// change on the way.
				if o := v.pics.Picture(p.data).FieldOrder(); o == codechdr.TopFieldFirst || o == codechdr.BottomFieldFirst {
					v.orders[p.pts] = o
				}
			}
			frames, err = v.decode(p, false)
			if err != nil && p.pts < v.start {
				// Pictures before the start are only decoded as references:
				// one that cannot be decoded is not needed for itself.
				err = nil
			}
		case !v.flushed:
			frames, err = v.decode(packet{}, true)
			v.flushed = true
		default:
			return nil, nil
		}
		v.held = append(v.held, frames...) // even with an error: they are released on close
		if err != nil {
			return nil, err
		}
	}
}

// frameAt returns the frame to show for the output frame that starts at time
// t, and a sequence number that changes whenever the picture does. t must
// not decrease between calls.
//
// The frame chosen is the last one that starts no later than t+slack. With
// slack a little under half an output frame, that is the source frame on
// screen for most of the output frame, and the choice is stable against
// timestamp rounding (Matroska stores whole milliseconds) and against
// sources whose frames sit at a constant offset from the output grid.
//
// The frame belongs to the stream, which writes the next picture over it:
// it is valid until the sequence number changes or the stream is closed.
func (v *videoStream) frameAt(t time.Duration) (*codec.VideoFrame, int64, error) {
	eps := max(v.slack, 50*time.Microsecond)
	var err error
	if v.cur == nil {
		if v.cur, err = v.next(); err != nil {
			return nil, 0, err
		}
		if v.cur == nil {
			return nil, 0, errors.New("govideo: video stream has no frames")
		}
		v.seq = 1
	}
	for {
		if v.peek == nil {
			if v.peek, err = v.next(); err != nil {
				return nil, 0, err
			}
		}
		if v.peek == nil || v.peek.pts > t+eps {
			break
		}
		v.drop(v.cur)
		v.cur, v.peek = v.peek, nil
		v.seq++
	}
	if v.shownSeq != v.seq {
		c := v.cur
		f := c.frame
		switch {
		case c.pic != nil:
			// The two fields of a frame are shown from one copy of it.
			if c.pic != v.rawPic {
				if v.raw == nil {
					v.raw = v.decs.frame()
				}
				v.rawPic = nil
				if v.raw, err = c.pic.Frame(v.raw); err != nil {
					return nil, 0, err
				}
				v.rawPic = c.pic
			}
			f = v.raw
		case f == nil:
			if f, err = v.intra.Decode(c.data); err != nil {
				return nil, 0, err
			}
			if c.twin != nil {
				c.twin.frame = f
			}
		}
		if c.bob {
			if v.bobbed == nil {
				v.bobbed = v.decs.frame()
			}
			v.bobbed = process.BobInto(v.bobbed, f, c.field)
			f = v.bobbed
		}
		if v.turned == nil && v.rotation%360 != 0 {
			v.turned = v.decs.frame()
		}
		if r := process.RotateInto(v.turned, f, v.rotation); r != f {
			v.turned, f = r, r
		}
		if v.mirror {
			if f == v.raw {
				v.rawPic = nil // no longer the picture as it was decoded
			}
			f = process.FlipH(f) // the frame is this stream's own
		}
		v.shown, v.shownSeq = f, v.seq
	}
	return v.shown, v.seq, nil
}

// decodeStill loads a still image as a frame, honouring EXIF orientation.
// Transparent parts of the image show bg (nil is black).
func decodeStill(path string, bg color.Color) (*codec.VideoFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		var uf *videoprobe.UnsupportedFormatError
		if _, perr := videoprobe.Probe(context.Background(), bytes.NewReader(data), int64(len(data))); errors.As(perr, &uf) && uf.Format == "heif" {
			return nil, fmt.Errorf("govideo: %s: %w", path, &ErrCodecUnavailable{Codec: "heic", Op: Decode,
				Reason: "HEIC and AVIF stills need the operating system's image decoder, which is not connected yet"})
		}
		return nil, fmt.Errorf("govideo: %s: %w", path, err)
	}
	// JPEG, PNG and WebP can each say which way up the picture goes.
	orient := process.EXIFOrientation(bytes.NewReader(data))
	if format == "webp" {
		// Lossy WebP is limited-range video Y'CbCr, not JPEG's full range.
		switch m := img.(type) {
		case *image.YCbCr:
			return process.OrientFrame(process.FromVideoYCbCr(m), orient), nil
		case *image.NYCbCrA:
			return process.OrientFrame(process.FromVideoNYCbCrA(m, bg), orient), nil
		}
	}
	return process.FromImageOver(process.Orient(img, orient), bg), nil
}
