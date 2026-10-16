package traecn

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// liveCatalogueBody mirrors the shape the upstream really answers with: three
// agent tiers carrying the same model twice, a model the upstream marks
// invisible to the user, and a capitalised config name.
func liveCatalogueBody() string {
	return `{"code":0,"data":{"list":[` +
		`{"function":"solo_agent_remote","models":[` +
		`{"name":"mimo-v2.6-pro","config_name":"mimo-v2.6-pro","max_mode":true,"support_thinking":true,` +
		`"context_window_size":{"default":200000,"max":[1000000]}},` +
		`{"name":"aquila","config_name":"aquila","is_invisible_to_user":true},` +
		`{"name":"Doubao-Seed-Code","config_name":"Doubao-Seed-Code","multimodal":true}]},` +
		`{"function":"solo_work_remote","models":[` +
		`{"name":"mimo-v2.6-pro","config_name":"mimo-v2.6-pro","max_mode":false},` +
		`{"name":"glm-5.3-flash","config_name":"glm-5.3-flash"}]},` +
		`{"function":"solo_design_remote","models":[` +
		`{"name":"glm-5.2","config_name":"glm-5.2"}]}` +
		`]}}`
}

// soloCatalogueBody carries a model Remote has not published, plus the three
// shapes that must never reach the picker: an invisible entry, a non-chat
// profile and a custom model.
func soloCatalogueBody() string {
	return `{"config_info_list":[` +
		`{"config_name":"Solo-Only-Model","usage":"chat_completion","is_invisible_to_user":false},` +
		`{"config_name":"aquila","usage":"chat_completion","is_invisible_to_user":true},` +
		`{"config_name":"browser_use_subagent","usage":"chat_completion","is_invisible_to_user":true},` +
		`{"config_name":"internal-runner","usage":"tool_call"},` +
		`{"config_name":"custom-thing","usage":"chat_completion","custom_models":true}` +
		`]}`
}

func liveStub(t *testing.T) *stub {
	t.Helper()
	s := newStub(t)
	s.catalogStatus = http.StatusOK
	s.catalogBody = liveCatalogueBody()
	s.soloStatus = http.StatusOK
	s.soloBody = soloCatalogueBody()
	return s
}

func modelIDs(specs []upstream.ModelSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.ID)
	}
	return out
}

func assertIDs(t *testing.T, got []upstream.ModelSpec, want ...string) {
	t.Helper()
	have := modelIDs(got)
	if len(have) != len(want) {
		t.Fatalf("models = %v, want %v", have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Fatalf("models = %v, want %v", have, want)
		}
	}
}

// The advertised list is the account's live catalogue: every visible Remote
// entry, then whatever Solo has that Remote does not, then the ids the gateway
// routes itself.
func TestAdvertisedModelsReflectTheLiveCatalogue(t *testing.T) {
	s := liveStub(t)
	c := s.client(Config{})
	acc := testAccount()

	c.loadCatalogue(context.Background(), acc, true)
	models := c.advertisedModels(acc)

	assertIDs(t, models,
		"doubao-seed-code", "mimo-v2.6-pro", "glm-5.2", "glm-5.3-flash",
		"solo-only-model", "auto", "work",
	)
}

// A profile the upstream keeps out of its own picker must not be offered here,
// and neither must a tool runner or a custom model.
func TestAdvertisedModelsHideWhatTheUpstreamHides(t *testing.T) {
	s := liveStub(t)
	c := s.client(Config{})
	acc := testAccount()
	c.loadCatalogue(context.Background(), acc, true)

	for _, spec := range c.advertisedModels(acc) {
		switch spec.ID {
		case "aquila", "browser_use_subagent", "internal-runner", "custom-thing":
			t.Errorf("%s is not a model a user may pick", spec.ID)
		}
	}
	// It stays in the tier map: a client that asks for one by name is still
	// answered by the upstream rather than rejected here.
	if _, ok := c.modelEntry(context.Background(), acc, remoteAgent, "aquila"); !ok {
		t.Error("an invisible entry must remain resolvable by name")
	}
}

func TestAdvertisedModelsDescribeTheEntrysOwnFlags(t *testing.T) {
	s := liveStub(t)
	c := s.client(Config{})
	acc := testAccount()
	c.loadCatalogue(context.Background(), acc, true)

	byID := map[string]upstream.ModelSpec{}
	for _, spec := range c.advertisedModels(acc) {
		byID[spec.ID] = spec
	}

	max := byID["mimo-v2.6-pro"]
	if max.ContextLength != 200000 {
		t.Errorf("context length = %d, want the advertised 200000", max.ContextLength)
	}
	for _, want := range []string{capChat, capReasoning, capMax} {
		if !hasString(max.Capabilities, want) {
			t.Errorf("mimo-v2.6-pro capabilities = %v, want %s", max.Capabilities, want)
		}
	}
	if caps := byID["glm-5.2"].Capabilities; hasString(caps, capReasoning) || hasString(caps, capMax) {
		t.Errorf("glm-5.2 capabilities = %v, want nothing the entry did not state", caps)
	}
	if !hasString(byID["doubao-seed-code"].Capabilities, capVision) {
		t.Errorf("multimodal entry lost its vision flag: %v", byID["doubao-seed-code"].Capabilities)
	}
}

