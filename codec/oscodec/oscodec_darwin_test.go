//go:build darwin

package oscodec

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gomcpgo/govideo/codec"
)

// gradientFrame is a frame whose luma is a moving diagonal ramp, tagged with
// its number in the top-left pixel block.
func gradientFrame(w, h, n int) *codec.VideoFrame {
	f := codec.NewVideoFrame(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f.Y[y*f.YStride+x] = byte(16 + (x+y+4*n)%200)
		}
	}
	return f
}

// timedFrame is a decoded picture copied out of the decoder.
type timedFrame struct {
	Frame *codec.VideoFrame
	PTS   time.Duration
}

// frames copies pictures out of the decoder and releases them.
func frames(t *testing.T, pics []codec.TimedPicture) (out []timedFrame) {
	t.Helper()
	for _, p := range pics {
		f, err := p.Picture.Frame(nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Picture.Release()
		out = append(out, timedFrame{f, p.PTS})
	}
	return out
}

// lumaPSNR returns the PSNR of a frame's luma plane against want's.
func lumaPSNR(f, want *codec.VideoFrame) float64 {
	var sum float64
	for j := range want.Y {
		d := float64(want.Y[j]) - float64(f.Y[j])
		sum += d * d
	}
	return 10 * math.Log10(255*255/(sum/float64(len(want.Y))+1e-9))
}

// TestH264RoundTrip encodes frames with VideoToolbox and decodes them again,
// without any container in between.
func TestH264RoundTrip(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h, count = 320, 240, 40
	enc, err := NewH264Encoder(EncodeConfig{Width: w, Height: h, FrameNum: 30, FrameDen: 1, Bitrate: 2_000_000, KeyInterval: 15})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var packets []codec.EncodedFrame
	for n := 0; n < count; n++ {
		out, err := enc.Encode(gradientFrame(w, h, n))
		if err != nil {
			t.Fatalf("encode frame %d: %v", n, err)
		}
		packets = append(packets, out...)
	}
	out, err := enc.Flush()
	if err != nil {
		t.Fatal(err)
	}
	packets = append(packets, out...)
	if len(packets) != count {
		t.Fatalf("%d packets for %d frames", len(packets), count)
	}
	cfg := enc.Config()
	if len(cfg) < 8 || cfg[0] != 1 || cfg[1] != 100 {
		t.Fatalf("configuration record % x is not High profile avcC", cfg)
	}
	keys := 0
	for i, p := range packets {
		if p.Index != int64(i) {
			t.Fatalf("packet %d carries frame %d: output is not in presentation order", i, p.Index)
		}
		if p.Sync {
			keys++
		}
	}
	if !packets[0].Sync || keys < 3 || keys == count {
		t.Errorf("%d sync samples in %d frames with a key interval of 15", keys, count)
	}

	dec, err := NewVideoDecoder(VideoConfig{Codec: "h264", Width: w, Height: h, Record: cfg, Matrix: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var decoded []timedFrame
	for i, p := range packets {
		got, err := dec.Decode(p.Data, time.Duration(i)*time.Second/30)
		if err != nil {
			t.Fatalf("decode packet %d: %v", i, err)
		}
		decoded = append(decoded, frames(t, got)...)
	}
	rest, err := dec.Flush()
	if err != nil {
		t.Fatal(err)
	}
	decoded = append(decoded, frames(t, rest)...)
	if len(decoded) != count {
		t.Fatalf("decoded %d frames, want %d", len(decoded), count)
	}
	for i, f := range decoded {
		if f.PTS != time.Duration(i)*time.Second/30 {
			t.Fatalf("frame %d has PTS %v", i, f.PTS)
		}
		if f.Frame.Width != w || f.Frame.Height != h || f.Frame.Matrix != 1 || f.Frame.FullRange {
			t.Fatalf("frame %d: %dx%d matrix %d full %v", i, f.Frame.Width, f.Frame.Height, f.Frame.Matrix, f.Frame.FullRange)
		}
		if psnr := lumaPSNR(f.Frame, gradientFrame(w, h, i)); psnr < 32 {
			t.Errorf("frame %d: luma PSNR %.1f dB", i, psnr)
		}
	}

	// Errors, not crashes, on bad input.
	if _, err := NewVideoDecoder(VideoConfig{Codec: "h264", Width: w, Height: h}); err == nil {
		t.Error("decoder opened without a configuration record")
	}
	if _, err := NewVideoDecoder(VideoConfig{Codec: "vp9", Width: w, Height: h, Record: cfg}); err != ErrUnsupported {
		t.Errorf("vp9: %v, want ErrUnsupported", err)
	}
	if _, err := NewH264Encoder(EncodeConfig{Width: 321, Height: 240, FrameNum: 30, FrameDen: 1}); err == nil {
		t.Error("encoder accepted an odd width")
	}
	if _, err := dec.Decode([]byte{0, 0, 0, 3, 0x65, 0xff, 0xff}, 0); err == nil {
		t.Log("the decoder tolerated a garbage packet")
	}
}

// TestAACRoundTrip encodes a sine with AudioToolbox and decodes it again.
func TestAACRoundTrip(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const rate, seconds, freq = 48000, 2, 440.0
	enc, err := NewAACEncoder(AudioEncodeConfig{SampleRate: rate, Channels: 2, Bitrate: 128000})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	in := make([]float32, 2*rate*seconds)
	for i := 0; i < rate*seconds; i++ {
		v := float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		in[2*i], in[2*i+1] = v, -v
	}
	var packets [][]byte
	for off := 0; off < len(in); off += 2 * 1600 { // uneven blocks, as the exporter sends
		out, err := enc.Encode(in[off:min(off+2*1600, len(in))])
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, out...)
	}
	out, err := enc.Flush()
	if err != nil {
		t.Fatal(err)
	}
	packets = append(packets, out...)
	prime := enc.Priming()
	if want := (rate*seconds + prime + 1023) / 1024; len(packets) < want || len(packets) > want+1 {
		t.Errorf("%d packets, want %d (priming %d)", len(packets), want, prime)
	}
	if enc.FrameSize() != 1024 || prime < 1024 || prime > 4096 {
		t.Errorf("frame size %d, priming %d", enc.FrameSize(), prime)
	}
	if cfg := enc.Config(); len(cfg) != 2 || cfg[0] != 0x11 || cfg[1] != 0x90 {
		t.Errorf("AudioSpecificConfig % x, want 11 90 (AAC-LC, 48 kHz, stereo)", cfg)
	}

	dec, err := NewAudioDecoder(AudioConfig{Codec: "aac", SampleRate: rate, Channels: 2, Config: enc.Config()})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.(interface{ Close() error }).Close()
	var pcm []float32
	for i, p := range packets {
		fr, err := dec.Decode(p)
		if err != nil {
			t.Fatalf("decode packet %d: %v", i, err)
		}
		if fr.SampleRate != rate || fr.Channels != 2 {
			t.Fatalf("decoded format %d Hz, %d channels", fr.SampleRate, fr.Channels)
		}
		pcm = append(pcm, fr.Samples...)
	}
	// After the priming samples the output is the input, sample for sample.
	if len(pcm) < 2*(prime+rate*seconds) {
		t.Fatalf("decoded %d frames, want at least %d", len(pcm)/2, prime+rate*seconds)
	}
	pcm = pcm[2*prime:]
	var errSum, sigSum float64
	for i := 2 * 2048; i < 2*rate*seconds-2*2048; i++ {
		d := float64(pcm[i] - in[i])
		errSum += d * d
		sigSum += float64(in[i]) * float64(in[i])
	}
	if snr := 10 * math.Log10(sigSum/(errSum+1e-12)); snr < 30 {
		t.Errorf("round trip SNR %.1f dB: the decoded audio is not aligned with the input", snr)
	}

	if _, err := NewAACEncoder(AudioEncodeConfig{SampleRate: 12345, Channels: 2}); err == nil {
		t.Error("encoder accepted an unsupported sample rate")
	}
	if _, err := NewAudioDecoder(AudioConfig{Codec: "opus", SampleRate: rate, Channels: 2}); err != ErrUnsupported {
		t.Errorf("opus: %v, want ErrUnsupported", err)
	}
}

// TestSessionsAreReleased opens and closes many sessions; a leak of handles
// shows up as the registry growing.
func TestSessionsAreReleased(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	before := len(handles)
	for i := 0; i < 20; i++ {
		enc, err := NewH264Encoder(EncodeConfig{Width: 160, Height: 120, FrameNum: 25, FrameDen: 1, Bitrate: 500_000})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Encode(gradientFrame(160, 120, i)); err != nil {
			t.Fatal(err)
		}
		enc.Close()
		enc.Close() // closing twice is harmless
		a, err := NewAACEncoder(AudioEncodeConfig{SampleRate: 48000, Channels: 2, Bitrate: 128000})
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
	}
	if after := len(handles); after != before {
		t.Errorf("handle registry grew from %d to %d", before, after)
	}
}

// TestH264Reorder encodes with B-frames allowed: the samples come out in
// decode order, each frame exactly once, and decode back to the pictures
// that went in.
func TestH264Reorder(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h, count = 320, 240, 60
	enc, err := NewH264Encoder(EncodeConfig{Width: w, Height: h, FrameNum: 30, FrameDen: 1, Bitrate: 1_000_000, Reorder: true})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var packets []codec.EncodedFrame
	for n := 0; n < count; n++ {
		out, err := enc.Encode(gradientFrame(w, h, n))
		if err != nil {
			t.Fatalf("encode frame %d: %v", n, err)
		}
		packets = append(packets, out...)
	}
	out, err := enc.Flush()
	if err != nil {
		t.Fatal(err)
	}
	packets = append(packets, out...)
	seen, reordered := make([]bool, count), false
	for i, p := range packets {
		if p.Index < 0 || p.Index >= count || seen[p.Index] {
			t.Fatalf("packet %d carries frame %d (out of range or repeated)", i, p.Index)
		}
		seen[p.Index] = true
		reordered = reordered || p.Index != int64(i)
	}
	if len(packets) != count {
		t.Fatalf("%d packets for %d frames", len(packets), count)
	}
	if !reordered {
		t.Error("the encoder used no B-frames although reordering was allowed")
	}
	dec, err := NewVideoDecoder(VideoConfig{Codec: "h264", Width: w, Height: h, Record: enc.Config(), Matrix: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var decoded []timedFrame
	for i, p := range packets {
		got, err := dec.Decode(p.Data, time.Duration(p.Index)*time.Second/30)
		if err != nil {
			t.Fatalf("decode packet %d: %v", i, err)
		}
		decoded = append(decoded, frames(t, got)...)
	}
	rest, err := dec.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if decoded = append(decoded, frames(t, rest)...); len(decoded) != count {
		t.Fatalf("decoded %d frames, want %d", len(decoded), count)
	}
	for _, f := range decoded {
		want := gradientFrame(w, h, int((f.PTS*30+time.Second/2)/time.Second))
		if psnr := lumaPSNR(f.Frame, want); psnr < 32 {
			t.Errorf("frame at %v: luma PSNR %.1f dB", f.PTS, psnr)
		}
	}
}

// TestH264Quality checks that the quality setting steers the size and that
// impossible settings are refused.
func TestH264Quality(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h = 320, 240
	size := func(q float64) (n int) {
		enc, err := NewH264Encoder(EncodeConfig{Width: w, Height: h, FrameNum: 30, FrameDen: 1, Quality: q})
		if errors.Is(err, ErrNoQualityMode) {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		for i := 0; i <= 30; i++ {
			var out []codec.EncodedFrame
			if i < 30 {
				out, err = enc.Encode(gradientFrame(w, h, i))
			} else {
				out, err = enc.Flush()
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range out {
				n += len(p.Data)
			}
		}
		return n
	}
	if low, high := size(0.3), size(0.8); 2*high < 3*low {
		t.Errorf("quality 0.8 gave %d bytes and quality 0.3 gave %d: the setting has little effect", high, low)
	}
	if _, err := NewH264Encoder(EncodeConfig{Width: w, Height: h, FrameNum: 30, FrameDen: 1, Quality: 1.5}); err == nil {
		t.Error("quality 1.5 was accepted")
	}
}

// encodeGradient encodes count frames of the moving gradient, the first
// numbered from, with a keyframe every key frames.
func encodeGradient(t *testing.T, w, h, from, count, key int) (packets []codec.EncodedFrame, config []byte) {
	t.Helper()
	enc, err := NewH264Encoder(EncodeConfig{Width: w, Height: h, FrameNum: 30, FrameDen: 1, Bitrate: 2_000_000, KeyInterval: key})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	for n := 0; n <= count; n++ {
		var out []codec.EncodedFrame
		if n < count {
			out, err = enc.Encode(gradientFrame(w, h, from+n))
		} else {
			out, err = enc.Flush()
		}
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, out...)
	}
	if len(packets) != count {
		t.Fatalf("%d packets for %d frames", len(packets), count)
	}
	return packets, enc.Config()
}

// TestPictures covers the life of decoded pictures: they are copied out only
// on request, into the frame offered; they are counted until released; and
// closing the decoder releases the ones still out.
func TestPictures(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h, count = 320, 240, 12
	packets, cfg := encodeGradient(t, w, h, 0, count, 30)
	dec, err := NewVideoDecoder(VideoConfig{Codec: "h264", Width: w, Height: h, Record: cfg, Matrix: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	before := VideoDecoderStats().Pictures
	var pics []codec.TimedPicture
	for i, p := range packets {
		got, err := dec.Decode(p.Data, time.Duration(i))
		if err != nil {
			t.Fatal(err)
		}
		pics = append(pics, got...) // the slice itself is the decoder's
	}
	if len(pics) != count {
		t.Fatalf("%d pictures, want %d", len(pics), count)
	}
	if out := VideoDecoderStats().Pictures - before; out != count {
		t.Errorf("%d pictures counted as out, want %d", out, count)
	}

	// Read out of order and into one frame: each copy is the right picture
	// and lands in the same memory.
	var frame *codec.VideoFrame
	var first *byte
	for _, i := range []int{7, 2, 11, 0} {
		if frame, err = pics[i].Picture.Frame(frame); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = &frame.Y[0]
		} else if first != &frame.Y[0] {
			t.Error("the frame offered was not reused")
		}
		if psnr := lumaPSNR(frame, gradientFrame(w, h, i)); psnr < 32 {
			t.Errorf("picture %d: luma PSNR %.1f dB", i, psnr)
		}
	}
	// A frame of another size is replaced, not written past.
	small := codec.NewVideoFrame(16, 16)
	if f, err := pics[3].Picture.Frame(small); err != nil || f.Width != w || f.Height != h || len(f.Y) != w*h {
		t.Errorf("copy into a small frame: %v, %dx%d", err, f.Width, f.Height)
	}
	allocs := testing.AllocsPerRun(20, func() {
		if frame, err = pics[5].Picture.Frame(frame); err != nil {
			t.Fatal(err)
		}
	})
	if size := float64(w * h * 3 / 2); allocs*16 > size/64 { // the binding allocates a few words per call
		t.Errorf("copying a picture into a frame of its size makes %.0f allocations", allocs)
	}

	pics[0].Picture.Release()
	pics[0].Picture.Release() // releasing twice is harmless
	if _, err := pics[0].Picture.Frame(nil); err == nil {
		t.Error("a released picture could still be read")
	}
	if out := VideoDecoderStats().Pictures - before; out != count-1 {
		t.Errorf("%d pictures out after one release, want %d", out, count-1)
	}
	dec.Close()
	if out := VideoDecoderStats().Pictures - before; out != 0 {
		t.Errorf("%d pictures still out after Close", out)
	}
	if _, err := pics[1].Picture.Frame(nil); err == nil {
		t.Error("a picture could be read after its decoder was closed")
	}
}

// TestRestart moves a decoder from one stream to another. The session is
// kept when the new stream starts on an IDR picture, and replaced, without
// the caller noticing, when it starts anywhere else.
func TestRestart(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h, count = 320, 240, 20
	a, cfgA := encodeGradient(t, w, h, 0, count, 10)
	b, cfgB := encodeGradient(t, w, h, 100, count, 10)
	big, cfgBig := encodeGradient(t, 640, 480, 0, 4, 10)
	conf := func(rec []byte, w, h int) VideoConfig {
		return VideoConfig{Codec: "h264", Width: w, Height: h, Record: rec, Matrix: 1}
	}
	dec, err := NewVideoDecoder(conf(cfgA, w, h))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	r, ok := dec.(Restarter)
	if !ok {
		t.Fatal("the decoder cannot be restarted")
	}
	// run decodes packets from index from on and checks the pictures, which
	// are numbered base+index.
	run := func(what string, packets []codec.EncodedFrame, from, base, w, h int) {
		t.Helper()
		n := 0
		for i := from; i <= len(packets); i++ {
			var got []codec.TimedPicture
			if i < len(packets) {
				got, err = dec.Decode(packets[i].Data, time.Duration(i))
			} else {
				got, err = dec.Flush()
			}
			if err != nil {
				t.Fatalf("%s: packet %d: %v", what, i, err)
			}
			for _, f := range frames(t, got) {
				if psnr := lumaPSNR(f.Frame, gradientFrame(w, h, base+int(f.PTS))); psnr < 32 {
					t.Errorf("%s: picture %d: luma PSNR %.1f dB", what, f.PTS, psnr)
				}
				n++
			}
		}
		if n != len(packets)-from {
			t.Errorf("%s: %d pictures, want %d", what, n, len(packets)-from)
		}
	}
	delta := func(s0 DecoderStats) (sessions, reused int64) {
		s := VideoDecoderStats()
		return s.Sessions - s0.Sessions, s.Reused - s0.Reused
	}

	s0 := VideoDecoderStats()
	run("first stream", a, 0, 0, w, h)
	// Another stream of the same size, entered at its first IDR picture,
	// then the first stream again from its second: one session throughout.
	if !r.Restart(conf(cfgB, w, h)) {
		t.Fatal("the decoder refused a second stream of the same kind")
	}
	run("second stream", b, 0, 100, w, h)
	if !r.Restart(conf(cfgA, w, h)) {
		t.Fatal("the decoder refused the first stream again")
	}
	if !a[10].Sync {
		t.Fatal("packet 10 is not a keyframe")
	}
	run("first stream again", a, 10, 0, w, h)
	if sessions, reused := delta(s0); sessions != 0 || reused != 2 {
		t.Errorf("%d new sessions and %d reuses for two restarts on IDR pictures, want 0 and 2", sessions, reused)
	}

	// Entered between keyframes the stream cannot be decoded properly, but
	// the session must not be the one that holds the other stream's
	// pictures: a new one is opened.
	s0 = VideoDecoderStats()
	if !r.Restart(conf(cfgB, w, h)) {
		t.Fatal("the decoder refused a restart")
	}
	got, _ := dec.Decode(b[3].Data, 0)
	frames(t, got)
	if sessions, reused := delta(s0); sessions != 1 || reused != 0 {
		t.Errorf("%d new sessions and %d reuses for a restart between keyframes, want 1 and 0", sessions, reused)
	}
	// That may have failed, and a decoder that has failed is not reused.
	if _, err := dec.Decode([]byte{0, 0, 0, 3, 0x65, 0xff, 0xff}, 0); err != nil {
		if r.Restart(conf(cfgA, w, h)) {
			t.Error("a decoder that had failed accepted another stream")
		}
		dec.Close()
		if dec, err = NewVideoDecoder(conf(cfgA, w, h)); err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		r = dec.(Restarter)
		run("after a failure", a, 0, 0, w, h)
	}

	// Another codec is refused. Another picture size is for the system to
	// accept or not; whichever it says, the pictures must be right.
	if r.Restart(VideoConfig{Codec: "hevc", Width: w, Height: h, Record: cfgA}) {
		t.Error("an H.264 decoder accepted an HEVC stream")
	}
	if r.Restart(conf(cfgBig, 640, 480)) {
		t.Log("the session accepts a stream of another picture size")
		run("larger stream", big, 0, 0, 640, 480)
	} else {
		t.Log("the session does not accept a stream of another picture size")
		run("first stream, after a refusal", a, 0, 0, w, h)
	}
	dec.Close()
	if r.Restart(conf(cfgA, w, h)) {
		t.Error("a closed decoder accepted a stream")
	}
}

func TestCleanStart(t *testing.T) {
	nal := func(lenSize int, units ...[]byte) (s []byte) {
		for _, u := range units {
			for i := lenSize - 1; i >= 0; i-- {
				s = append(s, byte(len(u)>>(8*i)))
			}
			s = append(s, u...)
		}
		return s
	}
	sei, idr, slice := []byte{0x06, 1, 2}, []byte{0x65, 0x88}, []byte{0x41, 0x9a}
	hevc := func(typ byte) []byte { return []byte{typ << 1, 1, 0xaf} }
	for _, c := range []struct {
		name    string
		codec   string
		lenSize int
		sample  []byte
		want    bool
	}{
		{"H.264 IDR", "h264", 4, nal(4, idr), true},
		{"H.264 IDR after SEI", "h264", 4, nal(4, sei, idr), true},
		{"H.264 IDR, 2-byte lengths", "h264", 2, nal(2, sei, idr), true},
		{"H.264 non-IDR", "h264", 4, nal(4, sei, slice, slice), false},
		{"H.264 recovery point only", "h264", 4, nal(4, []byte{0x06, 6, 1, 0x80}, slice), false},
		{"HEVC IDR_W_RADL", "hevc", 4, nal(4, hevc(39), hevc(19)), true},
		{"HEVC IDR_N_LP", "hevc", 4, nal(4, hevc(20)), true},
		{"HEVC BLA", "hevc", 4, nal(4, hevc(16)), true},
		{"HEVC CRA", "hevc", 4, nal(4, hevc(21)), false},
		{"HEVC trailing", "hevc", 4, nal(4, hevc(1)), false},
		{"unknown length size", "h264", 0, nal(4, idr), false},
		{"length past the end", "h264", 4, []byte{0, 0, 1, 0, 0x65, 0x88}, false},
		{"empty", "h264", 4, nil, false},
		{"other codec", "vp9", 4, nal(4, idr), false},
	} {
		if got := cleanStart(c.codec, c.lenSize, c.sample); got != c.want {
			t.Errorf("%s: cleanStart = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDecodeFailures checks that a sample the decoder refuses is counted,
// and that a session which has failed is not reused for another stream.
func TestDecodeFailures(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	const w, h = 320, 240
	_, cfg := encodeGradient(t, w, h, 0, 4, 10)
	vc := VideoConfig{Codec: "h264", Width: w, Height: h, Record: cfg, Matrix: 1}
	dec, err := NewVideoDecoder(vc)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	before := VideoDecoderStats()
	// A slice NAL unit of an IDR picture whose header is noise.
	bad := []byte{0, 0, 0, 9, 0x65, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	pics, err := dec.Decode(bad, 0)
	for _, p := range pics {
		p.Picture.Release()
	}
	if err == nil {
		t.Skip("the system decoder accepted a corrupt sample without an error")
	}
	if n := VideoDecoderStats().Failures - before.Failures; n != 1 {
		t.Errorf("%d failures counted for one refused sample (%v)", n, err)
	}
	if dec.(Restarter).Restart(vc) {
		t.Error("a session that failed was reused")
	}
}
