package traecn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The account's live model list is the only place the upstream states which
// models it currently offers and which optional profiles they carry: whether a
// model is max-capable (the 1M agent tier) and which thinking strengths it
// accepts. The gateway therefore consults it before touching either, and never
// invents a capability the account does not advertise.
//
// It is also the source of the model list the gateway serves: a hand-written
// table goes stale the moment the upstream adds or retires a model, and the
// user sees a picker full of models that no longer resolve.
//
// Two catalogues are read, because they are not the same list:
//
//   - The Remote/web catalogue, grouped by agent tier. It is the primary
//     source: it carries the Agent/Work/Design split that decides which
//     profile applies (the Agent tier holds the 1M entry, Work stays on the
//     smaller fallback).
//   - The native Solo catalogue. Solo can expose a newly enabled model before
//     the Remote groups carry it, so it fills in only what Remote has not
//     published.
//
// The list is fetched once per account per TTL and cached; a failure is cached
// briefly so a broken endpoint cannot turn every request into an extra call.
const (
	catalogueTTL        = 10 * time.Minute
	catalogueErrorTTL   = 1 * time.Minute
	catalogueModelPath  = "/models"
	catalogueModelQuery = "functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"
	soloCataloguePath   = "/api/ide/v1/get_detail_param"
	// soloCatalogueFunction is the executor whose catalogue the desktop client
	// asks for; the upstream answers with the models it can run.
	soloCatalogueFunction = agentFunction
)

// routingModelIDs are ids the gateway itself understands rather than upstream
// config names: the default model and the work executor. They are always part
// of the advertised list.
var routingModelIDs = []string{"auto", "work"}

// modelCatalogue caches one account's catalogues, grouped by agent tier because
// a tier decides which profile applies.
type modelCatalogue struct {
	mu    sync.Mutex
	uid   string
	tiers map[string]map[string]map[string]any
	// list is the client-facing catalogue: every visible model from Remote
	// plus the Solo-only ones. It survives a failed refresh, so one bad fetch
	// does not empty /v1/models.
	list    []upstream.ModelSpec
	fetched time.Time
	lastErr bool
	// loading is non-nil while a fetch runs; it is closed when that fetch
	// finishes so concurrent callers can wait instead of stacking requests.
	loading chan struct{}
}

// catalogueTier returns the account's model entries for one agent tier.
//
// The second result reports whether the catalogue was actually consulted for
// that tier. Callers must treat "false" as "unknown" and leave the request
// untouched: guessing a max profile or a thinking level the account does not
// own would either be rejected upstream or silently mis-bill.
func (c *Client) catalogueTier(ctx context.Context, acc *account.Account, tier string) (map[string]map[string]any, bool) {
	if acc == nil || acc.Token() == "" || tier == "" {
		return nil, false
	}
	if !c.catalogueFresh(acc) {
		c.loadCatalogue(ctx, acc, true)
	}
	c.cat.mu.Lock()
	defer c.cat.mu.Unlock()
	if c.cat.uid != acc.UID || c.cat.tiers == nil {
		return nil, false
	}
	models, ok := c.cat.tiers[tier]
	if !ok {
		return nil, false
	}
	return models, true
}

// modelEntry looks up one model's catalogue entry for a tier.
func (c *Client) modelEntry(ctx context.Context, acc *account.Account, tier, configName string) (map[string]any, bool) {
	models, ok := c.catalogueTier(ctx, acc, tier)
	if !ok {
		return nil, false
	}
	entry, found := models[configName]
	if !found {
		return nil, false
	}
	return entry, true
}

// advertisedModels is what /v1/models answers: the live catalogue when it has
// been read for this account, and the built-in snapshot otherwise.
//
// It never blocks. A stale cache starts a background refresh and answers from
// what is already known, because a model list that hangs is worse than one that
// is a few seconds behind: the client re-lists as soon as it is told the list
// changed, while a request that never returns is a dead end.
func (c *Client) advertisedModels(acc *account.Account) []upstream.ModelSpec {
	if acc != nil && acc.Token() != "" && !c.catalogueFresh(acc) {
		go c.loadCatalogue(context.Background(), acc, false)
	}
	c.cat.mu.Lock()
	list := c.cat.list
	known := acc != nil && c.cat.uid == acc.UID
	c.cat.mu.Unlock()
	if known && len(list) > 0 {
		return list
	}
	return defaultModelSpecs()
}

