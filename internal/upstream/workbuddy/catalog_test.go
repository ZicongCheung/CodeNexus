package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// newCatalogueClient points the adapter at a stub that serves one path and
// answers 404 for everything else, so a test can prove which path was used.
// The 404 doubles as the failure half of the two-probe contract: the probe
// that misses degrades to the one that answers.
func newCatalogueClient(t *testing.T, path string, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})
}

func catalogueAccount() *account.Account {
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u-1", Enabled: true}
	acc.SetCredentials("tok", "", "", 0)
	return acc
}

// The configuration catalogue is where price, window and output cap live. If
// the adapter lost these columns the console would still look like it worked,
// so the test asserts the values and not merely that some models came back.
func TestCatalogModelsReadsPriceWindowAndOutputCap(t *testing.T) {
	body := `{"code":0,"data":{"models":[
		{"id":"glm-5.3","credits":"x0.79","maxInputTokens":200000,"maxOutputTokens":64000,
		 "supportsToolCall":true,"supportsImages":true,"supportsReasoning":true,"isDefault":true,
		 "reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}},
		{"id":"tiny","credits":"x0.05","maxInputTokens":8000}
	]}}`
	c := newCatalogueClient(t, configPath, body)

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	m := specs[0]
	if m.Credits != "x0.79" {
		t.Fatalf("credits = %q", m.Credits)
	}
	if m.ContextLength != 200000 || m.MaxOutput != 64000 {
		t.Fatalf("context/max = %d/%d", m.ContextLength, m.MaxOutput)
	}
	if len(m.ThinkingEfforts) != 2 || m.ThinkingEfforts[1] != "high" {
		t.Fatalf("efforts = %v", m.ThinkingEfforts)
	}
	if !m.Default {
		t.Fatalf("default flag lost")
	}
	want := map[string]bool{"chat": true, "tools": true, "vision": true, "reasoning": true}
	for _, cap := range m.Capabilities {
		delete(want, cap)
	}
	if len(want) != 0 {
		t.Fatalf("capabilities = %v, missing %v", m.Capabilities, want)
	}
}

// The catalogue endpoint is User-Agent-sensitive: without a parseable
// CodeBuddy version it refuses or trims. The probe must therefore send the
// official IDE header, not the gateway's own identity — an empty or foreign
// UA here is what once turned the whole model page into a 404.
func TestCatalogProbeSendsIDEUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == configPath {
			got = r.Header.Get("User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"m"}]}}`))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})

	if _, err := c.ListModels(context.Background(), catalogueAccount()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != probeUA {
		t.Fatalf("catalogue UA = %q, want %q", got, probeUA)
	}
}

// The console has one price cell, so overlapping offers must resolve to one.
// Priority decides: glm-5.2 carries a daytime badge and a nightly discount,
// and only one of them is charged at a time.
func TestPromotionIndexPicksHighestPriority(t *testing.T) {
	promos := []cataloguePromotion{
		{Enabled: true, Priority: 50, ModelIDs: []string{"glm-5.2"},
			Badge:    &struct{ Label string `json:"label"` }{Label: "专属补贴"},
			Discount: &struct {
				DiscountedCredits string  `json:"discountedCredits"`
				Factor            float64 `json:"factor"`
			}{DiscountedCredits: "x0.40", Factor: 0.5},
			Schedule: &promotionSchedule{Daily: []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			}{{Start: "08:00", End: "23:00"}}}},
		{Enabled: true, Priority: 100, ModelIDs: []string{"glm-5.2"},
			Badge:    &struct{ Label string `json:"label"` }{Label: "夜间折扣"},
			Discount: &struct {
				DiscountedCredits string  `json:"discountedCredits"`
				Factor            float64 `json:"factor"`
			}{DiscountedCredits: "x0.20", Factor: 0.25},
			Schedule: &promotionSchedule{Daily: []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			}{{Start: "23:00", End: "07:50"}}}},
	}
	zone := promoZone

	// 00:30 — only the nightly window is open, including across midnight.
	night := promotionIndex(promos, time.Date(2026, 10, 23, 0, 30, 0, 0, zone))
	if got := night["glm-5.2"]; got.label != "夜间折扣" || got.credits != "x0.20" {
		t.Fatalf("night offer = %+v, want the 23:00→07:50 window", got)
	}
	// 12:00 — the daytime badge is the only one open.
	day := promotionIndex(promos, time.Date(2026, 10, 23, 12, 0, 0, 0, zone))
	if got := day["glm-5.2"]; got.label != "专属补贴" || got.credits != "x0.40" {
		t.Fatalf("day offer = %+v, want the daytime badge", got)
	}
	// 07:55 — between windows, so neither applies and the listed rate stands.
	gap := promotionIndex(promos, time.Date(2026, 10, 23, 7, 55, 0, 0, zone))
	if _, ok := gap["glm-5.2"]; ok {
		t.Fatalf("offer at 07:55 = %+v, want none", gap["glm-5.2"])
	}
}

