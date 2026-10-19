package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/plugintest"
)

const wantHint = "code-foundry thread subscribe --plugin test-watcher/tests-finished --arg thread=s-1"

// manifestWakes are plugin.json's wake sources, for plugintest.Options.Wakes (the fake
// then refuses a fire the host would).
func manifestWakes(t *testing.T) []pluginsdk.WakeSpec {
	t.Helper()
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := pluginsdk.LoadManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return m.Wakes
}

func TestManifestWakes(t *testing.T) {
	ws := manifestWakes(t)
	if len(ws) != 1 || ws[0].Name != wakeTestsFinished || ws[0].ScopeOr() != pluginsdk.WakeScopeThread {
		t.Fatalf("wakes = %+v", ws)
	}
	if a, ok := ws[0].Arg("thread"); !ok || a.Context != pluginsdk.WakeContextThread || !a.Required {
		t.Errorf("thread arg = %+v", a)
	}
	if a, ok := ws[0].Arg("result"); !ok || a.Required || a.Check("pass") != nil || a.Check("fail") != nil || a.Check("ok") == nil {
		t.Errorf("result arg = %+v", a)
	}
}

// setProjectCommand stores a test command for the fixture's project the way set-command
// does, without following the thread (set-command does).
func (f *fixture) setProjectCommand(cmd string) {
	f.t.Helper()
	if err := f.a.st.update(func(s *settings) { s.Commands["p1"] = cmd }); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) subscribe(args map[string]string, repeat bool) string {
	f.t.Helper()
	id, err := f.h.SubscribeWakeWith(pluginsdk.WakeSubscription{Source: wakeTestsFinished, Args: args, Repeat: repeat,
		Context: pluginsdk.UiContext{ThreadID: thread, ProjectID: "p1", WorktreePath: f.dir}})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// A subscription follows the thread (wake-only), a run fires with {thread, result} as
// the match; a re-arm does not double it; once unsubscribed, the thread is let go after
// the linger and changes run nothing.
func TestTestsFinishedWake(t *testing.T) {
	f := start(t, "")
	f.setProjectCommand(`sh -c 'echo "--- FAIL: TestAdd (0.00s)"; echo "FAIL	example.com/calc	0.01s"; [ -e pass ] || exit 1'`)
	any := f.subscribe(map[string]string{"thread": thread}, true)
	passOnly := f.subscribe(map[string]string{"thread": thread, "result": "pass"}, true)
	f.eventually("the thread is followed for the wake", func() bool {
		return contains(f.a.st.followed(), thread) && f.a.st.wakeOnly(thread) && f.a.existingRunner(f.dir) != nil
	})

	f.change()
	f.h.WaitFor(10*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 1 })
	fire := f.h.WakeFires()[0]
	if fire.Source != wakeTestsFinished || fire.Match["thread"] != thread || fire.Match["result"] != "fail" ||
		!strings.Contains(fire.Title, "failed: TestAdd") {
		t.Errorf("fire = %+v", fire)
	}
	if d := f.h.WakeDelivered(); d[0] != 1 {
		t.Errorf("delivered %v: the result=pass subscription should not get a failure", d)
	}
	var p struct {
		Thread   string `json:"thread"`
		Result   string `json:"result"`
		State    string `json:"state"`
		Exit     int    `json:"exit"`
		Count    int    `json:"failed_count"`
		Failures []struct {
			Name, Where string
		} `json:"failures"`
	}
	if err := json.Unmarshal(fire.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Thread != thread || p.Result != "fail" || p.State != "failed" || p.Exit != 1 || p.Count != 1 ||
		len(p.Failures) != 1 || p.Failures[0].Name != "TestAdd" || p.Failures[0].Where != "example.com/calc" {
		t.Errorf("payload = %s", fire.Payload)
	}

	// A restart's re-arm is the same subscription.
	if _, err := f.h.SubscribeWakeWith(pluginsdk.WakeSubscription{Subscription: any, Source: wakeTestsFinished,
		Args: map[string]string{"thread": thread}, Repeat: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "pass"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.change()
	f.h.WaitFor(10*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 2 })
	if fire := f.h.WakeFires()[1]; fire.Match["result"] != "pass" || !strings.HasSuffix(fire.Title, " passed") || f.h.WakeDelivered()[1] != 2 {
		t.Errorf("passing fire = %+v, delivered %v", fire, f.h.WakeDelivered())
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(f.h.WakeFires()); n != 2 {
		t.Errorf("%d fires for two runs", n)
	}

	// Unsubscribed: let go after the linger; a change runs nothing.
	for _, id := range []string{any, passOnly} {
		if err := f.h.UnsubscribeWake(id); err != nil {
			t.Fatal(err)
		}
	}
	f.eventually("the thread is let go", func() bool {
		return !contains(f.a.st.followed(), thread) && f.a.existingRunner(f.dir) == nil
	})
	f.change()
	time.Sleep(400 * time.Millisecond)
	if n := len(f.h.WakeFires()); n != 2 {
		t.Errorf("fired after the last unsubscription: %d fires", n)
	}
}

// A thread the user follows (its Tests tab) stays followed after its subscription; a
// run that could not run fires as a failure.
func TestTestsFinishedFollowedThread(t *testing.T) {
	f := start(t, "")
	f.open()
	f.setProjectCommand("definitely-not-a-command-xyz")
	id := f.subscribe(map[string]string{"thread": thread, "result": "fail"}, false)
	if f.a.st.wakeOnly(thread) {
		t.Error("an open Tests tab's thread became wake-only")
	}
	f.command("run", nil)
	f.h.WaitFor(10*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 1 })
	if fire := f.h.WakeFires()[0]; fire.Match["result"] != "fail" || !strings.Contains(fire.Title, "could not run: command not found") {
		t.Errorf("fire = %+v", fire)
	}
	// The one-shot went with its fire.
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeSubscriptions()) == 0 })
	f.eventually("the plugin drops the fired one-shot", func() bool {
		f.a.mu.Lock()
		defer f.a.mu.Unlock()
		_, still := f.a.wakeSubs[id]
		return !still
	})
	time.Sleep(150 * time.Millisecond)
	if !contains(f.a.st.followed(), thread) {
		t.Error("unfollowed a thread the user follows")
	}
}

