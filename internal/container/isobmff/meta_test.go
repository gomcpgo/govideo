package isobmff

import "testing"

func TestGaplessTag(t *testing.T) {
	item := func(name, data string) []byte {
		return append(append(Box("mean", []byte("\x00\x00\x00\x00com.apple.iTunes")),
			Box("name", []byte("\x00\x00\x00\x00"+name))...),
			Box("data", []byte("\x00\x00\x00\x01\x00\x00\x00\x00"+data))...)
	}
	f := &File{Tags: map[string]string{}}
	f.parseFreeform(item("iTunNORM", " 00000000 00000800 00000240 0000000000005DC0"))
	if f.Gapless != nil {
		t.Fatal("another freeform tag was read as gapless information")
	}
	for _, bad := range []string{"", " 00000000 00000800", " 00000000 zzzz 00000240 0000000000005DC0", " 0 FFFFFFFFFF 0 10"} {
		f.parseFreeform(item("iTunSMPB", bad))
		if f.Gapless != nil {
			t.Fatalf("malformed value %q was accepted", bad)
		}
	}
	f.parseFreeform(item("iTunSMPB", " 00000000 00000800 00000240 0000000000005DC0 00000000 00000000"))
	if g := f.Gapless; g == nil || g.Priming != 2048 || g.Padding != 576 || g.Samples != 24000 {
		t.Fatalf("got %+v, want priming 2048, padding 576, 24000 samples", g)
	}
}
