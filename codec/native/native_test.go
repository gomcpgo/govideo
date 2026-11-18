package native

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// stripDHT removes every Huffman table segment from a JPEG header.
func stripDHT(t *testing.T, p []byte) []byte {
	t.Helper()
	out := []byte{0xff, 0xd8}
	for i := 2; i+4 <= len(p); {
		if p[i] != 0xff {
			t.Fatalf("no marker at %d", i)
		}
		n := int(p[i+2])<<8 | int(p[i+3])
		if p[i+1] == 0xda {
			return append(out, p[i:]...)
		}
		if p[i+1] != 0xc4 {
			out = append(out, p[i:i+2+n]...)
		}
		i += 2 + n
	}
	t.Fatal("no scan found")
	return nil
}

// TestStandardHuffmanTables checks the Annex K tables against the Go
// encoder, which writes the same ones: a frame with its tables removed must
// decode, with ours in their place, to exactly the picture it held.
func TestStandardHuffmanTables(t *testing.T) {
	for _, tb := range [][]byte{dcLuma, dcChroma, acLuma, acChroma} {
		n := 0
		for _, c := range tb[:16] {
			n += int(c)
		}
		if n != len(tb)-16 {
			t.Fatalf("table lists %d codes but has %d symbols", n, len(tb)-16)
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.RGBA{uint8(x*5 ^ y*3), uint8(x*y + 7*x), uint8(y*4 + x%7*30), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	bare := stripDHT(t, full)
	if len(bare) >= len(full) {
		t.Fatal("no tables were removed")
	}
	if _, err := jpeg.Decode(bytes.NewReader(bare)); err == nil {
		t.Fatal("the standard library decoded a frame without tables; the fixture proves nothing")
	}
	want, err := MJPEGDecoder{}.Decode(full)
	if err != nil {
		t.Fatal(err)
	}
	got, err := MJPEGDecoder{}.Decode(bare)
	if err != nil {
		t.Fatalf("frame without Huffman tables: %v", err)
	}
	if !bytes.Equal(got.Y, want.Y) || !bytes.Equal(got.Cb, want.Cb) || !bytes.Equal(got.Cr, want.Cr) {
		t.Error("the picture decoded with the standard tables differs")
	}
	if out := withHuffmanTables(full); !bytes.Equal(out, full) {
		t.Error("a frame with its own tables was modified")
	}
}

func FuzzMJPEGDecode(f *testing.F) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil)
	f.Add(buf.Bytes())
	f.Add([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2})
	f.Add([]byte{0xff, 0xd8, 0xff, 0xe0, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		out := withHuffmanTables(data)
		if len(out) != len(data) && len(out) != len(data)+len(standardDHT) {
			t.Fatalf("length went from %d to %d", len(data), len(out))
		}
		MJPEGDecoder{}.Decode(data)
	})
}
