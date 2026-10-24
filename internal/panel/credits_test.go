package panel

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// packsAdapter returns an itemised grant, which is what a Trae CN account
// looks like: the balance is a sum over packs that lapse on their own dates.
type packsAdapter struct {
	maintOnly
	packs []upstream.CreditPack
}

func (a *packsAdapter) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	return &upstream.Credits{
		Remain: 627,
		Total:  800,
		Used:   173,
		Packs:  a.packs,
	}, nil
}

func TestCreditsAggregatesItemisedPacks(t *testing.T) {
	packs := []upstream.CreditPack{
		{Name: "每月登录赠送", Size: 500, Used: 173, Remain: 327, ExpiresAt: 1792000000, ExpiresText: "2026-10-31"},
		{Name: "签到奖励", Size: 300, Used: 0, Remain: 300, ExpiresAt: 1790000000, ExpiresText: "2026-10-08"},
	}
	m := &packsAdapter{packs: packs}
	c, p := newMaintConsole(t, m)
	second := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Enabled: true}
	second.SetCredentials("tok2", "", "trae", 0)
	p.Add(second)

	res := do(c, http.MethodGet, "/panel/api/credits", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	rows, ok := out["accounts"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("accounts = %#v, want 2 rows", out["accounts"])
	}
	first, _ := rows[0].(map[string]any)
	got, ok := first["packs"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("packs = %#v, want 2", first["packs"])
	}
	head, _ := got[0].(map[string]any)
	// The itemised row has to reconcile with the balance: remain is what the
	// console shows, and the sum of the packs is where it came from.
	if head["remain"].(float64)+got[1].(map[string]any)["remain"].(float64) != first["remain"].(float64) {
		t.Fatalf("packs do not sum to remain: %#v vs %v", head, first["remain"])
	}
	if head["expires_text"] != "2026-10-31" {
		t.Fatalf("expires_text = %v", head["expires_text"])
	}
}

// brokenAdapter answers with an error, so the page has to survive one bad
// token instead of blanking out.
type brokenAdapter struct{ maintOnly }

func (brokenAdapter) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	return nil, errors.New("upstream unreachable")
}

func TestCreditsKeepsGoingWhenOneAccountFails(t *testing.T) {
	m := &brokenAdapter{}
	c, p := newMaintConsole(t, m)
	good := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Enabled: true}
	good.SetCredentials("tok2", "", "trae", 0)
	p.Add(good)

	res := do(c, http.MethodGet, "/panel/api/credits", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	rows := decode(t, &res)["accounts"].([]any)
	failed := 0
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["failed"] == true {
			failed++
		}
	}
	// Both accounts share the broken source, so both fail — but the page must
	// still answer 200 with per-account errors instead of an error response.
	if failed != len(rows) || failed == 0 {
		t.Fatalf("failed rows = %d of %d, want all", failed, len(rows))
	}
}

// catalogSource is an upstream whose catalogue differs per account, which is
// exactly what the model page has to show.
type catalogSource struct {
	upstream.Upstream
	perUID map[string][]upstream.ModelSpec
}

func (s catalogSource) Source() upstream.Source { return upstream.SourceTraeCN }

func (s catalogSource) ListModels(_ context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	if acc == nil {
		return nil, nil
	}
	return s.perUID[acc.UID], nil
}

func TestModelsMergesTheSameModelAcrossAccounts(t *testing.T) {
	src := catalogSource{perUID: map[string][]upstream.ModelSpec{
		"u-1": {
			{ID: "glm-5.3", Source: upstream.SourceTraeCN, ContextLength: 1000000,
				MaxOutput: 64000, Capabilities: []string{"chat", "reasoning", "max"},
				ThinkingEfforts: []string{"light", "high"}, Credits: "x0.79"},
			{ID: "mimo-v2.6-flash", Source: upstream.SourceTraeCN, ContextLength: 981504,
				Capabilities: []string{"chat"}},
		},
		"u-2": {
			{ID: "glm-5.3", Source: upstream.SourceTraeCN, ContextLength: 200000,
				Capabilities: []string{"chat", "reasoning", "tools"}},
		},
	}}
	m := &maintOnly{}
	c, p := newMaintConsole(t, m)
	second := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Enabled: true}
	second.SetCredentials("tok2", "", "trae", 0)
	p.Add(second)

	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	res := do(c, http.MethodGet, "/panel/api/models", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	rows := decode(t, &res)["models"].([]any)
	if len(rows) != 2 {
		t.Fatalf("models = %d rows, want 2: one row per model, not per account (%#v)", len(rows), rows)
	}

	byID := map[string]map[string]any{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		byID[row["id"].(string)] = row
	}
	glm, ok := byID["glm-5.3"]
	if !ok {
		t.Fatalf("glm-5.3 missing: %#v", rows)
	}
	// Two accounts carry it, so the row counts two and names both.
	if glm["accounts"].(float64) != 2 {
		t.Fatalf("glm accounts = %v, want 2", glm["accounts"])
	}
	if keys := glm["account_keys"].([]any); len(keys) != 2 {
		t.Fatalf("glm account_keys = %v", keys)
	}
	// The widest window wins: routing may pick either account, so the model can
	// serve the larger context.
	if glm["context_length"].(float64) != 1000000 {
		t.Fatalf("glm context = %v, want the widest", glm["context_length"])
	}
	if glm["max_output_tokens"].(float64) != 64000 {
		t.Fatalf("glm max output = %v", glm["max_output_tokens"])
	}
	if glm["credits"].(string) != "x0.79" {
		t.Fatalf("glm credits = %v", glm["credits"])
	}
	// Capability bits unite: a tool call works if any account offers it.
	caps := glm["capabilities"].([]any)
	hasTools := false
	for _, v := range caps {
		if v == "tools" {
			hasTools = true
		}
	}
	if !hasTools {
		t.Fatalf("glm capabilities = %v, want tools merged in", caps)
	}
}

func TestModelsSkipsLocalEntries(t *testing.T) {
	src := catalogSource{perUID: map[string][]upstream.ModelSpec{}}
	m := &maintOnly{}
	c, p := newMaintConsole(t, m)
	local := &account.Account{Source: account.SourceTraeCN, UID: "cli", Enabled: true, Local: true}
	p.Add(local)

	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	// The local entry has no credential to read a catalogue from; it must be
	// skipped rather than reported as a failure.
	res := do(c, http.MethodGet, "/panel/api/models", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	failed := decode(t, &res)["failed"].(map[string]any)
	if len(failed) != 0 {
		t.Fatalf("failed = %#v, want empty", failed)
	}
}
