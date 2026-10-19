package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

const maxCommandLen = 500

// threadFor resolves the command's thread: its thread arg, else the caller's context.
func (a *app) threadFor(req pluginsdk.CommandRequest) (threadInfo, error) {
	id := req.Arg("thread")
	if id == "" {
		id = req.Context.ThreadID
	}
	if id == "" {
		return threadInfo{}, pluginsdk.NewError(pluginsdk.CodeInvalidParams, "no thread: pass --thread or run it from a thread")
	}
	a.mu.Lock()
	t, ok := a.threads[id]
	a.mu.Unlock()
	if !ok {
		if req.Context.ThreadID == id && req.Context.WorktreePath != "" {
			return threadInfo{ID: id, ProjectID: req.Context.ProjectID, WorktreePath: req.Context.WorktreePath}, nil
		}
		return threadInfo{}, pluginsdk.NewError(pluginsdk.CodeNotFound, "unknown thread %s", id)
	}
	if t.WorktreePath == "" {
		return threadInfo{}, pluginsdk.NewError(pluginsdk.CodeUnavailable, "thread %s has no worktree", id)
	}
	return t, nil
}

type runJSON struct {
	Thread  string `json:"thread"`
	Dir     string `json:"dir"`
	Command string `json:"command,omitempty"`
	Run     int    `json:"run,omitempty"`
	Watch   *bool  `json:"watch,omitempty"`
	Stopped *bool  `json:"stopped,omitempty"`
	Source  string `json:"source,omitempty"`
}

func result(msg string, js runJSON) (pluginsdk.CommandResult, error) {
	data, err := json.Marshal(js)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	return pluginsdk.CommandResult{Message: msg, JSON: data}, nil
}

func (a *app) cmdRun(_ context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	r := a.follow(t)
	rn, err := r.start("manual")
	if errors.Is(err, errNoCommand) {
		return pluginsdk.CommandResult{}, pluginsdk.NewError(pluginsdk.CodeUnavailable,
			"no test command for this project: set one with `code-foundry plugin test-watcher set-command \"<command>\"`")
	}
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	return result("Running `"+rn.Command+"` in "+t.WorktreePath+"\nWait for it to finish: "+testsHint(h, t.ID), runJSON{Thread: t.ID, Dir: t.WorktreePath, Command: rn.Command, Run: rn.ID})
}

func (a *app) cmdStop(_ context.Context, _ *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	r := a.existingRunner(t.WorktreePath)
	stopped := r != nil && r.stop()
	msg := "No test run in progress"
	if stopped {
		msg = "Stopped the test run"
		// The tests' state is unknown now: the flag goes until a run fails again.
		a.clearFlagsOn(t.WorktreePath)
	}
	return result(msg, runJSON{Thread: t.ID, Dir: t.WorktreePath, Stopped: &stopped})
}

// userClearFlag takes the failing-tests flag off a thread for the user. It stays down
// for the same failing tests; a different set raises it again, a passing run resets.
func (a *app) userClearFlag(h *pluginsdk.Host, t threadInfo) (bool, error) {
	was := a.isFlagged(t.ID)
	if a.st.flagSet(t.ID) == "" {
		// A flag from before this version, or none: remember the failures shown now.
		if r := a.existingRunner(t.WorktreePath); r != nil {
			cur, res, _, _, _ := r.snapshot(false)
			if rn := failing(model{cur: cur, result: res}); rn != nil && rn.State == stateFailed {
				a.saveFlagSet(t.ID, failSet(rn))
			}
		}
	}
	if err := a.clearFlag(h, t.ID, false); err != nil {
		return false, err
	}
	a.changed(t.WorktreePath)
	return was, nil
}

func (a *app) cmdClearFlag(_ context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	was, err := a.userClearFlag(h, t)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	msg := "No flag on this thread"
	if was {
		msg = "Cleared the flag; it comes back when other tests fail"
	}
	return result(msg, runJSON{Thread: t.ID, Dir: t.WorktreePath})
}

func (a *app) cmdSendFailures(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	if _, err := a.sendFailures(ctx, h, t, -1, -1); err != nil {
		return pluginsdk.CommandResult{}, err
	}
	return result("Sent the failing tests to Claude", runJSON{Thread: t.ID, Dir: t.WorktreePath})
}

// validCommand checks a test command typed by the user: one line, not too long.
func validCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	switch {
	case strings.ContainsAny(cmd, "\n\r\x00"):
		return "", errors.New("the command must be one line")
	case utf8.RuneCountInString(cmd) > maxCommandLen:
		return "", errors.New("the command is longer than 500 characters")
	}
	return cmd, nil
}

// setCommand saves (or with "" forgets) a project's test command.
func (a *app) setCommand(t threadInfo, cmd string) (string, string, error) {
	cmd, err := validCommand(cmd)
	if err != nil {
		return "", "", pluginsdk.NewError(pluginsdk.CodeInvalidParams, "%v", err)
	}
	project, _ := a.projectOf(t.WorktreePath)
	if project == "" {
		project = t.ProjectID
	}
	if project == "" {
		return "", "", pluginsdk.NewError(pluginsdk.CodeUnavailable, "thread %s has no project", t.ID)
	}
	if err := a.st.update(func(s *settings) {
		if cmd == "" {
			delete(s.Commands, project)
		} else {
			s.Commands[project] = cmd
		}
	}); err != nil {
		return "", "", err
	}
	a.follow(t)
	a.changed(t.WorktreePath)
	now, source := a.commandAndSource(t.WorktreePath)
	return now, source, nil
}

func (a *app) cmdSetCommand(_ context.Context, _ *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	cmd, source, err := a.setCommand(t, req.Arg("command"))
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	msg := "Test command: " + cmd
	switch {
	case cmd == "":
		msg = "No test command set, and none detected"
	case source == "detected":
		msg = "Test command reset to the detected " + cmd
	}
	return result(msg, runJSON{Thread: t.ID, Dir: t.WorktreePath, Command: cmd, Source: source})
}

// setWatch turns watching on or off for a thread.
func (a *app) setWatch(t threadInfo, on bool) error {
	if err := a.st.update(func(s *settings) {
		if on {
			delete(s.Unwatched, t.ID)
		} else {
			s.Unwatched[t.ID] = true
		}
	}); err != nil {
		return err
	}
	r := a.follow(t)
	if !on {
		if h := a.host(); h != nil {
			_ = a.clearFlag(h, t.ID, true)
		}
	}
	if !on && !a.watched(t.WorktreePath) {
		r.mu.Lock()
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		r.mu.Unlock()
	}
	a.changed(t.WorktreePath)
	return nil
}

func (a *app) cmdToggleWatch(_ context.Context, _ *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.threadFor(req)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	on := !a.st.watching(t.ID)
	if err := a.setWatch(t, on); err != nil {
		return pluginsdk.CommandResult{}, err
	}
	msg := "Watching for changes: off"
	if on {
		msg = "Watching for changes: on"
	}
	return result(msg, runJSON{Thread: t.ID, Dir: t.WorktreePath, Watch: &on})
}
