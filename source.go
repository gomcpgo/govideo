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
		return fail(err)
	}
	switch m.info.Format {
	case "mp4", "mov":
		m.iso, err = isobmff.Open(ctx, f, st.Size())
	case "matroska", "webm":
		m.mkv, err = matroska.Open(ctx, f, st.Size())
	case "wav":
		m.wv, err = wav.Open(ctx, f, st.Size())
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
			r := &isoReader{f: m.iso, it: t.Iter(), ts: int64(t.Timescale), pcm: pcmFrame > 0}
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
			return r, nil
		}
	case m.mkv != nil:
		for _, t := range m.mkv.Tracks {
			if int64(t.Number) == s.ID {
				if t.Err != nil {
					return nil, t.Err
				}
				return &mkvReader{pr: m.mkv.NewReaderContext(ctx), track: t.Number}, nil
			}
		}
	case m.wv != nil:
		return &wavReader{f: m.f, w: m.wv}, nil
	}
	return nil, fmt.Errorf("govideo: %s: stream %d not found", m.path, s.Index)
}

type isoReader struct {
	f          *isobmff.File
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
	if err != nil {
		return packet{}, err
	}
	pts := s.PTS() - r.mediaStart
	return packet{pts: r.shift + time.Duration(float64(pts)/float64(r.ts)*1e9), data: data}, nil
}

type mkvReader struct {
	pr    *matroska.PacketReader
	track uint64
}

func (r *mkvReader) next() (packet, error) {
	for {
		p, err := r.pr.Next()
		if err != nil {
			return packet{}, err
		}
		if p.Track == r.track {
			return packet{pts: p.Timestamp, data: p.Data}, nil
		}
	}
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
	rs      *process.Resampler
	buf     []float32
	skip    int64 // output frames still to drop (trim, negative start)
	started bool
	eof     bool
}

func audioDecoder(s *videoprobe.Stream) (codec.AudioDecoder, error) {
	if _, err := Available(CodecID(s.Codec), Decode); err != nil {
		return nil, err
	}
	return native.NewPCMDecoder(s.Codec, s.Audio.Channels, s.Audio.SampleRate)
}

// openAudio starts reading stream s of m, positioned trim into the stream.
func openAudio(ctx context.Context, m *media, s *videoprobe.Stream, trim time.Duration) (*audioStream, error) {
	dec, err := audioDecoder(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	rd, err := m.reader(ctx, s)
	if err != nil {
		return nil, err
	}
	return &audioStream{
		rd: rd, dec: dec, skip: durToFrames(trim),
		rs: process.NewResampler(s.Audio.SampleRate, outRate, 2),
	}, nil
}

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
			out = a.rs.Flush()
		case err != nil:
			return 0, err
		default:
			fr, err := a.dec.Decode(p.data)
			if err != nil {
				return 0, err
			}
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

// videoStream selects, for a rising sequence of times, the source frame to
// show. Phase 0 decodes intra-only codecs, so packets are decoded lazily and
// only when selected; inter-frame codecs will need a decode-order path with
// reordering here.
type videoStream struct {
	rd        packetReader
	dec       codec.VideoDecoder
	rotation  int
	cur, peek *packet
	seq       int64
	eof       bool
	frame     *codec.VideoFrame
	frameSeq  int64
}

func openVideo(ctx context.Context, m *media, s *videoprobe.Stream) (*videoStream, error) {
	if _, err := Available(CodecID(s.Codec), Decode); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	rd, err := m.reader(ctx, s)
	if err != nil {
		return nil, err
	}
	return &videoStream{rd: rd, dec: native.MJPEGDecoder{}, rotation: s.Video.Rotation, frameSeq: -1}, nil
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

// frameAt returns the frame presented at time t and a sequence number that
// changes whenever the picture does. t must not decrease between calls.
func (v *videoStream) frameAt(t time.Duration) (*codec.VideoFrame, int64, error) {
	const eps = 50 * time.Microsecond
	var err error
	if v.cur == nil {
		if v.cur, err = v.pull(); err != nil {
			return nil, 0, err
		}
		if v.cur == nil {
			return nil, 0, errors.New("govideo: video stream has no frames")
		}
		v.seq = 1
	}
	for {
		if v.peek == nil {
			if v.peek, err = v.pull(); err != nil {
				return nil, 0, err
			}
		}
		if v.peek == nil || v.peek.pts > t+eps {
			break
		}
		v.cur, v.peek = v.peek, nil
		v.seq++
	}
	if v.frameSeq != v.seq {
		f, err := v.dec.Decode(v.cur.data)
		if err != nil {
			return nil, 0, err
		}
		v.frame, v.frameSeq = process.Rotate(f, v.rotation), v.seq
	}
	return v.frame, v.seq, nil
}

// decodeStill loads a still image as a frame, honouring EXIF orientation.
func decodeStill(path string) (*codec.VideoFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("govideo: %s: %w", path, err)
	}
	orient := process.EXIFOrientation(bytes.NewReader(data))
	return process.FromImage(process.Orient(img, orient)), nil
}
