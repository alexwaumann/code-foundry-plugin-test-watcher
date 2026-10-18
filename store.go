package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// settings is what the plugin keeps in its data directory (settings.json): the test
// command per project and the threads the user turned watching off for.
type settings struct {
	// Commands are the test commands set per project id; a project without one uses the
	// detected default.
	Commands map[string]string `json:"commands,omitempty"`
	// Unwatched are the threads whose watching is off (it is on by default).
	Unwatched map[string]bool `json:"unwatched,omitempty"`
	// Threads are the threads the plugin follows: the ones whose Tests tab was opened or
	// that ran a command. They are watched again after a restart.
	Threads map[string]bool `json:"threads,omitempty"`
}

// store guards settings and writes them through to dir/settings.json.
type store struct {
	mu   sync.Mutex
	path string
	s    settings
}

func openStore(dir string) (*store, error) {
	st := &store{path: filepath.Join(dir, "settings.json")}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading settings: %w", err)
	default:
		if err := json.Unmarshal(data, &st.s); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", st.path, err)
		}
	}
	return st, nil
}

func (st *store) command(project string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.Commands[project]
}

func (st *store) watching(thread string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return !st.s.Unwatched[thread]
}

func (st *store) followed() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.s.Threads))
	for id := range st.s.Threads {
		out = append(out, id)
	}
	return out
}

// update changes the settings under the lock and saves them.
func (st *store) update(f func(s *settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Commands == nil {
		st.s.Commands = map[string]string{}
	}
	if st.s.Unwatched == nil {
		st.s.Unwatched = map[string]bool{}
	}
	if st.s.Threads == nil {
		st.s.Threads = map[string]bool{}
	}
	f(&st.s)
	data, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}
