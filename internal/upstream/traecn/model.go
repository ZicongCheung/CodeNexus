package traecn

import (
	"strings"
	"sync"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The upstream matches models by an exact, case-sensitive config name, while
// clients expect the loose lowercase ids every other OpenAI-compatible surface
// uses. This file holds the translation between the two.
//
// The table below is a resolution index, not the advertised list: it is what
// maps a client's spelling onto the exact string the upstream wants, including
// the alias families (the Claude ids that are all served through one config
// name). /v1/models is answered from the account's live catalogue instead, and
// only falls back to advertisedModelIDs when that cannot be read at all.
//
// Keeping retired names resolvable is deliberate — an old client that still
// asks for one gets the upstream's own answer, which is more useful than a
// local rejection.

// Capability labels reported on a ModelSpec.
const (
	capChat      = "chat"
	capTools     = "tools"
	capVision    = "vision"
	capReasoning = "reasoning"
	capMax       = "max"
)

// Model is one resolvable model.
type Model struct {
	// ID is the client-facing identifier, always lowercase.
	ID string
	// ConfigName is the exact string the upstream expects.
	ConfigName string
	// Aliases are extra client-facing ids that resolve to the same model.
	Aliases []string
	// Capabilities is a best-effort static view; the live catalog refines it.
	Capabilities []string
	// ContextLen is the advertised context window in tokens (0 when unknown).
	ContextLen int
	// MaxOutput is the per-response output cap in tokens.
	MaxOutput int
}

// defaultContext is the window the desktop client assumes when a model does not
// advertise one.
const defaultContext = 200000

// agentMaxOutput is the output ceiling for the agent-family models.
const agentMaxOutput = 64000

// advertisedModelIDs is what /v1/models answers when no account has a usable
// live catalogue — every account cooling, the upstream unreachable, or a fresh
// install with no credential yet.
//
// It is a snapshot of what the upstream advertised when the adapter was last
// checked against it, so the fallback cannot offer a model the upstream has
// already retired. The live catalogue is the real answer; this only has to be
// honest about the moment it was taken.
var advertisedModelIDs = []string{
	"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx", "glm-5.2", "glm-4.7-flash",
	"deepseek-v4-pro-official", "deepseek-v4-flash-official", "deepseek-v4.1-flash",
	"doubao-seed-2.1-pro", "doubao-seed-2.1-turbo", "doubao-seed-code", "doubao-seed-evolving",
	"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "minimax-m3",
	"mimo-v2.6-pro", "mimo-v2.6-flash", "qwen-3.7-plus", "qwen3.8-max", "step-5-preview",
	"auto", "work",
}

// liveNames maps a lowercase id onto the exact spelling the live catalogue
// published for it (DeepSeek-V4-Pro-Official, Doubao-Seed-Code, ...). A client
// that echoes back an id from /v1/models has to reach that exact string, and
// the built-in table cannot know about a model that was released after it.
var liveNames sync.Map

// rememberLiveName records the upstream's own spelling for an advertised id.
func rememberLiveName(name string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed == strings.ToLower(trimmed) {
		return
	}
	liveNames.Store(strings.ToLower(trimmed), trimmed)
}

// catalog lists one entry per distinct upstream config name, in the order the
// console should present them.
var catalog = []Model{
	{
		ID: "glm-5.3", ConfigName: "glm-5.3",
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "glm-5.2", ConfigName: "glm-5.2",
		// "auto" is the routing id for the default model, and the whole Claude
		// family is served through this config name upstream-side.
		Aliases: []string{
			"auto", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
			"claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4",
			"claude-3.7-sonnet", "claude-3-7-sonnet", "claude-3.5-sonnet",
			"claude3.5", "aws_sdk_claude37_sonnet",
		},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "glm-5.1", ConfigName: "glm-5.1",
		Aliases:      []string{"claude-haiku-4-5"},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "glm-5", ConfigName: "glm-5",
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "glm-4.7", ConfigName: "glm-4.7",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "glm-4.6", ConfigName: "glm-4.6",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "deepseek-v4-pro", ConfigName: "DeepSeek-V4-Pro",
		Aliases:      []string{"gpt-4o", "gpt-4o-latest", "gpt-4.1", "deepseek-v3", "deepseek-r1"},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "deepseek-v4-flash", ConfigName: "DeepSeek-V4-Flash",
		Aliases:      []string{"gpt-4o-mini"},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "deepseek-v4-pro-official", ConfigName: "DeepSeek-V4-Pro-Official",
		Aliases:      []string{"deepseek-v4-pro 正式版"},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "deepseek-v4-flash-official", ConfigName: "DeepSeek-V4-Flash-Official",
		Aliases:      []string{"deepseek-v4-flash 正式版"},
		Capabilities: []string{capChat, capTools, capReasoning},
		MaxOutput:    agentMaxOutput,
	},
	{
		ID: "doubao-seed-2.1-pro", ConfigName: "Doubao-Seed-2.1-Pro",
		Aliases:      []string{"seed-2.1-pro"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "doubao-seed-2.1-turbo", ConfigName: "Doubao-Seed-2.1-Turbo",
		Aliases:      []string{"seed-2.1-turbo"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "doubao-seed-code", ConfigName: "Doubao-Seed-Code",
		Aliases:      []string{"seed-code"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "doubao-seed-2.0-code", ConfigName: "Doubao-Seed-2.0-Code",
		Aliases:      []string{"seed-2.0-code"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "doubao-seed-evolving", ConfigName: "Doubao-Seed-Evolving",
		Aliases:      []string{"seed-evolving"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "qwen-3.7-plus", ConfigName: "qwen-3.7-plus",
		Aliases:      []string{"qwen3.7-plus"},
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "qwen-3.6-plus", ConfigName: "qwen-3.6-plus",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "qwen-3.5", ConfigName: "qwen-3.5",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "qwen3.8-max", ConfigName: "qwen3.8-max",
		Capabilities: []string{capChat, capTools, capVision, capMax},
	},
	{
		ID: "qwen3-coder", ConfigName: "qwen3-coder",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "qwen36-35b", ConfigName: "qwen36-35b",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "kimi-k2.6", ConfigName: "kimi-k2.6",
		Capabilities: []string{capChat, capTools, capVision, capReasoning},
	},
	{
		ID: "kimi-k2.7-code", ConfigName: "kimi-k2.7-code",
		Capabilities: []string{capChat, capTools, capVision, capReasoning},
	},
	{
		ID: "kimi-k3", ConfigName: "kimi-k3",
		Capabilities: []string{capChat, capTools, capVision, capReasoning},
	},
	{
		ID: "kimi-k2.5", ConfigName: "kimi-k2.5",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "kimi-k2", ConfigName: "kimi-k2",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "minimax-m3", ConfigName: "minimax-m3",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "minimax-m2.7", ConfigName: "minimax-m2.7",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "minimax-m2.1", ConfigName: "minimax-m2.1",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "minimax-m25", ConfigName: "minimax-m25",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "mimo-v2.5-pro", ConfigName: "mimo-v2.5-pro",
		Capabilities: []string{capChat, capTools},
	},
	{
		ID: "mimo-v2.5", ConfigName: "mimo-v2.5",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		ID: "glm-5v-turbo", ConfigName: "glm-5v-turbo",
		Capabilities: []string{capChat, capTools, capVision},
	},
	{
		// The Work executor. Selecting it pins the request to the work agent
		// rather than letting the router choose.
		ID: "work", ConfigName: "work",
		Capabilities: []string{capChat, capTools},
	},
}

// index maps every client-facing id, lowercased, onto its model.
var index = buildIndex()

func buildIndex() map[string]Model {
	m := make(map[string]Model, len(catalog)*3)
	for _, model := range catalog {
		model.ContextLen = defaultContext
		if model.MaxOutput == 0 {
			model.MaxOutput = agentMaxOutput
		}
		m[strings.ToLower(model.ID)] = model
		m[strings.ToLower(model.ConfigName)] = model
		for _, a := range model.Aliases {
			m[strings.ToLower(a)] = model
		}
	}
	// The IDE Surface and a few older names appear in client configs without a
	// matching catalog entry; they all resolve through the Claude alias family.
	for _, stray := range []string{"claude-opus-4-1", "claude-sonnet-3-7"} {
		if _, ok := m[stray]; !ok {
			m[stray] = m["glm-5.2"]
		}
	}
	return m
}

// Catalog returns the models /v1/models advertises when the account's live
// catalogue is unavailable. An id the table does not describe is passed
// through by ResolveConfigName, so it is still listed with a plain spec.
func Catalog() []Model {
	out := make([]Model, 0, len(advertisedModelIDs))
	for _, id := range advertisedModelIDs {
		if m, ok := LookupModel(id); ok {
			m.ID = id
			m.ContextLen = defaultContext
			if m.MaxOutput == 0 {
				m.MaxOutput = agentMaxOutput
			}
			out = append(out, m)
			continue
		}
		out = append(out, Model{
			ID: id, ConfigName: id,
			Capabilities: []string{capChat},
			ContextLen:   defaultContext,
			MaxOutput:    agentMaxOutput,
		})
	}
	return out
}

// defaultModelSpecs is Catalog() in the shape the upstream interface wants.
func defaultModelSpecs() []upstream.ModelSpec {
	models := Catalog()
	out := make([]upstream.ModelSpec, 0, len(models))
	for _, m := range models {
		out = append(out, upstream.ModelSpec{
			ID:            m.ID,
			Source:        upstream.SourceTraeCN,
			ContextLength: m.ContextLen,
			Capabilities:  m.Capabilities,
		})
	}
	return out
}

// LookupModel resolves a client-supplied name. Matching is case-insensitive and
// accepts canonical ids, upstream config names and aliases. The boolean reports
// whether the name is known at all; when it is not, callers may still pass it
// through, because the upstream keeps adding models.
func LookupModel(name string) (Model, bool) {
	m, ok := index[strings.ToLower(strings.TrimSpace(name))]
	return m, ok
}

// ResolveConfigName maps a client name onto the exact upstream config name,
// passing unknown names through unchanged. A name the live catalogue taught us
// keeps its exact spelling, which is what a client echoing back a lowercase id
// needs.
func ResolveConfigName(name string) string {
	if m, ok := LookupModel(name); ok {
		return m.ConfigName
	}
	trimmed := strings.TrimSpace(name)
	if exact, ok := liveNames.Load(strings.ToLower(trimmed)); ok {
		return exact.(string)
	}
	return trimmed
}

// WorkModelID is the routing id that pins the Work executor.
const WorkModelID = "work"
