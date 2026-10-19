package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

// Output caps: what the plugin keeps of one run, the host's own per-log-node limits.
const (
	maxLines    = 2000
	maxBytes    = 256 << 10
	maxLineLen  = 2000
	killGrace   = 2 * time.Second
	flushPeriod = 200 * time.Millisecond
)

// runState is where a run is.
type runState string

const (
	stateRunning runState = "running"
	statePassed  runState = "passed"
	stateFailed  runState = "failed"
	stateStopped runState = "stopped" // the user stopped it, or a newer run replaced it
	stateError   runState = "error"   // it could not run: command not found, worktree gone
)

// outLine is one line of a run's output, numbered so a surface knows what it sent.
type outLine struct {
	Seq  int
	Text string
	Tone ui.Tone
}

// run is one execution of the test command in a worktree.
type run struct {
	ID      int
	Command string
	Trigger string // "change" or "manual"
	Started time.Time
	Ended   time.Time
	State   runState
	Exit    int
	// Problem says why a run is in stateError ("command not found: gotest").
	Problem string
	// Superseded: a newer run replaced this one (it is not a result).
	Superseded bool
	// Terminal is the plugin terminal the run executes in ("" when it runs as a plain
	// process); TermOpen while the terminal exists (a stop that had to close it, or
	// the user closing it, ends that).
	Terminal string
	TermOpen bool

	parser parser
	lines  []outLine
	bytes  int
	// Dropped counts lines cut from the head to stay under the caps.
	Dropped int
	nextSeq int
	// Gen counts the times the output was replaced as a whole (a terminal read with
	// the scrollback), so a view re-sends its log instead of appending.
	Gen int
}

// runSnap is a copy of a run for rendering.
type runSnap struct {
	ID          int
	Command     string
	Trigger     string
	Started     time.Time
	Ended       time.Time
	State       runState
	Exit        int
	Problem     string
	Failures    []failure
	FailedCount int
	Format      format
	Lines       []outLine
	Dropped     int
	Gen         int
	Terminal    string
	TermOpen    bool
}

func (r *run) snap(withLines bool) *runSnap {
	if r == nil {
		return nil
	}
	s := &runSnap{ID: r.ID, Command: r.Command, Trigger: r.Trigger, Started: r.Started, Ended: r.Ended,
		State: r.State, Exit: r.Exit, Problem: r.Problem, Failures: r.parser.Failures(),
		FailedCount: r.parser.FailedCount(), Format: r.parser.format, Dropped: r.Dropped, Gen: r.Gen,
		Terminal: r.Terminal, TermOpen: r.TermOpen}
	if withLines {
		s.Lines = append([]outLine(nil), r.lines...)
	}
	return s
}

func (r *run) addLine(text string, tone ui.Tone) {
	if len(text) > maxLineLen {
		text = text[:maxLineLen] + "…"
	}
	r.nextSeq++
	r.lines = append(r.lines, outLine{Seq: r.nextSeq, Text: text, Tone: tone})
	r.bytes += len(text) + 1
	for len(r.lines) > 1 && (len(r.lines) > maxLines || r.bytes > maxBytes) {
		r.bytes -= len(r.lines[0].Text) + 1
		r.lines = r.lines[1:]
		r.Dropped++
	}
}

// runner owns the test runs of one worktree: at most one process at a time, newest wins.
type runner struct {
	a   *app
	dir string

	mu      sync.Mutex
	cur     *run // the latest run, running or finished
	result  *run // the latest finished run with a result (passed, failed, error)
	cancel  context.CancelFunc
	done    chan struct{} // closed when cur's process is gone
	nextID  int
	timer   *time.Timer
	flush   *time.Timer
	changed map[string]bool // files changed since the latest run started
	print   map[string]string
	gone    bool
	closed  bool
	// term is the plugin terminal of the latest terminal run, kept open (exited, its
	// output still on screen) until the next run replaces it.
	term string
}

func newRunner(a *app, dir string) *runner {
	r := &runner{a: a, dir: dir, changed: map[string]bool{}}
	if a.cfg.poll > 0 {
		go r.pollLoop()
	}
	return r
}

// trigger notes a change in the worktree: with watching on it (re)starts the debounce,
// and the run starts once changes settle.
func (r *runner) trigger(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.gone || !r.a.watched(r.dir) {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.a.cfg.debounce, func() {
		if _, err := r.start(reason); err != nil {
			r.a.log().Info("not running tests", "dir", r.dir, "err", err)
		}
	})
}

