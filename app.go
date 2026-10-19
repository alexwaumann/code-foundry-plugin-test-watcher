package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// config holds what tests shorten or replace.
type config struct {
	debounce time.Duration // quiet time after a change before a run starts
	now      func() time.Time
	shell    func() (string, string) // the shell and its flag that run the command
	home     func() string           // the home directory (terminal runs need the worktree under it)
	termPoll time.Duration           // how often a script(1) terminal run's output and state are read
	readPoll time.Duration           // how often a terminal run's scrollback is read while it runs
	// wakeLinger is how long a thread followed only for a tests-finished subscription
	// stays followed after its last one ended (an agent subscribing again soon, a
	// one-shot after each wake, misses no change).
	wakeLinger time.Duration
}

func defaultConfig() config {
	return config{
		debounce:   2 * time.Second,
		now:        time.Now,
		shell:      loginShell,
		home:       func() string { home, _ := os.UserHomeDir(); return home },
		termPoll:   200 * time.Millisecond,
		readPoll:   time.Second,
		wakeLinger: 10 * time.Minute,
	}
}

// loginShell runs the test command the way the user's terminal would: their login
// shell, so PATH has go, npm and the rest even when the daemon's PATH is short.
func loginShell() (string, string) {
	if sh := os.Getenv("SHELL"); filepath.IsAbs(sh) {
		if _, err := os.Stat(sh); err == nil {
			return sh, "-lc"
		}
	}
	return "/bin/sh", "-c"
}

// threadInfo is what the threads event source says about a thread.
type threadInfo struct {
	ID           string `json:"id"`
	ProjectID    string `json:"projectId"`
	WorktreePath string `json:"worktreePath"`
	Name         string `json:"name"`
	Archived     bool   `json:"archived"`
	ArchivedAt   string `json:"archivedAt"`
	// Flags are the plugins' flags on the thread; the plugin reads whether its own is
	// still up (the user may clear it from the thread header or the CLI).
	Flags []struct {
		Plugin string `json:"plugin"`
	} `json:"flags"`
}

// pluginName is the plugin's name in plugin.json, the source of its flags.
const pluginName = "test-watcher"

// hasOwnFlag: the thread carries this plugin's flag.
func (t threadInfo) hasOwnFlag() bool {
	for _, f := range t.Flags {
		if f.Plugin == pluginName {
			return true
		}
	}
	return false
}

// worktreeInfo is what the project event source says about a worktree.
type worktreeInfo struct {
	ProjectID string          `json:"projectId"`
	Path      string          `json:"path"`
	Branch    string          `json:"branch"`
	Head      string          `json:"head"`
	Status    *worktreeStatus `json:"status"`
}

type worktreeStatus struct {
	Modified  int  `json:"modified"`
	Added     int  `json:"added"`
	Deleted   int  `json:"deleted"`
	Untracked int  `json:"untracked"`
	Renamed   int  `json:"renamed"`
	Conflicts int  `json:"conflicts"`
	Staged    int  `json:"staged"`
	Dirty     bool `json:"dirty"`
}

type projectInfo struct {
	ID        string         `json:"id"`
	Path      string         `json:"path"`
	Name      string         `json:"name"`
	Git       bool           `json:"git"`
	Worktrees []worktreeInfo `json:"worktrees"`
}

// app is the plugin's state: what it knows of threads, projects and worktrees from the
// host's events, a runner per watched worktree, and the open Tests tabs.
type app struct {
	cfg config

	mu        sync.Mutex
	h         *pluginsdk.Host
	st        *store
	threads   map[string]threadInfo
	projects  map[string]projectInfo
	worktrees map[string]worktreeInfo
	runners   map[string]*runner
	surfaces  map[string]*testsSurface
	flagged   map[string]bool // threads this plugin's flag is up on, as far as it knows
	// projectSynced: the project source's first snapshot came; a later one is a resync
	// (the plugin fell behind and may have missed files events).
	projectSynced bool
	wakeSubs      map[string]string      // tests-finished subscription → its thread
	lingers       map[string]*time.Timer // thread → the end of its wake-only follow

	// Terminal exits (terminal/exited) by terminal: a run waiting for one, or an exit
	// that came before the run started waiting.
	exitMu  sync.Mutex
	exits   map[string]chan int
	exitedE map[string]int
}

