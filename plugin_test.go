package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/plugintest"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

// plugin.json is valid and declares exactly what the plugin serves.
func TestManifest(t *testing.T) {
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	m, warnings, err := pluginsdk.LoadManifest(data)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("plugin.json: %v %v", err, warnings)
	}
	p := plugin()
	for _, c := range m.Commands {
		if p.Commands[c.Name] == nil {
			t.Errorf("command %s is declared but not served", c.Name)
		}
	}
	for _, s := range m.Surfaces {
		if p.Surfaces[s.ID] == nil {
			t.Errorf("surface %s is declared but not served", s.ID)
		}
	}
	if len(p.Commands) != len(m.Commands) || len(p.Surfaces) != len(m.Surfaces) {
		t.Errorf("serves %d commands and %d surfaces, declares %d and %d", len(p.Commands), len(p.Surfaces), len(m.Commands), len(m.Surfaces))
	}
	if m.Permissions.Secrets || m.Permissions.SessionEnv || m.Permissions.API {
		t.Errorf("asks for more than it needs: %+v", m.Permissions)
	}
}

const (
	thread = "s-1"
	inst   = "test-watcher/tests/thread:s-1"
)

// fixture is a started plugin with one thread whose worktree is a temp dir.
type fixture struct {
	t   *testing.T
	ctx context.Context
	a   *app
	h   *plugintest.Host
	dir string

	mu   sync.Mutex
	sent []string // thread.send texts
}

func testConfig() config {
	return config{
		debounce: 150 * time.Millisecond,
		poll:     0,
		now:      time.Now,
		shell:    func() (string, string) { return "/bin/sh", "-c" },
		home:     func() string { return "" },
		termPoll: 10 * time.Millisecond,
	}
}

func start(t *testing.T, dataDir string) *fixture { return startWith(t, dataDir, false) }

// startWith with terminals grants the terminals permission on a host that renders
// terminal nodes, and puts the worktree under the "home" folder, so runs go into
// plugin terminals (plugintest's fakes: nothing runs; the test writes the output).
func startWith(t *testing.T, dataDir string, terminals bool) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	home := t.TempDir()
	cfg := testConfig()
	cfg.home = func() string { return home }
	f := &fixture{t: t, ctx: ctx, a: newApp(cfg), dir: filepath.Join(home, "worktree")}
	if err := os.Mkdir(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	granted := pluginsdk.Permissions{Commands: []string{"thread.send"}, Events: []string{"project", "threads"}, ThreadStatus: true}
	caps := []string{"surfaces", "view.tabs"}
	if terminals {
		granted.Terminals = true
		caps = append(caps, "terminals", "view.node.terminal")
	}
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	f.h = plugintest.Start(t, f.a.plugin(), plugintest.Options{
		Name:    "test-watcher",
		DataDir: dataDir,
		Granted: granted,
		Invoke: func(_ context.Context, p pluginsdk.InvokeParams) (pluginsdk.CommandResult, error) {
			if p.Command != "thread.send" || p.Context.ThreadID != thread {
				return pluginsdk.CommandResult{}, fmt.Errorf("unexpected %s %+v", p.Command, p.Context)
			}
			f.mu.Lock()
			f.sent = append(f.sent, p.Args["text"])
			f.mu.Unlock()
			return pluginsdk.CommandResult{Message: "sent"}, nil
		},
		Capabilities: caps,
	})
	t.Cleanup(func() { f.a.shutdown(context.Background(), nil) })
	f.event(fmt.Sprintf(`{"threadList":{"snapshot":{"threads":[{"id":%q,"projectId":"p1","worktreePath":%q,"name":"fix it"}]}},"epoch":"e"}`, thread, f.dir), true)
	f.event(f.worktreeEvent(`"snapshot":{"projects":[{"id":"p1","path":%q,"name":"scratch","git":true,"worktrees":[{"projectId":"p1","path":%q,"branch":"main","head":"aaa","status":{}}]}]}`, f.dir, f.dir), true)
	f.eventually("the thread and worktree are known", func() bool {
		return f.a.threadDir(thread) == f.dir && f.a.branchOf(f.dir) == "main"
	})
	return f
}

func (f *fixture) worktreeEvent(format string, a ...any) string {
	return `{"project":{` + fmt.Sprintf(format, a...) + `},"epoch":"e"}`
}

