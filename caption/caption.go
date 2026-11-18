// Package caption parses SRT and WebVTT caption files and produces the soft
// subtitle payloads used by the muxers (tx3g samples, WebVTT sidecars).
//
// Parsing is tolerant of the usual real-world damage (BOM, CRLF, missing
// index lines, comma or dot millisecond separators) and never panics on
// hostile input. Burn-in rendering is not part of this package yet.
package caption

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxInputSize is the largest caption file the parsers accept.
const MaxInputSize = 16 << 20

// ErrTooLarge is returned when the input exceeds MaxInputSize.
var ErrTooLarge = errors.New("caption: input larger than 16 MiB")

// ErrNotWebVTT is returned by ParseWebVTT when the WEBVTT signature is missing.
var ErrNotWebVTT = errors.New("caption: missing WEBVTT signature")

// Cue is one caption. Text is plain UTF-8 with lines separated by "\n";
// markup has been stripped and entities decoded.
type Cue struct {
	Start, End time.Duration
	Text       string
}

// Sample is one timed-text sample for a subtitle track.
type Sample struct {
	Start, Duration time.Duration
	Data            []byte
}

// ParseSRT parses SubRip captions. Cues are returned sorted by start time.
func ParseSRT(r io.Reader) ([]Cue, error) {
	s, err := readAll(r)
	if err != nil {
		return nil, err
	}
	return parseSRT(s), nil
}

// ParseWebVTT parses WebVTT captions. Cues are returned sorted by start time.
func ParseWebVTT(r io.Reader) ([]Cue, error) {
	s, err := readAll(r)
	if err != nil {
		return nil, err
	}
	return parseWebVTT(s)
}

// Parse picks the format from the extension of name (.srt, .vtt, .webvtt).
// With any other name it sniffs the content for a WEBVTT signature and
// falls back to SRT.
func Parse(r io.Reader, name string) ([]Cue, error) {
	s, err := readAll(r)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt":
		return parseSRT(s), nil
	case ".vtt", ".webvtt":
		return parseWebVTT(s)
	}
	if hasVTTSignature(s) {
		return parseWebVTT(s)
	}
	return parseSRT(s), nil
}

// readAll reads the whole input (bounded), and normalises it: BOM removed,
// invalid UTF-8 replaced, NULs replaced, line endings converted to "\n".
func readAll(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxInputSize+1))
	if err != nil {
		return "", fmt.Errorf("caption: %w", err)
	}
	if len(b) > MaxInputSize {
		return "", ErrTooLarge
	}
	s := strings.ToValidUTF8(string(b), "\uFFFD")
	s = strings.TrimPrefix(s, "\uFEFF")
	if strings.ContainsAny(s, "\r\x00") {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		s = strings.ReplaceAll(s, "\x00", "\uFFFD")
	}
	return s, nil
}

