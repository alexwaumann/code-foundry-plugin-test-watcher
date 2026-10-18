package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// Runs in a plugin terminal (permission terminals). The Output tab then shows the run
// in a real terminal (`terminal` node, with the host's "Open in bottom panel"), while
// the plugin still needs the output for the failures list, the flag and "Send failures
// to Claude", and the exit status. The terminal runs the command under script(1),
// which records everything the terminal shows into a file in the data dir (flushed on
// every write) and exits with the command's status:
//
//	/usr/bin/script -q -F <out> /bin/sh -c 'echo $$ > "$1"; exec "$2" "$3" "$4"' sh <pid> $SHELL -lc <command>
//
// The plugin tails <out> into the run, and reads the exit status from terminal/list.
// The inner shell writes its pid, which is the run's process group (script makes it a
// session leader), so Stop signals the group and the terminal stays with the output up
// to the stop; without a pid it closes the terminal.

// scriptPath is macOS's script(1).
const scriptPath = "/usr/bin/script"

// errNoTerminal wraps a refused terminal/create: the run goes on as a plain process.
var errNoTerminal = errors.New("no terminal")

// terminalHost is the host when runs in dir go into a plugin terminal: the host draws
// terminal nodes and makes plugin terminals, the user granted terminals, and the
// worktree is under the home directory (where a terminal's cwd must be). nil: run as
// a plain process and stream into a log node.
func (a *app) terminalHost(dir string) *pluginsdk.Host {
	h := a.host()
	if h == nil || !terminalsUsable(h) {
		return nil
	}
	home := a.cfg.home()
	if home == "" || !strings.HasPrefix(dir, strings.TrimSuffix(home, "/")+"/") {
		return nil
	}
	return h
}

func terminalsUsable(h *pluginsdk.Host) bool {
	return h.Granted().Terminals && h.HasCapability("terminals") && h.HasCapability("view.node.terminal")
}

// terminalArgv is the terminal's program: script recording into out, the inner shell
// writing its pid to pidFile, then the user's login shell running the command.
func terminalArgv(out, pidFile, shell, flag, command string) []string {
	return []string{scriptPath, "-q", "-F", out, "/bin/sh", "-c", `echo $$ > "$1"; exec "$2" "$3" "$4"`, "sh", pidFile, shell, flag, command}
}

// terminalEnv is what a terminal run adds to the environment: no watch modes or
// prompts (vitest and jest watch on a terminal unless CI is set). Colours stay: it is
// a real terminal, and the plugin strips them from what it parses.
var terminalEnv = map[string]string{"CI": "true", "GIT_TERMINAL_PROMPT": "0"}

// runFiles are the output and pid files of a worktree's terminal runs.
func runFiles(dataDir, dir string) (out, pid string) {
	sum := sha256.Sum256([]byte(dir))
	base := filepath.Join(dataDir, "runs", hex.EncodeToString(sum[:8]))
	return base + ".out", base + ".pid"
}

