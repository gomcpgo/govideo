//go:build darwin

package oscodec

import (
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
	var frames []codec.TimedFrame
	for i, p := range packets {
		got, err := dec.Decode(p.Data, time.Duration(i)*time.Second/30)
		if err != nil {
			t.Fatalf("decode packet %d: %v", i, err)
		}
		frames = append(frames, got...)
	}
	rest, err := dec.Flush()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, rest...)
	if len(frames) != count {
		t.Fatalf("decoded %d frames, want %d", len(frames), count)
	}
	for i, f := range frames {
		if f.PTS != time.Duration(i)*time.Second/30 {
			t.Fatalf("frame %d has PTS %v", i, f.PTS)
		}
		if f.Frame.Width != w || f.Frame.Height != h || f.Frame.Matrix != 1 || f.Frame.FullRange {
			t.Fatalf("frame %d: %dx%d matrix %d full %v", i, f.Frame.Width, f.Frame.Height, f.Frame.Matrix, f.Frame.FullRange)
		}
		want := gradientFrame(w, h, i)
		var sum float64
		for j := range want.Y {
			d := float64(want.Y[j]) - float64(f.Frame.Y[j])
			sum += d * d
		}
		if psnr := 10 * math.Log10(255*255/(sum/float64(len(want.Y))+1e-9)); psnr < 32 {
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
