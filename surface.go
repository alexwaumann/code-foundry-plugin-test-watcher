package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

const (
	tabStatus   = "tab-status"
	tabFailures = "tab-failures"
	tabOutput   = "tab-output"

	maxFailureRows = 100
	logKey         = "output-log"
	termKey        = "output-terminal"
	logRenderBytes = 60 << 10 // under the 64 KB a render may carry
)

// testsSurface is one open Tests tab: the side panel of one thread.
type testsSurface struct {
	a      *app
	h      *pluginsdk.Host
	inst   string
	thread string
	dir    string // the open context's worktree, until the thread list says

	mu        sync.Mutex
	tab       string
	formReset int
	cmdErr    string
	pending   bool
	looping   bool
	closed    bool

	// sendMu orders views and appends; the fields below say what the host's log node
	// holds: the lines of run logRun up to logSeq.
	sendMu   sync.Mutex
	lastJSON []byte
	logLive  bool
	logRun   int
	logGen   int
	logSeq   int
}

func (a *app) openTests(_ context.Context, h *pluginsdk.Host, open pluginsdk.SurfaceOpen) (pluginsdk.Surface, error) {
	s := &testsSurface{a: a, h: h, inst: open.Instance, thread: open.Context.ThreadID, dir: open.Context.WorktreePath, tab: tabStatus}
	a.mu.Lock()
	t, ok := a.threads[s.thread]
	a.surfaces[s.inst] = s
	a.mu.Unlock()
	if !ok {
		t = threadInfo{ID: s.thread, ProjectID: open.Context.ProjectID, WorktreePath: open.Context.WorktreePath}
	}
	if s.thread != "" && t.WorktreePath != "" {
		a.follow(t)
	}
	return s, s.render()
}

func (s *testsSurface) Visibility(bool) {}

func (s *testsSurface) Close(pluginsdk.CloseReason) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.a.mu.Lock()
	delete(s.a.surfaces, s.inst)
	s.a.mu.Unlock()
}

// dirty asks for a render; renders coalesce on one goroutine per instance.
func (s *testsSurface) dirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pending = true
	if s.looping {
		return
	}
	s.looping = true
	go func() {
		for {
			s.mu.Lock()
			if !s.pending || s.closed {
				s.looping = false
				s.mu.Unlock()
				return
			}
			s.pending = false
			s.mu.Unlock()
			if err := s.render(); err != nil && !errors.Is(err, pluginsdk.ErrClosed) && pluginsdk.ErrorCode(err) != pluginsdk.CodeNotFound {
				s.h.Log().Warn("render", "instance", s.inst, "err", err)
			}
		}
	}()
}

func (s *testsSurface) workdir() string {
	if d := s.a.threadDir(s.thread); d != "" {
		return d
	}
	return s.dir
}

// model is everything one render needs.
type model struct {
	thread, dir, project, branch string
	command, source              string
	watch                        bool
	flagged                      bool
	cur, result                  *runSnap
	changed                      int
	changedMore                  bool // more files changed than changed counts
	gone                         bool
	tab                          string
	formReset                    int
	cmdErr                       string
	// hint is the line that subscribes the thread to its next run (tests-finished).
	hint string
}

func (s *testsSurface) model() model {
	dir := s.workdir()
	m := model{thread: s.thread, dir: dir}
	if s.thread != "" {
		m.hint = testsHint(s.h, s.thread)
	}
	s.mu.Lock()
	m.tab, m.formReset, m.cmdErr = s.tab, s.formReset, s.cmdErr
	s.mu.Unlock()
	if dir == "" {
		return m
	}
	_, m.project = s.a.projectOf(dir)
	m.branch = s.a.branchOf(dir)
	m.command, m.source = s.a.commandAndSource(dir)
	m.watch = s.a.st.watching(s.thread)
	m.flagged = s.a.isFlagged(s.thread)
	if r := s.a.existingRunner(dir); r != nil {
		m.cur, m.result, m.changed, m.changedMore, m.gone = r.snapshot(m.tab == tabOutput)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		m.gone = true
	}
	return m
}