func hasVTTSignature(s string) bool {
	if !strings.HasPrefix(s, "WEBVTT") {
		return false
	}
	rest := s[len("WEBVTT"):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n'
}

func parseSRT(s string) []Cue {
	lines := strings.Split(s, "\n")
	var cues []Cue
	var cur *Cue
	var text []string
	flush := func() {
		if cur != nil {
			cur.Text = cleanText(text)
			if cur.Text != "" {
				cues = append(cues, *cur)
			}
		}
		cur, text = nil, nil
	}
	for i, line := range lines {
		if start, end, ok := parseTiming(line); ok {
			flush()
			if end > start {
				cur = &Cue{Start: start, End: end}
			}
			continue
		}
		if cur == nil {
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// An index line directly before the next timing line is not text.
		if isDigits(t) && i+1 < len(lines) {
			if _, _, ok := parseTiming(lines[i+1]); ok {
				continue
			}
		}
		text = append(text, line)
	}
	flush()
	sortCues(cues)
	return cues
}

func parseWebVTT(s string) ([]Cue, error) {
	if !hasVTTSignature(s) {
		return nil, ErrNotWebVTT
	}
	lines := strings.Split(s, "\n")
	var cues []Cue
	i := 1 // skip the signature line
	header := true
	for i < len(lines) {
		// Collect one block (up to the next blank line).
		j := i
		for j < len(lines) && strings.TrimSpace(lines[j]) != "" {
			j++
		}
		block := lines[i:j]
		i = j + 1
		if len(block) == 0 {
			header = false
			continue
		}
		// Find the timing line. In a normal cue block it is line 0 or 1; in
		// the header block (no blank line before the first cue) or a damaged
		// block it may be later.
		t := -1
		limit := 2
		if header {
			limit = len(block)
		}
		for k := 0; k < len(block) && k < limit; k++ {
			if strings.Contains(block[k], "-->") {
				t = k
				break
			}
		}
		header = false
		if t < 0 {
			continue // NOTE, STYLE, REGION or junk
		}
		start, end, ok := parseTiming(block[t])
		if !ok || end <= start {
			continue
		}
		text := cleanText(block[t+1:])
		if text == "" {
			continue
		}
		cues = append(cues, Cue{Start: start, End: end, Text: text})
	}
	sortCues(cues)
	return cues, nil
}

func sortCues(cues []Cue) {
	sort.SliceStable(cues, func(a, b int) bool { return cues[a].Start < cues[b].Start })
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseTiming parses "start --> end [settings]".
func parseTiming(line string) (start, end time.Duration, ok bool) {
	if len(line) > 1024 {
		return 0, 0, false
	}
	l, r, found := strings.Cut(line, "-->")
	if !found {
		return 0, 0, false
	}
	start, ok = parseTimestamp(strings.TrimSpace(l))
	if !ok {
		return 0, 0, false
	}
	f := strings.Fields(r)
	if len(f) == 0 {
		return 0, 0, false
	}
	end, ok = parseTimestamp(f[0])
	return start, end, ok
}

// parseTimestamp accepts [hh:]mm:ss[.,]fff with 1+ fraction digits.
func parseTimestamp(s string) (time.Duration, bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	last := parts[len(parts)-1]
	sec, frac := last, ""
	if k := strings.IndexAny(last, ".,"); k >= 0 {
		sec, frac = last[:k], last[k+1:]
		if frac == "" {
			return 0, false
		}
	}
	var h, m int64
	var ok bool
	if len(parts) == 3 {
		if h, ok = atoiBounded(parts[0], 6); !ok {
			return 0, false
		}
	}
	if m, ok = atoiBounded(parts[len(parts)-2], 4); !ok {
		return 0, false
	}
	sv, ok := atoiBounded(sec, 4)
	if !ok {
		return 0, false
	}
	var ns int64
	if frac != "" {
		if !isDigits(frac) {
			return 0, false
		}
		if len(frac) > 9 {
			frac = frac[:9]
		}
		for _, c := range []byte(frac) {
			ns = ns*10 + int64(c-'0')
		}
		for k := len(frac); k < 9; k++ {
			ns *= 10
		}
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(sv)*time.Second + time.Duration(ns), true
}

func atoiBounded(s string, maxDigits int) (int64, bool) {
	if !isDigits(s) || len(s) > maxDigits {
		return 0, false
	}
	var v int64
	for i := 0; i < len(s); i++ {
		v = v*10 + int64(s[i]-'0')
	}
	return v, true
}

var entities = []struct{ name, val string }{
	{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&nbsp;", "\u00A0"},
	{"&quot;", "\""}, {"&lrm;", "\u200E"}, {"&rlm;", "\u200F"},
}

// cleanText strips markup from each line, decodes entities, drops empty
// lines and joins the rest with "\n".
func cleanText(lines []string) string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(stripMarkup(l))
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// stripMarkup removes <tag> and {\override} markup and decodes entities in
// one pass, so an escaped "&lt;i&gt;" survives as literal "<i>".
func stripMarkup(s string) string {
	if !strings.ContainsAny(s, "<{&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '<':
			if i+1 < len(s) && isTagStart(s[i+1]) {
				if k := strings.IndexByte(s[i:], '>'); k > 0 {
					i += k + 1
					continue
				}
			}
		case '{':
			if k := strings.IndexByte(s[i:], '}'); k > 0 {
				body := s[i+1 : i+k]
				if strings.HasPrefix(body, "\\") || isSimpleBraceTag(body) {
					i += k + 1
					continue
				}
			}
		case '&':
			matched := false
			for _, e := range entities {
				if strings.HasPrefix(s[i:], e.name) {
					b.WriteString(e.val)
					i += len(e.name)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isTagStart(c byte) bool {
	return c == '/' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isSimpleBraceTag matches the SRT brace variants {b} {/b} {i} {u} {s}.
func isSimpleBraceTag(body string) bool {
	body = strings.TrimPrefix(body, "/")
	switch body {
	case "b", "i", "u", "s", "B", "I", "U", "S":
		return true
	}
	return false
}

// WriteWebVTT writes cues as a WebVTT file. Times are rounded to the
// millisecond; cues with no text or a non-positive duration are skipped.
func WriteWebVTT(w io.Writer, cues []Cue) error {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	for _, c := range cues {
		start, end := c.Start, c.End
		if start < 0 {
			start = 0
		}
		start, end = start.Round(time.Millisecond), end.Round(time.Millisecond)
		if end <= start {
			continue
		}
		var lines []string
		for _, l := range strings.FieldsFunc(c.Text, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, esc.Replace(l))
			}
		}
		if len(lines) == 0 {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(formatVTTTime(start))
		b.WriteString(" --> ")
		b.WriteString(formatVTTTime(end))
		b.WriteByte('\n')
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteByte('\n')
		if b.Len() > 1<<16 {
			if _, err := io.WriteString(w, b.String()); err != nil {
				return err
			}
			b.Reset()
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func formatVTTTime(d time.Duration) string {
	ms := int64(d / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// maxActive bounds how many overlapping cues are shown together, which also
// bounds the cost of Tx3gSamples on hostile input.
const maxActive = 8

// Tx3gSamples converts cues to 3GPP Timed Text (tx3g) samples that cover
// [0, total) without gaps or overlaps. Each sample is a big-endian uint16
// byte length followed by UTF-8 text; stretches with no caption are empty
// samples (two zero bytes). Where cues overlap, their texts are shown
// together on separate lines. Cues are clipped to [0, total).
func Tx3gSamples(cues []Cue, total time.Duration) []Sample {
	if total <= 0 {
		return nil
	}
	cs := make([]Cue, 0, len(cues))
	for _, c := range cues {
		if c.Start < 0 {
			c.Start = 0
		}
		if c.End > total {
			c.End = total
		}
		if c.End > c.Start && c.Text != "" {
			cs = append(cs, c)
		}
	}
	sortCues(cs)

	var out []Sample
	emit := func(start, end time.Duration, text string) {
		if end <= start {
			return
		}
		if n := len(out); n > 0 && text == "" && len(out[n-1].Data) == 2 {
			out[n-1].Duration += end - start // merge adjacent empty samples
			return
		}
		out = append(out, Sample{Start: start, Duration: end - start, Data: tx3gData(text)})
	}

	var active []Cue
	cursor := time.Duration(0)
	next := 0
	for cursor < total {
		// Drop finished cues, admit the ones that have started.
		k := 0
		for _, a := range active {
			if a.End > cursor {
				active[k] = a
				k++
			}
		}
		active = active[:k]
		for next < len(cs) && cs[next].Start <= cursor {
			if cs[next].End > cursor && len(active) < maxActive {
				active = append(active, cs[next])
			}
			next++
		}
		// The interval runs to the nearest boundary.
		end := total
		if next < len(cs) && cs[next].Start < end {
			end = cs[next].Start
		}
		for _, a := range active {
			if a.End < end {
				end = a.End
			}
		}
		text := ""
		if len(active) == 1 {
			text = active[0].Text
		} else if len(active) > 1 {
			parts := make([]string, len(active))
			for i, a := range active {
				parts[i] = a.Text
			}
			text = strings.Join(parts, "\n")
		}
		emit(cursor, end, text)
		cursor = end
	}
	return out
}

func tx3gData(text string) []byte {
	const max = 0xFFFF
	if len(text) > max {
		n := max
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n]
	}
	d := make([]byte, 2+len(text))
	d[0], d[1] = byte(len(text)>>8), byte(len(text))
	copy(d[2:], text)
	return d
}