// An off-peak offer states a label and a sentence but no discount object. It
// still has to reach the console: dropping scheduled offers is what kept
// 错峰使用 invisible.
func TestPromotionIndexKeepsLabelOnlyOffers(t *testing.T) {
	promos := []cataloguePromotion{{
		Enabled:  true,
		Priority: 10,
		ModelIDs: []string{"deepseek-v4-flash"},
		Badge:    &struct{ Label string `json:"label"` }{Label: "错峰使用"},
		Hover:    &struct{ TextZh string `json:"textZh"` }{TextZh: "北京时间工作日 00:00-09:00"},
		Schedule: &promotionSchedule{Daily: []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		}{{Start: "00:00", End: "09:00"}}},
	}}
	got, ok := promotionIndex(promos, time.Date(2026, 10, 23, 3, 0, 0, 0, promoZone))["deepseek-v4-flash"]
	if !ok || got.label != "错峰使用" || got.credits != "" || got.factor != nil {
		t.Fatalf("off-peak offer = %+v (present %v)", got, ok)
	}
	if got.note == "" {
		t.Fatal("hover sentence must survive: it is the only explanation of the window")
	}
}

// An offer outside its validity dates is not in force, whatever its window says.
func TestPromotionIndexHonoursValidDates(t *testing.T) {
	promos := []cataloguePromotion{{
		Enabled:  true,
		ModelIDs: []string{"m"},
		Badge:    &struct{ Label string `json:"label"` }{Label: "限时免费"},
		Schedule: &promotionSchedule{ValidUntil: "2026-10-01T00:00:00+08:00"},
	}}
	if _, ok := promotionIndex(promos, time.Date(2026, 10, 23, 12, 0, 0, 0, promoZone))["m"]; ok {
		t.Fatal("expired offer must not be shown")
	}
	if _, ok := promotionIndex(promos, time.Date(2026, 9, 20, 12, 0, 0, 0, promoZone))["m"]; !ok {
		t.Fatal("offer inside its validity must be shown")
	}
}

// A price on the wire must still be read as the listed rate when no offer is
// open: the promo fields are additive, never a replacement.
func TestCatalogModelsKeepsListedRateWithoutOffer(t *testing.T) {
	body := `{"code":0,"data":{"models":[{"id":"m","credits":"x1"}],
		"modelPromotions":[
		  {"enabled":true,"modelIds":["m"],"badge":{"label":"五折"},
		   "discount":{"discountedCredits":"0.5x","factor":0.5}}
		]}}`
	c := newCatalogueClient(t, configPath, body)

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if specs[0].Credits != "x1" {
		t.Fatalf("listed credits = %q, must stay untouched", specs[0].Credits)
	}
	// No schedule means always in force, so the offer lands on the spec.
	// The discounted rate is normalised to the same "x0.5" spelling as a
	// listed rate — the wire sends "0.5x" here and "x1" there.
	if specs[0].PromoCredits != "x0.5" || specs[0].PromoLabel != "五折" {
		t.Fatalf("promo = %q/%q", specs[0].PromoCredits, specs[0].PromoLabel)
	}
	if specs[0].PromoFactor == nil || *specs[0].PromoFactor != 0.5 {
		t.Fatalf("promo factor = %v, want 0.5", specs[0].PromoFactor)
	}
}

// Models the picker cannot serve must never reach it: choosing one upstream
// answers code=11102, which reads to the operator as a broken gateway.
func TestCatalogModelsFiltersNonChatEntries(t *testing.T) {
	body := `{"code":0,"data":{"models":[
		{"id":"glm-5.3","credits":"x1"},
		{"id":"nes-embed-1","credits":"x1"},
		{"id":"completion-fast","credits":"x1"},
		{"id":"codewise-x","credits":"x1"},
		{"id":"micro-helper","credits":"x1","maxOutputTokens":128},
		{"id":"painter","credits":"x1","tags":["text-to-image"]},
		{"id":"gone","credits":"x1","disabled":true}
	]}}`
	c := newCatalogueClient(t, configPath, body)

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(specs) != 1 || specs[0].ID != "glm-5.3" {
		t.Fatalf("specs = %+v, want only glm-5.3", specs)
	}
}