func newApp(cfg config) *app {
	return &app{
		cfg:       cfg,
		threads:   map[string]threadInfo{},
		projects:  map[string]projectInfo{},
		worktrees: map[string]worktreeInfo{},
		runners:   map[string]*runner{},
		surfaces:  map[string]*testsSurface{},
		flagged:   map[string]bool{},
		wakeSubs:  map[string]string{},
		lingers:   map[string]*time.Timer{},
		exits:     map[string]chan int{},
		exitedE:   map[string]int{},
	}
}

// onTerminalExit hands a terminal's exit status to the run waiting for it.
func (a *app) onTerminalExit(_ context.Context, _ *pluginsdk.Host, ex pluginsdk.TerminalExited) {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	if ch, ok := a.exits[ex.TerminalID]; ok {
		select {
		case ch <- ex.ExitCode:
		default:
		}
		return
	}
	a.exitedE[ex.TerminalID] = ex.ExitCode
}

// waitExit is the channel a terminal's exit status comes on (at once when it came
// already); doneExit forgets the terminal.
func (a *app) waitExit(id string) chan int {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	ch := make(chan int, 1)
	if code, ok := a.exitedE[id]; ok {
		ch <- code
		delete(a.exitedE, id)
	}
	a.exits[id] = ch
	return ch
}

func (a *app) doneExit(id string) {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	delete(a.exits, id)
	delete(a.exitedE, id)
}

func (a *app) plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Init:               a.init,
		OnEvent:            a.onEvent,
		Shutdown:           a.shutdown,
		OnTerminalExit:     a.onTerminalExit,
		OnWakeSubscribed:   a.wakeSubscribed,
		OnWakeUnsubscribed: a.wakeUnsubscribed,
		Commands: map[string]pluginsdk.CommandFunc{
			"run":           a.cmdRun,
			"stop":          a.cmdStop,
			"set-command":   a.cmdSetCommand,
			"toggle-watch":  a.cmdToggleWatch,
			"send-failures": a.cmdSendFailures,
			"clear-flag":    a.cmdClearFlag,
		},
		Surfaces: map[string]pluginsdk.SurfaceFactory{"tests": a.openTests},
	}
}

func (a *app) init(ctx context.Context, h *pluginsdk.Host) error {
	st, err := openStore(h.DataDir())
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.h, a.st = h, st
	for _, f := range h.Flags() {
		if f.Level == pluginsdk.FlagError {
			a.flagged[f.ThreadID] = true
		}
	}
	a.mu.Unlock()
	h.Log().Info("test-watcher is up", "host", h.HostVersion(), "dev", h.Dev(), "followed", len(st.followed()), "terminals", terminalsUsable(h))
	a.closeStaleTerminals(ctx, h)
	a.initWakes()
	// threads: which worktree and project each thread works in, and when one goes away;
	// project: worktree changes (a file changed, a commit, a branch switch) and removals.
	if _, err := h.Subscribe(ctx, "threads", "project"); err != nil {
		return err
	}
	return nil
}

func (a *app) shutdown(context.Context, *pluginsdk.Host) {
	a.mu.Lock()
	rs := make([]*runner, 0, len(a.runners))
	for _, r := range a.runners {
		rs = append(rs, r)
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(1)
		go func() { defer wg.Done(); r.close(false) }()
	}
	wg.Wait()
}

func (a *app) log() *slog.Logger {
	a.mu.Lock()
	h := a.h
	a.mu.Unlock()
	if h == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log()
}

func (a *app) host() *pluginsdk.Host {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.h
}

// threadDir is a thread's worktree ("" when unknown).
func (a *app) threadDir(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.threads[id].WorktreePath
}