// render builds the view and sends it, then the output lines the host's log node does
// not have yet.
func (s *testsSurface) render() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	m := s.model()
	v := buildView(m)
	var pending []pluginsdk.AppendLine
	if log := findLog(v.Nodes); log != nil && m.cur != nil {
		if !s.logLive || s.logRun != m.cur.ID || s.logGen != m.cur.Gen {
			log.Lines, log.Truncated = tailLines(m.cur.Lines, logRenderBytes)
			log.Truncated = log.Truncated || m.cur.Dropped > 0
			s.logLive, s.logRun, s.logGen = true, m.cur.ID, m.cur.Gen
			s.logSeq = lastSeq(m.cur.Lines)
		} else {
			log.Truncated = m.cur.Dropped > 0
			for _, l := range m.cur.Lines {
				if l.Seq > s.logSeq {
					pending = append(pending, pluginsdk.AppendLine{Text: l.Text, Tone: string(l.Tone)})
					s.logSeq = l.Seq
				}
			}
		}
	} else {
		s.logLive = false
	}
	// Lines for the log node the host already has go first, so they land before a view
	// that says the run ended.
	for len(pending) > 0 {
		n, size := 0, 0
		for n < len(pending) && size+len(pending[n].Text)+1 < 60<<10 {
			size += len(pending[n].Text) + 1
			n++
		}
		if n == 0 {
			n = 1
		}
		if err := s.h.AppendLogLines(s.inst, logKey, pending[:n]...); err != nil {
			s.logLive = false
			return err
		}
		pending = pending[n:]
	}
	// Compare views without the log's lines: a view that only stops carrying the lines
	// it started the log with is the same view to the host.
	var lines []ui.LogLine
	if log := findLog(v.Nodes); log != nil {
		lines, log.Lines = log.Lines, nil
	}
	data, err := json.Marshal(v)
	if log := findLog(v.Nodes); log != nil {
		log.Lines = lines
	}
	if err != nil {
		return err
	}
	if fresh := len(lines) > 0; fresh || !bytes.Equal(data, s.lastJSON) {
		if err := s.h.UpdateView(s.inst, v); err != nil {
			s.lastJSON, s.logLive = nil, false
			return err
		}
		s.lastJSON = data
	}
	return nil
}

func findLog(nodes []ui.Node) *ui.Log {
	for i := range nodes {
		if nodes[i].Log != nil && nodes[i].Key == logKey {
			return nodes[i].Log
		}
	}
	return nil
}

func lastSeq(lines []outLine) int {
	if len(lines) == 0 {
		return 0
	}
	return lines[len(lines)-1].Seq
}

// tailLines keeps the last lines that fit in max bytes; true when some were cut.
func tailLines(lines []outLine, max int) ([]ui.LogLine, bool) {
	size, i := 0, len(lines)
	for i > 0 && size+len(lines[i-1].Text)+1 <= max {
		size += len(lines[i-1].Text) + 1
		i--
	}
	out := make([]ui.LogLine, 0, len(lines)-i)
	for _, l := range lines[i:] {
		out = append(out, ui.LogLine{Text: l.Text, Tone: l.Tone})
	}
	return out, i > 0
}

// buildView is the whole Tests view for a model.
func buildView(m model) ui.View {
	v := ui.View{Header: header(m)}
	switch {
	case m.dir == "":
		v.Nodes = []ui.Node{{Key: "no-worktree", Empty: &ui.Empty{Glyph: ui.IconGlyph("folder", ui.ToneMuted),
			Title: "No worktree", Body: ui.Plain("This thread has no project checkout to run tests in.")}}}
		return v
	case m.gone:
		v.Nodes = []ui.Node{{Key: "gone", Notice: &ui.Notice{Tone: ui.ToneError, Icon: "triangle-alert", Title: "Worktree deleted",
			Body: ui.Spans(ui.T(m.dir).AsMono(), ui.T(" no longer exists, so its tests no longer run.")),
		}}}
		return v
	}
	tabs := ui.NewTabs(m.tab, ui.NewTab(tabStatus, "Status"), failuresTab(m), outputTab(m))
	v.Tabs = tabs
	if m.command == "" {
		v.Nodes = noCommand(m)
		return v
	}
	nodes := []ui.Node{actionsBar(m)}
	switch m.tab {
	case tabFailures:
		nodes = append(nodes, failuresNodes(m)...)
	case tabOutput:
		nodes = append(nodes, outputNodes(m)...)
	default:
		nodes = append(nodes, statusNodes(m)...)
	}
	v.Nodes = nodes
	return v
}

