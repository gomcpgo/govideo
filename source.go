package govideo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // still image formats
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/codec/native"
	"github.com/gomcpgo/govideo/codec/oscodec"
	"github.com/gomcpgo/govideo/internal/codechdr"
	"github.com/gomcpgo/govideo/internal/container/avi"
	"github.com/gomcpgo/govideo/internal/container/isobmff"
	"github.com/gomcpgo/govideo/internal/container/matroska"
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
			if id, ok := map[string]CodecID{"mp3": "mp3", "flac": "flac", "ac3": "ac3"}[uf.Format]; ok {
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
			r := &isoReader{f: m.iso, tr: t, it: t.Iter(), ts: int64(t.Timescale), pcm: pcmFrame > 0}
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
				return &mkvReader{f: m.mkv, ctx: ctx, pr: m.mkv.NewReaderContext(ctx), track: t.Number, delay: t.CodecDelay}, nil
			}
		}
	case m.wv != nil:
		return &wavReader{f: m.f, w: m.wv}, nil
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
// its MP4 sample entry, or the Matroska CodecPrivate.
func (m *media) config(s *videoprobe.Stream, box string) []byte {
	switch {
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

func (r *isoReader) seekSync(t time.Duration) {
	it := r.tr.Iter()
	skip := 0
	for i := 0; ; i++ {
		s, err := it.Next()
		if err != nil || r.time(s.DTS) > t {
			break
		}
		if s.Sync && r.time(s.PTS()) <= t {
			skip = i
		}
	}
	for ; skip > 0; skip-- {
		if _, err := r.it.Next(); err != nil {
			return
		}
	}
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
	// needKey drops the track's packets up to its first keyframe: after a
	// seek, those that open the Cluster belong to the previous group of
	// pictures.
	needKey bool
}

func (r *mkvReader) next() (packet, error) {
	for {
		p, err := r.pr.Next()
		if err != nil {
			return packet{}, err
		}
		if p.Track != r.track || r.needKey && !p.Keyframe {
			continue
		}
		r.needKey = false
		return packet{pts: p.Timestamp - r.delay, data: p.Data, sync: p.Keyframe}, nil
	}
}

func (r *mkvReader) seekSync(t time.Duration) {
	if pr, ok := r.f.NewReaderAt(r.ctx, r.track, t+r.delay); ok {
		r.pr, r.needKey = pr, true
	}
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

// --- audio -------------------------------------------------------------------

// audioStream delivers a source audio stream as 48 kHz stereo float32.
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
}

func audioDecoder(m *media, s *videoprobe.Stream) (codec.AudioDecoder, error) {
	ci, err := Available(CodecID(s.Codec), Decode)
	if err != nil {
		return nil, err
	}
	if ci.ID == "pcm" {
		return native.NewPCMDecoder(s.Codec, s.Audio.Channels, s.Audio.SampleRate)
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
	return &audioStream{rd: rd, dec: dec, skip: durToFrames(trim), codec: s.Codec, profile: s.Profile}, nil
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
				// A positive first timestamp is leading silence; a negative
				// one (encoder priming) is trimmed.
				if lead := durToFrames(p.pts); p.pts > 0 {
					if lead > a.skip {
						a.buf = append(a.buf, make([]float32, 2*(lead-a.skip))...)
						a.skip = 0
					} else {
						a.skip -= lead
					}
				} else if p.pts < 0 {
					a.skip += durToFrames(-p.pts)
				}
			}
			if a.rs == nil {
				// The decoder knows the real rate (it can differ from the
				// container's for HE-AAC).
				if fr.SampleRate < 1 || fr.Channels < 1 {
					return 0, errors.New("govideo: audio decoder returned no format")
				}
				a.rs = process.NewResampler(fr.SampleRate, outRate, 2)
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

// vframe is a source picture, decoded (frame) or not yet (data).
type vframe struct {
	pts   time.Duration
	data  []byte
	frame *codec.VideoFrame
}

// videoStream selects, for a rising sequence of times, the source frame to
// show. Intra-only codecs are decoded lazily, only when a frame is selected.
// Inter-frame codecs are decoded in full, in decode order, and the pictures
// are put back into presentation order here.
type videoStream struct {
	rd       packetReader
	intra    codec.VideoDecoder       // intra-only codecs
	dec      codec.VideoStreamDecoder // inter-frame codecs
	rotation int
	mirror   bool          // flip left to right after rotating
	start    time.Duration // first time that will be asked for
	slack    time.Duration // how far past the asked time a frame may start

	look    []packet           // read but not yet decoded
	held    []codec.TimedFrame // decoded, waiting for their turn
	flushed bool
	eof     bool

	cur, peek *vframe
	seq       int64
	shown     *codec.VideoFrame
	shownSeq  int64
}

// openVideo starts reading stream s of m; the first frame asked for will be
// the one shown at start. slack is the tolerance of frame selection (see
// frameAt).
func openVideo(ctx context.Context, m *media, s *videoprobe.Stream, start, slack time.Duration) (*videoStream, error) {
	ci, err := Available(CodecID(s.Codec), Decode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	rd, err := m.reader(ctx, s)
	if err != nil {
		return nil, err
	}
	v := &videoStream{rd: rd, rotation: s.Video.Rotation, mirror: s.Video.Mirrored, start: start, slack: slack, shownSeq: -1}
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
	v.dec, err = oscodec.NewVideoDecoder(oscodec.VideoConfig{Codec: string(ci.ID), Width: w, Height: h,
		Record: m.config(s, box), Matrix: matrix})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	if sk, ok := rd.(syncSeeker); ok && start > 0 {
		sk.seekSync(start)
	}
	return v, nil
}

func (v *videoStream) close() {
	if v.dec != nil {
		v.dec.Close()
	}
}

func (v *videoStream) pull() (*packet, error) {
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
	return &p, nil
}

// next returns the following frame in presentation order, or nil at the end.
func (v *videoStream) next() (*vframe, error) {
	if v.intra != nil {
		p, err := v.pull()
		if p == nil {
			return nil, err
		}
		return &vframe{pts: p.pts, data: p.data}, nil
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
				return &vframe{pts: f.PTS, frame: f.Frame}, nil
			}
		}
		var (
			frames []codec.TimedFrame
			err    error
		)
		switch {
		case len(v.look) > 0:
			p := v.look[0]
			v.look = v.look[1:]
			frames, err = v.dec.Decode(p.data, p.pts)
			if err != nil && p.pts < v.start {
				// Pictures before the start are only decoded as references.
				// After a seek some of them refer to the previous group of
				// pictures and cannot be decoded; they are not needed.
				err = nil
			}
		case !v.flushed:
			frames, err = v.dec.Flush()
			v.flushed = true
		default:
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		v.held = append(v.held, frames...)
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
		v.cur, v.peek = v.peek, nil
		v.seq++
	}
	if v.shownSeq != v.seq {
		f := v.cur.frame
		if f == nil {
			if f, err = v.intra.Decode(v.cur.data); err != nil {
				return nil, 0, err
			}
		}
		f = process.Rotate(f, v.rotation)
		if v.mirror {
			f = process.FlipH(f) // the frame is this stream's own
		}
		v.shown, v.shownSeq = f, v.seq
	}
	return v.shown, v.seq, nil
}

// decodeStill loads a still image as a frame, honouring EXIF orientation.
func decodeStill(path string) (*codec.VideoFrame, error) {
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
	if yuv, ok := img.(*image.YCbCr); ok && format == "webp" {
		// Lossy WebP is limited-range video Y'CbCr, not JPEG's full range.
		return process.FromVideoYCbCr(yuv), nil
	}
	orient := process.EXIFOrientation(bytes.NewReader(data))
	return process.FromImage(process.Orient(img, orient)), nil
}
