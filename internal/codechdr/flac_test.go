package codechdr

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The frame headers of the example files of RFC 9639 (appendix D), with
// what the standard reads in them.
const (
	seedFLACFrame1 = "fff869180000bf"
	seedFLACFrame2 = "fff86998000f99"
	seedFLACFrame3 = "fff869180102a4"
	seedFLACFrame4 = "fff868020017e9"
)

func TestFLACFrameHeader(t *testing.T) {
	for _, c := range []struct {
		hdr  string
		want FLACFrame
	}{
		{seedFLACFrame1, FLACFrame{BlockSize: 1, SampleRate: 44100, BitDepth: 16, Channels: 2, Assignment: 1, Size: 7}},
		{seedFLACFrame2, FLACFrame{BlockSize: 16, SampleRate: 44100, BitDepth: 16, Channels: 2, Assignment: FLACSideRight, Size: 7}},
		{seedFLACFrame3, FLACFrame{Number: 1, BlockSize: 3, SampleRate: 44100, BitDepth: 16, Channels: 2, Assignment: 1, Size: 7}},
		{seedFLACFrame4, FLACFrame{BlockSize: 24, SampleRate: 32000, BitDepth: 8, Channels: 1, Size: 7}},
	} {
		b, _ := hex.DecodeString(c.hdr)
		// What follows a header is not part of it.
		if h, ok := FLACFrameHeader(append(b, 0xff, 0xff, 0xff)); !ok || h != c.want {
			t.Errorf("%s: %+v, %v; want %+v", c.hdr, h, ok, c.want)
		}
		for cut := 0; cut < len(b); cut++ {
			if _, ok := FLACFrameHeader(b[:cut]); ok {
				t.Errorf("%s: read from its first %d bytes", c.hdr, cut)
			}
		}
		for bit := 0; bit < 8*len(b); bit++ {
			bad := bytes.Clone(b)
			bad[bit/8] ^= 0x80 >> uint(bit%8)
			if _, ok := FLACFrameHeader(bad); ok {
				t.Errorf("%s: read with bit %d flipped", c.hdr, bit)
			}
		}
	}

	// seal puts the checksum on a header.
	seal := func(b ...byte) []byte { return append(b, FLACCRC8(b)) }
	// The standard's example of a coded number: sample 51 billion of a
	// stream of blocks of varying size (§9.1.5). And every field that goes
	// behind the number: a block size in two bytes and a rate in tens of Hz.
	h, ok := FLACFrameHeader(seal(0xff, 0xf9, 0x7e, 0x70, 0xfe, 0xaf, 0x9f, 0xb5, 0xa3, 0xb8, 0x80, 0x12, 0x34, 0x56, 0x78))
	if want := (FLACFrame{Variable: true, Number: 51_000_000_000, BlockSize: 0x1235, SampleRate: 0x5678 * 10, Channels: 8, Assignment: 7, Size: 16}); !ok || h != want {
		t.Errorf("the longest header: %+v, %v; want %+v", h, ok, want)
	}
	if FLACMaxHeader != 16 {
		t.Errorf("FLACMaxHeader is %d; the longest header has 16 bytes", FLACMaxHeader)
	}
	// Every block size and rate that has a code.
	sizes := map[byte]int{1: 192, 2: 576, 3: 1152, 4: 2304, 5: 4608, 8: 256, 9: 512, 10: 1024, 11: 2048, 12: 4096, 13: 8192, 14: 16384, 15: 32768}
	rates := map[byte]int{0: 0, 1: 88200, 2: 176400, 3: 192000, 4: 8000, 5: 16000, 6: 22050, 7: 24000, 8: 32000, 9: 44100, 10: 48000, 11: 96000}
	depths := map[byte]int{0: 0, 1: 8, 2: 12, 4: 16, 5: 20, 6: 24, 7: 32}
	for sc, size := range sizes {
		for rc, rate := range rates {
			for dc, depth := range depths {
				h, ok := FLACFrameHeader(seal(0xff, 0xf8, sc<<4|rc, 0xa0|dc<<1, 0x7f))
				if !ok || h.BlockSize != size || h.SampleRate != rate || h.BitDepth != depth || h.Channels != 2 || h.Assignment != FLACMidSide || h.Number != 0x7f || h.Size != 6 {
					t.Errorf("codes %d, %d, %d: %+v, %v; want %d samples at %d Hz of %d bits", sc, rc, dc, h, ok, size, rate, depth)
				}
			}
		}
	}
	// A rate in kHz and in Hz, and a block size in one byte.
	if h, ok := FLACFrameHeader(seal(0xff, 0xf8, 0x6c, 0x08, 0x00, 0xff, 0x25)); !ok || h.BlockSize != 256 || h.SampleRate != 37000 {
		t.Errorf("a block size in a byte and a rate in kHz: %+v, %v", h, ok)
	}
	if h, ok := FLACFrameHeader(seal(0xff, 0xf8, 0xcd, 0x08, 0xc2, 0x80, 0x2b, 0x11)); !ok || h.Number != 0x80 || h.SampleRate != 11025 || h.Size != 9 {
		t.Errorf("a number in two bytes and a rate in Hz: %+v, %v", h, ok)
	}
	// What is not allowed, each with a checksum that holds.
	for name, b := range map[string][]byte{
		"a reserved block size code":        seal(0xff, 0xf8, 0x09, 0x08, 0),
		"the forbidden rate code":           seal(0xff, 0xf8, 0xcf, 0x08, 0),
		"a reserved channel code":           seal(0xff, 0xf8, 0xc9, 0xb8, 0),
		"a reserved sample size code":       seal(0xff, 0xf8, 0xc9, 0x06, 0),
		"the reserved bit":                  seal(0xff, 0xf8, 0xc9, 0x09, 0),
		"a number that starts mid-code":     seal(0xff, 0xf8, 0xc9, 0x08, 0x80),
		"a number of eight bytes":           seal(0xff, 0xf8, 0xc9, 0x08, 0xff, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80),
		"a number cut off":                  seal(0xff, 0xf8, 0xc9, 0x08, 0xc2, 0x41),
		"a frame number of 36 bits":         seal(0xff, 0xf8, 0xc9, 0x08, 0xfe, 0xaf, 0x9f, 0xb5, 0xa3, 0xb8, 0x80),
		"a block of 65536 samples":          seal(0xff, 0xf8, 0x79, 0x08, 0, 0xff, 0xff),
		"a rate of 0 Hz":                    seal(0xff, 0xf8, 0xcd, 0x08, 0, 0, 0),
		"a rate of 0 kHz":                   seal(0xff, 0xf8, 0xcc, 0x08, 0, 0),
		"the sync code of another codec":    seal(0xff, 0xfb, 0xc9, 0x08, 0),
		"a metadata block header, type 127": seal(0xff, 0x00, 0x00, 0x22, 0),
	} {
		if h, ok := FLACFrameHeader(b); ok {
			t.Errorf("%s: read as %+v", name, h)
		}
	}
}