func header(m model) *ui.Header {
	title := m.project
	if title == "" {
		title = "Tests"
	}
	h := &ui.Header{Title: title}
	if m.branch != "" {
		h.Trailing = ui.Spans(ui.T(m.branch).AsMono())
	}
	h.Pill = statusPill(m)
	switch {
	case m.cur != nil && m.cur.State == stateRunning:
		h.Subline = ui.Spans(ui.T("running for "), ui.At(m.cur.Started, ui.TimeElapsed))
	case m.cur != nil:
		h.Subline = ui.Spans(ui.T("took "), ui.For(m.cur.Ended.Sub(m.cur.Started)), ui.T(" · "), ui.At(m.cur.Ended, ui.TimeRelative))
	case m.command != "":
		h.Subline = ui.Plain("no run yet")
	}
	if m.dir != "" && !m.gone && m.command != "" {
		h.Actions = []ui.Button{ui.CommandButton("hdr-run", "Run now", "plugin.test-watcher.run", nil).WithIcon("play").AsIconOnly()}
	}
	return h
}

func statusPill(m model) *ui.Pill {
	switch {
	case m.gone:
		return ui.NewPill("gone", ui.ToneError)
	case m.cur == nil:
		return ui.NewPill("idle", ui.ToneMuted)
	}
	switch m.cur.State {
	case stateRunning:
		return ui.NewPill("running", ui.ToneNeutral)
	case statePassed:
		return ui.NewPill("passing", ui.ToneOK)
	case stateFailed:
		return ui.NewPill("failing", ui.ToneError)
	case stateError:
		return ui.NewPill("error", ui.ToneError)
	}
	return ui.NewPill("stopped", ui.ToneMuted)
}

func failuresTab(m model) ui.Tab {
	t := ui.NewTab(tabFailures, "Failures")
	if rn := failuresRun(m); rn != nil && rn.State == stateFailed {
		n := rn.FailedCount
		if n == 0 {
			n = len(rn.Failures)
		}
		if n > 0 {
			t.Badge = ui.NewPill(strconv.Itoa(n), ui.ToneError)
		}
	}
	return t
}

func outputTab(m model) ui.Tab {
	t := ui.NewTab(tabOutput, "Output")
	if m.cur != nil && m.cur.State == stateRunning {
		t.Badge = ui.NewPill("live", ui.ToneNeutral)
	}
	return t
}

// failuresRun is the run whose failures to show: the current one when it finished or
// already found some, else the last finished one.
func failuresRun(m model) *runSnap {
	if m.cur != nil && (m.cur.State != stateRunning || len(m.cur.Failures) > 0) && m.cur.State != stateStopped {
		return m.cur
	}
	if m.result != nil {
		return m.result
	}
	return m.cur
}

func failing(m model) *runSnap {
	if rn := failuresRun(m); rn != nil && rn.State == stateFailed || rn != nil && rn.State == stateRunning && len(rn.Failures) > 0 {
		return rn
	}
	return nil
}

func actionsBar(m model) ui.Node {
	running := m.cur != nil && m.cur.State == stateRunning
	// Command buttons: the host fills each command's thread arg from the panel.
	buttons := []ui.Button{ui.CommandButton("run-now", "Run now", "plugin.test-watcher.run", nil).WithIcon("play")}
	if m.flagged {
		// The flag stays until the tests pass; this is the user's way to drop it sooner
		// (the thread header's × does the same).
		buttons = append(buttons, ui.EventButton("clear-flag", "Clear flag", "clear-flag").WithIcon("x"))
	}
	if running {
		buttons = append(buttons, ui.CommandButton("stop", "Stop", "plugin.test-watcher.stop", nil).
			WithIcon("square").WithStyle(ui.ButtonDestructive))
	}
	if failing(m) != nil {
		buttons = append(buttons, ui.CommandButton("send-failures", "Send failures to Claude", "plugin.test-watcher.send-failures", nil).
			WithIcon("send").WithStyle(ui.ButtonAttention))
	}
	return ui.NewActions("bar", buttons...)
}

func noCommand(m model) []ui.Node {
	return []ui.Node{
		{Key: "no-command", Empty: &ui.Empty{Glyph: ui.IconGlyph("flask-conical", ui.ToneMuted), Title: "No test command",
			Body: ui.Plain("Nothing here says how to run the tests (no go.mod, package.json, Makefile test target or pyproject.toml). Set the command for this project.")}},
		commandForm(m, true),
	}
}

