package main

import (
	"context"
	"strings"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// The tests-finished wake source (plugin.json's wakes): an agent's thread subscribes
// with `code-foundry thread subscribe --plugin test-watcher/tests-finished [--arg
// result=fail]` (thread defaults to the subscriber) and is woken after every test run
// in that thread's worktree that ends with a result (passed, failed, or could not run;
// not a stopped or replaced one), the match being {thread, result}. A subscription
// makes the plugin follow the thread as an open Tests tab does, so the tests run when
// its files change; a thread followed only for subscriptions is let go wakeLinger after
// the last one ended. The host sends every live subscription again after each start.

const wakeTestsFinished = "tests-finished"

// maxWakeFailures bounds the failures a fire's payload lists.
const maxWakeFailures = 20

// testsHint is the line that subscribes to a thread's next test run.
func testsHint(h *pluginsdk.Host, thread string) string {
	return h.WakeHint(wakeTestsFinished, map[string]string{"thread": thread})
}

// wakeSubscribed arms a subscription: the thread is followed (wake-only when nothing
// else followed it) and its worktree gets a runner. A repeat of a known id is a re-arm.
func (a *app) wakeSubscribed(_ context.Context, h *pluginsdk.Host, s pluginsdk.WakeSubscription) {
	if s.Source != wakeTestsFinished {
		h.Log().Warn("subscription to an unknown wake source", "source", s.Source, "subscription", s.Subscription)
		return
	}
	id := s.Args["thread"]
	if id == "" {
		h.Log().Warn("tests-finished: no thread", "subscription", s.Subscription)
		return
	}
	a.mu.Lock()
	if a.wakeSubs[s.Subscription] == id {
		a.mu.Unlock()
		return
	}
	a.wakeSubs[s.Subscription] = id
	if t := a.lingers[id]; t != nil {
		t.Stop()
		delete(a.lingers, id)
	}
	if _, ok := a.threads[id]; !ok && s.Context.ThreadID == id && s.Context.WorktreePath != "" {
		a.threads[id] = threadInfo{ID: id, ProjectID: s.Context.ProjectID, WorktreePath: s.Context.WorktreePath}
	}
	st := a.st
	a.mu.Unlock()
	h.Log().Info("tests-finished armed", "thread", id, "subscription", s.Subscription)
	if st != nil { // else init follows it
		a.followForWake(id)
	}
}

// followForWake follows a subscribed thread: in the settings (wake-only unless it was
// followed already) and with a runner for its worktree when that is known (else the
// thread list brings it).
func (a *app) followForWake(id string) {
	st := a.st
	if !contains(st.followed(), id) {
		if err := st.update(func(s *settings) { s.Threads[id], s.WakeOnly[id] = true, true }); err != nil {
			a.log().Warn("saving settings", "err", err)
		}
	}
	if dir := a.threadDir(id); dir != "" {
		a.runnerFor(dir)
		a.changed(dir)
	}
}

// wakeUnsubscribed disarms a subscription. A wake-only thread without another one is let
// go after wakeLinger.
func (a *app) wakeUnsubscribed(_ context.Context, h *pluginsdk.Host, u pluginsdk.WakeUnsubscribed) {
	a.mu.Lock()
	id, ok := a.wakeSubs[u.Subscription]
	delete(a.wakeSubs, u.Subscription)
	a.mu.Unlock()
	if !ok {
		return
	}
	h.Log().Info("tests-finished disarmed", "thread", id, "subscription", u.Subscription)
	a.linger(id)
}

// linger starts the countdown to letting go of a thread followed only for wake
// subscriptions, unless one is live.
func (a *app) linger(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st == nil || !a.st.wakeOnly(id) || a.lingers[id] != nil {
		return
	}
	for _, t := range a.wakeSubs {
		if t == id {
			return
		}
	}
	a.lingers[id] = time.AfterFunc(a.cfg.wakeLinger, func() { a.unfollowWake(id) })
}

// unfollowWake lets go of a wake-only thread with no subscription: it is no longer
// followed, and its worktree's runner stops when no other followed thread works there.
func (a *app) unfollowWake(id string) {
	a.mu.Lock()
	delete(a.lingers, id)
	for _, t := range a.wakeSubs {
		if t == id {
			a.mu.Unlock()
			return
		}
	}
	st, dir := a.st, a.threads[id].WorktreePath
	a.mu.Unlock()
	if st == nil || !st.wakeOnly(id) {
		return
	}
	if err := st.update(func(s *settings) {
		delete(s.Threads, id)
		delete(s.WakeOnly, id)
		delete(s.FlagSets, id)
	}); err != nil {
		a.log().Warn("saving settings", "err", err)
		return
	}
	a.log().Info("tests-finished: no longer following", "thread", id)
	if h := a.host(); h != nil {
		_ = a.clearFlag(h, id, true)
	}
	if dir != "" {
		a.forgotten(dir)
	}
}

// initWakes runs in Init once the settings are open: it follows the threads subscribed
// before (a re-arm can come first), and starts the countdown for wake-only threads
// without a subscription (one that ended while the plugin was down; a re-arm stops it).
func (a *app) initWakes() {
	a.mu.Lock()
	subscribed := map[string]bool{}
	for _, id := range a.wakeSubs {
		subscribed[id] = true
	}
	a.mu.Unlock()
	for id := range subscribed {
		a.followForWake(id)
	}
	for _, id := range a.st.wakeOnlyThreads() {
		a.linger(id)
	}
}

// wakeThreads are the threads with a tests-finished subscription.
func (a *app) wakeThreads() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]bool{}
	for _, id := range a.wakeSubs {
		out[id] = true
	}
	return out
}