// errNoCommand: neither a set nor a detected test command.
var errNoCommand = errors.New("no test command for this project")

// start begins a run, stopping the one in progress (newest wins). The new process
// starts only once the old one's process group is gone.
func (r *runner) start(trigger string) (*run, error) {
	command := r.a.commandFor(r.dir)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("the test watcher is stopping")
	}
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if command == "" {
		r.mu.Unlock()
		return nil, errNoCommand
	}
	prevCancel, prevDone := r.cancel, r.done
	if r.cur != nil && r.cur.State == stateRunning {
		r.cur.Superseded = true
	}
	r.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	nr := &run{ID: r.nextID, Command: command, Trigger: trigger, Started: r.a.cfg.now(), State: stateRunning}
	nr.addLine("$ "+command, ui.ToneMuted)
	done := make(chan struct{})
	r.cur, r.cancel, r.done = nr, cancel, done
	r.changed = map[string]bool{}
	r.mu.Unlock()
	r.a.changed(r.dir)
	go func() {
		if prevCancel != nil {
			prevCancel()
			<-prevDone
		}
		r.exec(ctx, nr, done)
	}()
	return nr, nil
}

// stop ends the run in progress; false when none runs.
func (r *runner) stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.cur == nil || r.cur.State != stateRunning {
		return false
	}
	r.cancel()
	return true
}

// close stops everything for good (the plugin is shutting down or the worktree went
// away) and waits, bounded, for the process group to die.
func (r *runner) close(gone bool) {
	r.mu.Lock()
	r.closed = !gone
	r.gone = r.gone || gone
	if r.timer != nil {
		r.timer.Stop()
	}
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(killGrace + 500*time.Millisecond):
		}
	}
}

func (r *runner) exec(ctx context.Context, rn *run, done chan struct{}) {
	defer close(done)
	finish := func(state runState, exit int, problem string) {
		r.mu.Lock()
		rn.State, rn.Exit, rn.Problem, rn.Ended = state, exit, problem, r.a.cfg.now()
		if problem != "" {
			rn.addLine(problem, ui.ToneError)
		}
		if state != stateStopped {
			r.result = rn
		}
		r.mu.Unlock()
		r.a.finished(r.dir, rn.snap(true))
	}
	if ctx.Err() != nil {
		finish(stateStopped, 0, "")
		return
	}
	if fi, err := os.Stat(r.dir); err != nil || !fi.IsDir() {
		r.mu.Lock()
		r.gone = true
		r.mu.Unlock()
		finish(stateError, 0, "the worktree "+r.dir+" no longer exists")
		return
	}
	var exit int
	var err error
	if h := r.a.terminalHost(r.dir); h != nil {
		exit, err = r.execTerminal(ctx, h, rn)
		if errors.Is(err, errNoTerminal) { // at the terminal limit, say: run it ourselves
			r.a.log().Warn("running the tests without a terminal", "dir", r.dir, "err", err)
			exit, err = r.execProcess(ctx, rn)
		}
	} else {
		exit, err = r.execProcess(ctx, rn)
	}
	switch {
	case err != nil:
		finish(stateError, 0, "could not start: "+err.Error())
		return
	case ctx.Err() != nil:
		finish(stateStopped, exit, "")
	case exit == 0:
		finish(statePassed, 0, "")
	case exit == 127:
		finish(stateError, exit, "command not found: "+firstWord(rn.Command)+" (exit 127; is it on your login shell's PATH?)")
	default:
		finish(stateFailed, exit, "")
	}
	r.a.log().Info("test run ended", "dir", r.dir, "run", rn.ID, "exit", exit, "stopped", ctx.Err() != nil)
}

// execProcess runs the command as the plugin's own child, its output piped into the
// run (hosts without plugin terminals). It returns the exit status, or an error when
// the process did not start.
func (r *runner) execProcess(ctx context.Context, rn *run) (int, error) {
	shell, flag := r.a.cfg.shell()
	cmd := exec.Command(shell, flag, rn.Command)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "CI=true", "NO_COLOR=1", "FORCE_COLOR=0", "TERM=dumb", "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return 0, err
	}
	pw.Close()
	r.a.log().Info("test run started", "dir", r.dir, "run", rn.ID, "command", rn.Command, "pid", cmd.Process.Pid)
	pgid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(killGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-exited:
		}
	}()
	r.read(pr, rn)
	err = cmd.Wait()
	close(exited)
	pr.Close()
	// Children left in the group (a test binary's helpers) go with it.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	case err != nil:
		return -1, nil
	}
	return 0, nil
}

