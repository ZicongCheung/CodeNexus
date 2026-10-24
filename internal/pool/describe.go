package pool

import (
	"sort"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// AccountStatus is a read-only snapshot of one account's runtime state, used by
// the console. It never exposes credential material.
type AccountStatus struct {
	Key          string          `json:"key"`
	Source       upstream.Source `json:"source"`
	UID          string          `json:"uid"`
	Label        string          `json:"label,omitempty"`
	Enabled      bool            `json:"enabled"`
	Paused       bool            `json:"paused"`
	Healthy      bool            `json:"healthy"`
	Cooling      bool            `json:"cooling"`
	Broken       bool            `json:"broken"`
	BlockedUntil time.Time       `json:"blocked_until,omitempty"`
	Credits      int64           `json:"credits"`
	CreditsTotal int64           `json:"credits_total,omitempty"`
	CreditsKnown bool            `json:"credits_known"`
	// Unlimited marks an unmetered grant, which is not a zero balance.
	Unlimited       bool   `json:"unlimited,omitempty"`
	Expiring        int64  `json:"expiring,omitempty"`
	EarliestExpiry  int64  `json:"earliest_expiry,omitempty"`
	InFlight        int    `json:"in_flight"`
	Success         int64  `json:"success"`
	Errors          int64  `json:"errors"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	CheckinDay      string `json:"checkin_day,omitempty"`
	// Local marks an entry that stands for this machine's CLI rather than a
	// credential, so the console can say so instead of showing an empty quota.
	Local bool `json:"local,omitempty"`

	// Token throughput for this process, shown in the account row beside the
	// success and failure counts it shares a lifetime with.
	TotalTokens         int64   `json:"total_tokens,omitempty"`
	LastTokensPerSecond float64 `json:"last_tokens_per_second,omitempty"`
	// LastSuccessAt is when this account last served a turn, so an account that
	// has quietly stopped being chosen is visible without reading the log.
	LastSuccessAt int64 `json:"last_success_at,omitempty"`
	// ModelLocks are per-model cooldowns: the model is not missing, it is
	// temporarily refused for this account. The console says so with a time
	// instead of letting it look like a missing capability.
	ModelLocks []ModelLock `json:"model_locks,omitempty"`
	// Actions are the maintenance operations this account can actually take
	// right now. The pool owns them because it owns the state they depend on;
	// the console renders the list and never guesses.
	Actions []string `json:"actions,omitempty"`
}

// ModelLock is one model refused for one account until a point in time.
type ModelLock struct {
	Model string `json:"model"`
	Until int64  `json:"until"`
}

// Describe returns a snapshot of every account in the pool, in pool order.
func (p *Pool) Describe() []AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]AccountStatus, 0, len(p.order))
	for _, k := range p.order {
		e := p.entries[k]
		acc := e.acc
		st := AccountStatus{
			Key:             e.key,
			Source:          acc.Source,
			UID:             acc.UID,
			Label:           acc.Label,
			Enabled:         e.active(),
			Paused:          e.paused,
			Healthy:         e.selectable(now, e.lastModel, p.opts.MaxInFlight),
			Cooling:         e.cooling(now),
			Broken:          e.broken(now),
			Credits:         e.credits,
			CreditsTotal:    e.creditsTotal,
			CreditsKnown:    e.creditsKnown,
			Unlimited:       e.creditsUnlimited,
			Expiring:        e.expiring,
			InFlight:        e.inFlight,
			Success:         e.success,
			Errors:          e.errors,
			HasRefreshToken: acc.RefreshTokenValue() != "",
			ExpiresAt:       acc.ExpiresAtValue(),
			CheckinDay:      e.checkinDay,
			Local:           acc.Local,
		}
		if !e.earliestAt.IsZero() {
			st.EarliestExpiry = e.earliestAt.Unix()
		}
		if d := e.blockDelay(now, e.lastModel); d > 0 {
			st.BlockedUntil = now.Add(d)
		}
		st.TotalTokens = e.totalTokens
		st.LastTokensPerSecond = e.lastTPS
		if !e.lastSuccess.IsZero() {
			st.LastSuccessAt = e.lastSuccess.Unix()
		}
		st.ModelLocks = e.modelLocks(now)
		st.Actions = e.actions()
		out = append(out, st)
	}
	return out
}

// modelLocks reports the models this account is currently refusing, each with
// the moment the refusal lifts. Expired entries are dropped rather than shown
// as locked-until-the-past.
func (e *entry) modelLocks(now time.Time) []ModelLock {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	out := make([]ModelLock, 0, len(e.modelCooldowns))
	for m, until := range e.modelCooldowns {
		if !until.After(now) {
			continue
		}
		out = append(out, ModelLock{Model: m, Until: until.Unix()})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until < out[j].Until })
	return out
}

// actions reports the maintenance operations that are valid for this account
// in its current state, so the console can render exactly the buttons that
// will work instead of a fixed row with half of them permanently greyed out.
//
// Check-in is deliberately absent: only the scheduler knows whether a source
// has a check-in programme at all, so the panel adds it from CheckinApplicable
// rather than the pool guessing.
//
// The task board is WorkBuddy's: it mirrors that task API, and a Trae account
// has no task list to open, so offering the entry would only produce an empty
// drawer.
func (e *entry) actions() []string {
	out := []string{"balance"}
	if e.acc.Source == upstream.SourceWorkBuddy {
		out = append(out, "tasks")
	}
	if e.disabled {
		out = append(out, "revive")
	} else {
		out = append(out, "disable")
	}
	if e.paused {
		out = append(out, "resume")
	} else {
		out = append(out, "pause")
	}
	return append(out, "remove")
}