// execTerminal runs rn in a new plugin terminal and waits for it; it returns the exit
// status, or an error when the terminal could not start.
func (r *runner) execTerminal(ctx context.Context, h *pluginsdk.Host, rn *run) (int, error) {
	out, pidFile := runFiles(h.DataDir(), r.dir)
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return 0, err
	}
	_ = os.Remove(pidFile)
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// The previous run's terminal goes (start waited for that run to end).
	r.mu.Lock()
	prev := r.term
	r.term = ""
	r.mu.Unlock()
	if prev != "" {
		r.closeTerminal(h, prev)
	}

	shell, flag := r.a.cfg.shell()
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	id, err := h.CreateTerminal(cctx, pluginsdk.TerminalCreateParams{
		Argv:  terminalArgv(out, pidFile, shell, flag, rn.Command),
		Cwd:   r.dir,
		Env:   terminalEnv,
		Title: truncRunes(rn.Command, 80),
	})
	cancel()
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errNoTerminal, err)
	}
	r.mu.Lock()
	r.term, rn.Terminal, rn.TermOpen = id, id, true
	r.mu.Unlock()
	r.a.changed(r.dir)
	r.a.log().Info("test run started", "dir", r.dir, "run", rn.ID, "command", rn.Command, "terminal", id)

	tail := &tailer{f: f}
	tick := time.NewTicker(r.a.cfg.termPoll)
	defer tick.Stop()
	stop := ctx.Done()
	var killAt time.Time
	for n := 0; ; n++ {
		select {
		case <-stop:
			stop = nil
			r.signal(h, rn, pidFile, syscall.SIGTERM)
			killAt = time.Now().Add(killGrace)
		case <-tick.C:
		}
		tail.read(func(line string) { r.output(rn, line) })
		if !killAt.IsZero() && time.Now().After(killAt) {
			r.signal(h, rn, pidFile, syscall.SIGKILL)
			killAt = time.Time{}
		}
		if n%2 == 1 && stop != nil {
			continue // the terminal's state every other tick, unless stopping
		}
		state, exit, ok := terminalState(h, id)
		if ok && state == "running" {
			continue
		}
		tail.read(func(line string) { r.output(rn, line) })
		tail.flush(func(line string) { r.output(rn, line) })
		if !ok { // closed: by a stop without a pid, or by the user
			r.mu.Lock()
			rn.TermOpen = false
			if r.term == id {
				r.term = ""
			}
			r.mu.Unlock()
			return -1, nil
		}
		return exit, nil
	}
}

// terminalState is a plugin terminal's state and exit status; ok false when it is gone.
func terminalState(h *pluginsdk.Host, id string) (state string, exit int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	terms, err := h.Terminals(ctx)
	if err != nil {
		return "running", 0, true // try again next tick
	}
	for _, t := range terms {
		if t.TerminalID == id {
			return t.State, t.ExitCode, true
		}
	}
	return "", 0, false
}

// signal sends sig to the run's process group; without one (no pid yet, or a host
// that does not run the argv) it closes the terminal.
func (r *runner) signal(h *pluginsdk.Host, rn *run, pidFile string, sig syscall.Signal) {
	data, _ := os.ReadFile(pidFile)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
		if err := syscall.Kill(-pid, sig); err == nil || errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	r.closeTerminal(h, rn.Terminal)
}

func (r *runner) closeTerminal(h *pluginsdk.Host, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.CloseTerminal(ctx, id); err != nil && pluginsdk.ErrorCode(err) != pluginsdk.CodeNotFound {
		r.a.log().Warn("closing a terminal", "terminal", id, "err", err)
	}
}

// closeStaleTerminals closes the terminals an earlier process of the plugin left (a
// crash or restart keeps them; the runs they belonged to are gone).
func (a *app) closeStaleTerminals(ctx context.Context, h *pluginsdk.Host) {
	if !terminalsUsable(h) {
		return
	}
	terms, err := h.Terminals(ctx)
	if err != nil {
		a.log().Warn("listing terminals", "err", err)
		return
	}
	for _, t := range terms {
		if err := h.CloseTerminal(ctx, t.TerminalID); err != nil {
			a.log().Warn("closing a stale terminal", "terminal", t.TerminalID, "err", err)
		}
	}
}

// tailer reads a growing file a line at a time.
type tailer struct {
	f       *os.File
	partial []byte
	buf     [32 << 10]byte
}

// read hands every complete new line to fn.
func (t *tailer) read(fn func(string)) {
	for {
		n, err := t.f.Read(t.buf[:])
		t.partial = append(t.partial, t.buf[:n]...)
		for {
			i := bytes.IndexByte(t.partial, '\n')
			if i < 0 {
				break
			}
			fn(string(t.partial[:i+1]))
			t.partial = t.partial[i+1:]
		}
		if len(t.partial) > maxLineLen*4 { // a very long line: hand it over in pieces
			fn(string(t.partial))
			t.partial = nil
		}
		if n == 0 || err != nil { // io.EOF until the terminal writes more
			return
		}
	}
}

// flush hands over a last line without a newline.
func (t *tailer) flush(fn func(string)) {
	if len(t.partial) > 0 {
		fn(string(t.partial))
		t.partial = nil
	}
}