// Listing models must never wait on the upstream: a stale cache answers from
// what is known and refreshes behind the request.
func TestAdvertisedModelsNeverBlockOnASlowUpstream(t *testing.T) {
	s := liveStub(t)
	gate := make(chan struct{})
	s.catalogGate = gate
	c := s.client(Config{})
	acc := testAccount()

	started := time.Now()
	models := c.advertisedModels(acc)
	elapsed := time.Since(started)
	if elapsed > time.Second {
		t.Fatalf("advertisedModels waited %s on a held upstream", elapsed)
	}
	// Nothing has been read yet, so the answer is the built-in snapshot.
	if len(models) == 0 || models[0].ID != advertisedModelIDs[0] {
		t.Fatalf("models = %v, want the built-in snapshot", modelIDs(models))
	}

	// Let the fetch finish so the background goroutine does not outlive the
	// test (httptest.Server.Close waits for it).
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for !c.catalogueFresh(acc) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.catalogueFresh(acc) {
		t.Fatal("the background refresh never landed")
	}
	if models := c.advertisedModels(acc); models[1].ID != "mimo-v2.6-pro" {
		t.Fatalf("models = %v, want the live catalogue after the refresh", modelIDs(models))
	}
}

// A failed refresh must not empty the list, and must not keep handing out a
// model profile either.
func TestFailedRefreshKeepsTheListAndDropsTheTiers(t *testing.T) {
	s := liveStub(t)
	c := s.client(Config{MaxMode: MaxModeConfig{Enabled: true}})
	acc := testAccount()

	c.loadCatalogue(context.Background(), acc, true)
	before := modelIDs(c.advertisedModels(acc))

	s.mu.Lock()
	s.catalogStatus = http.StatusInternalServerError
	s.mu.Unlock()
	expireCatalogue(c)
	c.loadCatalogue(context.Background(), acc, true)

	if after := modelIDs(c.advertisedModels(acc)); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("models = %v, want the last good list %v", after, before)
	}
	if _, ok := c.catalogueTier(context.Background(), acc, remoteAgent); ok {
		t.Error("a failed fetch must report the tier as unknown, not stale")
	}
}

// With nothing ever fetched the snapshot answers, and it must name what the
// upstream currently serves rather than what it served months ago.
func TestSnapshotIsServedWhenNothingHasBeenRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*stub)
	}{
		{"upstream failing", func(s *stub) { s.catalogStatus = http.StatusInternalServerError }},
		{"route missing", func(s *stub) { s.catalogStatus = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			tc.prepare(s)
			c := s.client(Config{})
			acc := testAccount()
			c.loadCatalogue(context.Background(), acc, true)

			ids := map[string]bool{}
			for _, spec := range c.advertisedModels(acc) {
				ids[spec.ID] = true
			}
			for _, want := range []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "glm-5.3-flash", "auto", "work"} {
				if !ids[want] {
					t.Errorf("snapshot is missing %s", want)
				}
			}
			for _, gone := range []string{"mimo-v2.5", "mimo-v2.5-pro", "glm-4.6", "kimi-k2.5"} {
				if ids[gone] {
					t.Errorf("snapshot still offers the retired model %s", gone)
				}
			}
		})
	}
}

// An id the live catalogue taught us keeps its exact spelling, which is what a
// client echoing back the lowercase id from /v1/models needs.
func TestResolveConfigNameLearnsTheLiveSpelling(t *testing.T) {
	s := liveStub(t)
	s.catalogBody = `{"code":0,"data":{"list":[{"function":"solo_agent_remote","models":[` +
		`{"name":"Kimi-K9-Turbo","config_name":"Kimi-K9-Turbo"}]}]}}`
	c := s.client(Config{})
	c.loadCatalogue(context.Background(), testAccount(), true)

	if got := ResolveConfigName("kimi-k9-turbo"); got != "Kimi-K9-Turbo" {
		t.Errorf("ResolveConfigName = %q, want the spelling the upstream used", got)
	}
	// A name nobody published still passes through untouched.
	if got := ResolveConfigName("  Some-Unreleased-Model  "); got != "Some-Unreleased-Model" {
		t.Errorf("ResolveConfigName = %q, want the trimmed name", got)
	}
}

// The built-in snapshot is a fallback, not the answer: it must move with the
// upstream instead of accumulating every model that ever existed.
func TestSnapshotTracksTheUpstreamRatherThanEveryModelEverSeen(t *testing.T) {
	ids := map[string]bool{}
	for _, id := range advertisedModelIDs {
		ids[id] = true
	}
	for _, want := range []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "glm-5.3-flash", "glm-5.3-flashx", "deepseek-v4.1-flash", "step-5-preview"} {
		if !ids[want] {
			t.Errorf("snapshot is missing the live model %s", want)
		}
	}
	for _, gone := range []string{"mimo-v2.5", "mimo-v2.5-pro", "glm-4.6", "glm-5v-turbo", "qwen3-coder"} {
		if ids[gone] {
			t.Errorf("snapshot still advertises the retired model %s", gone)
		}
	}
	// Retired names stay resolvable so an existing client config keeps working.
	if got := ResolveConfigName("gpt-4o"); got != "DeepSeek-V4-Pro" {
		t.Errorf("an alias must still resolve, got %q", got)
	}
	if specs := defaultModelSpecs(); len(specs) != len(advertisedModelIDs) {
		t.Errorf("snapshot has %d specs for %d ids", len(specs), len(advertisedModelIDs))
	}
}

// expireCatalogue makes the cache stale without waiting out its TTL.
func expireCatalogue(c *Client) {
	c.cat.mu.Lock()
	defer c.cat.mu.Unlock()
	c.cat.fetched = time.Time{}
}

func hasString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