func (f *fixture) event(js string, snapshot bool) {
	f.t.Helper()
	if err := f.h.Event(pluginsdk.Event{Subscription: "sub-1", Snapshot: snapshot, Event: json.RawMessage(js)}); err != nil {
		f.t.Fatal(err)
	}
}

// change sends a worktree event that says files changed (the modified count moved).
var changes int

func (f *fixture) change() {
	changes++
	f.event(f.worktreeEvent(`"worktreeUpdated":{"projectId":"p1","path":%q,"branch":"main","head":"aaa","status":{"modified":%d,"dirty":true}}`, f.dir, changes), false)
}

func (f *fixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) open() {
	f.t.Helper()
	if err := f.h.OpenSurface(f.ctx, inst, "tests", pluginsdk.UiContext{ThreadID: thread, ProjectID: "p1", WorktreePath: f.dir}); err != nil {
		f.t.Fatal(err)
	}
}

// view decodes the instance's last view and checks it as the host will.
func (f *fixture) view() ui.View {
	f.t.Helper()
	raw, ok := f.h.View(inst)
	if !ok {
		f.t.Fatal("no view")
	}
	var v ui.View
	if err := json.Unmarshal(raw, &v); err != nil {
		f.t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		f.t.Fatalf("invalid view: %v\n%s", err, raw)
	}
	return v
}

func (f *fixture) pill() string {
	raw, ok := f.h.View(inst)
	if !ok {
		return ""
	}
	var v ui.View
	if json.Unmarshal(raw, &v) != nil || v.Header == nil || v.Header.Pill == nil {
		return ""
	}
	return v.Header.Pill.Label
}

func (f *fixture) waitPill(label string) ui.View {
	f.t.Helper()
	f.h.WaitFor(10*time.Second, func(*plugintest.Host) bool { return f.pill() == label })
	return f.view()
}

func (f *fixture) command(name string, args map[string]string) pluginsdk.CommandResult {
	f.t.Helper()
	if args == nil {
		args = map[string]string{}
	}
	args["thread"] = thread
	res, err := f.h.Command(f.ctx, name, args, pluginsdk.UiContext{ThreadID: thread})
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func (f *fixture) act(act pluginsdk.SurfaceAct) {
	f.t.Helper()
	act.Instance = inst
	if err := f.h.Act(f.ctx, act); err != nil {
		f.t.Fatalf("act %s: %v", act.Action, err)
	}
}

func (f *fixture) tab(key string) ui.View {
	f.t.Helper()
	f.act(pluginsdk.SurfaceAct{Kind: pluginsdk.ActTab, Action: "tab", Key: key, Value: key})
	v := f.view()
	if v.Tabs == nil || v.Tabs.Active != key {
		f.t.Fatalf("active tab is not %s", key)
	}
	return v
}

func (f *fixture) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(data)
}

func node(v ui.View, key string) *ui.Node {
	var find func([]ui.Node) *ui.Node
	find = func(nodes []ui.Node) *ui.Node {
		for i := range nodes {
			if nodes[i].Key == key {
				return &nodes[i]
			}
			if nodes[i].Section != nil {
				if n := find(nodes[i].Section.Children); n != nil {
					return n
				}
			}
		}
		return nil
	}
	return find(v.Nodes)
}

const goFailing = `printf -- '=== RUN   TestAdd\n--- FAIL: TestAdd (0.00s)\n    calc_test.go:9: Add(2, 3) = 6, want 5\nwarning: flaky clock\nFAIL\nFAIL\texample.com/calc\t0.01s\nFAIL\n'; exit 1`