// Upstream lists trial models only in the banner block, not in data.models.
// Missing them would hide a model the account can actually call; inheriting
// the price would advertise a charge the trial does not make.
func TestCatalogModelsJoinsTrialBannerModels(t *testing.T) {
	body := `{"code":0,"data":{
		"models":[{"id":"hy4-preview","credits":"x0.29","maxInputTokens":1000000}],
		"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
			{"modelId":"hy4-preview-f","targetModelId":"hy4-preview"}
		]}}}}`
	c := newCatalogueClient(t, configPath, body)

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var trial *struct {
		credits string
		ctx     int
	}
	for i := range specs {
		if specs[i].ID == "hy4-preview-f" {
			trial = &struct {
				credits string
				ctx     int
			}{specs[i].Credits, specs[i].ContextLength}
		}
	}
	if trial == nil {
		t.Fatalf("trial model missing from specs = %+v", specs)
	}
	if trial.credits != "" {
		t.Fatalf("trial carried the target's price %q", trial.credits)
	}
	if trial.ctx != 1000000 {
		t.Fatalf("trial lost the family context window: %d", trial.ctx)
	}
}

// When the rich catalogue is unavailable the page must still list models: a
// catalogue outage costs columns, not the whole view. The cli roster is the
// other probe and the fallback.
func TestListModelsFallsBackToEnterpriseRoster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case configPath:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":1001,"message":"no"}`))
		case enterpriseModelsPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{
				"models":[
					{"id":"glm-5.3","credits":"x1"},
					{"id":"kimi-k3","credits":"x2"}
				],
				"agents":[{"name":"cli","models":["glm-5.3","kimi-k3"]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(specs) != 2 || specs[0].ID != "glm-5.3" {
		t.Fatalf("specs = %+v", specs)
	}
}

// The roster is the model set the CLI surface offers, not everything the
// catalogue lists: entries outside agents[cli] stay out.
func TestEnterpriseRosterNarrowsToCLIAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != enterpriseModelsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{
			"models":[
				{"id":"cli-a","credits":"x1"},
				{"id":"web-only","credits":"x1"}
			],
			"agents":[{"name":"web","models":["web-only"]},{"name":"cli","models":["cli-a"]}]}}`))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(specs) != 1 || specs[0].ID != "cli-a" {
		t.Fatalf("specs = %+v, want only cli-a", specs)
	}
}

// Each probe holds models the other lacks; the merge must keep both and let
// the catalogue's richer shape win on a shared id.
func TestListModelsMergesBothProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case configPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"shared","credits":"x0.79","maxInputTokens":200000},
				{"id":"only-v3","credits":"x1"}]}}`))
		case enterpriseModelsPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{
				"models":[{"id":"shared","credits":"x9"},{"id":"only-roster","credits":"x2"}],
				"agents":[{"name":"cli","models":["shared","only-roster"]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})

	specs, err := c.ListModels(context.Background(), catalogueAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	ids := map[string]upstreamModelShape{}
	for _, s := range specs {
		ids[s.ID] = upstreamModelShape{credits: s.Credits, ctx: s.ContextLength}
	}
	if len(specs) != 3 {
		t.Fatalf("specs = %+v, want 3 models", specs)
	}
	if ids["shared"].credits != "x0.79" || ids["shared"].ctx != 200000 {
		t.Fatalf("shared entry = %+v, want the catalogue shape to win", ids["shared"])
	}
	if _, ok := ids["only-roster"]; !ok {
		t.Fatalf("roster-only model dropped: %+v", specs)
	}
	if _, ok := ids["only-v3"]; !ok {
		t.Fatalf("catalogue-only model dropped: %+v", specs)
	}
}

type upstreamModelShape struct {
	credits string
	ctx     int
}

// Both probes down is a real outage and must be reported as one, not papered
// over with an empty list.
func TestListModelsBothProbesDownIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1001}`))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: 5})

	if _, err := c.ListModels(context.Background(), catalogueAccount()); err == nil {
		t.Fatal("ListModels succeeded with both probes down")
	}
}
