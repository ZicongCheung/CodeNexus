package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

const stateVersion = 1

type stateEntry struct {
	Credits      int64  `json:"credits"`
	CreditsKnown bool   `json:"credits_known"`
	CoolingUntil int64  `json:"cooling_until,omitempty"`
	CoolKind     string `json:"cool_kind,omitempty"`
	SoftStreak   int    `json:"soft_streak,omitempty"`
	Fails        int    `json:"fails,omitempty"`
	BreakerUntil int64  `json:"breaker_until,omitempty"`
	Success      int64  `json:"success,omitempty"`
	Errors       int64  `json:"errors,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	LastUsed     int64  `json:"last_used,omitempty"`
}

type stateFile struct {
	Version int                   `json:"version"`
	Entries map[string]stateEntry `json:"entries"`
}

// SetStatePath sets the file used to persist runtime pool state.
func (p *Pool) SetStatePath(path string) {
	p.mu.Lock()
	p.statePath = path
	p.mu.Unlock()
}

// SetErrorHandler registers a callback for background flush failures.
func (p *Pool) SetErrorHandler(fn func(error)) {
	p.mu.Lock()
	p.errFn = fn
	p.mu.Unlock()
}

// LoadState restores runtime state previously written by Flush. A missing file
// is not an error.
func (p *Pool) LoadState() error {
	p.mu.Lock()
	path := p.statePath
	p.mu.Unlock()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, se := range sf.Entries {
		e, ok := p.entries[k]
		if !ok {
			continue
		}
		e.credits = se.Credits
		e.creditsKnown = se.CreditsKnown
		e.coolingUntil = fromUnix(se.CoolingUntil)
		e.coolKind = upstream.ErrorKind(se.CoolKind)
		e.softStreak = se.SoftStreak
		e.fails = se.Fails
		e.breakerUntil = fromUnix(se.BreakerUntil)
		e.success = se.Success
		e.errors = se.Errors
		e.disabled = se.Disabled
		e.lastUsed = fromUnix(se.LastUsed)
	}
	return nil
}

// Flush writes the current runtime state to disk atomically.
func (p *Pool) Flush() error {
	p.mu.Lock()
	path := p.statePath
	if path == "" || !p.dirty {
		p.mu.Unlock()
		return nil
	}
	sf := stateFile{Version: stateVersion, Entries: make(map[string]stateEntry, len(p.entries))}
	for k, e := range p.entries {
		sf.Entries[k] = stateEntry{
			Credits:      e.credits,
			CreditsKnown: e.creditsKnown,
			CoolingUntil: toUnix(e.coolingUntil),
			CoolKind:     string(e.coolKind),
			SoftStreak:   e.softStreak,
			Fails:        e.fails,
			BreakerUntil: toUnix(e.breakerUntil),
			Success:      e.success,
			Errors:       e.errors,
			Disabled:     e.disabled,
			LastUsed:     toUnix(e.lastUsed),
		}
	}
	p.dirty = false
	p.mu.Unlock()

	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StartFlusher periodically flushes state and drops expired sticky bindings
// until Close is called.
func (p *Pool) StartFlusher(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-t.C:
				p.GCSticky()
				if err := p.Flush(); err != nil {
					p.mu.Lock()
					fn := p.errFn
					p.mu.Unlock()
					if fn != nil {
						fn(err)
					}
				}
			}
		}
	}()
}

// Close stops the flusher and flushes one last time.
func (p *Pool) Close() error {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.stopCh)
	}
	p.mu.Unlock()
	return p.Flush()
}

func fromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
