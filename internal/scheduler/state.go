package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// State remembers the last day each scheduled family completed a run. The
// upstream rewards settle on a daily calendar, so the day is what matters: if
// the process was down when a slot passed, the catch-up pass can tell that
// today's run never happened.
//
// The record is advisory. Losing it only costs at most one redundant sweep,
// which every task tolerates because every task is idempotent.
type State struct {
	mu   sync.Mutex
	path string
	data stateData
}

type stateData struct {
	// LastRun maps a family name to the last completed day (YYYY-MM-DD in the
	// scheduler's timezone).
	LastRun map[string]string `json:"last_run"`
}

// LoadState reads the state file, returning an empty state when it is absent
// or unreadable. An empty path keeps the state in memory only.
func LoadState(path string) *State {
	s := &State{path: path, data: stateData{LastRun: map[string]string{}}}
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var d stateData
	if err := json.Unmarshal(b, &d); err != nil {
		return s
	}
	if d.LastRun == nil {
		d.LastRun = map[string]string{}
	}
	s.data = d
	return s
}

// Last returns the last completed day for a family, or "" when it has never
// run.
func (s *State) Last(name string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LastRun[name]
}

// Mark records a family as completed for a day and persists the state. A write
// failure is returned but is not fatal: the in-memory record still prevents a
// redundant catch-up within this process.
func (s *State) Mark(name, day string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.data.LastRun == nil {
		s.data.LastRun = map[string]string{}
	}
	if s.data.LastRun[name] == day {
		s.mu.Unlock()
		return nil
	}
	s.data.LastRun[name] = day
	path := s.path
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil || path == "" {
		return err
	}
	b = append(b, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return mkErr
		}
	}
	tmp := path + ".tmp"
	if wErr := os.WriteFile(tmp, b, 0o600); wErr != nil {
		return wErr
	}
	return os.Rename(tmp, path)
}