// fireFinished fires tests-finished for each subscribed thread working in dir after a
// run with a result.
func (a *app) fireFinished(h *pluginsdk.Host, dir string, rn *runSnap) {
	result := "fail"
	if rn.State == statePassed {
		result = "pass"
	}
	subs := a.wakeThreads()
	title := runTitle(rn)
	for _, id := range a.threadsOn(dir) {
		if !subs[id] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := h.FireWake(ctx, wakeTestsFinished, map[string]string{"thread": id, "result": result}, title, wakePayload(id, dir, result, rn))
		cancel()
		if err != nil {
			h.Log().Warn("tests-finished: fire", "thread", id, "err", err)
			continue
		}
		h.Log().Info("tests-finished fired", "thread", id, "title", title, "woke", n)
	}
}

// runTitle is a finished run in a line: "go test ./... passed", "go test ./... failed:
// TestAdd and 2 more", "go test ./... could not run: command not found: gotest".
func runTitle(rn *runSnap) string {
	cmd := truncRunes(rn.Command, 60)
	var t string
	switch rn.State {
	case statePassed:
		t = cmd + " passed"
	case stateFailed:
		t = cmd + " failed"
		switch n := rn.FailedCount; {
		case len(rn.Failures) == 1:
			t += ": " + rn.Failures[0].Name
		case len(rn.Failures) > 1:
			t += ": " + rn.Failures[0].Name + " and " + itoa(len(rn.Failures)-1) + " more"
		case n > 0:
			t += ": " + itoa(n) + " tests"
		default:
			t += " (exit " + itoa(rn.Exit) + ")"
		}
	default:
		t = cmd + " could not run"
		if rn.Problem != "" {
			t += ": " + rn.Problem
		}
	}
	return truncRunes(strings.ReplaceAll(t, "\n", " "), pluginsdk.MaxWakeTitle)
}

func wakePayload(thread, dir, result string, rn *runSnap) map[string]any {
	fails := []map[string]string{}
	for i, f := range rn.Failures {
		if i == maxWakeFailures {
			break
		}
		fails = append(fails, map[string]string{"name": truncRunes(f.Name, 200), "where": truncRunes(f.Where, 200)})
	}
	count := rn.FailedCount
	if count == 0 {
		count = len(rn.Failures)
	}
	p := map[string]any{
		"thread": thread, "dir": dir, "run": rn.ID, "result": result, "state": string(rn.State),
		"command": truncRunes(rn.Command, 500), "exit": rn.Exit, "trigger": rn.Trigger,
		"duration_ms": rn.Ended.Sub(rn.Started).Milliseconds(), "failed_count": count, "failures": fails,
	}
	if rn.Problem != "" {
		p["problem"] = truncRunes(rn.Problem, 500)
	}
	return p
}
