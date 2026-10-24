package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The console's model table reads price, context window, output cap and the
// thinking efforts a model accepts. Two upstream surfaces carry that, and each
// holds models the other lacks, so both are probed concurrently and merged:
//
//   - /v3/config — the official IDE configuration catalogue. It is
//     User-Agent-sensitive: without a parseable CodeBuddy version it answers
//     400 (code=12403) or serves a trimmed list, which is why the gateway's
//     own identity UA used to fall through to a 404.
//   - /console/enterprises/personal/models — the console-web listing. Its
//     agents[cli] roster is the model set the CLI surface actually offers.
//
// A failure on either side degrades to the other side's answer; the page loses
// columns, it does not go empty. (Contract aligned with the reference
// workbuddy2api-panel client.)
const configPath = "/v3/config"

const enterpriseModelsPath = "/console/enterprises/personal/models"

// probeUA is the official IDE header the catalogue endpoints expect. The
// version must track upstream IDE releases: an UA too old can be served the
// trimmed catalogue again.
const probeUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// catalogueEntry is the subset of one catalogue model this gateway models. The
// endpoints carry far more (descriptions, vendors, output parameters); a field
// the console does not show is not parsed, so a shape change upstream cannot
// break what is displayed.
type catalogueEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Credits         string   `json:"credits"`
	MaxInputTokens  int      `json:"maxInputTokens"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsTool    bool     `json:"supportsToolCall"`
	SupportsReason  bool     `json:"supportsReasoning"`
	IsDefault       bool     `json:"isDefault"`
	Disabled        bool     `json:"disabled"`
	Tags            []string `json:"tags"`
	Reasoning       struct {
		Effort           string   `json:"effort"`
		DefaultEffort    string   `json:"defaultEffort"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// cataloguePromotion is a time-boxed price offer. A promotion is only worth
// printing if it is in force right now, so its schedule is parsed rather than
// ignored: an off-peak or nightly offer is the difference between 0.79x and
// 0.40x, and a badge shown outside its window advertises a price nobody pays.
type cataloguePromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	// Hover carries the sentence the desktop client shows on hover; for
	// off-peak offers it is the only place the window is written down.
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *promotionSchedule `json:"schedule"`
}

type catalogueEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Models          []catalogueEntry     `json:"models"`
		ModelPromotions []cataloguePromotion `json:"modelPromotions"`
		// ModelTrialBanner carries the "N days free trial" models. Upstream
		// lists them here only — not in data.models — so a plain catalogue
		// parse silently misses a model that is actually callable.
		ProductFeaturesConfig struct {
			ModelTrialBanner struct {
				Banners []struct {
					ModelID       string `json:"modelId"`
					TargetModelID string `json:"targetModelId"`
				} `json:"banners"`
			} `json:"ModelTrialBanner"`
		} `json:"productFeaturesConfig"`
	} `json:"data"`
}

type enterpriseEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Models []catalogueEntry `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	} `json:"data"`
}

// listModels answers from the two probes merged, preferring the catalogue.
func (c *Client) listModels(ctx context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	type probe struct {
		specs []upstream.ModelSpec
		err   error
	}
	v3ch := make(chan probe, 1)
	entch := make(chan probe, 1)
	go func() {
		specs, err := c.catalogModels(ctx, acc)
		v3ch <- probe{specs, err}
	}()
	go func() {
		specs, err := c.enterpriseModels(ctx, acc)
		entch <- probe{specs, err}
	}()
	ent := <-entch
	v3 := <-v3ch
	uid := ""
	if acc != nil {
		uid = acc.UID
	}
	if ent.err != nil && v3.err != nil {
		return nil, ent.err
	}
	if v3.err != nil {
		logx.Debugf("workbuddy: v3/config probe failed for %s, enterprise endpoint only: %v", uid, v3.err)
	}
	if ent.err != nil {
		logx.Debugf("workbuddy: enterprise probe failed for %s, v3/config only: %v", uid, ent.err)
	}
	out := mergeSpecs(v3.specs, ent.specs)
	if len(out) == 0 {
		return nil, fmt.Errorf("models: both probes returned no models")
	}
	return out, nil
}

// mergeSpecs merges two probe results: same-id entries keep the primary's
// shape (its credits and windows are the authoritative surface), the secondary
// only contributes ids the primary lacks. Order = primary first, then the
// secondary's additions, so the console's ordering is stable.
func mergeSpecs(primary, secondary []upstream.ModelSpec) []upstream.ModelSpec {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]upstream.ModelSpec, 0, len(primary)+len(secondary))
	for _, m := range primary {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range secondary {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// catalogModels reads the official IDE configuration catalogue.
func (c *Client) catalogModels(ctx context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	req, err := c.newRequest(ctx, http.MethodGet, configPath, nil, acc)
	if err != nil {
		return nil, err
	}
	// The endpoint answers 400 unless the caller identifies as a client rather
	// than as a browser navigation, and it selects the model set by UA and by
	// the X-Domain it is asked about. The UA is the endpoint's requirement, not
	// the gateway's identity — chat keeps whatever the config says.
	req.Header.Set("User-Agent", probeUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-Product", "SaaS")
	if d := c.domainOf(acc); d != "" {
		req.Header.Set("X-Domain", d)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError("config", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var env catalogueEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("config: code=%d", env.Code)
	}

	byID := make(map[string]catalogueEntry, len(env.Data.Models))
	ordered := make([]catalogueEntry, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, dup := byID[id]; dup {
			continue
		}
		byID[id] = m
		ordered = append(ordered, m)
	}
	// Trial-banner models inherit the target model's capabilities — a trial is
	// the same family — but keep neither its price nor its marketing tags:
	// those describe the post-trial model, and printing them on a free trial
	// would advertise a charge that is not happening.
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, dup := byID[id]; dup {
			continue
		}
		base, ok := byID[strings.TrimSpace(b.TargetModelID)]
		if !ok {
			continue
		}
		base.ID = id
		base.Credits = ""
		base.Tags = nil
		base.IsDefault = false
		base.Disabled = false
		byID[id] = base
		ordered = append(ordered, base)
	}

	promos := promotionIndex(env.Data.ModelPromotions, time.Now().In(promoZone))
	out := make([]upstream.ModelSpec, 0, len(ordered))
	for _, m := range ordered {
		id := strings.TrimSpace(m.ID)
		if id == "" || m.Disabled || nonChatModel(id, int64(m.MaxOutputTokens), m.Tags) {
			continue
		}
		spec := specFromEntry(m)
		if p, ok := promos[id]; ok {
			spec.PromoCredits = normalizeRate(p.credits)
			spec.PromoLabel = p.label
			spec.PromoNote = p.note
			if p.factor != nil {
				factor := *p.factor
				spec.PromoFactor = &factor
			}
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("config: no models")
	}
	return out, nil
}

// enterpriseModels reads the console-web listing and narrows it to the model
// set the CLI agent actually offers.
func (c *Client) enterpriseModels(ctx context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	req, err := c.newRequest(ctx, http.MethodGet, enterpriseModelsPath, nil, acc)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", probeUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError("models", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var env enterpriseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models: code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("models: no cli agent roster")
	}
	dyn := make(map[string]catalogueEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || nonChatModel(id, int64(m.MaxOutputTokens), m.Tags) {
			continue
		}
		dyn[id] = m
	}
	out := make([]upstream.ModelSpec, 0, len(cliIDs))
	for _, id := range cliIDs {
		if m, ok := dyn[strings.TrimSpace(id)]; ok && !m.Disabled {
			out = append(out, specFromEntry(m))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models: empty cli roster")
	}
	return out, nil
}

// normalizeRate rewrites the two spellings the upstream uses into the one the
// console prints. Discounts arrive as "0.50x" while listed rates arrive as
// "x0.79"; a column that mixes both reads as two different units.
func normalizeRate(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	if strings.HasSuffix(text, "x") && !strings.HasPrefix(text, "x") {
		return "x" + strings.TrimSuffix(text, "x")
	}
	return text
}

// specFromEntry maps one catalogue entry to the console's model shape.
func specFromEntry(m catalogueEntry) upstream.ModelSpec {
	return upstream.ModelSpec{
		ID:              strings.TrimSpace(m.ID),
		Source:          upstream.SourceWorkBuddy,
		ContextLength:   m.MaxInputTokens,
		MaxOutput:       m.MaxOutputTokens,
		Credits:         normalizeRate(m.Credits),
		ThinkingEfforts: m.Reasoning.SupportedEfforts,
		Default:         m.IsDefault,
		Capabilities:    capabilitiesOf(m),
	}
}

// nonChatModel reports models that must not reach the console's picker:
// selecting one upstream answers code=11102. Three rules, from the client's
// own filter: embedding/completion id prefixes, a token output cap that marks
// a helper model, and generation tags (image/video models are served by their
// own endpoints).
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// promotionSchedule is when an offer applies. A nil pointer means "always",
// which is how a plain launch discount arrives.
type promotionSchedule struct {
	Daily []struct {
		Start string `json:"start"` // "23:00"
		End   string `json:"end"`   // "07:50", may wrap past midnight
	} `json:"daily"`
	Timezone   string `json:"timezone"`
	ValidFrom  string `json:"validFrom"`
	ValidUntil string `json:"validUntil"`
}

type activePromotion struct {
	credits string
	label   string
	note    string
	factor  *float64
}

// promoZone is the clock the upstream states its windows in. A FixedZone keeps
// this independent of system tzdata, which Windows does not ship.
var promoZone = time.FixedZone("CST", 8*3600)

// promotionIndex keeps the promotions in force at now, and only the strongest
// one per model.
//
// Priority decides between offers that overlap in time: glm-5.2 carries a
// daytime badge (50) and a nightly five-fold discount (100), both enabled, and
// showing both would print two prices for one model. An offer with no discount
// object still lands here — off-peak promotions state only a label and a hover
// sentence, and dropping them is what hid 错峰使用 from the console.
func promotionIndex(list []cataloguePromotion, now time.Time) map[string]activePromotion {
	out := make(map[string]activePromotion, len(list))
	best := make(map[string]int, len(list))
	for i := range list {
		p := &list[i]
		if !p.Enabled || !promoActive(p.Schedule, now) {
			continue
		}
		offer := activePromotion{}
		if p.Badge != nil {
			offer.label = strings.TrimSpace(p.Badge.Label)
		}
		if p.Hover != nil {
			offer.note = strings.TrimSpace(p.Hover.TextZh)
		}
		if p.Discount != nil {
			offer.credits = strings.TrimSpace(p.Discount.DiscountedCredits)
			factor := p.Discount.Factor
			offer.factor = &factor
		}
		if offer.label == "" && offer.credits == "" {
			continue
		}
		for _, raw := range p.ModelIDs {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if held, ok := best[id]; ok && held >= p.Priority {
				continue
			}
			best[id] = p.Priority
			out[id] = offer
		}
	}
	return out
}

// promoActive reports whether the offer is open at now: inside its valid dates
// and inside any of its daily windows. Windows may cross midnight, so
// 23:00 → 07:50 is open at 00:30.
func promoActive(sc *promotionSchedule, now time.Time) bool {
	if sc == nil {
		return true
	}
	if sc.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, sc.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if sc.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, sc.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	if len(sc.Daily) == 0 {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	for _, w := range sc.Daily {
		start, okStart := promoClock(w.Start)
		end, okEnd := promoClock(w.End)
		if !okStart || !okEnd {
			continue
		}
		switch {
		case start <= end:
			if cur >= start && cur < end {
				return true
			}
		case cur >= start || cur < end: // window wraps past midnight
			return true
		}
	}
	return false
}

// promoClock parses "HH:MM" into minutes since midnight. A malformed window is
// skipped rather than treated as always-open, so a bad value cannot turn a
// nightly discount into a permanent one.
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHour != nil || errMinute != nil || hour < 0 || hour > 24 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// capabilitiesOf states what the catalogue says the model can do.
func capabilitiesOf(m catalogueEntry) []string {
	out := []string{"chat"}
	if m.SupportsTool {
		out = append(out, "tools")
	}
	if m.SupportsImages {
		out = append(out, "vision")
	}
	if m.SupportsReason {
		out = append(out, "reasoning")
	}
	return out
}

// domainOf reports the domain the account belongs to, defaulting to the host it
// talks to. The catalogue endpoint uses it to pick the regional model set.
func (c *Client) domainOf(acc *account.Account) string {
	if acc != nil {
		if d := strings.TrimSpace(acc.DomainValue()); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(c.cfg.BaseURL); err == nil {
		return u.Host
	}
	return ""
}