// Changes are debounced into one run; a change during a run stops it (its whole process
// group) and the newest run is the one that finishes. Never two at once.
func TestDebounceNewestWins(t *testing.T) {
	f := start(t, "")
	f.open()
	// Each run logs its start; it runs long while ./slow exists; ./busy catches overlap.
	script := `n=$(($(cat n 2>/dev/null || echo 0)+1)); echo $n > n; [ -e busy ] && echo overlap >> log; touch busy; echo start $n >> log; ` +
		`if [ -e slow ]; then (sleep 30; echo child $n >> log) & sleep 30; fi; rm -f busy; echo end $n >> log`
	f.command("set-command", map[string]string{"command": script})
	if err := os.WriteFile(filepath.Join(f.dir, "slow"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		f.change()
		time.Sleep(20 * time.Millisecond)
	}
	f.eventually("run 1 starts", func() bool { return strings.Contains(f.read("log"), "start 1") })
	time.Sleep(400 * time.Millisecond)
	if log := f.read("log"); strings.Count(log, "start") != 1 {
		t.Fatalf("five quick changes started %d runs:\n%s", strings.Count(log, "start"), log)
	}
	f.waitPill("running")

	// A newer change: run 1 is stopped, run 2 runs to the end.
	os.Remove(filepath.Join(f.dir, "slow"))
	os.Remove(filepath.Join(f.dir, "busy")) // run 1 is killed before it can tidy up
	f.change()
	f.eventually("run 2 ends", func() bool { return strings.Contains(f.read("log"), "end 2") })
	log := f.read("log")
	for _, bad := range []string{"end 1", "child 1", "overlap", "start 3"} {
		if strings.Contains(log, bad) {
			t.Errorf("log has %q:\n%s", bad, log)
		}
	}
	v := f.waitPill("passing")
	if v.Header.Subline == nil {
		t.Error("no duration line")
	}
	// The killed run's background child is gone too.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(f.read("log"), "child 1") {
		t.Error("run 1's process group outlived it")
	}
}

// With watching off, changes start nothing; Run now still runs.
func TestToggleWatch(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": "echo ran >> log"})
	f.act(pluginsdk.SurfaceAct{Kind: pluginsdk.ActChange, Action: "toggle-watch", Key: "f-watch", Field: "watch", Value: false})
	v := f.view()
	if fl := node(v, "watch-form").Form.Fields[0]; fl.Toggle == nil || fl.Toggle.Value {
		t.Fatalf("watch toggle not off: %+v", fl)
	}
	f.change()
	time.Sleep(500 * time.Millisecond)
	if f.read("log") != "" {
		t.Fatal("a change ran the tests with watching off")
	}
	f.command("run", nil)
	f.waitPill("passing")
	if res := f.command("toggle-watch", nil); res.Message != "Watching for changes: on" {
		t.Errorf("toggle-watch: %q", res.Message)
	}
	f.change()
	f.eventually("a change runs again", func() bool { return strings.Count(f.read("log"), "ran") == 2 })
}

// A failing run flags the thread with its first failing test; a passing run clears it.
func TestFlagLifecycle(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": goFailing})
	f.command("run", nil)
	f.waitPill("failing")
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.Flags()) == 1 })
	fl := f.h.Flags()[0]
	if fl.ThreadID != thread || fl.Level != pluginsdk.FlagError || fl.Reason != "Tests failing: TestAdd" || !fl.UntilSeen {
		t.Errorf("flag = %+v", fl)
	}
	// Failing again replaces it; nothing clears in between.
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.Flags()) == 2 })
	f.command("set-command", map[string]string{"command": "true"})
	f.command("run", nil)
	f.waitPill("passing")
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.Flags()) == 3 })
	if fl := f.h.Flags()[2]; fl.Level != pluginsdk.FlagNone {
		t.Errorf("passing run: %+v, want a clear", fl)
	}
	// Passing again: nothing to clear.
	f.command("run", nil)
	time.Sleep(300 * time.Millisecond)
	f.waitPill("passing")
	if n := len(f.h.Flags()); n != 3 {
		t.Errorf("%d flag calls, want 3", n)
	}
	for _, tt := range []struct {
		rn   runSnap
		want string
	}{
		{runSnap{Failures: []failure{{Name: "A"}, {Name: "B"}, {Name: "C"}}}, "Tests failing: A and 2 more"},
		{runSnap{FailedCount: 4}, "Tests failing: 4 failed"},
		{runSnap{Exit: 2}, "Tests failing: exit 2"},
		{runSnap{Failures: []failure{{Name: strings.Repeat("T", 100)}}}, "Tests failing: " + strings.Repeat("T", 64) + "…"},
	} {
		if got := flagReason(&tt.rn); got != tt.want || len([]rune(got)) > 80 {
			t.Errorf("flagReason = %q, want %q", got, tt.want)
		}
	}
}

