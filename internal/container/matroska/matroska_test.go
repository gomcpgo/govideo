package matroska

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"
)

// --- tiny EBML writer for hand-built fixtures -------------------------------

func idBytes(id uint32) []byte {
	switch {
	case id > 0xFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFF:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

func sizeBytes(n int) []byte {
	if n < 127 {
		return []byte{0x80 | byte(n)}
	}
	return []byte{0x08, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func el(id uint32, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	return append(append(idBytes(id), sizeBytes(len(p))...), p...)
}

// elUnknown writes an element with the 1-byte unknown-size marker.
func elUnknown(id uint32, payload ...[]byte) []byte {
	return append(append(idBytes(id), 0xFF), bytes.Join(payload, nil)...)
}

func u(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func f64(v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return b
}

func simpleBlock(track byte, rel int16, flags byte, payload []byte) []byte {
	return el(idSimpleBlock, []byte{0x80 | track, byte(rel >> 8), byte(rel), flags}, payload)
}

func header(docType string) []byte {
	return el(idEBML, el(idDocType, []byte(docType)))
}

func videoTrack(num byte) []byte {
	return el(idTrackEntry,
		el(idTrackNumber, u(uint64(num))), el(idTrackUID, u(77)), el(idTrackType, u(1)),
		el(idCodecID, []byte("V_VP8")),
		el(idVideo, el(idPixelWidth, u(320)), el(idPixelHeight, u(240)),
			el(idColour, el(idMatrixCoefficients, u(1)), el(idRange, u(1)))))
}

func audioTrack(num byte) []byte {
	return el(idTrackEntry,
		el(idTrackNumber, u(uint64(num))), el(idTrackType, u(2)),
		el(idCodecID, []byte("A_OPUS")), el(idCodecPrivate, []byte("OpusHead")),
		el(idLanguage, []byte("fra")), el(idFlagDefault, u(0)),
		el(idDefaultDuration, u(20_000_000)),
		el(idAudio, el(idSamplingFrequency, f64(48000)), el(idChannels, u(2))))
}

func open(t testing.TB, data []byte) *File {
	t.Helper()
	f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func packets(t testing.TB, f *File) []*Packet {
	t.Helper()
	var out []*Packet
	pr := f.NewReader()
	for {
		p, err := pr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, p)
	}
}

// --- unit tests -------------------------------------------------------------

func TestVint(t *testing.T) {
	cases := []struct {
		in      []byte
		v       uint64
		n       int
		unknown bool
		ok      bool
	}{
		{[]byte{0x81}, 1, 1, false, true},
		{[]byte{0xFF}, 127, 1, true, true},
		{[]byte{0x40, 0x7F}, 127, 2, false, true},
		{[]byte{0x7F, 0xFF}, 0x3FFF, 2, true, true},
		{[]byte{0x01, 0, 0, 0, 0, 0, 1, 0}, 256, 8, false, true},
		{[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, 1<<56 - 1, 8, true, true},
		{[]byte{0x00, 1, 2, 3, 4, 5, 6, 7, 8}, 0, 0, false, false},
		{[]byte{0x40}, 0, 0, false, false},
		{nil, 0, 0, false, false},
	}
	for _, c := range cases {
		v, n, unk, ok := vint(c.in)
		if v != c.v || n != c.n || unk != c.unknown || ok != c.ok {
			t.Errorf("vint(%x) = %d,%d,%v,%v want %d,%d,%v,%v", c.in, v, n, unk, ok, c.v, c.n, c.unknown, c.ok)
		}
	}
}

func TestSniff(t *testing.T) {
	if !Sniff(header("webm")) || Sniff([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p'}) || Sniff(nil) {
		t.Fatal("Sniff")
	}
	if _, err := Open(context.Background(), bytes.NewReader([]byte("not matroska")), 12); err != ErrNotMatroska {
		t.Fatalf("err = %v", err)
	}
}

func TestBasic(t *testing.T) {
	data := bytes.Join([][]byte{
		header("webm"),
		el(idSegment,
			el(idVoid, make([]byte, 10)),
			el(idInfo, el(idTimestampScale, u(1_000_000)), el(idDuration, f64(120)),
				el(idTitle, []byte("hello")), el(idMuxingApp, []byte("test\x00\x00")),
				el(idDateUTC, make([]byte, 8))),
			el(idTracks, el(idCRC32, []byte{1, 2, 3, 4}), videoTrack(1), audioTrack(2)),
			el(idCluster, el(idTimestamp, u(0)),
				simpleBlock(1, 0, 0x80, []byte("key0")),
				simpleBlock(2, 0, 0x80, []byte("a0")),
				simpleBlock(1, 40, 0x00, []byte("dlt1")),
				el(idBlockGroup,
					el(idBlock, []byte{0x82, 0, 20, 0}, []byte("a1")),
					el(idBlockDuration, u(15))),
			),
			el(idCluster, el(idTimestamp, u(80)),
				el(idBlockGroup,
					el(idReferenceBlock, []byte{0xD8}),
					el(idBlock, []byte{0x81, 0, 0, 0}, []byte("dlt2"))),
			),
			el(idTags,
				el(idTag, el(idTargets), el(idSimpleTag, el(idTagName, []byte("ENCODER")), el(idTagString, []byte("x")))),
				el(idTag, el(idTargets, el(idTagTrackUID, u(77))),
					el(idSimpleTag, el(idTagName, []byte("DURATION")), el(idTagString, []byte("00:00:00.120"))))),
		),
	}, nil)
	f := open(t, data)
	if f.DocType != "webm" || f.Title != "hello" || f.MuxingApp != "test" {
		t.Errorf("info: %+v", f)
	}
	if f.Duration != 120*time.Millisecond || f.DurationFromScan {
		t.Errorf("duration %v scan=%v", f.Duration, f.DurationFromScan)
	}
	if !f.DateUTC.Equal(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date %v", f.DateUTC)
	}
	if f.Tags["encoder"] != "x" || len(f.Tags) != 1 {
		t.Errorf("tags %v", f.Tags)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("tracks %d", len(f.Tracks))
	}
	v, a := f.Tracks[0], f.Tracks[1]
	if v.Type != TrackVideo || v.CodecID != "V_VP8" || v.PixelWidth != 320 || v.DisplayHeight != 240 ||
		v.Language != "eng" || !v.Default || !v.Enabled || v.Colour == nil ||
		v.Colour.MatrixCoefficients != 1 || v.Colour.Range != 1 || v.Colour.Primaries != 2 {
		t.Errorf("video track %+v colour %+v", v, v.Colour)
	}
	if v.Frames != 3 || v.TotalBytes != 12 || v.LastTimestamp != 80*time.Millisecond ||
		string(v.FirstFrame) != "key0" || v.VFR || v.Tags["duration"] == "" {
		t.Errorf("video stats %+v", v)
	}
	if a.Type != TrackAudio || a.CodecID != "A_OPUS" || string(a.CodecPrivate) != "OpusHead" ||
		a.Language != "fra" || a.Default || a.SamplingFrequency != 48000 ||
		a.OutputSamplingFrequency != 48000 || a.Channels != 2 || a.Frames != 2 ||
		a.DefaultDuration != 20*time.Millisecond {
		t.Errorf("audio track %+v", a)
	}
	ps := packets(t, f)
	if len(ps) != 5 {
		t.Fatalf("packets %d", len(ps))
	}
	want := []struct {
		track uint64
		ts    time.Duration
		dur   time.Duration
		key   bool
		data  string
	}{
		{1, 0, 0, true, "key0"},
		{2, 0, 20 * time.Millisecond, true, "a0"},
		{1, 40 * time.Millisecond, 0, false, "dlt1"},
		{2, 20 * time.Millisecond, 15 * time.Millisecond, true, "a1"},
		{1, 80 * time.Millisecond, 0, false, "dlt2"},
	}
	for i, w := range want {
		p := ps[i]
		if p.Track != w.track || p.Timestamp != w.ts || p.Duration != w.dur || p.Keyframe != w.key || string(p.Data) != w.data {
			t.Errorf("packet %d = %+v want %+v", i, p, w)
		}
	}
}

// Live WebM: unknown-size Segment and Clusters, no Duration, no Cues.
func liveFile() []byte {
	return bytes.Join([][]byte{
		header("webm"),
		elUnknown(idSegment,
			el(idInfo, el(idTimestampScale, u(1_000_000))),
			el(idTracks, videoTrack(1)),
			elUnknown(idCluster, el(idTimestamp, u(0)),
				simpleBlock(1, 0, 0x80, []byte("f0")),
				simpleBlock(1, 33, 0, []byte("f1")),
				simpleBlock(1, 67, 0, []byte("f2"))),
			elUnknown(idCluster, el(idTimestamp, u(100)),
				simpleBlock(1, 0, 0x80, []byte("f3")),
				simpleBlock(1, 33, 0, []byte("f4"))),
		),
	}, nil)
}

func TestUnknownSizes(t *testing.T) {
	data := liveFile()
	f := open(t, data)
	v := f.Tracks[0]
	if v.Frames != 5 || v.VFR {
		t.Errorf("frames %d vfr %v", v.Frames, v.VFR)
	}
	// last block at 133 ms + average frame duration (133/4 = 33 ticks)
	if !f.DurationFromScan || f.Duration != 166*time.Millisecond {
		t.Errorf("duration %v scan=%v", f.Duration, f.DurationFromScan)
	}
	if ps := packets(t, f); len(ps) != 5 || ps[3].Timestamp != 100*time.Millisecond || !ps[3].Keyframe {
		t.Errorf("packets %d", len(ps))
	}
}

func TestTruncation(t *testing.T) {
	data := liveFile()
	full := open(t, data).Tracks[0].Frames
	for n := len(data) - 1; n > 0; n-- {
		f, err := Open(context.Background(), bytes.NewReader(data[:n]), int64(n))
		if err != nil {
			continue // header itself cut off
		}
		var frames int64
		for _, tr := range f.Tracks {
			frames += tr.Frames
		}
		if frames > full {
			t.Fatalf("cut %d: %d frames", n, frames)
		}
		if got := int64(len(packets(t, f))); got != frames {
			t.Fatalf("cut %d: %d packets, %d frames", n, got, frames)
		}
	}
	// Cutting the last byte loses exactly the last block.
	f := open(t, data[:len(data)-1])
	if f.Tracks[0].Frames != full-1 {
		t.Errorf("frames %d", f.Tracks[0].Frames)
	}
	// A reader lying about its size must not break anything either.
	f2, err := Open(context.Background(), bytes.NewReader(data), int64(len(data))+1000)
	if err != nil || f2.Tracks[0].Frames != full {
		t.Errorf("oversize: %v", err)
	}
}

func TestLacing(t *testing.T) {
	xiph := append([]byte{2, 255, 5, 3}, bytes.Repeat([]byte{'a'}, 260)...) // 260, 3, rest
	xiph = append(xiph, []byte("bbbcc")...)
	ebml := []byte{2, 0x84, 0xBE} // sizes 4, 4-1=3, rest
	ebml = append(ebml, []byte("aaaabbbcc")...)
	fixed := append([]byte{2}, []byte("aabbcc")...)
	data := bytes.Join([][]byte{
		header("matroska"),
		el(idSegment,
			el(idTracks, audioTrack(2)),
			el(idCluster, el(idTimestamp, u(1000)),
				simpleBlock(2, 0, 0x80|laceXiph<<1, xiph),
				simpleBlock(2, 60, 0x80|laceEBML<<1, ebml),
				simpleBlock(2, 120, 0x80|laceFixed<<1, fixed),
				simpleBlock(2, 180, 0x80|laceFixed<<1, []byte{2, 1, 2, 3, 4}), // invalid: dropped
			)),
	}, nil)
	f := open(t, data)
	a := f.Tracks[0]
	if a.Frames != 12 || len(a.FirstFrame) != 64 || a.FirstFrame[0] != 'a' {
		t.Errorf("frames %d first %q", a.Frames, a.FirstFrame)
	}
	ps := packets(t, f)
	if len(ps) != 9 {
		t.Fatalf("packets %d", len(ps))
	}
	wantLen := []int{260, 3, 2, 4, 3, 2, 2, 2, 2}
	for i, p := range ps {
		if len(p.Data) != wantLen[i] {
			t.Errorf("packet %d len %d want %d", i, len(p.Data), wantLen[i])
		}
		// laced frames advance by DefaultDuration (20 ms)
		want := time.Second + time.Duration(i/3)*60*time.Millisecond + time.Duration(i%3)*20*time.Millisecond
		if p.Timestamp != want {
			t.Errorf("packet %d ts %v want %v", i, p.Timestamp, want)
		}
	}
	if string(ps[4].Data) != "bbb" || string(ps[8].Data) != "cc" {
		t.Errorf("data %q %q", ps[4].Data, ps[8].Data)
	}
}

func TestContentEncoding(t *testing.T) {
	enc := func(parts ...[]byte) []byte {
		return el(idContentEncs, el(idContentEncoding, parts...))
	}
	track := func(num byte, extra []byte) []byte {
		return el(idTrackEntry, el(idTrackNumber, u(uint64(num))), el(idTrackType, u(2)),
			el(idCodecID, []byte("A_MPEG/L3")), extra)
	}
	data := bytes.Join([][]byte{
		header("matroska"),
		el(idSegment,
			el(idTracks,
				track(1, enc(el(idContentCompression, el(idContentCompAlgo, u(3)), el(idContentCompSettings, []byte{0xFF, 0xFB})))),
				track(2, enc(el(idContentEncodingType, u(1)))),
				track(3, enc(el(idContentEncodingScope, u(2)), el(idContentEncodingType, u(1))))),
			el(idCluster, el(idTimestamp, u(0)),
				simpleBlock(1, 0, 0x80, []byte("xy")),
				simpleBlock(2, 0, 0x80, []byte("secret")),
				simpleBlock(3, 0, 0x80, []byte("plain")))),
	}, nil)
	f := open(t, data)
	if f.Tracks[0].Err != nil || f.Tracks[1].Err == nil || f.Tracks[2].Err != nil {
		t.Fatalf("errs %v %v %v", f.Tracks[0].Err, f.Tracks[1].Err, f.Tracks[2].Err)
	}
	if !bytes.Equal(f.Tracks[0].FirstFrame, []byte{0xFF, 0xFB, 'x', 'y'}) {
		t.Errorf("first frame %x", f.Tracks[0].FirstFrame)
	}
	ps := packets(t, f)
	if len(ps) != 2 || !bytes.Equal(ps[0].Data, []byte{0xFF, 0xFB, 'x', 'y'}) || string(ps[1].Data) != "plain" {
		t.Errorf("packets %+v", ps)
	}
}

func TestVFR(t *testing.T) {
	var cfr, cfrNTSC, vfr, drop []int64
	for i := 0; i < 300; i++ {
		cfr = append(cfr, int64(math.Round(float64(i)*1000/60)))
		cfrNTSC = append(cfrNTSC, int64(math.Round(float64(i)*1001/30)))
		vfr = append(vfr, int64(i*33+(i%7)*3))
		if i != 150 {
			drop = append(drop, int64(i*40))
		}
	}
	// decode order (B-frames) must not matter
	cfr[10], cfr[12] = cfr[12], cfr[10]
	if isVFR(cfr) || isVFR(cfrNTSC) || !isVFR(vfr) || !isVFR(drop) || isVFR(cfr[:2]) {
		t.Errorf("isVFR: %v %v %v %v", isVFR(cfr), isVFR(cfrNTSC), isVFR(vfr), isVFR(drop))
	}
}

func TestContextCancel(t *testing.T) {
	var blocks [][]byte
	blocks = append(blocks, el(idTimestamp, u(0)))
	for i := 0; i < 5000; i++ {
		blocks = append(blocks, simpleBlock(1, int16(i), 0x80, []byte{1}))
	}
	data := append(header("webm"), el(idSegment, el(idTracks, videoTrack(1)), el(idCluster, blocks...))...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, bytes.NewReader(data), int64(len(data))); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if f := open(t, data); f.Tracks[0].Frames != 5000 {
		t.Fatalf("frames %d", f.Tracks[0].Frames)
	}
}

// countingReader records how many bytes Open reads.
type countingReader struct {
	r io.ReaderAt
	n int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

func TestOpenSkipsPayloads(t *testing.T) {
	var clusters [][]byte
	payload := make([]byte, 100_000)
	for i := 0; i < 50; i++ {
		clusters = append(clusters, el(idCluster, el(idTimestamp, u(uint64(i*40))), simpleBlock(1, 0, 0x80, payload)))
	}
	data := append(header("webm"), el(idSegment, append([][]byte{el(idTracks, videoTrack(1))}, clusters...)...)...)
	cr := &countingReader{r: bytes.NewReader(data)}
	f, err := Open(context.Background(), cr, int64(len(data)))
	if err != nil || f.Tracks[0].Frames != 50 {
		t.Fatalf("open: %v", err)
	}
	if cr.n > int64(len(data))/20 {
		t.Errorf("Open read %d of %d bytes", cr.n, len(data))
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(liveFile())
	f.Add(bytes.Join([][]byte{header("matroska"), el(idSegment,
		el(idInfo, el(idDuration, f64(10))),
		el(idTracks, videoTrack(1), audioTrack(2)),
		el(idCluster, el(idTimestamp, u(5)),
			simpleBlock(2, 0, 0x80|laceXiph<<1, []byte{1, 2, 'a', 'b', 'c'}),
			simpleBlock(2, 0, 0x80|laceEBML<<1, []byte{2, 0x82, 0xBF, 'a', 'b', 'c', 'd', 'e'}),
			el(idBlockGroup, el(idBlock, []byte{0x81, 0, 1, 0, 9}), el(idBlockDuration, u(3)))),
		el(idTags, el(idTag, el(idSimpleTag, el(idTagName, []byte("A")), el(idTagString, []byte("b"))))),
	)}, nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		mf, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		pr := mf.NewReader()
		for i := 0; i < 100000; i++ {
			if _, err := pr.Next(); err == io.EOF {
				break
			}
		}
	})
}
