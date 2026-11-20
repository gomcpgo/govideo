package isobmff

import (
	"bytes"
	"context"
	"io"
	"testing"
)

type memFile struct {
	bytes.Buffer
	pos int64
	b   []byte
}

func (m *memFile) Write(p []byte) (int, error) { m.b = append(m.b, p...); return len(p), nil }
func (m *memFile) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[m.pos:])
	m.pos += int64(n)
	return n, nil
}
func (m *memFile) Seek(off int64, whence int) (int64, error) { m.pos = off; return off, nil }

// buildFile muxes a small file: 5 video samples with B-frame style offsets,
// interleaved PCM, and a subtitle track.
func buildFile(t testing.TB, brand Brand) []byte {
	t.Helper()
	m := NewMuxer(brand, &memFile{})
	v := m.AddTrack(TrackConfig{Handler: "vide", Timescale: 30000, Entry: VisualEntry("jpeg", 64, 48, "test"), Width: 64, Height: 48})
	a := m.AddTrack(TrackConfig{Handler: "soun", Timescale: 48000, Entry: AudioEntry("sowt", 2, 16, 48000), SampleSize: 4, Language: "fra"})
	s := m.AddTrack(TrackConfig{Handler: "sbtl", Timescale: 1000, Entry: Tx3gEntry(), AllSync: true})
	for i := 0; i < 5; i++ {
		data := bytes.Repeat([]byte{byte('A' + i)}, 10+i)
		cts := int32(0)
		if i%2 == 1 {
			cts = 1001
		}
		if err := m.WriteSample(v, data, 1001, i == 0 || i == 3, cts); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(a, bytes.Repeat([]byte{byte(i)}, 4*(100+i)), 0, true, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.WriteSample(s, []byte{0, 2, 'h', 'i'}, 167, true, 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := m.Finish(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestMuxRoundTrip(t *testing.T) {
	for _, brand := range []Brand{BrandMP4, BrandMOV} {
		data := buildFile(t, brand)
		f, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if !f.FastStart || f.Fragmented || f.QuickTime != (brand == BrandMOV) || len(f.Tracks) != 3 {
			t.Fatalf("FastStart=%v Fragmented=%v QuickTime=%v tracks=%d", f.FastStart, f.Fragmented, f.QuickTime, len(f.Tracks))
		}
		v, a := f.Tracks[0], f.Tracks[1]
		if v.Handler != "vide" || v.SampleCount != 5 || v.MediaDuration != 5005 || v.SyncCount != 2 || v.VariableRate || v.CommonDelta != 1001 {
			t.Errorf("video track: %+v", v)
		}
		if a.Language != "fra" || a.SampleCount != 510 || a.TotalBytes != 2040 || a.Entries[0].Channels != 2 {
			t.Errorf("audio track: lang %q samples %d bytes %d", a.Language, a.SampleCount, a.TotalBytes)
		}
		it := v.Iter()
		for i := 0; ; i++ {
			s, err := it.Next()
			if err == io.EOF {
				if i != 5 {
					t.Errorf("iterated %d video samples, want 5", i)
				}
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.ReadSample(s, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := bytes.Repeat([]byte{byte('A' + i)}, 10+i); !bytes.Equal(got, want) {
				t.Errorf("sample %d: got %q", i, got)
			}
			if s.DTS != int64(i)*1001 || s.Sync != (i == 0 || i == 3) || (s.CTSOffset != 0) != (i%2 == 1) {
				t.Errorf("sample %d: %+v", i, s)
			}
		}
		// PCM comes back as whole chunks through NextBlock.
		ait := a.Iter()
		var frames int
		for i := 0; ; i++ {
			s, n, err := ait.NextBlock(4096)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.ReadSample(s, n, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n != 100+i || !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, 4*n)) || s.DTS != int64(frames) {
				t.Errorf("PCM block %d: n=%d dts=%d", i, n, s.DTS)
			}
			frames += n
		}
		if frames != 510 {
			t.Errorf("PCM frames: got %d, want 510", frames)
		}
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("hello world, this is not a movie"), {0, 0, 0, 8, 'f', 't', 'y', 'p'}} {
		if _, err := Open(context.Background(), bytes.NewReader(in), int64(len(in))); err == nil {
			t.Errorf("Open(%q) succeeded", in)
		}
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(buildFile(f, BrandMP4))
	f.Add(buildFile(f, BrandMOV))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Open(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		for _, tr := range file.Tracks {
			it := tr.Iter()
			for i := 0; i < 10000; i++ {
				s, n, err := it.NextBlock(64)
				if err != nil {
					break
				}
				file.ReadSample(s, n, nil)
			}
		}
	})
}

// TestMuxReorderedSamples writes a B-frame stream: samples in decode order,
// some shown before their decode position. The offsets must come back
// non-negative, with an edit that starts the presentation at the first
// frame shown, so that presentation times still run 0, 1, 2, ...
func TestMuxReorderedSamples(t *testing.T) {
	const dur = 1001
	order := []int{0, 4, 2, 1, 3, 8, 6, 5, 7, 9} // presentation index, in decode order
	m := NewMuxer(BrandMP4, &memFile{})
	v := m.AddTrack(TrackConfig{Handler: "vide", Timescale: 30000, Entry: VisualEntry("avc1", 64, 48, ""), Width: 64, Height: 48})
	for pos, shown := range order {
		if err := m.WriteSample(v, []byte{byte(shown)}, dur, pos == 0, int32(shown-pos)*dur); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := m.Finish(&out); err != nil {
		t.Fatal(err)
	}
	f, err := Open(context.Background(), bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	tr := f.Tracks[0]
	// The earliest lead is two frames (sample 3 shows frame 1).
	if len(tr.Edits) != 1 || tr.Edits[0].MediaTime != 2*dur || tr.Edits[0].SegmentDuration != 334 {
		t.Fatalf("edits %+v, want one starting at media time %d and lasting 334 ms", tr.Edits, 2*dur)
	}
	it := tr.Iter()
	for pos, shown := range order {
		s, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if s.CTSOffset < 0 || s.PTS()-tr.Edits[0].MediaTime != int64(shown)*dur {
			t.Errorf("sample %d: offset %d, presented at %d, want %d", pos, s.CTSOffset, s.PTS()-tr.Edits[0].MediaTime, shown*dur)
		}
	}
}
