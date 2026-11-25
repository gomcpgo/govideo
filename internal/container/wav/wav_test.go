package wav

import (
	"bytes"
	"context"
	"testing"
)

// TestPCMHeader checks the header against the parser, and against the
// canonical 44 bytes written out by hand.
func TestPCMHeader(t *testing.T) {
	h, err := PCMHeader(1, 48000, 16, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := "RIFF\xec\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\xbb\x00\x00\x00\x77\x01\x00\x02\x00\x10\x00data\xc8\x00\x00\x00"
	if string(h) != want || len(h) != HeaderSize {
		t.Errorf("100 frames of 48 kHz mono:\n got %q\nwant %q", h, want)
	}

	for _, c := range []struct{ channels, rate, bits int }{{1, 16000, 16}, {2, 44100, 16}, {2, 96000, 24}, {6, 48000, 32}, {1, 8000, 8}} {
		const frames = 123
		h, err := PCMHeader(c.channels, c.rate, c.bits, frames)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		data := append(h, make([]byte, frames*c.channels*c.bits/8)...)
		f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("%+v: the parser refuses the header: %v", c, err)
		}
		if f.Float || f.Channels != c.channels || f.SampleRate != c.rate || f.Bits != c.bits || f.DataOffset != HeaderSize || f.Frames() != frames {
			t.Errorf("%+v: read back as %+v", c, *f)
		}
	}

	// A header for no samples is a valid, empty file's.
	if h, err := PCMHeader(2, 48000, 16, 0); err != nil || len(h) != HeaderSize {
		t.Errorf("no frames: %d bytes, %v", len(h), err)
	}
	// The most a file holds, and one frame more.
	const most = MaxDataSize / 4
	if _, err := PCMHeader(2, 48000, 16, most); err != nil {
		t.Errorf("%d frames: %v", most, err)
	}
	for name, err := range map[string]error{
		"too long":       second(PCMHeader(2, 48000, 16, most+1)),
		"negative":       second(PCMHeader(2, 48000, 16, -1)),
		"no channels":    second(PCMHeader(0, 48000, 16, 1)),
		"no sample rate": second(PCMHeader(2, 0, 16, 1)),
		"12 bits":        second(PCMHeader(2, 48000, 12, 1)),
		"70000 channels": second(PCMHeader(70000, 48000, 16, 1)),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func second(_ []byte, err error) error { return err }
