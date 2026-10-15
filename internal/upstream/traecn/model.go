package traecn

import "strings"

// The upstream matches models by an exact, case-sensitive config name, while
// clients expect the loose lowercase ids every other OpenAI-compatible surface
// uses. This file holds the translation between the two.
//
// The catalog is deliberately static: /v1/models has to answer even when every
// account is cooling, and a deterministic list keeps the router's same-name
// logic stable across restarts. The live catalog the upstream publishes is
// merged on top of it when it is reachable.

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

// Catalog returns every client-facing model, de-duplicated by id.
func Catalog() []Model {
	out := make([]Model, 0, len(catalog))
	for _, m := range catalog {
		m.ContextLen = defaultContext
		if m.MaxOutput == 0 {
			m.MaxOutput = agentMaxOutput
		}
		out = append(out, m)
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
// passing unknown names through unchanged.
func ResolveConfigName(name string) string {
	if m, ok := LookupModel(name); ok {
		return m.ConfigName
	}
	return strings.TrimSpace(name)
}

// WorkModelID is the routing id that pins the Work executor.
const WorkModelID = "work"
