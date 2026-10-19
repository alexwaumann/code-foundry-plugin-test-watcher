package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// event is the part of the host's events this plugin reads (the JSON `code-foundry
// events --json --source thread-list|project` prints).
type event struct {
	ThreadList *struct {
		Snapshot *struct {
			Threads []threadInfo `json:"threads"`
		} `json:"snapshot"`
		Updated   *threadInfo `json:"updated"`
		RemovedID string      `json:"removedId"`
	} `json:"threadList"`
	Project *struct {
		Snapshot *struct {
			Projects []projectInfo `json:"projects"`
		} `json:"snapshot"`
		ProjectUpdated   *projectInfo    `json:"projectUpdated"`
		ProjectRemovedID string          `json:"projectRemovedId"`
		WorktreeUpdated  *worktreeInfo   `json:"worktreeUpdated"`
		WorktreeRemoved  json.RawMessage `json:"worktreeRemoved"`
		FilesChanged     *filesChanged   `json:"worktreeFilesChanged"`
	} `json:"project"`
}

// filesChanged is project.worktreeFilesChanged: the files that changed in a worktree
// over the daemon's 1 s window. Gitignored paths are left out, and only worktrees with
// a live thread send it. A directory ends in "/" (only when removed or renamed);
// truncated: more files changed than listed.
type filesChanged struct {
	ProjectID string `json:"projectId"`
	Path      string `json:"path"`
	Files     []struct {
		Path string `json:"path"`
		Op   string `json:"op"` // FILE_OP_CREATED, _MODIFIED, _REMOVED, _RENAMED
	} `json:"files"`
	Truncated bool `json:"truncated"`
}

// onEvent updates what the plugin knows and pokes the runners. It runs on the
// notification goroutine, so it only records and hands off.
func (a *app) onEvent(_ context.Context, _ *pluginsdk.Host, ev pluginsdk.Event) {
	var e event
	if err := json.Unmarshal(ev.Event, &e); err != nil {
		a.log().Warn("undecodable event", "err", err)
		return
	}
	switch {
	case e.ThreadList != nil:
		a.onThreads(e)
	case e.Project != nil:
		a.onProject(e)
	}
}

func (a *app) onThreads(e event) {
	tl := e.ThreadList
	var removed []string
	a.mu.Lock()
	switch {
	case tl.Snapshot != nil:
		old := a.threads
		a.threads = map[string]threadInfo{}
		for _, t := range tl.Snapshot.Threads {
			if !t.Archived && t.ArchivedAt == "" {
				a.threads[t.ID] = t
			}
			a.syncFlagLocked(t)
		}
		for id := range old {
			if _, ok := a.threads[id]; !ok {
				removed = append(removed, id)
			}
		}
	case tl.Updated != nil:
		t := *tl.Updated
		if t.Archived || t.ArchivedAt != "" {
			delete(a.threads, t.ID)
			removed = append(removed, t.ID)
		} else {
			a.threads[t.ID] = t
		}
		a.syncFlagLocked(t)
	case tl.RemovedID != "":
		delete(a.threads, tl.RemovedID)
		delete(a.flagged, tl.RemovedID)
		removed = append(removed, tl.RemovedID)
	}
	runnerDirs := make([]string, 0, len(a.runners))
	for dir := range a.runners {
		runnerDirs = append(runnerDirs, dir)
	}
	st := a.st
	followed := []string{}
	if st != nil {
		followed = st.followed()
	}
	var dirs []string
	for _, id := range followed {
		if t, ok := a.threads[id]; ok && t.WorktreePath != "" {
			dirs = append(dirs, t.WorktreePath)
		}
	}
	a.mu.Unlock()
	// A followed thread's worktree gets its runner (after a restart, or when the
	// thread moved to another worktree).
	for _, d := range dirs {
		a.runnerFor(d)
		a.changed(d)
	}
	// A worktree no followed thread works in any more (archived, removed, moved) lets
	// its runner go.
	for _, d := range runnerDirs {
		a.forgotten(d)
	}
	var gone []string
	for _, id := range removed {
		if contains(followed, id) {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 && st != nil {
		go func() {
			if err := st.update(func(s *settings) {
				for _, id := range gone {
					delete(s.Threads, id)
					delete(s.Unwatched, id)
					delete(s.FlagSets, id)
					delete(s.WakeOnly, id)
				}
			}); err != nil {
				a.log().Warn("saving settings", "err", err)
			}
		}()
	}
}

func (a *app) onProject(e event) {
	p := e.Project
	var trigger, gone []string
	a.mu.Lock()
	switch {
	case p.Snapshot != nil:
		// A resync: files events may have been lost, so each followed worktree runs
		// once (trigger skips the ones with watching off).
		if a.projectSynced {
			for dir := range a.runners {
				trigger = append(trigger, dir)
			}
		}
		a.projectSynced = true
		a.projects = map[string]projectInfo{}
		old := a.worktrees
		a.worktrees = map[string]worktreeInfo{}
		for _, pr := range p.Snapshot.Projects {
			a.projects[pr.ID] = pr
			for _, w := range pr.Worktrees {
				a.worktrees[w.Path] = w
			}
		}
		for dir := range old {
			if _, ok := a.worktrees[dir]; !ok {
				gone = append(gone, dir)
			}
		}
	case p.ProjectUpdated != nil:
		a.projects[p.ProjectUpdated.ID] = *p.ProjectUpdated
		for _, w := range p.ProjectUpdated.Worktrees {
			a.worktrees[w.Path] = w
		}
	case p.WorktreeUpdated != nil:
		w := *p.WorktreeUpdated
		old, had := a.worktrees[w.Path]
		a.worktrees[w.Path] = w
		// The status counts, the head or the branch moved: files changed (a commit or a
		// checkout; an edit's files event comes too, and the debounce makes it one run).
		if had && (old.Head != w.Head || old.Branch != w.Branch || !sameStatus(old.Status, w.Status)) {
			trigger = append(trigger, w.Path)
		}
	case p.FilesChanged != nil:
		fc := p.FilesChanged
		a.mu.Unlock()
		// Only a followed worktree has a runner; an event for another does nothing.
		if r := a.existingRunner(fc.Path); r != nil && (len(fc.Files) > 0 || fc.Truncated) {
			files := make([]string, len(fc.Files))
			for i, f := range fc.Files {
				files[i] = f.Path
			}
			r.filesChanged(files, fc.Truncated)
		}
		return
	case len(p.WorktreeRemoved) > 0:
		var w struct {
			Path string `json:"path"`
		}
		var path string
		if json.Unmarshal(p.WorktreeRemoved, &w) == nil && w.Path != "" {
			path = w.Path
		} else {
			_ = json.Unmarshal(p.WorktreeRemoved, &path)
		}
		if path != "" {
			delete(a.worktrees, path)
			gone = append(gone, path)
		}
	}
	a.mu.Unlock()
	for _, dir := range trigger {
		if r := a.existingRunner(dir); r != nil {
			r.trigger("change")
		}
	}
	for _, dir := range gone {
		if _, err := os.Stat(dir); err != nil {
			a.worktreeGone(dir)
		}
	}
}

// syncFlagLocked follows the thread's own flag as the host reports it: the user may
// clear it (the thread header's ×, `code-foundry thread flag clear`), and archiving
// clears it. Events arrive in order, so the last one is the host's state. a.mu held.
func (a *app) syncFlagLocked(t threadInfo) {
	if t.hasOwnFlag() {
		a.flagged[t.ID] = true
	} else {
		delete(a.flagged, t.ID)
	}
}

func sameStatus(a, b *worktreeStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