// TestFLACCRC16 holds the checksum taken eight bytes at a time to the one
// taken bit by bit, at every length and from every starting value it is
// carried on from.
func TestFLACCRC16(t *testing.T) {
	bitwise := func(c uint16, b []byte) uint16 {
		for _, v := range b {
			c ^= uint16(v) << 8
			for range 8 {
				if c&0x8000 != 0 {
					c = c<<1 ^ 0x8005
				} else {
					c <<= 1
				}
			}
		}
		return c
	}
	b := make([]byte, 300)
	for i := range b {
		b[i] = byte(i*i*7 + i>>3 + 131)
	}
	for n := 0; n <= len(b); n++ {
		if got, want := FLACCRC16(0, b[:n]), bitwise(0, b[:n]); got != want {
			t.Fatalf("of %d bytes: %#x, want %#x", n, got, want)
		}
		// In two parts, cut anywhere.
		if cut := n * 3 / 7; FLACCRC16(FLACCRC16(0, b[:cut]), b[cut:n]) != bitwise(0, b[:n]) {
			t.Fatalf("of %d bytes, taken in two parts cut at %d: not that of the whole", n, cut)
		}
	}
	if got := FLACCRC16(0, []byte("123456789")); got != 0xfee8 {
		t.Errorf(`of "123456789": %#x, want 0xfee8`, got)
	}
	if got := FLACCRC8([]byte("123456789")); got != 0xf4 {
		t.Errorf(`the header checksum of "123456789": %#x, want 0xf4`, got)
	}
}
