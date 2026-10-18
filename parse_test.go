package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

func TestParsers(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tt := range []struct {
		name   string
		out    string
		format format
		want   []failure
		count  int
	}{
		{"go test: subtests, plain failure, build failure", read("go-test.txt"), formatGo, []failure{
			{Name: "TestTable/one", Where: "example.com/cap/a"},
			{Name: "TestPlain", Where: "example.com/cap/a"},
			{Name: "[build failed]", Where: "example.com/cap/c"},
		}, 3},
		{"go test: passing", "ok  \texample.com/cap/b\t0.108s\n", formatGo, []failure{}, 0},
		{"go test: verbose with a pass", "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n    b_test.go:9: nope\n--- FAIL: TestB (0.01s)\nFAIL\nFAIL\texample.com/x\t0.2s\nFAIL\n",
			formatGo, []failure{{Name: "TestB", Where: "example.com/x"}}, 1},
		{"vitest", read("vitest.txt"), formatVitest, []failure{
			{Name: "math > adds", Where: "src/math.test.ts"},
			{Name: "top level fails", Where: "src/math.test.ts"},
		}, 2},
		{"vitest: colours stripped", "\x1b[31m FAIL \x1b[39m src/a.test.ts > adds\n\x1b[2m      Tests \x1b[22m \x1b[1m\x1b[31m1 failed\x1b[39m\x1b[22m\n",
			formatVitest, []failure{{Name: "adds", Where: "src/a.test.ts"}}, 1},
		{"jest", read("jest.txt"), formatJest, []failure{
			{Name: "math › adds", Where: "./math.test.js"},
			{Name: "top level fails", Where: "./math.test.js"},
		}, 2},
		{"jest: summary only", "Tests:       4 failed, 2 passed, 6 total\n", formatJest, []failure{}, 4},
		{"unknown", "Traceback (most recent call last):\nAssertionError\n", formatUnknown, []failure{}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := parseOutput(tt.out)
			if p.format != tt.format {
				t.Errorf("format = %q, want %q", p.format, tt.format)
			}
			got := p.Failures()
			if got == nil {
				got = []failure{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("failures =\n%#v\nwant\n%#v", got, tt.want)
			}
			if c := p.FailedCount(); c != tt.count {
				t.Errorf("count = %d, want %d", c, tt.count)
			}
		})
	}
}

func TestLineTone(t *testing.T) {
	for _, tt := range []struct {
		line string
		want ui.Tone
	}{
		{"--- FAIL: TestX (0.00s)", ui.ToneError},
		{"    --- FAIL: TestX/sub (0.00s)", ui.ToneError},
		{"FAIL\texample.com/x\t0.1s", ui.ToneError},
		{" FAIL  src/a.test.ts > adds", ui.ToneError},
		{"   × math > adds 4ms", ui.ToneError},
		{"  ● math › adds", ui.ToneError},
		{"panic: runtime error", ui.ToneError},
		{"warning: unused variable", ui.ToneAttention},
		{"npm WARN deprecated foo@1", ui.ToneAttention},
		{"ok  \texample.com/x\t0.1s", ui.ToneNeutral},
		{"=== RUN   TestX", ui.ToneNeutral},
	} {
		if got := lineTone(tt.line); got != tt.want {
			t.Errorf("lineTone(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestExcerpt(t *testing.T) {
	var fails []failure
	for i := 0; i < 50; i++ {
		fails = append(fails, failure{Name: "TestSomethingRatherLong" + strings.Repeat("x", 40) + itoa(i), Where: "example.com/pkg"})
	}
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, "line "+itoa(i)+" "+strings.Repeat("y", 100))
	}
	for _, tt := range []struct {
		name     string
		fails    []failure
		count    int
		lines    []string
		contains []string
		absent   []string
	}{
		{"small", fails[:2], 2, lines[:3], []string{"`go test ./...`", "Failing tests (2):", fails[0].Title(), "line 2 "}, nil},
		{"huge: output trimmed from the front", fails, 50, lines, []string{"Failing tests (50):", "… and 30 more", "line 499 "}, []string{"line 400 "}},
		{"count only", nil, 7, lines[:1], []string{"7 tests failed."}, []string{"Failing tests"}},
		{"one enormous line", fails[:1], 1, []string{strings.Repeat("z", 10000)}, []string{fails[0].Title()}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := excerpt("go test ./...", tt.fails, tt.count, tt.lines)
			if len(got) > maxExcerpt {
				t.Errorf("excerpt is %d bytes, cap %d", len(got), maxExcerpt)
			}
			if !strings.HasPrefix(got, "The tests in this worktree are failing. Please") {
				t.Errorf("not framed as a request: %.80q", got)
			}
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("excerpt lacks %q", c)
				}
			}
			for _, c := range tt.absent {
				if strings.Contains(got, c) {
					t.Errorf("excerpt has %q", c)
				}
			}
		})
	}
}

func TestFailureLines(t *testing.T) {
	data, err := os.ReadFile("testdata/go-test.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	got := failureLines(failure{Name: "TestPlain"}, lines)
	want := []string{"--- FAIL: TestPlain (0.00s)", "    a_test.go:17: boom"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("failureLines = %q, want %q", got, want)
	}
}

func TestDetectCommand(t *testing.T) {
	for _, tt := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"go.mod": "module x\n", "Makefile": "test:\n\tgo test\n"}, "go test ./..."},
		{map[string]string{"package.json": "{}"}, "npm test"},
		{map[string]string{"Makefile": "build:\n\techo\ntest: build\n\techo\n"}, "make test"},
		{map[string]string{"Makefile": "build:\n\techo\n", "pyproject.toml": ""}, "pytest"},
		{map[string]string{"README.md": "hi"}, ""},
	} {
		dir := t.TempDir()
		for name, body := range tt.files {
			if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := detectCommand(dir); got != tt.want {
			t.Errorf("detectCommand(%v) = %q, want %q", tt.files, got, tt.want)
		}
	}
}
