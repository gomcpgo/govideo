package videoprobe

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMatrixRotation(t *testing.T) {
	const one = 0x10000
	for _, c := range []struct {
		name       string
		a, b, c, d int32
		rotation   int
		mirrored   bool
	}{
		{"identity", one, 0, 0, one, 0, false},
		{"90", 0, one, -one, 0, 90, false},
		{"180", -one, 0, 0, -one, 180, false},
		{"270", 0, -one, one, 0, 270, false},
		{"90 with a scale", 0, 2 * one, -one, 0, 90, false},
		{"horizontal flip", -one, 0, 0, one, 0, true},
		{"vertical flip", one, 0, 0, -one, 180, true},
		{"transpose", 0, one, one, 0, 90, true},
		{"anti-transpose", 0, -one, -one, 0, 270, true},
		{"empty", 0, 0, 0, 0, 0, false},
	} {
		r, m := matrixRotation([9]int32{c.a, c.b, 0, c.c, c.d, 0, 0, 0, 0x40000000})
		if r != c.rotation || m != c.mirrored {
			t.Errorf("%s: rotation %d mirrored %v, want %d %v", c.name, r, m, c.rotation, c.mirrored)
		}
	}
}

func TestUnsupportedFormats(t *testing.T) {
	ts := make([]byte, 3*188)
	ts[0], ts[188], ts[376] = 0x47, 0x47, 0x47
	m2ts := make([]byte, 3*192)
	m2ts[4], m2ts[196], m2ts[388] = 0x47, 0x47, 0x47
	pad := func(s string) []byte { return append([]byte(s), make([]byte, 64)...) }
	for _, c := range []struct {
		format string
		data   []byte
	}{
		{"flac", pad("fLaC\x00\x00\x00\x22")},
		{"ogg", pad("OggS\x00\x02")},
		{"mp3", pad("ID3\x04\x00\x00")},
		{"mp3", pad("\xff\xfb\x90\x00")},
		{"adts", pad("\xff\xf1\x50\x80")},
		{"ac3", pad("\x0b\x77\x12\x34")},
		{"mxf", pad("\x06\x0e\x2b\x34\x02\x05\x01\x01\x0d\x01\x02")},
		{"heif", pad("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic")},
		{"heif", pad("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1")},
		{"asf", pad("\x30\x26\xb2\x75\x8e\x66\xcf\x11")},
		{"flv", pad("FLV\x01\x05")},
		{"mpegts", ts},
		{"mpegts", m2ts},
	} {
		_, err := Probe(context.Background(), bytes.NewReader(c.data), int64(len(c.data)))
		var uf *UnsupportedFormatError
		if !errors.As(err, &uf) || uf.Format != c.format {
			t.Errorf("%s: got %v", c.format, err)
			continue
		}
		if !errors.Is(err, ErrUnknownFormat) || err.Error() == "videoprobe:  files are not supported" {
			t.Errorf("%s: error %q does not match ErrUnknownFormat or has no name", c.format, err)
		}
	}
	// A lone 0x47 is not a transport stream.
	one := pad("\x47hello")
	if _, err := Probe(context.Background(), bytes.NewReader(one), int64(len(one))); err != ErrUnknownFormat {
		t.Errorf("stray sync byte: %v, want ErrUnknownFormat", err)
	}
}
