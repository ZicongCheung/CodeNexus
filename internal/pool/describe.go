package pool

import (
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// AccountStatus is a read-only snapshot of one account's runtime state, used by
// the console. It never exposes credential material.
type AccountStatus struct {
	Key             string          `json:"key"`
	Source          upstream.Source `json:"source"`
	UID             string          `json:"uid"`
	Label           string          `json:"label,omitempty"`
	Enabled         bool            `json:"enabled"`
	Paused          bool            `json:"paused"`
	Healthy         bool            `json:"healthy"`
	Cooling         bool            `json:"cooling"`
	Broken          bool            `json:"broken"`
	BlockedUntil    time.Time       `json:"blocked_until,omitempty"`
	Credits         int64           `json:"credits"`
	CreditsTotal    int64           `json:"credits_total,omitempty"`
	CreditsKnown    bool            `json:"credits_known"`
	Expiring        int64           `json:"expiring,omitempty"`
	EarliestExpiry  int64           `json:"earliest_expiry,omitempty"`
	InFlight        int             `json:"in_flight"`
	Success         int64           `json:"success"`
	Errors          int64           `json:"errors"`
	HasRefreshToken bool            `json:"has_refresh_token"`
	ExpiresAt       int64           `json:"expires_at,omitempty"`
	CheckinDay      string          `json:"checkin_day,omitempty"`
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
			Expiring:        e.expiring,
			InFlight:        e.inFlight,
			Success:         e.success,
			Errors:          e.errors,
			HasRefreshToken: acc.RefreshTokenValue() != "",
			ExpiresAt:       acc.ExpiresAtValue(),
			CheckinDay:      e.checkinDay,
		}
		if !e.earliestAt.IsZero() {
			st.EarliestExpiry = e.earliestAt.Unix()
		}
		if d := e.blockDelay(now, e.lastModel); d > 0 {
			st.BlockedUntil = now.Add(d)
		}
		out = append(out, st)
	}
	return out
}
