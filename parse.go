package main

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

// failure is one failing test parsed from a run's output.
type failure struct {
	// Name is the test ("TestAdd", "TestAdd/zero", "suite > adds"), or a package's
	// "[build failed]" note.
	Name string
	// Where is the package (go test) or file (vitest, jest), when the output says.
	Where string
}

// Title is the failure as one line: "TestAdd (example.com/calc)".
func (f failure) Title() string {
	if f.Where == "" {
		return f.Name
	}
	return f.Name + " (" + f.Where + ")"
}

// format is the test runner the output came from, when a parser recognised it.
type format string

const (
	formatUnknown format = ""
	formatGo      format = "go test"
	formatVitest  format = "vitest"
	formatJest    format = "jest"
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

	goFailRe    = regexp.MustCompile(`^(\s*)--- FAIL: (\S+)`)
	goPkgFailRe = regexp.MustCompile(`^FAIL\s+(\S+)(?:\s+\[([^\]]+)\]|\s+[0-9.]+s)?\s*$`)
	goPkgOKRe   = regexp.MustCompile(`^ok\s+\S+\s`)
	goPassRe    = regexp.MustCompile(`^(\s*)--- PASS: `)

	// vitest: " FAIL  src/a.test.ts > math > adds" (the failed tests section).
	vitestFailRe = regexp.MustCompile(`^\s*FAIL\s+(\S+\.(?:[cm]?[jt]sx?))\s+>\s+(.+?)\s*$`)
	// vitest: " Tests  2 failed | 5 passed (7)".
	vitestSumRe = regexp.MustCompile(`^\s*Tests\s+(\d+) failed\b`)
	// vitest's " ❯ src/a.test.ts (3 tests | 1 failed) 12ms" and " RUN  v1.6.0".
	vitestMarkRe = regexp.MustCompile(`^\s*(?:RUN\s+v\d|❯ \S+ \(\d+ tests?)`)

	// jest: "FAIL src/a.test.js" then "  ● math › adds".
	jestFileRe = regexp.MustCompile(`^\s*FAIL\s+(\S+\.(?:[cm]?[jt]sx?))\s*$`)
	jestTestRe = regexp.MustCompile(`^\s*● (.+ › .+|[^●]+?)\s*$`)
	// jest: "Tests:       1 failed, 2 passed, 3 total".
	jestSumRe = regexp.MustCompile(`^\s*Tests:\s+(\d+) failed\b`)

	failToneRe = regexp.MustCompile(`(^\s*--- FAIL|^FAIL\b|^\s+FAIL\s|^\s*(?:×|✗|✕|●)\s|^panic:|\bfailed\b|^\s*Error:|^npm ERR!|^make: \*\*\*)`)
	warnToneRe = regexp.MustCompile(`(?i)(\bwarn(?:ing)?\b|\bdeprecated\b)`)
)

// stripANSI removes colour and cursor escapes (the log node does not interpret them).
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// lineTone colours one output line: red for failures, amber for warnings.
func lineTone(line string) ui.Tone {
	switch {
	case failToneRe.MatchString(line):
		return ui.ToneError
	case warnToneRe.MatchString(line):
		return ui.ToneAttention
	}
	return ui.ToneNeutral
}

// parser reads a run's output a line at a time and collects failing tests for the
// formats it knows: go test, vitest and jest.
type parser struct {
	format   format
	failures []failure
	// failedCount is a summary line's count (vitest, jest), 0 when none was seen.
	failedCount int

	// go test: failures seen since the last package line, waiting for their package.
	goPending []int
	// jest: the file of the last "FAIL <file>" line.
	jestFile string
	seen     map[string]bool
}

// Line feeds one line (without its newline, ANSI already stripped).
func (p *parser) Line(line string) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	switch {
	case goFailRe.MatchString(line):
		m := goFailRe.FindStringSubmatch(line)
		p.format = formatGo
		p.goPending = append(p.goPending, len(p.failures))
		p.add(failure{Name: m[2]})
	case vitestFailRe.MatchString(line):
		m := vitestFailRe.FindStringSubmatch(line)
		p.format = formatVitest
		p.add(failure{Name: m[2], Where: m[1]})
	case jestFileRe.MatchString(line):
		// "FAIL src/a.test.js": a file, not a Go package.
		p.jestLine(line)
	case goPkgFailRe.MatchString(line):
		m := goPkgFailRe.FindStringSubmatch(line)
		pkg, note := m[1], m[2]
		p.format = formatGo
		for _, i := range p.goPending {
			p.failures[i].Where = pkg
		}
		if len(p.goPending) == 0 && note != "" {
			// "FAIL pkg [build failed]" or "[setup failed]": no test ran.
			p.add(failure{Name: "[" + note + "]", Where: pkg})
		}
		p.goPending = nil
	case goPkgOKRe.MatchString(line), goPassRe.MatchString(line):
		p.format = formatGo
	case vitestSumRe.MatchString(line):
		p.format = formatVitest
		p.failedCount, _ = strconv.Atoi(vitestSumRe.FindStringSubmatch(line)[1])
	case vitestMarkRe.MatchString(line):
		if p.format == formatUnknown {
			p.format = formatVitest
		}
	default:
		p.jestLine(line)
	}
}

func (p *parser) jestLine(line string) {
	switch {
	case jestFileRe.MatchString(line):
		if p.format != formatVitest {
			p.format = formatJest
		}
		p.jestFile = jestFileRe.FindStringSubmatch(line)[1]
	case jestSumRe.MatchString(line):
		p.format = formatJest
		p.failedCount, _ = strconv.Atoi(jestSumRe.FindStringSubmatch(line)[1])
	case p.format == formatJest && jestTestRe.MatchString(line):
		name := jestTestRe.FindStringSubmatch(line)[1]
		if strings.HasPrefix(name, "Console") {
			return
		}
		p.add(failure{Name: name, Where: p.jestFile})
	}
}

func (p *parser) add(f failure) {
	k := f.Name + "\x00" + f.Where
	if p.seen[k] {
		return
	}
	p.seen[k] = true
	p.failures = append(p.failures, f)
}

// Failures are the failing tests found so far, in output order. For go test a parent
// test whose subtests failed is dropped in favour of them ("TestA/sub" over "TestA").
func (p *parser) Failures() []failure {
	if p.format != formatGo {
		return append([]failure(nil), p.failures...)
	}
	out := make([]failure, 0, len(p.failures))
	for _, f := range p.failures {
		parent := false
		for _, g := range p.failures {
			if g.Where == f.Where && strings.HasPrefix(g.Name, f.Name+"/") {
				parent = true
				break
			}
		}
		if !parent {
			out = append(out, f)
		}
	}
	return out
}

// FailedCount is how many tests failed: the parsed list, else a summary line's count.
func (p *parser) FailedCount() int {
	if n := len(p.Failures()); n > 0 {
		return n
	}
	return p.failedCount
}

// parseOutput runs a parser over a whole output (tests use it).
func parseOutput(out string) *parser {
	p := &parser{}
	for _, l := range strings.Split(stripANSI(out), "\n") {
		p.Line(strings.TrimRight(l, "\r"))
	}
	return p
}
