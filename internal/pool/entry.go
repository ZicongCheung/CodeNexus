package pool

import (
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// entry is the runtime state for one account. Every field is guarded by the
// owning Pool's mutex.
type entry struct {
	key string
	acc *account.Account

	disabled bool
	paused   bool

	// account-level cooldown, e.g. after a 429
	coolingUntil time.Time
	coolKind     upstream.ErrorKind
	softStreak   int

	// circuit breaker, tripped by repeated transient failures
	breakerUntil time.Time
	fails        int

	// model-scoped cooldown for "model temporarily unavailable"
	modelCooldowns map[string]time.Time
	lastModel      string

	credits      int64
	creditsKnown bool

	inFlight int

	success     int64
	errors      int64
	lastUsed    time.Time
	lastSuccess time.Time
}

func newEntry(acc *account.Account) *entry {
	return &entry{key: acc.Key(), acc: acc}
}

func (e *entry) cooling(now time.Time) bool { return now.Before(e.coolingUntil) }
func (e *entry) broken(now time.Time) bool  { return now.Before(e.breakerUntil) }

func (e *entry) modelCooled(now time.Time, model string) bool {
	if model == "" || e.modelCooldowns == nil {
		return false
	}
	until, ok := e.modelCooldowns[model]
	return ok && now.Before(until)
}

// active reports whether the account is enabled and not administratively off.
func (e *entry) active() bool { return e.acc.Enabled && !e.disabled && !e.paused }

// selectable reports whether the account can serve a request for model now.
func (e *entry) selectable(now time.Time, model string, maxInFlight int) bool {
	return e.active() &&
		!e.cooling(now) &&
		!e.broken(now) &&
		!e.modelCooled(now, model) &&
		e.inFlight < maxInFlight
}

// blockDelay returns how long until this entry could become selectable again,
// ignoring in-flight saturation (which is handled separately). It returns 0
// when the entry is administratively unusable so it never gates a wait.
func (e *entry) blockDelay(now time.Time, model string) time.Duration {
	if !e.active() {
		return 0
	}
	best := time.Duration(0)
	consider := func(until time.Time) {
		if until.After(now) {
			if d := until.Sub(now); best == 0 || d < best {
				best = d
			}
		}
	}
	consider(e.coolingUntil)
	consider(e.breakerUntil)
	if e.modelCooldowns != nil {
		consider(e.modelCooldowns[model])
	}
	return best
}
