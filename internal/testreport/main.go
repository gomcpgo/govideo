// Command testreport runs go test and says which tests were skipped.
//
//	go run ./internal/testreport ./...
//
// Its arguments are go test's. It prints what go test prints without -v (the
// output of failed tests and a line per package), then every skipped test
// with the reason it gave, and exits with go test's status. In GitHub
// Actions the list also goes to the job summary.
//
// Plain go test shows a skipped test exactly as it shows one that passed, and
// many tests here skip when ffmpeg, one of its encoders or the system codecs
// are missing: without the list nothing says whether a runner exercised them.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

func main() {
	cmd := exec.Command("go", append([]string{"test", "-json"}, os.Args[1:]...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(2)
	}
	skips, readErr := report(stdout, os.Stdout)
	runErr := cmd.Wait()

	list(os.Stdout, skips)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if err := appendFile(path, markdown(skips)); err != nil {
			fmt.Fprintln(os.Stderr, "testreport: job summary:", err)
		}
	}
	var exit *exec.ExitError
	switch {
	case errors.As(runErr, &exit):
		os.Exit(exit.ExitCode())
	case runErr != nil:
		fmt.Fprintln(os.Stderr, "testreport:", runErr)
		os.Exit(2)
	case readErr != nil:
		fmt.Fprintln(os.Stderr, "testreport:", readErr)
		os.Exit(2)
	}
}

// event is one line of go test -json (cmd/test2json, plus the build events
// of cmd/go).
type event struct {
	Action     string
	Package    string
	ImportPath string
	Test       string
	Output     string
}

// skip is a test that skipped itself.
type skip struct {
	Package, Test string
	Reason        string // what it logged last, lines joined by "\n"
}

// line is a line of a package's output and the test that wrote it ("" for
// the package itself).
type line struct{ test, text string }

// pkg is what is known of a package whose tests are running.
type pkg struct {
	lines []line
	ended map[string]string // test → "pass", "fail" or "skip"
}

// report reads go test -json events and writes to w what go test without -v
// would have: for a package that fails, the output of the tests that failed
// or never finished; for every package, its own lines (ok, FAIL, no test
// files). It returns the tests that skipped: package by package, and within
// a package in the order they ran.
func report(events io.Reader, w io.Writer) (skips []skip, err error) {
	// Packages run at once, so their events arrive interleaved.
	defer func() {
		sort.SliceStable(skips, func(i, j int) bool { return skips[i].Package < skips[j].Package })
	}()
	pkgs := map[string]*pkg{}
	r := bufio.NewReader(events)
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			var ev event
			if json.Unmarshal(raw, &ev) != nil || ev.Action == "" {
				// Not an event: something wrote to standard output
				// around go test's encoder. Show it.
				w.Write(raw)
			} else if s, ok := handle(pkgs, ev, w); ok {
				skips = append(skips, s)
			}
		}
		if err == io.EOF {
			return skips, nil
		}
		if err != nil {
			return skips, err
		}
	}
}

// handle takes one event; it returns a skip when the event ends a test that
// skipped.
func handle(pkgs map[string]*pkg, ev event, w io.Writer) (skip, bool) {
	if ev.Package == "" {
		// A build event: compiler output is wanted, the rest is not.
		if ev.Action == "build-output" {
			io.WriteString(w, ev.Output)
		}
		return skip{}, false
	}
	p := pkgs[ev.Package]
	if p == nil {
		p = &pkg{ended: map[string]string{}}
		pkgs[ev.Package] = p
	}
	switch ev.Action {
	case "output":
		p.lines = append(p.lines, line{ev.Test, ev.Output})
	case "pass", "fail", "skip":
		if ev.Test == "" {
			p.flush(w, ev.Action == "fail")
			delete(pkgs, ev.Package)
			break
		}
		p.ended[ev.Test] = ev.Action
		if ev.Action == "skip" {
			return skip{ev.Package, ev.Test, p.reason(ev.Test)}, true
		}
	}
	return skip{}, false
}

// flush writes a finished package's output: its own lines, and, if it
// failed, those of every test that did not pass or skip.
func (p *pkg) flush(w io.Writer, failed bool) {
	for _, l := range p.lines {
		switch {
		case l.test == "":
			if l.text != "PASS\n" {
				io.WriteString(w, l.text)
			}
		case failed && p.ended[l.test] != "pass" && p.ended[l.test] != "skip":
			io.WriteString(w, l.text)
		}
	}
}

var (
	// marker matches the lines the testing package writes about a test
	// ("=== RUN", "--- SKIP:"), indented for subtests.
	marker = regexp.MustCompile(`^\s*(=== |--- )`)
	// logged matches the start of a message written with t.Log or t.Skip.
	logged = regexp.MustCompile(`^\s*[^\s:]+\.go:\d+: `)
)

// reason returns the last message a test logged, which for one that skipped
// is why: t.Skip logs and then stops the test.
func (p *pkg) reason(test string) string {
	var msg []string
	for _, l := range p.lines {
		if l.test != test || marker.MatchString(l.text) {
			continue
		}
		if logged.MatchString(l.text) {
			msg = msg[:0]
		}
		if text := strings.TrimSpace(l.text); text != "" {
			msg = append(msg, text)
		}
	}
	return strings.Join(msg, "\n")
}

// list writes the skipped tests as text.
func list(w io.Writer, skips []skip) {
	if len(skips) == 0 {
		fmt.Fprintln(w, "\nNo tests were skipped.")
		return
	}
	fmt.Fprintf(w, "\nSkipped tests (%d):\n", len(skips))
	for _, s := range skips {
		fmt.Fprintf(w, "  %s %s\n", s.Package, s.Test)
		for _, l := range strings.Split(s.Reason, "\n") {
			fmt.Fprintf(w, "      %s\n", l)
		}
	}
}

// markdown returns the skipped tests as a table for the job summary, with
// the first line of each reason.
func markdown(skips []skip) string {
	if len(skips) == 0 {
		return "\nNo tests were skipped.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n### Skipped tests (%d)\n\n| Package | Test | Reason |\n|---|---|---|\n", len(skips))
	for _, s := range skips {
		first, _, more := strings.Cut(s.Reason, "\n")
		if r := []rune(first); len(r) > 300 {
			first, more = string(r[:300]), true
		}
		if more {
			first += " …"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(s.Package), cell(s.Test), cell(first))
	}
	return b.String()
}

// cell escapes text for a Markdown table cell.
var cell = strings.NewReplacer("|", `\|`, "<", "&lt;", "`", "'", "*", `\*`, "_", `\_`, "[", `\[`).Replace

// appendFile adds text to the end of a file.
func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