// follow makes the plugin watch a thread's worktree from now on (and after restarts),
// for good: one followed for wake subscriptions only is no longer let go after them.
func (a *app) follow(t threadInfo) *runner {
	a.mu.Lock()
	if _, ok := a.threads[t.ID]; !ok {
		a.threads[t.ID] = t
	}
	st := a.st
	if l := a.lingers[t.ID]; l != nil {
		l.Stop()
		delete(a.lingers, t.ID)
	}
	a.mu.Unlock()
	if st != nil && (!contains(st.followed(), t.ID) || st.wakeOnly(t.ID)) {
		if err := st.update(func(s *settings) { s.Threads[t.ID] = true; delete(s.WakeOnly, t.ID) }); err != nil {
			a.log().Warn("saving settings", "err", err)
		}
	}
	return a.runnerFor(t.WorktreePath)
}

// runnerFor returns the worktree's runner, starting one.
func (a *app) runnerFor(dir string) *runner {
	if dir == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runners[dir]
	if r == nil {
		r = newRunner(a, dir)
		a.runners[dir] = r
	}
	return r
}

func (a *app) existingRunner(dir string) *runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runners[dir]
}

// threadsOn lists the followed threads working in dir.
func (a *app) threadsOn(dir string) []string {
	a.mu.Lock()
	st := a.st
	var ids []string
	for id, t := range a.threads {
		if t.WorktreePath == dir {
			ids = append(ids, id)
		}
	}
	a.mu.Unlock()
	if st == nil {
		return nil
	}
	followed := st.followed()
	out := ids[:0]
	for _, id := range ids {
		if contains(followed, id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// watched: a followed thread in dir has watching on.
func (a *app) watched(dir string) bool {
	for _, id := range a.threadsOn(dir) {
		if a.st.watching(id) {
			return true
		}
	}
	return false
}

// forgotten: no followed thread works in dir any more (its runner ends).
func (a *app) forgotten(dir string) bool {
	if len(a.threadsOn(dir)) > 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.runners[dir]; r != nil {
		delete(a.runners, dir)
		go r.close(false)
	}
	return true
}

// projectOf is the project a worktree belongs to.
func (a *app) projectOf(dir string) (id, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if w, ok := a.worktrees[dir]; ok && w.ProjectID != "" {
		return w.ProjectID, a.projects[w.ProjectID].Name
	}
	for _, t := range a.threads {
		if t.WorktreePath == dir && t.ProjectID != "" {
			return t.ProjectID, a.projects[t.ProjectID].Name
		}
	}
	return "", ""
}

func (a *app) branchOf(dir string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.worktrees[dir].Branch
}

// commandFor is the test command for a worktree: the project's saved one, else the
// detected default.
func (a *app) commandFor(dir string) string {
	cmd, _ := a.commandAndSource(dir)
	return cmd
}

// commandAndSource also says whether the command was set ("set") or detected
// ("detected").
func (a *app) commandAndSource(dir string) (string, string) {
	project, _ := a.projectOf(dir)
	if a.st != nil && project != "" {
		if c := a.st.command(project); c != "" {
			return c, "set"
		}
	}
	if c := detectCommand(dir); c != "" {
		return c, "detected"
	}
	return "", ""
}

// changed re-renders the Tests tabs showing dir.
func (a *app) changed(dir string) {
	a.mu.Lock()
	var ss []*testsSurface
	for _, s := range a.surfaces {
		if a.threads[s.thread].WorktreePath == dir || s.dir == dir {
			ss = append(ss, s)
		}
	}
	a.mu.Unlock()
	for _, s := range ss {
		s.dirty()
	}
}

// finished records a run's result: flags on the followed threads in its worktree, and
// the views. The flag is sticky: a failing run raises it, a passing run clears it. A
// thread with watching off is flagged only by a run it asked for (Run now).
func (a *app) finished(dir string, rn *runSnap) {
	h := a.host()
	if h != nil && rn.State == stateError {
		a.fireFinished(h, dir, rn)
	}
	if h == nil || rn.State != statePassed && rn.State != stateFailed {
		a.changed(dir)
		return
	}
	for _, id := range a.threadsOn(dir) {
		switch {
		case rn.State == stateFailed && (rn.Trigger == "manual" || a.st.watching(id)):
			a.raiseFlag(h, id, rn)
		case rn.State == statePassed:
			a.clearFlag(h, id, true)
		}
	}
	a.fireFinished(h, dir, rn)
	a.changed(dir)
}

// raiseFlag puts the error flag on a thread for a failing run, unless it was already
// raised for this set of failing tests: then it is up still, or the user cleared it
// and it stays down until the set changes.
func (a *app) raiseFlag(h *pluginsdk.Host, id string, rn *runSnap) {
	set := failSet(rn)
	if a.st.flagSet(id) == set {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.SetFlag(ctx, pluginsdk.Flag{ThreadID: id, Level: pluginsdk.FlagError, Reason: flagReason(rn)}); err != nil {
		h.Log().Warn("flag", "thread", id, "err", err)
		return
	}
	a.setFlagged(id, true)
	a.saveFlagSet(id, set)
}

// clearFlag takes the flag off a thread. forget (a passing run, Stop, watching off)
// also forgets the failing set it was raised for, so the next failure raises it again;
// the user's Clear flag keeps it, so the same failures do not.
func (a *app) clearFlag(h *pluginsdk.Host, id string, forget bool) error {
	a.mu.Lock()
	up := a.flagged[id]
	a.mu.Unlock()
	known := a.st.flagSet(id) != ""
	if !up && !known && forget {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.SetFlag(ctx, pluginsdk.Flag{ThreadID: id, Level: pluginsdk.FlagNone}); err != nil {
		h.Log().Warn("flag", "thread", id, "err", err)
		return err
	}
	a.setFlagged(id, false)
	if forget && known {
		a.saveFlagSet(id, "")
	}
	return nil
}

// clearFlagsOn clears (and forgets) the flags of the followed threads working in dir.
func (a *app) clearFlagsOn(dir string) {
	h := a.host()
	if h == nil {
		return
	}
	for _, id := range a.threadsOn(dir) {
		_ = a.clearFlag(h, id, true)
	}
	a.changed(dir)
}

func (a *app) saveFlagSet(id, set string) {
	if err := a.st.update(func(s *settings) {
		if set == "" {
			delete(s.FlagSets, id)
		} else {
			s.FlagSets[id] = set
		}
	}); err != nil {
		a.log().Warn("saving settings", "err", err)
	}
}

func (a *app) isFlagged(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flagged[id]
}

func (a *app) setFlagged(id string, on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if on {
		a.flagged[id] = true
	} else {
		delete(a.flagged, id)
	}
}

// failSet identifies a failing run's failures: the failing tests with their packages
// or files, sorted; a run whose output names none, by its count or exit status.
func failSet(rn *runSnap) string {
	if len(rn.Failures) == 0 {
		if rn.FailedCount > 0 {
			return itoa(rn.FailedCount) + " failed"
		}
		return "exit " + itoa(rn.Exit)
	}
	names := make([]string, len(rn.Failures))
	for i, f := range rn.Failures {
		names[i] = f.Where + "\t" + f.Name
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// flagReason is the flag's one line: "Tests failing: TestAdd (example.com/calc)", or a
// count; at most 80 characters.
func flagReason(rn *runSnap) string {
	reason := "Tests failing"
	switch n := rn.FailedCount; {
	case len(rn.Failures) == 1:
		reason += ": " + rn.Failures[0].Name
	case len(rn.Failures) > 1:
		reason += ": " + rn.Failures[0].Name + " and " + itoa(len(rn.Failures)-1) + " more"
	case n > 0:
		reason += ": " + itoa(n) + " failed"
	default:
		reason += ": exit " + itoa(rn.Exit)
	}
	return truncRunes(reason, 80)
}

// worktreeGone stops a deleted worktree's runs and shows it.
func (a *app) worktreeGone(dir string) {
	if r := a.existingRunner(dir); r != nil {
		go func() {
			r.close(true)
			a.changed(dir)
		}()
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
