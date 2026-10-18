package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxExcerpt caps what "Send failures to Claude" sends.
const (
	maxExcerpt      = 4096
	excerptLines    = 60
	excerptFailures = 20
)

// excerpt frames a failing run as a request to fix it: the command, the failing
// tests and the output's last lines, at most maxExcerpt bytes. Output lines go first
// when it is too long, then failures.
func excerpt(command string, failures []failure, failedCount int, lines []string) string {
	if len(lines) > excerptLines {
		lines = lines[len(lines)-excerptLines:]
	}
	fails := failures
	if len(fails) > excerptFailures {
		fails = fails[:excerptFailures]
	}
	build := func(fails []failure, lines []string) string {
		var b strings.Builder
		b.WriteString("The tests in this worktree are failing. Please find the cause and fix it (the code, or the test if the test is wrong), then run the command again to confirm they pass.\n\nCommand: `")
		b.WriteString(command)
		b.WriteString("`\n")
		switch {
		case len(failures) > 0:
			b.WriteString("\nFailing tests (" + strconv.Itoa(len(failures)) + "):\n")
			for _, f := range fails {
				b.WriteString("- " + f.Title() + "\n")
			}
			if len(failures) > len(fails) {
				b.WriteString("- … and " + strconv.Itoa(len(failures)-len(fails)) + " more\n")
			}
		case failedCount > 0:
			b.WriteString("\n" + strconv.Itoa(failedCount) + " tests failed.\n")
		}
		if len(lines) > 0 {
			b.WriteString("\nLast output lines:\n```\n")
			for _, l := range lines {
				b.WriteString(l + "\n")
			}
			b.WriteString("```\n")
		}
		return b.String()
	}
	s := build(fails, lines)
	for len(s) > maxExcerpt && len(lines) > 0 {
		lines = lines[1:]
		s = build(fails, lines)
	}
	for len(s) > maxExcerpt && len(fails) > 1 {
		fails = fails[:len(fails)-1]
		s = build(fails, lines)
	}
	return truncBytes(s, maxExcerpt)
}

// failureLines picks a failure's own lines from the output. For go test: its
// "--- FAIL:" line and the indented log lines under it. Otherwise: from the first line
// that names it, up to 40 lines.
func failureLines(f failure, lines []string) []string {
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if t != "--- FAIL: "+f.Name && !strings.HasPrefix(t, "--- FAIL: "+f.Name+" ") {
			continue
		}
		indent := len(l) - len(t)
		end := i + 1
		for ; end < len(lines) && end-i < 40; end++ {
			n := lines[end]
			if strings.TrimSpace(n) != "" && len(n)-len(strings.TrimLeft(n, " \t")) <= indent {
				break
			}
			if strings.HasPrefix(strings.TrimLeft(n, " \t"), "--- ") {
				break
			}
		}
		return lines[i:end]
	}
	for i, l := range lines {
		if strings.Contains(l, f.Name) {
			return lines[i:min(len(lines), i+40)]
		}
	}
	return nil
}

func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func itoa(n int) string { return strconv.Itoa(n) }