// catalogueFresh reports whether the cached catalogue can be trusted for this
// account. A failed load is trusted for a shorter window, so a transient
// outage is retried soon without hammering a broken endpoint.
func (c *Client) catalogueFresh(acc *account.Account) bool {
	if acc == nil || acc.Token() == "" {
		return false
	}
	c.cat.mu.Lock()
	defer c.cat.mu.Unlock()
	ttl := catalogueTTL
	if c.cat.lastErr {
		ttl = catalogueErrorTTL
	}
	return c.cat.uid == acc.UID && !c.cat.fetched.IsZero() && time.Since(c.cat.fetched) < ttl
}

// loadCatalogue refreshes the cache from the upstream. When wait is false the
// caller gives up rather than queue behind a fetch that is already running,
// which is what the background path wants.
func (c *Client) loadCatalogue(ctx context.Context, acc *account.Account, wait bool) {
	c.cat.mu.Lock()
	for c.cat.loading != nil {
		done := c.cat.loading
		c.cat.mu.Unlock()
		if !wait {
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
		c.cat.mu.Lock()
	}
	c.cat.loading = make(chan struct{})
	c.cat.mu.Unlock()

	tiers, err := c.fetchCatalogue(ctx, acc)
	var list []upstream.ModelSpec
	if err == nil {
		list = c.catalogueList(ctx, acc, tiers)
	}

	c.cat.mu.Lock()
	defer c.cat.mu.Unlock()
	close(c.cat.loading)
	c.cat.loading = nil
	c.cat.uid = acc.UID
	c.cat.fetched = time.Now()
	if err != nil {
		// The tier map is deliberately cleared: "unknown" makes the chat path
		// leave the request alone, while a stale tier would apply a profile
		// the account may no longer own. The advertised list is kept, because
		// it is a name list rather than an entitlement claim.
		c.cat.tiers = nil
		c.cat.lastErr = true
		logx.Debugf("trae: model catalogue unavailable for %s: %v", shortUID(acc.UID), err)
		return
	}
	c.cat.tiers = tiers
	c.cat.list = list
	c.cat.lastErr = false
}

// fetchCatalogue performs the model-list call and normalizes every entry.
func (c *Client) fetchCatalogue(ctx context.Context, acc *account.Account) (map[string]map[string]map[string]any, error) {
	// The catalogue outlives one client request: a caller that disconnects
	// must not poison the cache for everyone else, so the fetch runs on its
	// own bounded context.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	url := c.remoteEndpoint(catalogueModelPath) + "?" + catalogueModelQuery
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(req, acc, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("trae: model catalogue returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			List []map[string]any `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae: model catalogue: %w", err)
	}
	tiers := make(map[string]map[string]map[string]any)
	for _, group := range out.Data.List {
		tier := firstString(group, "function", "agent_type", "agentType")
		if tier == "" {
			continue
		}
		items, _ := group["models"].([]any)
		bucket := tiers[tier]
		if bucket == nil {
			bucket = make(map[string]map[string]any)
			tiers[tier] = bucket
		}
		for _, item := range items {
			entry, _ := item.(map[string]any)
			if entry == nil {
				continue
			}
			name := firstString(entry, "config_name", "configName", "name", "model_name")
			if name == "" {
				continue
			}
			if _, exists := bucket[name]; exists {
				continue
			}
			bucket[name] = normalizeModelConfig(entry, name)
		}
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("trae: model catalogue carried no model groups")
	}
	if c.cfg.DebugUpstream {
		logx.Debugf("trae: model catalogue %s", describeCatalogue(tiers))
	}
	return tiers, nil
}

// fetchSoloCatalogue asks the native Solo gateway for the catalogue the desktop
// client itself uses.
//
// It is parsed separately from the Remote response on purpose: the two carry
// different metadata, and Solo can advertise a model before the Remote groups
// do. A failure is not fatal — Remote alone is a complete list.
func (c *Client) fetchSoloCatalogue(ctx context.Context, acc *account.Account) (map[string]map[string]any, error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	payload, err := json.Marshal(map[string]any{
		"function":            soloCatalogueFunction,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + soloCataloguePath
	req, err := http.NewRequestWithContext(fctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.setAgentHeaders(req, acc)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("trae: solo catalogue returned HTTP %d", resp.StatusCode)
	}
	var decoded struct {
		ConfigInfoList []map[string]any `json:"config_info_list"`
		Data           struct {
			ConfigInfoList []map[string]any `json:"config_info_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("trae: solo catalogue: %w", err)
	}
	items := decoded.ConfigInfoList
	if items == nil {
		items = decoded.Data.ConfigInfoList
	}
	out := make(map[string]map[string]any, len(items))
	for _, entry := range items {
		if entry == nil {
			continue
		}
		// A profile used for something other than chat (an internal tool
		// runner, say) is not a model anyone can pick.
		if usage := firstString(entry, "usage"); usage != "" && !strings.EqualFold(usage, "chat_completion") {
			continue
		}
		if truthy(entry["custom_models"]) {
			continue
		}
		name := firstString(entry, "config_name", "configName", "name")
		if name == "" {
			continue
		}
		if _, exists := out[name]; exists {
			continue
		}
		out[name] = normalizeModelConfig(entry, name)
	}
	return out, nil
}

// catalogueList turns the live catalogues into the client-facing model list.
//
// Remote goes first, and Solo fills the gaps — but a name seen twice is merged
// rather than ignored: the two surfaces state different things about the same
// model (Remote states the window pair, Solo states a flat token count), and
// keeping only the first meant the console printed no window for most models.
// Entries the upstream marks invisible to the user are dropped: they are
// internal runners (sub-agent and tool profiles) that the upstream itself keeps
// out of its own picker, so advertising them would offer a choice the desktop
// client never shows.
func (c *Client) catalogueList(ctx context.Context, acc *account.Account, tiers map[string]map[string]map[string]any) []upstream.ModelSpec {
	seen := make(map[string]int, 64)
	out := make([]upstream.ModelSpec, 0, 64)

	add := func(name string, entry map[string]any) {
		if strings.TrimSpace(name) == "" || truthy(entry["is_invisible_to_user"]) {
			return
		}
		id := strings.ToLower(strings.TrimSpace(name))
		if at, ok := seen[id]; ok {
			// Widen rather than replace: whichever surface states a longer
			// window is telling the truth about what can be requested.
			if window := contextLength(entry); window > out[at].ContextLength {
				out[at].ContextLength = window
			}
			return
		}
		seen[id] = len(out)
		rememberLiveName(name)
		spec := upstream.ModelSpec{
			ID:              id,
			Source:          upstream.SourceTraeCN,
			ContextLength:   contextLength(entry),
			Capabilities:    capabilitiesOf(entry),
			ThinkingEfforts: supportedEfforts(entry),
			// The live entries state the window but not the answer cap, so the
			// built-in table supplies it. A model released after that table was
			// written simply reports no cap rather than a guessed one.
			MaxOutput: outputCap(id),
		}
		applyFeatures(&spec, entry)
		out = append(out, spec)
	}

	tierNames := make([]string, 0, len(tiers))
	for tier := range tiers {
		tierNames = append(tierNames, tier)
	}
	sort.Strings(tierNames)
	for _, tier := range tierNames {
		for _, name := range sortedKeys(tiers[tier]) {
			add(name, tiers[tier][name])
		}
	}

	solo, err := c.fetchSoloCatalogue(ctx, acc)
	if err != nil {
		logx.Debugf("trae: solo catalogue unavailable for %s: %v", shortUID(acc.UID), err)
	}
	for _, name := range sortedKeys(solo) {
		add(name, solo[name])
	}

	// The client's own list is the only surface that states a price, so it is
	// read last as an enrichment pass rather than as another model source.
	if live := c.fetchClientModels(ctx, acc); len(live) > 0 {
		for i := range out {
			entry, ok := live[out[i].ID]
			if !ok {
				continue
			}
			applyFeatures(&out[i], entry)
			if window := contextLength(entry); window > out[i].ContextLength {
				out[i].ContextLength = window
			}
		}
	}

	for _, id := range routingModelIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = len(out)
		out = append(out, upstream.ModelSpec{ID: id, Source: upstream.SourceTraeCN, Capabilities: []string{capChat}})
	}
	return out
}

// applyFeatures reads the commercial block the desktop client caches per model.
//
// This is the only place Trae states a price: `features.consumption_rate.rate`
// is the multiplier the IDE prints as "积分消耗速度 x 倍", and
// `features.activity_discount.data.current` is the offer in force right now
// (before/after rates plus a discount kind). Neither is derivable from usage
// records, which only report what was already spent.
func applyFeatures(spec *upstream.ModelSpec, entry map[string]any) {
	features, _ := entry["features"].(map[string]any)
	if features == nil {
		return
	}
	listed := numericField(nestedMap(features, "consumption_rate", "data"), "rate")
	current := nestedMap(features, "activity_discount", "data", "current")
	if current != nil {
		kind := strings.TrimSpace(textField(current, "discount_type"))
		if label, ok := discountLabels[kind]; ok {
			spec.PromoLabel = label
		}
		before := numericField(current, "before_consumption_rate")
		after := numericField(current, "consumption_rate")
		switch {
		case before > 0 && after > 0 && after < before:
			factor := after / before
			// The discount is applied to `before`, so that is the listed rate.
			// `consumption_rate` is what the IDE prints as the current price.
			spec.Credits = formatRate(before)
			spec.PromoCredits = formatRate(after)
			spec.PromoFactor = &factor
			return
		case before > 0 && after == 0:
			// A zero rate is a real offer ("限时免费"), not missing data.
			factor := 0.0
			spec.Credits = formatRate(before)
			spec.PromoCredits = "0x"
			spec.PromoFactor = &factor
			return
		}
	}
	if listed > 0 {
		spec.Credits = formatRate(listed)
	}
}

// nestedMap walks a chain of object keys, tolerating missing links.
func nestedMap(root map[string]any, keys ...string) map[string]any {
	cur := root
	for _, key := range keys {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// numericField reads a numeric field, insisting on a real value: a missing or
// non-numeric key returns zero rather than a plausible-looking default.
func numericField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return f
		}
	}
	return 0
}

// textField reads a string field, tolerating absence.
func textField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// discountLabels maps the upstream's discount kinds onto the console's words.
// The names come from the desktop client's own i18n table, so the console and
// the IDE call the same offer the same thing.
var discountLabels = map[string]string{
	"subsidy":        "专属补贴",
	"member":         "会员折扣",
	"member_all_day": "会员全天折扣",
	"off_peak":       "闲时折扣",
	"limited":        "限时特惠",
}

// formatRate prints a multiplier the way the catalogue does ("x0.2", "x1.83").
func formatRate(v float64) string {
	text := strconv.FormatFloat(v, 'f', 2, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimSuffix(text, ".")
	return "x" + text
}

// clientModelPath is the desktop client's own model list. It is the only
// surface that states a price: `features.consumption_rate` (the multiplier the
// IDE prints as "积分消耗速度 x 倍") and `features.activity_discount` (the offer
// in force) live here and nowhere else — usage records only report what was
// already spent.
const clientModelPath = "/api/ide/v1/model_list"

// fetchClientModels reads that list and flattens its per-tier groups into one
// entry per model name.
//
// Failure is not fatal: the page still lists every model with its window and
// efforts, it just has no rate to show. That is the same state as before this
// probe existed, so a moved endpoint degrades instead of breaking the page.
func (c *Client) fetchClientModels(ctx context.Context, acc *account.Account) map[string]map[string]any {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + clientModelPath
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	c.setAgentHeaders(req, acc)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		logx.Debugf("trae: client model list unavailable for %s: %v", shortUID(acc.UID), err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		logx.Debugf("trae: client model list returned HTTP %d", resp.StatusCode)
		return nil
	}

	var groups map[string]json.RawMessage
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil
	}
	// The listing may arrive bare or wrapped in a data envelope.
	if raw, ok := groups["data"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil {
			groups = inner
		}
	}
	out := make(map[string]map[string]any, 32)
	for key, raw := range groups {
		if key == "code" || key == "msg" {
			continue
		}
		var items []map[string]any
		if json.Unmarshal(raw, &items) != nil {
			continue
		}
		for _, entry := range items {
			if entry == nil {
				continue
			}
			name := firstString(entry, "name", "config_name", "display_name")
			id := strings.ToLower(strings.TrimSpace(name))
			if id == "" {
				continue
			}
			// The same model appears once per tier; the first copy is enough.
			if _, seen := out[id]; seen {
				continue
			}
			out[id] = entry
		}
	}
	return out
}

// contextLength reads the window the entry advertises. Remote states a pair —
// the default the desktop client uses and the largest the account may ask for —
// while Solo states a flat token count, and the cached model list states it
// again under features.context_windows. The first two spell the default
// differently ("default" vs "dev"), so both are read before falling back to the
// maximum: a model that states only a ceiling still has a usable window.
func contextLength(entry map[string]any) int {
	for _, key := range []string{"context_window_tokens", "context_window_size"} {
		if m, ok := entry[key].(map[string]any); ok {
			for _, field := range []string{"default", "dev"} {
				if n := numeric(m[field]); n > 0 {
					return n
				}
			}
			continue
		}
		if n := numeric(entry[key]); n > 0 {
			return n
		}
	}
	if windows := nestedMap(entry, "features", "context_windows", "data"); windows != nil {
		for _, field := range []string{"dev_context", "max_context"} {
			if n := numeric(windows[field]); n > 0 {
				return n
			}
		}
	}
	return 0
}

// outputCap reports the answer-token ceiling the built-in table knows for a
// model id or alias. The live catalogue does not state one, so this is the only
// source; an unknown id returns zero, which the console renders as "-".
func outputCap(id string) int {
	if m, ok := LookupModel(id); ok {
		return m.MaxOutput
	}
	return 0
}

func numeric(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// capabilitiesOf reads the flags an entry actually states. Nothing is assumed
// beyond "it is a chat model", because a capability the account does not have
// would only produce a request the upstream rejects.
func capabilitiesOf(entry map[string]any) []string {
	caps := []string{capChat}
	if truthy(entry["support_thinking"]) || len(supportedEfforts(entry)) > 0 {
		caps = append(caps, capReasoning)
	}
	if truthy(entry["multimodal"]) {
		caps = append(caps, capVision)
	}
	if maxCapable(entry) {
		caps = append(caps, capMax)
	}
	return caps
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// describeCatalogue summarizes which optional profiles the account actually
// advertises. It is what a real-machine probe reads back: without it, "does
// this account have the 1M tier?" is unanswerable from the outside, and the
// gateway would have to guess.
func describeCatalogue(tiers map[string]map[string]map[string]any) string {
	var b strings.Builder
	for _, tier := range sortedKeys(tiers) {
		models := tiers[tier]
		var maxCapableModels, thinkingModels []string
		for name, entry := range models {
			if maxCapable(entry) {
				maxCapableModels = append(maxCapableModels, name)
			}
			if len(supportedEfforts(entry)) > 0 {
				thinkingModels = append(thinkingModels, name)
			}
		}
		sort.Strings(maxCapableModels)
		sort.Strings(thinkingModels)
		fmt.Fprintf(&b, "%s=%d models (max-capable %d: %v; thinking %d: %v); ",
			tier, len(models), len(maxCapableModels), maxCapableModels,
			len(thinkingModels), thinkingModels)
	}
	return b.String()
}

// normalizeModelConfig mirrors the client-side view of a model entry: the
// fields the upstream uses to accept a session have to exist, so a slimmed
// response is filled in rather than passed through half-formed.
func normalizeModelConfig(raw map[string]any, name string) map[string]any {
	cfg := make(map[string]any, len(raw)+6)
	for k, v := range raw {
		cfg[k] = v
	}
	// `features` arrives as a JSON string in some responses and as an object
	// in others; callers index into it either way.
	if s, ok := cfg["features"].(string); ok {
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) == nil {
			cfg["features"] = decoded
		} else {
			cfg["features"] = map[string]any{}
		}
	}
	if firstString(cfg, "config_name", "configName") == "" {
		cfg["config_name"] = name
	}
	if firstString(cfg, "model_name", "modelName") == "" {
		cfg["model_name"] = name
	}
	if _, ok := cfg["config_source"]; !ok {
		cfg["config_source"] = 1
	}
	if _, ok := cfg["provider"]; !ok {
		cfg["provider"] = ""
	}
	if _, ok := cfg["multimodal"]; !ok {
		cfg["multimodal"] = false
	}
	return cfg
}

// maxCapable reports whether the account marks this entry as max-profile.
func maxCapable(entry map[string]any) bool {
	if entry == nil {
		return false
	}
	return truthy(entry["max_mode"])
}

// truthy interprets the several spellings a boolean flag arrives in.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		}
	case float64:
		return t != 0
	case int:
		return t != 0
	case json.Number:
		f, err := t.Float64()
		return err == nil && f != 0
	}
	return false
}

func shortUID(uid string) string {
	if len(uid) > 12 {
		return uid[:12]
	}
	return uid
}
