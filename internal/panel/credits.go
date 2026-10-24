package panel

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// creditProbeLimit keeps the concurrent upstream balance reads bounded. The
// page is read on demand, so an operator with many accounts must not be able
// to fire a burst of upstream calls just by refreshing; three at a time drains
// a page quickly without looking like an attack.
const creditProbeLimit = 3

const creditProbeTimeout = 15 * time.Second

// creditRow is one account's itemised credit picture.
type creditRow struct {
	Key    string `json:"key"`
	UID    string `json:"uid"`
	Source string `json:"source"`
	Label  string `json:"label,omitempty"`
	// Local marks this machine's CLI entry: it has no grant to itemise, and
	// probing it would only return an error.
	Local bool `json:"local,omitempty"`
	// Failed says the balance could not be read this time. The rest of the
	// row stays empty so the console can show the failure per account instead
	// of turning one bad token into a blank page.
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`

	Unlimited bool  `json:"unlimited,omitempty"`
	Remain    int64 `json:"remain"`
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	// Packs mirrors upstream.CreditPack: the per-batch breakdown behind
	// Remain. Empty means the upstream did not itemise the grant.
	Packs []upstream.CreditPack `json:"packs,omitempty"`
}

// handleCredits aggregates the itemised credit balance of every account.
//
// A per-account probe is the only way to answer "when do these credits go
// away", because the answer lives in each account's entitlement list and no
// pool-level number carries it. Failures are per-account: one revoked token
// must not blank the whole page.
func (c *Console) handleCredits(w http.ResponseWriter, r *http.Request) {
	statuses := c.pool.Describe()
	rows := make([]creditRow, len(statuses))
	for i, st := range statuses {
		rows[i] = creditRow{
			Key:    st.Key,
			UID:    st.UID,
			Source: string(st.Source),
			Label:  st.Label,
			Local:  st.Local,
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, creditProbeLimit)
	window := time.Duration(c.cfg.Schedule.ExpiringSoonHours) * time.Hour
	for i := range statuses {
		if rows[i].Local {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			acc, ok := c.pool.Get(statuses[idx].Key)
			if !ok || acc == nil {
				rows[idx].Failed = true
				rows[idx].Error = "account left the pool"
				return
			}
			maint, ok := c.maintenanceFor(acc)
			if !ok {
				rows[idx].Failed = true
				rows[idx].Error = "balance surface not available for " + rows[idx].Source
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), creditProbeTimeout)
			defer cancel()
			credits, err := maint.Credits(ctx, acc, window)
			if err != nil {
				rows[idx].Failed = true
				rows[idx].Error = err.Error()
				return
			}
			rows[idx].Unlimited = credits.Unlimited
			rows[idx].Remain = credits.Remain
			rows[idx].Total = credits.Total
			rows[idx].Used = credits.Used
			rows[idx].Packs = credits.Packs
		}(i)
	}
	wg.Wait()

	// Deterministic order keeps the console steady across refreshes: by
	// source, then by remaining balance, so the accounts closest to running
	// out sit on top.
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Source != rows[b].Source {
			return rows[a].Source < rows[b].Source
		}
		return rows[a].Remain < rows[b].Remain
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":          rows,
		"expiring_soon_hrs": c.cfg.Schedule.ExpiringSoonHours,
	})
}
