package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// module is a small module whose tests end every way a test can: its real
// go test -json output is what report is tested on, so the test follows the
// format as the toolchain writes it.
var module = map[string]string{
	"go.mod": "module example\n\ngo 1.24\n",
	"a/a_test.go": `package a

import "testing"

func TestPass(t *testing.T) { t.Log("quiet in a") }

func TestSkip(t *testing.T) {
	t.Log("something first")
	t.Skip("no encoder | here\nand a second line")
}

func need(t *testing.T) {
	t.Helper()
	t.Skipf("the %s is missing", "oracle")
}

func TestSub(t *testing.T) {
	t.Run("runs", func(t *testing.T) {})
	t.Run("skips", func(t *testing.T) { need(t) })
}
`,
	"b/b_test.go": `package b

import "testing"

func TestQuiet(t *testing.T) { t.Log("quiet in b") }

func TestFail(t *testing.T) {
	t.Log("before failing")
	t.Error("this one fails")
}

func TestSkipB(t *testing.T) { t.SkipNow() }
`,
	"c/c_test.go": `package c

import "testing"

func TestBroken(t *testing.T) { undefinedName() }
`,
	"d/d.go": "package d\n",
}

func TestReport(t *testing.T) {
	dir := t.TempDir()
	for name, text := range module {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-json", "./...")
	cmd.Dir = dir
	// The module is not this one: whatever go test was asked for here does
	// not apply to it.
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	events, err := cmd.Output()
	if err == nil {
		t.Fatal("go test passed a module with a failing test")
	}
	if len(events) == 0 {
		t.Fatalf("go test wrote nothing: %v", err)
	}

	var out bytes.Buffer
	skips, err := report(bytes.NewReader(events), &out)
	if err != nil {
		t.Fatal(err)
	}
	want := []skip{
		{"example/a", "TestSkip", "a_test.go:9: no encoder | here\nand a second line"},
		{"example/a", "TestSub/skips", "a_test.go:19: the oracle is missing"},
		{"example/b", "TestSkipB", ""},
	}
	if !slices.Equal(skips, want) {
		t.Errorf("skips: got %q, want %q", skips, want)
	}

	got := out.String()
	for _, s := range []string{
		"ok  \texample/a",
		"--- FAIL: TestFail",
		"before failing",
		"this one fails",
		"FAIL\texample/b",
		"undefinedName",
		"FAIL\texample/c [build failed]",
		"example/d\t[no test files]",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("the output lacks %q", s)
		}
	}
	for _, s := range []string{"quiet in", "TestPass", "TestQuiet", "TestSkip", "PASS\n", `{"`} {
		if strings.Contains(got, s) {
			t.Errorf("the output has %q, which go test without -v does not show", s)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", got)
	}

	var text bytes.Buffer
	list(&text, skips)
	for _, s := range []string{"Skipped tests (3):", "example/a TestSub/skips", "      and a second line"} {
		if !strings.Contains(text.String(), s) {
			t.Errorf("the list lacks %q:\n%s", s, text.String())
		}
	}
	md := markdown(skips)
	for _, s := range []string{"### Skipped tests (3)", `| example/a | TestSkip | a\_test.go:9: no encoder \| here … |`, "| example/b | TestSkipB |  |"} {
		if !strings.Contains(md, s) {
			t.Errorf("the summary lacks %q:\n%s", s, md)
		}
	}
	if list(&text, nil); !strings.Contains(text.String(), "No tests were skipped") || !strings.Contains(markdown(nil), "No tests were skipped") {
		t.Error("an empty list does not say that nothing was skipped")
	}
}

// TestReportOtherOutput checks that lines which are not events are passed
// on, and that a package cut short still shows what its tests wrote.
func TestReportOtherOutput(t *testing.T) {
	in := "not json at all\n" +
		`{"Action":"output","Package":"p","Test":"TestHang","Output":"=== RUN   TestHang\n"}` + "\n" +
		`{"Action":"output","Package":"p","Test":"TestHang","Output":"panic: test timed out\n"}` + "\n" +
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t1.0s\n"}` + "\n" +
		`{"Action":"fail","Package":"p"}`
	var out bytes.Buffer
	skips, err := report(strings.NewReader(in), &out)
	if err != nil || len(skips) != 0 {
		t.Fatalf("report: %v, %q", err, skips)
	}
	if want := "not json at all\n=== RUN   TestHang\npanic: test timed out\nFAIL\tp\t1.0s\n"; out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