// A wake-only thread whose subscription ended while the plugin was down is let go after
// a start (no re-arm came for it).
func TestWakeOnlyLetGoAfterRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"threads":{"s-1":true},"wakeOnly":{"s-1":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := start(t, dir)
	f.eventually("the stale wake-only thread is let go", func() bool { return !contains(f.a.st.followed(), thread) })
}

// Every message about a run ends with the line that waits for the next one: the text
// Send failures sends, the run command's result, the Status tab.
func TestTestsHints(t *testing.T) {
	f := start(t, "")
	f.open()
	f.command("set-command", map[string]string{"command": `sh -c 'echo "--- FAIL: TestAdd (0.00s)"; exit 1'`})
	res := f.command("run", nil)
	if !strings.HasSuffix(res.Message, "\nWait for it to finish: "+wantHint) {
		t.Errorf("run message %q", res.Message)
	}
	f.waitPill("failing")
	f.command("send-failures", nil)
	f.mu.Lock()
	sent := f.sent[len(f.sent)-1]
	f.mu.Unlock()
	if !strings.HasSuffix(sent, "\n"+wantHint) {
		t.Errorf("send-failures text does not end with the hint:\n%s", sent)
	}
	v := f.tab(tabStatus)
	last := v.Nodes[len(v.Nodes)-1]
	if last.Key != "wake-hint" || last.Text == nil || last.Text.Content.Spans[1].Text != wantHint {
		t.Errorf("the Status tab's last node = %+v", last)
	}
}