// The failing view: header, Failures rows with Send to Claude, toned output; the
// excerpt sent to the thread; then the passing view.
func TestFailingAndPassingViews(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": goFailing})
	f.command("run", nil)
	v := f.waitPill("failing")
	if v.Header.Title != "scratch" || v.Header.Trailing == nil || v.Header.Trailing.Spans[0].Text != "main" {
		t.Errorf("header = %+v", v.Header)
	}
	if len(v.Header.Actions) != 1 || v.Header.Actions[0].Action.Command.Name != "plugin.test-watcher.run" {
		t.Errorf("header actions = %+v", v.Header.Actions)
	}
	bar := node(v, "bar").Actions.Buttons
	if len(bar) != 2 || bar[1].Key != "send-failures" || bar[1].Style != ui.ButtonAttention ||
		bar[1].Action.Command == nil || bar[1].Action.Command.Name != "plugin.test-watcher.send-failures" {
		t.Errorf("actions bar = %+v", bar)
	}
	for _, b := range append(bar, v.Header.Actions...) {
		if b.Action.Command != nil && len(b.Action.Command.Args) > 0 {
			t.Errorf("%s passes args the host fills from the panel: %v", b.Key, b.Action.Command.Args)
		}
	}

	v = f.tab(tabFailures)
	list := node(v, "failures").List
	rows := list.Rows
	if len(rows) != 1 || rows[0].Title != "TestAdd" || rows[0].Line2.Spans[0].Text != "example.com/calc" {
		t.Fatalf("failure rows = %+v", rows)
	}
	if list.Dense {
		t.Error("a dense list hides line 2, the package")
	}
	if badge := v.Tabs.Items[1].Badge; badge == nil || badge.Label != "1" {
		t.Errorf("failures badge = %+v", badge)
	}
	f.act(pluginsdk.SurfaceAct{Kind: pluginsdk.ActClick, Action: "send-failure", Key: rows[0].Actions[0].Key, Data: rows[0].Actions[0].Action.Data})
	if res := f.command("send-failures", nil); res.Message != "Sent the failing tests to Claude" {
		t.Errorf("send-failures = %q", res.Message)
	}
	f.mu.Lock()
	sent := append([]string(nil), f.sent...)
	f.mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages", len(sent))
	}
	for _, s := range sent {
		if !strings.Contains(s, "TestAdd (example.com/calc)") || !strings.Contains(s, "Add(2, 3) = 6, want 5") || len(s) > maxExcerpt {
			t.Errorf("excerpt:\n%s", s)
		}
	}
	if _, out, _ := strings.Cut(sent[0], "Last output lines:"); strings.Contains(out, "warning: flaky clock") {
		t.Errorf("one failure's excerpt carries other lines:\n%s", sent[0])
	}

	v = f.tab(tabOutput)
	log := node(v, logKey).Log
	if log == nil || len(log.Lines) == 0 || log.Lines[0].Text != "$ "+goFailing {
		t.Fatalf("output log = %+v", log)
	}
	tones := map[string]ui.Tone{}
	for _, l := range log.Lines {
		tones[l.Text] = l.Tone
	}
	if tones["--- FAIL: TestAdd (0.00s)"] != ui.ToneError || tones["warning: flaky clock"] != ui.ToneAttention || tones["    calc_test.go:9: Add(2, 3) = 6, want 5"] != ui.ToneNeutral {
		t.Errorf("tones = %v", tones)
	}

	f.command("set-command", map[string]string{"command": "echo ok"})
	f.command("run", nil)
	v = f.waitPill("passing")
	if len(node(v, "bar").Actions.Buttons) != 1 {
		t.Errorf("passing actions bar = %+v", node(v, "bar").Actions.Buttons)
	}
	v = f.tab(tabFailures)
	if n := node(v, "passing"); n == nil || n.Empty.Title != "All tests pass" {
		t.Errorf("passing failures tab = %+v", v.Nodes)
	}
}