// read streams the process's output into the run, line by line.
func (r *runner) read(src io.Reader, rn *run) {
	br := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			r.output(rn, line)
		}
		if err != nil {
			return
		}
	}
}

// cleanLine is one line of output as the plugin keeps it: no colours, no line end, only
// what a carriage-return progress line drew last, no control characters.
func cleanLine(line string) string {
	text := strings.TrimRight(stripANSI(line), "\r\n")
	if i := strings.LastIndexByte(text, '\r'); i >= 0 {
		text = text[i+1:]
	}
	return strings.Map(func(c rune) rune {
		if c < ' ' && c != '\t' || c == 0x7f {
			return -1 // backspaces, bells and other controls a terminal draws
		}
		return c
	}, text)
}

// output adds one line of output to the run (parsed for failures) and schedules a
// render.
func (r *runner) output(rn *run, line string) {
	text := cleanLine(line)
	r.mu.Lock()
	defer r.mu.Unlock()
	rn.parser.Line(text)
	rn.addLine(text, lineTone(text))
	r.renderSoonLocked()
}

// setOutput replaces the run's output with text, the whole of it so far (a terminal
// read with the scrollback, oldest line first; truncated: its head was cut), parsed
// again from the start, and schedules a render.
func (r *runner) setOutput(rn *run, text string, truncated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rn.parser = parser{}
	rn.lines, rn.bytes, rn.Dropped = nil, 0, 0
	rn.Gen++
	rn.addLine("$ "+rn.Command, ui.ToneMuted)
	if text != "" {
		for _, l := range strings.Split(text, "\n") {
			l = cleanLine(l)
			rn.parser.Line(l)
			rn.addLine(l, lineTone(l))
		}
	}
	if truncated && rn.Dropped == 0 {
		rn.Dropped = 1
	}
	r.renderSoonLocked()
}

// renderSoonLocked renders the worktree's views within flushPeriod. r.mu held.
func (r *runner) renderSoonLocked() {
	if r.flush == nil {
		r.flush = time.AfterFunc(flushPeriod, func() {
			r.mu.Lock()
			r.flush = nil
			r.mu.Unlock()
			r.a.changed(r.dir)
		})
	}
}

// snapshot copies the runner's state for a view; lines only when asked.
func (r *runner) snapshot(withLines bool) (cur, result *runSnap, changed int, gone bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur = r.cur.snap(withLines)
	if r.result != nil && r.result != r.cur {
		result = r.result.snap(false)
	}
	return cur, result, len(r.changed), r.gone
}

// findRun returns the current or last result run by id, with its lines.
func (r *runner) findRun(id int) *runSnap {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rn := range []*run{r.cur, r.result} {
		if rn != nil && rn.ID == id {
			return rn.snap(true)
		}
	}
	return nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return f[0]
}

// pollLoop notices file changes the worktree events do not report (an edit to a file
// that is already modified changes no counts): every poll interval it fingerprints the
// changed and untracked files (git status, then size and mtime of each).
func (r *runner) pollLoop() {
	t := time.NewTicker(r.a.cfg.poll)
	defer t.Stop()
	for range t.C {
		r.mu.Lock()
		stop, gone := r.closed, r.gone
		r.mu.Unlock()
		if stop || r.a.forgotten(r.dir) {
			return
		}
		if gone {
			continue
		}
		print, err := fingerprint(r.dir)
		if err != nil {
			if _, serr := os.Stat(r.dir); serr != nil {
				r.a.worktreeGone(r.dir)
			}
			continue
		}
		r.mu.Lock()
		first := r.print == nil
		var diff []string
		for p, v := range print {
			if r.print[p] != v {
				diff = append(diff, p)
			}
		}
		for p := range r.print {
			if _, ok := print[p]; !ok {
				diff = append(diff, p)
			}
		}
		r.print = print
		if !first {
			for _, p := range diff {
				r.changed[p] = true
			}
		}
		r.mu.Unlock()
		if first || len(diff) == 0 {
			continue
		}
		r.a.changed(r.dir)
		r.trigger("change")
	}
}

// fingerprint maps each changed or untracked file of a worktree to its size and mtime.
func fingerprint(dir string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	print := map[string]string{}
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, path := e[:2], e[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the rename's source follows
		}
		stamp := xy
		if fi, err := os.Lstat(filepath.Join(dir, path)); err == nil {
			stamp = fmt.Sprintf("%s %d %d", xy, fi.Size(), fi.ModTime().UnixNano())
		}
		print[path] = stamp
	}
	return print, nil
}
