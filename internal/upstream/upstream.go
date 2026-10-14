// Package upstream defines the pluggable upstream abstraction shared by every
// source adapter.
package upstream

import (
	"context"
	"sort"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// Source re-exports account.Source for convenience at the call site.
type Source = account.Source

const (
	// SourceWorkBuddy is the WorkBuddy / CodeBuddy family.
	SourceWorkBuddy = account.SourceWorkBuddy
	// SourceTraeCN is Trae CN.
	SourceTraeCN = account.SourceTraeCN
)

// ModelSpec describes one model exposed by a source.
type ModelSpec struct {
	ID            string   `json:"id"`
	Source        Source   `json:"source"`
	ContextLength int      `json:"context_length,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

// Usage is token accounting for a single request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Message is one chat message in the normalized form.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name,omitempty"`
}

// ToolCall is a normalized tool invocation.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest is the normalized inbound request handed to an upstream.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Stream      bool
	Temperature *float64
	MaxTokens   *int
	Tools       []any
	// Raw keeps the original decoded body so an adapter can pass through fields
	// it does not model explicitly.
	Raw map[string]any
}

// ChatResponse is the normalized non-streaming result.
type ChatResponse struct {
	ID               string
	Model            string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            Usage
}

// StreamDelta is one incremental chunk emitted by ChatStream.
type StreamDelta struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            *Usage
}

// EmitFunc receives stream deltas in order.
type EmitFunc func(StreamDelta) error

// Probe is a lightweight health/credit snapshot for an account.
type Probe struct {
	Credits int64
	Healthy bool
	Detail  string
}

// Upstream is implemented by every source adapter.
type Upstream interface {
	// Source returns the source this adapter serves.
	Source() Source

	// ListModels returns the model catalog visible to acc.
	ListModels(ctx context.Context, acc *account.Account) ([]ModelSpec, error)

	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, acc *account.Account, req *ChatRequest) (*ChatResponse, error)

	// ChatStream performs a streaming completion, invoking emit per chunk.
	ChatStream(ctx context.Context, acc *account.Account, req *ChatRequest, emit EmitFunc) error

	// Refresh renews the account credentials.
	Refresh(ctx context.Context, acc *account.Account) error

	// Probe reports account health and credits.
	Probe(ctx context.Context, acc *account.Account) (*Probe, error)
}

// DeviceCode is the user-facing challenge of an interactive device login.
type DeviceCode struct {
	// State is the opaque session identifier used when polling.
	State string
	// URL is where the user authorizes the device.
	URL string
	// Realm records which regional service issued the code.
	Realm string
}

// DeviceAuth is implemented by adapters that support interactive device-code
// login. The console drives it to add accounts without restarting the gateway.
type DeviceAuth interface {
	// DeviceStart requests a device code and its authorization URL.
	DeviceStart(ctx context.Context, realm string) (*DeviceCode, error)

	// DevicePoll reports whether authorization has completed. When done is true
	// the returned account is ready to be persisted.
	DevicePoll(ctx context.Context, realm, state string) (*account.Account, bool, error)
}

var registry = map[Source]Upstream{}

// Register makes an upstream available to the router.
func Register(u Upstream) { registry[u.Source()] = u }

// Unregister removes an upstream from the router.
func Unregister(s Source) { delete(registry, s) }

// Get returns the registered upstream for a source.
func Get(s Source) (Upstream, bool) {
	u, ok := registry[s]
	return u, ok
}

// Sources lists the currently registered sources in a stable order.
func Sources() []Source {
	out := make([]Source, 0, len(registry))
	for s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