// Output streams into the log node with appends while the run goes, and a huge output
// keeps its tail within the caps.
func TestOutputStreamsAndCaps(t *testing.T) {
	f := start(t, "")
	f.open()
	f.tab(tabOutput)
	f.command("set-command", map[string]string{"command": `echo first; sleep 0.5; echo second; sleep 0.5; i=0; while [ $i -lt 3000 ]; do echo "--- FAIL: line $i"; i=$((i+1)); done`})
	f.command("run", nil)
	f.h.WaitFor(10*time.Second, func(h *plugintest.Host) bool {
		for _, a := range h.Appends() {
			for _, l := range a.Lines {
				if l.Text == "second" {
					return true
				}
			}
		}
		return false
	})
	v := f.waitPill("passing")
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		as := h.Appends()
		if len(as) == 0 {
			return false
		}
		last := as[len(as)-1].Lines
		return len(last) > 0 && last[len(last)-1].Text == "--- FAIL: line 2999"
	})
	var appended int
	for _, a := range f.h.Appends() {
		if a.Key != logKey || a.Instance != inst {
			t.Errorf("append to %s %s", a.Instance, a.Key)
		}
		appended += len(a.Lines)
		size := 0
		for _, l := range a.Lines {
			size += len(l.Text) + 1
		}
		if size > ui.MaxLogText {
			t.Errorf("append of %d bytes", size)
		}
	}
	if appended < maxLines {
		t.Errorf("appended %d lines", appended)
	}
	// Coming back to the tab sends the kept tail at once, marked truncated.
	f.tab(tabStatus)
	v = f.tab(tabOutput)
	log := node(v, logKey).Log
	if !log.Truncated || len(log.Lines) > maxLines || log.Lines[len(log.Lines)-1].Text != "--- FAIL: line 2999" {
		t.Errorf("tail: truncated=%v lines=%d last=%q", log.Truncated, len(log.Lines), log.Lines[len(log.Lines)-1].Text)
	}
	if log.Lines[len(log.Lines)-1].Tone != ui.ToneError {
		t.Error("FAIL line not red")
	}
}

