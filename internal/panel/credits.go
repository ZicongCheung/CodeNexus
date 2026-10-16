package panel

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/pool"
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

	Unlimited bool `json:"unlimited,omitempty"`
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

// modelRow is one model as one account sees it. Capability bits are whatever
// the account's own catalogue states; a field the catalogue does not carry is
// left empty rather than guessed.
type modelRow struct {
	UID    string `json:"uid"`
	Key    string `json:"key"`
	Source string `json:"source"`
	Label  string `json:"label,omitempty"`

	ID      string   `json:"id"`
	Context int      `json:"context_length,omitempty"`
	Caps    []string `json:"capabilities,omitempty"`
	// Efforts lists the thinking strengths the catalogue advertises, weakest
	// first. Empty means not adjustable, which is a different claim from
	// "default".
	Efforts []string `json:"thinking_efforts,omitempty"`
}

// handleModels projects each account's own live catalogue.
//
// /v1/models flattens the pool into one deduplicated list, which is right for
// a client picking a model name but hides the thing the console has to show:
// which account carries which tier. Reading per account is safe here because
// ListModels never blocks — it answers from the cached catalogue and refreshes
// in the background, so the page does not multiply upstream load.
func (c *Console) handleModels(w http.ResponseWriter, r *http.Request) {
	statuses := c.pool.Describe()
	rows := make([]modelRow, 0, len(statuses)*8)
	failed := map[string]string{}

	var wg sync.WaitGroup
	sem := make(chan struct{}, creditProbeLimit)
	for _, st := range statuses {
		if st.Local {
			continue
		}
		wg.Add(1)
		go func(st pool.AccountStatus) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			acc, ok := c.pool.Get(st.Key)
			if !ok || acc == nil {
				failed[st.UID] = "account left the pool"
				return
			}
			u, ok := upstream.Get(st.Source)
			if !ok {
				failed[st.UID] = "source not registered: " + string(st.Source)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), creditProbeTimeout)
			defer cancel()
			models, err := u.ListModels(ctx, acc)
			if err != nil {
				failed[st.UID] = err.Error()
				return
			}
			for _, m := range models {
				if m.ID == "" {
					continue
				}
				rows = append(rows, modelRow{
					UID:     st.UID,
					Key:     st.Key,
					Source:  string(st.Source),
					Label:   st.Label,
					ID:      m.ID,
					Context: m.ContextLength,
					Caps:    m.Capabilities,
					Efforts: m.ThinkingEfforts,
				})
			}
		}(st)
	}
	wg.Wait()

	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Source != rows[b].Source {
			return rows[a].Source < rows[b].Source
		}
		if rows[a].UID != rows[b].UID {
			return rows[a].UID < rows[b].UID
		}
		return rows[a].ID < rows[b].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"models": rows,
		"failed": failed,
	})
}