func commandForm(m model, needed bool) ui.Node {
	f := ui.TextField("f-command", "command", "Test command")
	f.Input.Value = ""
	if m.source == "set" {
		f.Input.Value = m.command
	}
	detected := detectCommand(m.dir)
	f.Input.Placeholder = "go test ./..."
	if detected != "" {
		f.Input.Placeholder = detected
	}
	f.Input.Mono = true
	f.Input.MaxLength = maxCommandLen
	project := m.project
	if project == "" {
		project = "this project"
	}
	desc := "Runs in the worktree with your login shell, for every thread in " + project + "."
	if detected != "" {
		desc += " Leave it empty to use the detected `" + detected + "`."
	}
	f.Description = ui.Plain(desc)
	f.Error = m.cmdErr
	f.Required = needed
	save := ui.EventButton("save-command", "Save", "save-command")
	if needed {
		save = save.WithStyle(ui.ButtonAttention)
	} else {
		save = save.WithStyle(ui.ButtonOutline)
	}
	n := ui.NewForm("command-form", &save, f)
	n.Form.ResetToken = strconv.Itoa(m.formReset)
	return n
}

func statusNodes(m model) []ui.Node {
	var nodes []ui.Node
	if m.cur != nil && m.cur.State == stateError {
		title := "Could not run the tests"
		if strings.HasPrefix(m.cur.Problem, "command not found") {
			title = "Command not found"
		}
		nodes = append(nodes, ui.Node{Key: "run-error", Notice: &ui.Notice{Tone: ui.ToneError, Icon: "circle-alert", Title: title,
			Body: ui.Plain(m.cur.Problem), Detail: truncBytes(lastText(m.cur.Lines, 12), ui.MaxNoticeDetail)}})
	}
	cmd := ui.Spans(ui.T(m.command).AsMono())
	if m.source == "detected" {
		cmd = ui.Spans(ui.T(m.command).AsMono(), ui.T(" (detected)").WithTone(ui.ToneMuted))
	}
	pairs := []ui.Pair{{Key: "p-command", Label: "Command", Value: cmd, Copyable: true}}
	last := m.cur
	if last != nil && last.State == stateRunning {
		last = m.result
	}
	if last != nil {
		word := map[runState]string{statePassed: "passed", stateFailed: "failed", stateStopped: "stopped", stateError: "could not run"}[last.State]
		w := ui.T(word)
		if last.State == stateFailed || last.State == stateError {
			w = w.WithTone(ui.ToneError)
		}
		pairs = append(pairs,
			ui.NewPair("p-last", "Last run", ui.Spans(w, ui.T(" · "), ui.At(last.Ended, ui.TimeRelative))),
			ui.NewPair("p-took", "Duration", ui.Spans(ui.For(last.Ended.Sub(last.Started)))))
	} else {
		pairs = append(pairs, ui.NewPair("p-last", "Last run", ui.Plain("none yet")))
	}
	changed := "none"
	switch {
	case m.changedMore:
		changed = strconv.Itoa(m.changed) + "+ files" // a burst the event did not list in full
	case m.changed == 1:
		changed = "1 file"
	case m.changed > 1:
		changed = strconv.Itoa(m.changed) + " files"
	}
	pairs = append(pairs, ui.NewPair("p-changed", "Changed since", ui.Plain(changed)))
	nodes = append(nodes, ui.NewPairs("facts", pairs...))
	watch := ui.ToggleField("f-watch", "watch", "Watch for changes", m.watch)
	watch.Description = ui.Plain("Run the tests 2 s after files in this worktree stop changing.")
	watch.Change = ui.EventAction("toggle-watch", "")
	nodes = append(nodes, ui.NewForm("watch-form", nil, watch), commandForm(m, false))
	if m.hint != "" {
		nodes = append(nodes, ui.Node{Key: "wake-hint", Text: &ui.Text{Size: ui.TextSm, Copyable: true,
			Content: ui.Spans(ui.T("An agent waits for the next run with ").WithTone(ui.ToneMuted), ui.T(m.hint).AsMono())}})
	}
	return nodes
}