// No command: the empty state with the form; saving one through the form (kept in the
// data dir across restarts), validation, and resetting to the detected default.
func TestCommandForm(t *testing.T) {
	data := t.TempDir()
	f := start(t, data)
	f.open()
	v := f.view()
	if n := node(v, "no-command"); n == nil || n.Empty == nil {
		t.Fatalf("no empty state: %+v", v.Nodes)
	}
	if f.pill() != "idle" {
		t.Errorf("pill = %q", f.pill())
	}
	if _, err := f.h.Command(f.ctx, "run", map[string]string{"thread": thread}, pluginsdk.UiContext{}); err == nil || pluginsdk.ErrorCode(err) != pluginsdk.CodeUnavailable {
		t.Errorf("run without a command: %v", err)
	}
	f.act(pluginsdk.SurfaceAct{Kind: pluginsdk.ActSubmit, Action: "save-command", Key: "save-command", Values: map[string]any{"command": "a\nb"}})
	if fl := node(f.view(), "command-form").Form.Fields[0]; fl.Error == "" {
		t.Error("a two-line command was accepted")
	}
	f.act(pluginsdk.SurfaceAct{Kind: pluginsdk.ActSubmit, Action: "save-command", Key: "save-command", Values: map[string]any{"command": "  make check  "}})
	v = f.view()
	if p := node(v, "facts").Pairs.Items[0]; p.Value.Spans[0].Text != "make check" {
		t.Errorf("command pair = %+v", p)
	}
	if fl := node(v, "command-form").Form; fl.Fields[0].Error != "" || fl.Fields[0].Input.Value != "make check" || fl.ResetToken != "1" {
		t.Errorf("form after save = %+v", fl)
	}
	if err := f.h.Shutdown(f.ctx); err != nil {
		t.Fatal(err)
	}

	g := start(t, data)
	g.dir = f.dir
	g.event(fmt.Sprintf(`{"threadList":{"snapshot":{"threads":[{"id":%q,"projectId":"p1","worktreePath":%q}]}},"epoch":"e"}`, thread, f.dir), true)
	g.event(g.worktreeEvent(`"snapshot":{"projects":[{"id":"p1","path":%q,"name":"scratch","git":true,"worktrees":[{"projectId":"p1","path":%q,"branch":"main"}]}]}`, f.dir, f.dir), true)
	g.eventually("restarted", func() bool { return g.a.commandFor(f.dir) == "make check" })
	if err := os.WriteFile(filepath.Join(f.dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := g.command("set-command", map[string]string{"command": ""}); res.Message != "Test command reset to the detected go test ./..." {
		t.Errorf("reset: %q", res.Message)
	}
}

// A command that is not on PATH says so.
func TestCommandNotFound(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": "definitely-not-a-test-runner-xyz ./..."})
	f.command("run", nil)
	v := f.waitPill("error")
	n := node(v, "run-error")
	if n == nil || n.Notice.Title != "Command not found" || !strings.Contains(n.Notice.Body.Text, "definitely-not-a-test-runner-xyz") {
		t.Fatalf("notice = %+v", n)
	}
	if len(f.h.Flags()) != 0 {
		t.Error("a missing command flagged the thread")
	}
}

// A deleted worktree stops its run and says so.
func TestWorktreeDeleted(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": "sleep 30"})
	f.command("run", nil)
	f.waitPill("running")
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	f.event(f.worktreeEvent(`"worktreeRemoved":{"projectId":"p1","path":%q}`, f.dir), false)
	v := f.waitPill("gone")
	if n := node(v, "gone"); n == nil || n.Notice.Title != "Worktree deleted" {
		t.Errorf("view = %+v", v.Nodes)
	}
}

// Stop ends the run in progress.
func TestStop(t *testing.T) {
	f := start(t, "")
	f.open()
	if res := f.command("stop", nil); res.Message != "No test run in progress" {
		t.Errorf("stop with nothing running: %q", res.Message)
	}
	f.command("set-command", map[string]string{"command": "sleep 30"})
	f.command("run", nil)
	v := f.waitPill("running")
	if bar := node(v, "bar").Actions.Buttons; len(bar) != 2 || bar[1].Key != "stop" || bar[1].Style != ui.ButtonDestructive {
		t.Errorf("running actions bar = %+v", bar)
	}
	if res := f.command("stop", nil); res.Message != "Stopped the test run" {
		t.Errorf("stop: %q", res.Message)
	}
	f.waitPill("stopped")
}

// Unknown acts and threads are refused.
func TestRefusals(t *testing.T) {
	f := start(t, "")
	f.open()
	if err := f.h.Act(f.ctx, pluginsdk.SurfaceAct{Instance: inst, Kind: pluginsdk.ActClick, Action: "nope"}); err == nil {
		t.Error("an unknown action was accepted")
	}
	if err := f.h.Act(f.ctx, pluginsdk.SurfaceAct{Instance: inst, Kind: pluginsdk.ActTab, Action: "tab", Key: "nope"}); err == nil {
		t.Error("an unknown tab was accepted")
	}
	if _, err := f.h.Command(f.ctx, "run", map[string]string{"thread": "s-nope"}, pluginsdk.UiContext{}); pluginsdk.ErrorCode(err) != pluginsdk.CodeNotFound {
		t.Errorf("unknown thread: %v", err)
	}
}

// The outputFile a terminal run's argv records into.
func outputFile(t *testing.T, argv []string) string {
	t.Helper()
	if len(argv) < 4 || argv[0] != scriptPath || argv[1] != "-q" || argv[2] != "-F" {
		t.Fatalf("argv = %q", argv)
	}
	return argv[3]
}

// With plugin terminals, a run goes into a terminal in the worktree: the Output tab
// shows it, the plugin reads what it prints for the failures and the flag, and the
// exit status from the host; the next run replaces the terminal.
func TestTerminalRun(t *testing.T) {
	f := startWith(t, "", true)
	f.open()
	f.command("set-command", map[string]string{"command": "go test ./..."})
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.CreatedTerminals()) == 1 })
	spec := f.h.CreatedTerminals()[0]
	if spec.Cwd != f.dir || spec.Env["CI"] != "true" || spec.Title != "go test ./..." {
		t.Errorf("terminal spec = %+v", spec)
	}
	if n := len(spec.Argv); spec.Argv[n-1] != "go test ./..." || spec.Argv[n-2] != "-c" {
		t.Errorf("argv = %q", spec.Argv)
	}
	term := f.h.PluginTerminals()[0].TerminalID

	v := f.tab(tabOutput)
	n := node(v, termKey)
	if n == nil || n.Terminal == nil || n.Terminal.TerminalID != term || node(v, logKey) != nil {
		t.Fatalf("output tab = %+v", v.Nodes)
	}

	// What the terminal shows lands in the run (colours and CRLFs as script writes them).
	out := outputFile(t, spec.Argv)
	text := "=== RUN   TestAdd\r\n\x1b[31m--- FAIL: TestAdd (0.00s)\x1b[0m\r\n    calc_test.go:9: Add(2, 3) = 6, want 5\r\nFAIL\r\nFAIL\texample.com/calc\t0.01s\r\nFAIL"
	if err := os.WriteFile(out, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.WaitFor(5*time.Second, func(*plugintest.Host) bool {
		cur, _, _, _ := f.a.existingRunner(f.dir).snapshot(false)
		return len(cur.Failures) == 1
	})
	f.h.ExitTerminal(term, 1)
	v = f.waitPill("failing")
	if n := node(v, termKey); n == nil || n.Terminal.TerminalID != term {
		t.Errorf("the exited terminal left the Output tab: %+v", v.Nodes)
	}
	v = f.tab(tabFailures)
	if rows := node(v, "failures").List.Rows; len(rows) != 1 || rows[0].Title != "TestAdd" || rows[0].Line2.Spans[0].Text != "example.com/calc" {
		t.Errorf("failure rows = %+v", rows)
	}
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		fs := h.Flags()
		return len(fs) == 1 && fs[0].Reason == "Tests failing: TestAdd"
	})
	f.command("send-failures", nil)
	f.mu.Lock()
	sent := f.sent[len(f.sent)-1]
	f.mu.Unlock()
	if !strings.Contains(sent, "--- FAIL: TestAdd (0.00s)") || strings.Contains(sent, "\x1b") || strings.Contains(sent, "\r") {
		t.Errorf("excerpt:\n%q", sent)
	}

	// The next run closes the last one's terminal and opens its own; it passes.
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.CreatedTerminals()) == 2 })
	terms := f.h.PluginTerminals()
	if len(terms) != 1 || terms[0].TerminalID == term {
		t.Fatalf("terminals after the second run = %+v", terms)
	}
	if err := os.WriteFile(outputFile(t, f.h.CreatedTerminals()[1].Argv), []byte("ok  \texample.com/calc\t0.01s\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.ExitTerminal(terms[0].TerminalID, 0)
	f.waitPill("passing")
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		fs := h.Flags()
		return len(fs) == 2 && fs[1].Level == pluginsdk.FlagNone
	})
}

