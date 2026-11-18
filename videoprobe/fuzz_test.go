package videoprobe

import (
	"bytes"
	"context"
	"testing"
)

func TestUnknownFormat(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("plain text, not media at all........")} {
		if _, err := Probe(context.Background(), bytes.NewReader(in), int64(len(in))); err == nil {
			t.Errorf("Probe(%q) succeeded", in)
		}
	}
}

// FuzzProbe feeds arbitrary bytes through container sniffing, parsing and
// JSON rendering. Seeds for each container come from the corpus directory
// and the container packages' own fuzz targets.
func FuzzProbe(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2\x00\x00\x00\x08moov"))
	f.Add([]byte("\x1a\x45\xdf\xa3\x84\x42\x82\x84webm"))
	f.Add([]byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x80\xbb\x00\x00\x00\xee\x02\x00\x04\x00\x10\x00data\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := Probe(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if _, err := info.JSON(); err != nil {
			t.Fatalf("JSON: %v", err)
		}
	})
}