func failuresNodes(m model) []ui.Node {
	rn := failuresRun(m)
	switch {
	case rn == nil:
		return []ui.Node{ui.NewEmpty("no-failures", "No runs yet", ui.Plain("Tests run when files change, or with Run now."))}
	case rn.State == stateRunning && len(rn.Failures) == 0:
		return []ui.Node{ui.NewProgress("running", "Running "+rn.Command, nil)}
	case rn.State == stateStopped:
		return []ui.Node{ui.NewEmpty("stopped", "Stopped", ui.Plain("The run was stopped before it finished."))}
	case rn.State == stateError:
		return []ui.Node{{Key: "run-error", Notice: &ui.Notice{Tone: ui.ToneError, Icon: "circle-alert", Title: "Could not run the tests", Body: ui.Plain(rn.Problem)}}}
	case rn.State == statePassed:
		return []ui.Node{{Key: "passing", Empty: &ui.Empty{Glyph: ui.StatusGlyph(ui.StatusOK), Title: "All tests pass", Body: ui.Plain(rn.Command)}}}
	}
	var nodes []ui.Node
	if rn != m.cur && m.cur != nil && m.cur.State == stateRunning {
		nodes = append(nodes, ui.NewText("previous", ui.Plain("From the last finished run; a new run is in progress.")))
	}
	if len(rn.Failures) == 0 {
		body := "The command failed with exit status " + strconv.Itoa(rn.Exit) + ". Its output is not in a format this plugin reads (go test, vitest, jest): see Output."
		if rn.FailedCount > 0 {
			body = strconv.Itoa(rn.FailedCount) + " tests failed. See Output for which."
		}
		nodes = append(nodes, ui.Node{Key: "unparsed", Notice: &ui.Notice{Tone: ui.ToneError, Title: "Tests failed", Body: ui.Plain(body),
			Actions: []ui.Button{ui.EventButton("goto-output", "Show output", "goto-output")}}})
		return nodes
	}
	rows := make([]ui.Row, 0, min(len(rn.Failures), maxFailureRows))
	for i, f := range rn.Failures {
		if i == maxFailureRows {
			break
		}
		row := ui.Row{Key: "fail-" + strconv.Itoa(i), Glyph: ui.StatusGlyph(ui.StatusFailed), Title: truncRunes(f.Name, ui.MaxLabel),
			Actions: []ui.Button{ui.EventButton("send-"+strconv.Itoa(i), "Send to Claude", "send-failure").
				WithData(fmt.Sprintf("%d:%d", rn.ID, i)).WithIcon("send").AsIconOnly()}}
		if f.Where != "" {
			// Line 2, so the list is not dense (a dense row hides it).
			row.Line2 = ui.Spans(ui.T(truncRunes(f.Where, ui.MaxLabel)).AsMono())
		}
		rows = append(rows, row)
	}
	nodes = append(nodes, ui.Node{Key: "failures", List: &ui.List{Rows: rows}})
	if len(rn.Failures) > maxFailureRows {
		nodes = append(nodes, ui.NewText("more", ui.Plain(fmt.Sprintf("and %d more: see Output", len(rn.Failures)-maxFailureRows))))
	}
	return nodes
}

func outputNodes(m model) []ui.Node {
	if m.cur == nil {
		return []ui.Node{ui.NewEmpty("no-output", "No output yet", ui.Plain("Tests run when files change, or with Run now."))}
	}
	return []ui.Node{outputNode(m.cur)}
}

// outputNode is the run's output: the terminal it runs in (the host's terminal view,
// with "Open in bottom panel"), or, when the run is not in a terminal (a host without
// plugin terminals, the permission not granted, a worktree outside the home folder,
// or its terminal closed), a log node the plugin streams the lines into.
func outputNode(rn *runSnap) ui.Node {
	if rn.Terminal != "" && rn.TermOpen {
		return ui.Node{Key: termKey, Terminal: &ui.Terminal{TerminalID: rn.Terminal, Title: truncRunes(rn.Command, 80), Height: ui.HeightFill}}
	}
	return ui.Node{Key: logKey, Log: &ui.Log{Title: rn.Command, Rows: 24, Wrap: true}}
}