// Stop on a terminal run without a process group to signal (the fake host runs
// nothing, so no pid) closes the terminal; the Output tab falls back to the log of
// what the run printed.
func TestTerminalStop(t *testing.T) {
	f := startWith(t, "", true)
	f.open()
	f.command("set-command", map[string]string{"command": "go test ./..."})
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.CreatedTerminals()) == 1 })
	if err := os.WriteFile(outputFile(t, f.h.CreatedTerminals()[0].Argv), []byte("=== RUN   TestSlow\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.WaitFor(5*time.Second, func(*plugintest.Host) bool {
		cur, _, _, _ := f.a.existingRunner(f.dir).snapshot(true)
		return len(cur.Lines) == 2
	})
	if res := f.command("stop", nil); res.Message != "Stopped the test run" {
		t.Fatalf("stop = %q", res.Message)
	}
	v := f.waitPill("stopped")
	if len(f.h.PluginTerminals()) != 0 {
		t.Errorf("terminals after stop = %+v", f.h.PluginTerminals())
	}
	v = f.tab(tabOutput)
	log := node(v, logKey)
	if log == nil || node(v, termKey) != nil || len(log.Log.Lines) != 2 || log.Log.Lines[1].Text != "=== RUN   TestSlow" {
		t.Errorf("output after stop = %+v", v.Nodes)
	}
}

// The terminals permission alone is not enough: a host without terminal nodes keeps
// the log node and runs the command itself.
func TestTerminalNeedsCapability(t *testing.T) {
	f := startWith(t, "", false)
	f.a.mu.Lock()
	h := f.a.h
	f.a.mu.Unlock()
	if f.a.terminalHost(f.dir) != nil || terminalsUsable(h) {
		t.Error("terminal runs without the capability or the permission")
	}
	f.open()
	f.command("set-command", map[string]string{"command": "echo ok"})
	f.command("run", nil)
	f.waitPill("passing")
	if n := node(f.tab(tabOutput), logKey); n == nil {
		t.Error("no log node")
	}
	if len(f.h.CreatedTerminals()) != 0 {
		t.Error("made a terminal")
	}
}
