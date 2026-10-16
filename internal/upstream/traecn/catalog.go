package traecn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
)

// The account's live model list is the only place the upstream states which
// optional profiles a model really has: whether it is max-capable (the 1M
// agent tier) and which thinking strengths it accepts. The gateway therefore
// consults it before touching either, and never invents a capability the
// account does not advertise.
//
// The list is fetched once per account per TTL and cached; a failure is cached
// briefly so a broken endpoint cannot turn every request into an extra call.
const (
	catalogueTTL        = 10 * time.Minute
	catalogueErrorTTL   = 1 * time.Minute
	catalogueModelPath  = "/models"
	catalogueModelQuery = "functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"
)

// modelCatalogue caches one account's catalogue, grouped by agent tier because
// a tier decides which profile applies: the agent tier carries the 1M entry
// while the work tier stays on the smaller fallback.
type modelCatalogue struct {
	mu      sync.Mutex
	uid     string
	tiers   map[string]map[string]map[string]any
	fetched time.Time
	lastErr bool
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
	c.cat.mu.Lock()
	ttl := catalogueTTL
	if c.cat.lastErr {
		ttl = catalogueErrorTTL
	}
	fresh := c.cat.uid == acc.UID && time.Since(c.cat.fetched) < ttl
	if fresh {
		models, ok := c.cat.tiers[tier]
		c.cat.mu.Unlock()
		if !ok {
			return nil, false
		}
		return models, true
	}
	c.cat.mu.Unlock()

	tiers, err := c.fetchCatalogue(ctx, acc)

	c.cat.mu.Lock()
	defer c.cat.mu.Unlock()
	c.cat.uid = acc.UID
	c.cat.fetched = time.Now()
	if err != nil {
		c.cat.tiers = nil
		c.cat.lastErr = true
		logx.Debugf("trae: model catalogue unavailable for %s: %v", shortUID(acc.UID), err)
		return nil, false
	}
	c.cat.tiers = tiers
	c.cat.lastErr = false
	return tiers[tier], true
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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
			raw, _ := item.(map[string]any)
			if raw == nil {
				continue
			}
			name := firstString(raw, "config_name", "configName", "name", "model_name")
			if name == "" {
				continue
			}
			if _, exists := bucket[name]; exists {
				continue
			}
			bucket[name] = normalizeModelConfig(raw, name)
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

// describeCatalogue summarizes which optional profiles the account actually
// advertises. It is what a real-machine probe reads back: without it, "does
// this account have the 1M tier?" is unanswerable from the outside, and the
// gateway would have to guess.
func describeCatalogue(tiers map[string]map[string]map[string]any) string {
	var b strings.Builder
	names := make([]string, 0, len(tiers))
	for tier := range tiers {
		names = append(names, tier)
	}
	sort.Strings(names)
	for _, tier := range names {
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
