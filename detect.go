package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
)

var makeTestRe = regexp.MustCompile(`^test\s*:`)

// detectCommand guesses a worktree's test command from the files at its root, in
// order: go.mod, package.json, a Makefile with a test target, pyproject.toml. "" when
// none fits.
func detectCommand(dir string) string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case dir == "":
		return ""
	case exists("go.mod"):
		return "go test ./..."
	case exists("package.json"):
		return "npm test"
	case makefileHasTest(filepath.Join(dir, "Makefile")):
		return "make test"
	case exists("pyproject.toml"):
		return "pytest"
	}
	return ""
}

func makefileHasTest(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if makeTestRe.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}