func lastText(lines []outLine, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

// Act handles the view's events.
func (s *testsSurface) Act(ctx context.Context, act pluginsdk.SurfaceAct) error {
	t := s.threadInfo()
	switch act.Action {
	case "tab":
		switch act.Key {
		case tabStatus, tabFailures, tabOutput:
		default:
			return fmt.Errorf("unknown tab %q", act.Key)
		}
		s.mu.Lock()
		prev := s.tab
		s.tab = act.Key
		s.mu.Unlock()
		if err := s.render(); err != nil {
			s.mu.Lock()
			s.tab = prev
			s.mu.Unlock()
			return err
		}
		return nil
	case "goto-output":
		s.mu.Lock()
		s.tab = tabOutput
		s.mu.Unlock()
	case "toggle-watch":
		on, ok := act.Value.(bool)
		if !ok {
			on, ok = act.Values["watch"].(bool)
		}
		if !ok {
			on = !s.a.st.watching(s.thread)
		}
		if err := s.a.setWatch(t, on); err != nil {
			return err
		}
	case "save-command":
		cmd, _ := act.Values["command"].(string)
		_, _, err := s.a.setCommand(t, cmd)
		s.mu.Lock()
		if err != nil {
			s.cmdErr = strings.TrimPrefix(err.Error(), "invalid params: ")
		} else {
			s.cmdErr = ""
			s.formReset++
		}
		s.mu.Unlock()
	case "clear-flag":
		if _, err := s.a.userClearFlag(s.h, t); err != nil {
			return err
		}
	case "send-failures": // an older view's button; the view now runs the command
		if _, err := s.a.sendFailures(ctx, s.h, t, -1, -1); err != nil {
			return err
		}
	case "send-failure":
		var id, i int
		if _, err := fmt.Sscanf(act.Data, "%d:%d", &id, &i); err != nil {
			return fmt.Errorf("bad failure %q", act.Data)
		}
		what, err := s.a.sendFailures(ctx, s.h, t, id, i)
		if err != nil {
			return err
		}
		_ = s.h.ToastIn(s.inst, pluginsdk.ToastInfo, "Sent "+truncRunes(what, 120)+" to Claude")
	default:
		return fmt.Errorf("unknown action %q", act.Action)
	}
	return s.render()
}

func (s *testsSurface) threadInfo() threadInfo {
	s.a.mu.Lock()
	t, ok := s.a.threads[s.thread]
	s.a.mu.Unlock()
	if !ok {
		t = threadInfo{ID: s.thread, WorktreePath: s.dir}
	}
	return t
}

// sendFailures sends a failing run to Claude in the thread: every failure (run < 0) or
// failure i of run id. It returns what it sent ("the failures" or the test's name).
func (a *app) sendFailures(ctx context.Context, h *pluginsdk.Host, t threadInfo, id, i int) (string, error) {
	r := a.existingRunner(t.WorktreePath)
	if r == nil {
		return "", pluginsdk.NewError(pluginsdk.CodeUnavailable, "no test run to send")
	}
	var rn *runSnap
	if id < 0 {
		cur, result, _, _, _ := r.snapshot(false)
		if f := failing(model{cur: cur, result: result}); f != nil {
			rn = r.findRun(f.ID)
		}
		if rn == nil {
			return "", pluginsdk.NewError(pluginsdk.CodeUnavailable, "the tests are not failing")
		}
	} else {
		rn = r.findRun(id)
	}
	if rn == nil {
		return "", pluginsdk.NewError(pluginsdk.CodeUnavailable, "that run is gone: run the tests again")
	}
	lines := make([]string, 0, len(rn.Lines))
	for _, l := range rn.Lines {
		lines = append(lines, l.Text)
	}
	fails, count := rn.Failures, rn.FailedCount
	if id >= 0 {
		if i < 0 || i >= len(rn.Failures) {
			return "", pluginsdk.NewError(pluginsdk.CodeUnavailable, "that failure is gone: run the tests again")
		}
		fails, count = rn.Failures[i:i+1], 1
		if own := failureLines(rn.Failures[i], lines); len(own) > 0 {
			lines = own
		}
	}
	text := excerpt(rn.Command, fails, count, lines, testsHint(h, t.ID))
	if _, err := h.Invoke(ctx, "thread.send", map[string]string{"text": text}, pluginsdk.UiContext{ThreadID: t.ID}); err != nil {
		return "", fmt.Errorf("sending to Claude: %w", err)
	}
	if id >= 0 {
		return fails[0].Name, nil
	}
	return "the failures", nil
}
