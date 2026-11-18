package caption

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

const ms = time.Millisecond

func TestParseSRT(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Cue
	}{
		{"basic", "1\n00:00:01,000 --> 00:00:02,500\nHello\nworld\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n",
			[]Cue{{1000 * ms, 2500 * ms, "Hello\nworld"}, {3000 * ms, 4000 * ms, "Bye"}}},
		{"bom crlf dot", "\uFEFF1\r\n00:00:01.000 --> 00:00:02.000\r\nHi\r\n\r\n\r\n\r\n",
			[]Cue{{1000 * ms, 2000 * ms, "Hi"}}},
		{"no index, no blank", "00:00:01,000 --> 00:00:02,000\nA\n2\n00:00:03,000 --> 00:00:04,000\nB",
			[]Cue{{1000 * ms, 2000 * ms, "A"}, {3000 * ms, 4000 * ms, "B"}}},
		{"numeric text kept", "1\n00:00:01,000 --> 00:00:02,000\n42\n",
			[]Cue{{1000 * ms, 2000 * ms, "42"}}},
		{"markup", "1\n00:00:01,000 --> 00:00:02,000 X1:1 Y1:2\n{\\an8}<i>Tom</i> &amp; <font color=\"red\">Jerry</font> a < b &lt;i&gt;\n",
			[]Cue{{1000 * ms, 2000 * ms, "Tom & Jerry a < b <i>"}}},
		{"sorted, short ms, bad dropped", "1\n00:00:05,5 --> 00:00:06,000\nlate\n\n2\n00:00:01,000 --> 00:00:02,000\nearly\n\n3\n00:00:09,000 --> 00:00:08,000\nbackwards\n\n4\n00:00:10,000 --> 00:00:11,000\n\n",
			[]Cue{{1000 * ms, 2000 * ms, "early"}, {5500 * ms, 6000 * ms, "late"}}},
		{"empty", "", nil},
		{"garbage", "hello\nthere --> nothing\n", nil},
	}
	for _, tc := range tests {
		got, err := ParseSRT(strings.NewReader(tc.in))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestParseWebVTT(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Cue
	}{
		{"basic", "WEBVTT\n\n00:01.000 --> 00:02.500\nHello\nworld\n\n01:00:03.000 --> 01:00:04.000\nBye\n",
			[]Cue{{1000 * ms, 2500 * ms, "Hello\nworld"}, {time.Hour + 3000*ms, time.Hour + 4000*ms, "Bye"}}},
		{"header, blocks, ids, settings", "\uFEFFWEBVTT - title\r\nKind: captions\r\n\r\nNOTE a comment\r\nmore\r\n\r\nSTYLE\r\n::cue { color: red }\r\n\r\nREGION\r\nid:r1\r\n\r\nintro\r\n00:00:01.000 --> 00:00:02.000 line:0 position:20% align:start\r\n<v Fred>Hi <c.loud>there</c> <00:00:01.500>you &amp; me&nbsp;x\r\n",
			[]Cue{{1000 * ms, 2000 * ms, "Hi there you & me\u00A0x"}}},
		{"no blank after header", "WEBVTT\n00:00:01.000 --> 00:00:02.000\nA\n",
			[]Cue{{1000 * ms, 2000 * ms, "A"}}},
		{"signature only", "WEBVTT", nil},
	}
	for _, tc := range tests {
		got, err := ParseWebVTT(strings.NewReader(tc.in))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	if _, err := ParseWebVTT(strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nx\n")); err != ErrNotWebVTT {
		t.Errorf("missing signature: err = %v", err)
	}
	if _, err := ParseWebVTT(strings.NewReader("WEBVTTX\n")); err != ErrNotWebVTT {
		t.Errorf("bad signature: err = %v", err)
	}
}

func TestParseDispatch(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:02,000\nS\n"
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nV\n"
	for _, tc := range []struct{ in, name, want string }{
		{srt, "a.SRT", "S"}, {vtt, "a.vtt", "V"}, {vtt, "", "V"}, {srt, "subs.txt", "S"},
	} {
		got, err := Parse(strings.NewReader(tc.in), tc.name)
		if err != nil || len(got) != 1 || got[0].Text != tc.want {
			t.Errorf("Parse(%q): %v %v", tc.name, got, err)
		}
	}
	if _, err := Parse(strings.NewReader(srt), "a.vtt"); err != ErrNotWebVTT {
		t.Errorf("srt as .vtt: err = %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxInputSize+1)
	if _, err := ParseSRT(bytes.NewReader(big)); err != ErrTooLarge {
		t.Errorf("err = %v", err)
	}
}

func TestWebVTTRoundTrip(t *testing.T) {
	cues := []Cue{
		{0, 1500 * ms, "First <b> & co"},
		{2 * time.Second, 3 * time.Second, "two\nlines --> here"},
		{time.Hour + 61*time.Second, time.Hour + 62*time.Second, "späte Zeile"},
	}
	var buf bytes.Buffer
	if err := WriteWebVTT(&buf, cues); err != nil {
		t.Fatal(err)
	}
	got, err := ParseWebVTT(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cues) {
		t.Errorf("round trip:\n got %q\nwant %q", got, cues)
	}
}

func checkSamples(t *testing.T, s []Sample, total time.Duration) {
	t.Helper()
	var cursor time.Duration
	for i, x := range s {
		if x.Start != cursor || x.Duration <= 0 {
			t.Fatalf("sample %d: start %v dur %v, cursor %v", i, x.Start, x.Duration, cursor)
		}
		if len(x.Data) < 2 || int(x.Data[0])<<8|int(x.Data[1]) != len(x.Data)-2 {
			t.Fatalf("sample %d: bad length prefix", i)
		}
		cursor += x.Duration
	}
	if total > 0 && cursor != total {
		t.Fatalf("samples cover %v, want %v", cursor, total)
	}
	if total <= 0 && len(s) != 0 {
		t.Fatalf("want no samples for total %v", total)
	}
}

func TestTx3gSamples(t *testing.T) {
	sec := time.Second
	cues := []Cue{
		{1 * sec, 3 * sec, "A"},
		{2 * sec, 4 * sec, "B"},   // overlaps A
		{6 * sec, 20 * sec, "C"},  // runs past total
		{30 * sec, 31 * sec, "D"}, // beyond total
		{-2 * sec, -1 * sec, "E"}, // before zero
	}
	got := Tx3gSamples(cues, 10*sec)
	checkSamples(t, got, 10*sec)
	var texts []string
	for _, s := range got {
		texts = append(texts, s.Start.String()+":"+string(s.Data[2:]))
	}
	want := []string{"0s:", "1s:A", "2s:A\nB", "3s:B", "4s:", "6s:C"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("got %q want %q", texts, want)
	}

	checkSamples(t, Tx3gSamples(nil, 5*sec), 5*sec)
	checkSamples(t, Tx3gSamples(cues, 0), 0)

	long := Tx3gSamples([]Cue{{0, sec, strings.Repeat("é", 40000)}}, sec)
	checkSamples(t, long, sec)
	if len(long[0].Data) != 2+0xFFFE {
		t.Errorf("truncated length = %d", len(long[0].Data))
	}
}

func fuzzCheck(t *testing.T, cues []Cue) {
	for i, c := range cues {
		if c.End <= c.Start || c.Start < 0 || c.Text == "" {
			t.Fatalf("bad cue %d: %+v", i, c)
		}
		if i > 0 && cues[i-1].Start > c.Start {
			t.Fatalf("cues not sorted at %d", i)
		}
	}
	total := 10 * time.Second
	if n := len(cues); n > 0 && cues[n-1].Start < 1000*time.Hour {
		total = cues[n-1].Start + time.Second
	}
	checkSamples(t, Tx3gSamples(cues, total), total)
	var buf bytes.Buffer
	if err := WriteWebVTT(&buf, cues); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWebVTT(&buf); err != nil {
		t.Fatalf("reparse: %v", err)
	}
}

func FuzzParseSRT(f *testing.F) {
	f.Add("1\n00:00:01,000 --> 00:00:02,500\nHello <i>x</i>\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n")
	f.Add("00:01,5 --> 99:99:99.9\n{\\an8}&amp;\n1\n")
	f.Fuzz(func(t *testing.T, in string) {
		cues, err := ParseSRT(strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		fuzzCheck(t, cues)
	})
}

func FuzzParseWebVTT(f *testing.F) {
	f.Add("WEBVTT\n\nNOTE x\n\nid\n00:01.000 --> 00:02.500 align:start\n<v A>Hello\n\n00:00:03.000 --> 00:00:04.000\nBye\n")
	f.Add("WEBVTT\n00:00:01.000 --> 00:00:02.000\nA\n")
	f.Fuzz(func(t *testing.T, in string) {
		cues, err := ParseWebVTT(strings.NewReader(in))
		if err != nil {
			return
		}
		fuzzCheck(t, cues)
	})
}