// With terminal.scrollback the run's terminal runs the command without script(1), and
// the plugin parses the terminal's whole output, the scrollback above the screen too:
// a failure that scrolled off is still found. The exit status comes with terminal/exited.
func TestTerminalRunScrollback(t *testing.T) {
	f := startWith(t, "", true, "terminal.scrollback", "terminal.exited")
	f.open()
	f.command("set-command", map[string]string{"command": "go test ./..."})
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.CreatedTerminals()) == 1 })
	spec := f.h.CreatedTerminals()[0]
	if spec.Argv[0] != "/bin/sh" || strings.Contains(strings.Join(spec.Argv, " "), scriptPath) || spec.Argv[len(spec.Argv)-1] != "go test ./..." {
		t.Errorf("argv = %q", spec.Argv)
	}
	term := f.h.PluginTerminals()[0].TerminalID

	// 400 passing lines after the failure: the screen shows none of it.
	var b strings.Builder
	b.WriteString("=== RUN   TestEarly\n--- FAIL: TestEarly (0.00s)\n    calc_test.go:9: Add(2, 3) = 6, want 5\n")
	for i := range 400 {
		fmt.Fprintf(&b, "=== RUN   TestOK%d\n--- PASS: TestOK%d (0.00s)\n", i, i)
	}
	b.WriteString("FAIL\nFAIL\texample.com/calc\t0.01s\nFAIL")
	whole := b.String()
	lines := strings.Split(whole, "\n")
	f.h.SetTerminalText(term, strings.Join(lines[len(lines)-24:], "\n"))
	f.h.SetTerminalScrollback(term, whole)
	f.h.WaitFor(5*time.Second, func(*plugintest.Host) bool {
		cur, _, _, _ := f.a.existingRunner(f.dir).snapshot(false)
		return len(cur.Failures) == 1
	})
	f.h.ExitTerminal(term, 1)
	f.waitPill("failing")
	cur, _, _, _ := f.a.existingRunner(f.dir).snapshot(true)
	if len(cur.Failures) != 1 || cur.Failures[0].Title() != "TestEarly (example.com/calc)" || cur.Exit != 1 {
		t.Errorf("run = %+v failures %+v", cur.State, cur.Failures)
	}
	if len(cur.Lines) != len(lines)+1 || cur.Lines[2].Text != "--- FAIL: TestEarly (0.00s)" {
		t.Errorf("%d lines, want the command and %d; line 2 %q", len(cur.Lines), len(lines), cur.Lines[2].Text)
	}
	for _, r := range f.h.TerminalReads() {
		if !r.Scrollback {
			t.Errorf("a screen-only read: %+v", r)
		}
	}
	f.command("send-failures", nil)
	f.mu.Lock()
	sent := f.sent[len(f.sent)-1]
	f.mu.Unlock()
	if !strings.Contains(sent, "- TestEarly (example.com/calc)") || !strings.HasSuffix(sent, wantHint) {
		t.Errorf("excerpt:\n%s", sent)
	}
	if _, err := os.Stat(filepath.Join(f.a.host().DataDir(), "runs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(f.a.host().DataDir(), "runs"))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".out") {
				t.Errorf("a script(1) output file: %s", e.Name())
			}
		}
	}

	// The Output tab shows the terminal; once it closes, the log of what was read.
	if n := node(f.tab(tabOutput), termKey); n == nil || n.Terminal.TerminalID != term {
		t.Errorf("output tab without the terminal")
	}
}

// Stop on a scrollback run without a pid closes the terminal; the Output tab falls back
// to the log of what the last read had.
func TestTerminalStopScrollback(t *testing.T) {
	f := startWith(t, "", true, "terminal.scrollback", "terminal.exited")
	f.open()
	f.command("set-command", map[string]string{"command": "go test ./..."})
	f.command("run", nil)
	f.h.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.CreatedTerminals()) == 1 })
	f.h.SetTerminalScrollback(f.h.PluginTerminals()[0].TerminalID, "=== RUN   TestSlow")
	f.h.WaitFor(5*time.Second, func(*plugintest.Host) bool {
		cur, _, _, _ := f.a.existingRunner(f.dir).snapshot(true)
		return len(cur.Lines) == 2
	})
	if res := f.command("stop", nil); res.Message != "Stopped the test run" {
		t.Fatalf("stop = %q", res.Message)
	}
	f.waitPill("stopped")
	v := f.tab(tabOutput)
	log := node(v, logKey)
	if log == nil || node(v, termKey) != nil || len(log.Log.Lines) != 2 || log.Log.Lines[1].Text != "=== RUN   TestSlow" {
		t.Errorf("output after stop = %+v", v.Nodes)
	}
}
