// Package upstream defines the pluggable upstream abstraction shared by every
// source adapter.
package upstream

import (
	"context"
	"sort"
	"sync/atomic"

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
	// ThinkingEfforts lists the thinking strengths the account's own catalogue
	// advertises for this model, weakest first, in the upstream's vocabulary.
	//
	// Empty means the model takes no strength at all — the console shows that
	// as "not adjustable" rather than as "default", because the two are
	// different offers and only the catalogue can tell them apart.
	ThinkingEfforts []string `json:"thinking_efforts,omitempty"`
	// MaxOutput is the largest answer the model will produce, in tokens. The
	// catalogue states it, and it is the number that decides whether a long
	// generation gets truncated.
	MaxOutput int `json:"max_output_tokens,omitempty"`
	// Credits is the catalogue's own price wording for one turn of this model
	// (for example "x0.79"). It stays a string on purpose: a model with no
	// published price must read as "not stated", which a zero cannot express.
	Credits string `json:"credits,omitempty"`
	// PromoCredits is the discounted rate currently in force, when the
	// catalogue marks one. Empty means the listed rate is what is charged.
	PromoCredits string `json:"promo_credits,omitempty"`
	PromoLabel   string `json:"promo_label,omitempty"`
	// PromoNote is the upstream's own sentence about when the offer applies
	// ("工作日 00:00-09:00 …"). Off-peak offers carry no machine-readable
	// discount, so this sentence is the only thing that can be shown; it also
	// rides along as a tooltip for offers that do state a rate.
	PromoNote string `json:"promo_note,omitempty"`
	// PromoFactor is the discount as a fraction of the listed rate (0 = free,
	// 0.5 = half price). It is a pointer because "no discount object" and
	// "a discount of zero" are different offers: off-peak promotions carry a
	// label but no factor, and only the upstream knows the rate then.
	PromoFactor *float64 `json:"promo_factor,omitempty"`
	// Default marks the model the account's own catalogue offers first.
	Default bool `json:"default,omitempty"`
}

// Usage is token accounting for a single request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the part of the prompt the upstream served from its own
	// cache instead of reading again. It is billed cheaper than fresh input, so
	// a turn's cost cannot be read off its token count alone: without this the
	// console can only say how many tokens moved, never how many were paid for
	// at full price.
	CachedTokens int `json:"cached_tokens"`
}

// Message is one chat message in the normalized form. Tool calls and their
// results are first-class fields rather than a passthrough of one protocol's
// spelling: every wire format folds its own shape (OpenAI tool_calls/tool role,
// Anthropic tool_use/tool_result, Responses function_call/function_call_output)
// onto ToolCalls/ToolCallID on the way in, and re-expands them on the way out.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name,omitempty"`
	// ToolCalls is set on an assistant turn that asked for tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on the tool turn that answers one call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ReasoningContent is a bounded reasoning summary kept for continuation.
	// It is presentation history the upstream may regenerate, never a full
	// private chain.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall is a normalized tool invocation.
//
// Index is the ordinal slot the call occupies within one assistant turn. A
// streaming upstream may describe a call across several fragments that share
// an index; the ordinal is what lets a renderer group them again, so it must be
// stable for the whole turn.
type ToolCall struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Index int    `json:"index,omitempty"`
	// Function holds the invoked name and its JSON arguments. In a stream the
	// arguments may be a prefix increment rather than a whole value; the
	// tracker below is what folds those fragments together.
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
	// TurnID is the upstream identifier of this turn, when the surface exposes
	// one. A per-turn billing lookup is keyed on it.
	TurnID string
	// CreditsConsumed, when non-zero, is a charge the upstream already reported.
	// It takes precedence over any out-of-band settlement.
	CreditsConsumed float64
	// CreditsSource names where CreditsConsumed came from.
	CreditsSource string
	// Surface names the executor that served the turn — "remote", "ide",
	// "cli" or "cloud". It is what decides whether the max profile and the
	// thinking strength could be applied at all, so the console can explain a
	// setting that is on but had no effect.
	Surface string
	// MaxProfile reports that the 1M max profile was applied, and MaxContext
	// is the window the account's own catalogue gave it.
	MaxProfile bool
	MaxContext int
	// ContinuedRounds counts the extra upstream turns auto-continue added on
	// top of the client's one.
	ContinuedRounds int
}

// Route is what served one turn: the executor, and whether the max profile
// applied with which window. A streaming turn never builds a response object,
// so its first delta carries this instead.
type Route struct {
	Surface    string
	MaxProfile bool
	MaxContext int
}

// StreamDelta is one incremental chunk emitted by ChatStream.
type StreamDelta struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            *Usage
	// Route accompanies the first delta of a streaming turn so the caller
	// learns which executor served it and whether a capability applied.
	Route *Route
	// TurnID carries the upstream turn identifier so a streaming caller can
	// settle the turn's charge after the stream ends.
	TurnID string
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

// CredentialAcceptor is implemented by adapters that can turn a manually pasted
// credential (a long-lived token, optionally with a refresh token) into a ready
// account. The console drives it so a user can onboard without an interactive
// browser flow; adapters without it simply report manual entry as unsupported.
type CredentialAcceptor interface {
	// AcceptCredential validates and normalizes a pasted credential. uid and
	// label may be empty and are then derived from the token when possible.
	AcceptCredential(token, refreshToken, uid, label string) (*account.Account, error)
}

// LocalDiscoverer is implemented by adapters that can import credentials the
// upstream's own desktop client already holds on this machine. It is a separate
// interface from CredentialAcceptor because the two answer different questions:
// one normalizes a secret the user handed over, the other finds one that is
// already present. Reading another application's store is done only when the
// user asks for it, never on a background scan.
type LocalDiscoverer interface {
	// DiscoverLocal returns accounts found on this machine. A returned error
	// with no accounts means nothing usable was found, and its message explains
	// why so the console can show it.
	DiscoverLocal() ([]*account.Account, error)
}

// TurnUsage is implemented by adapters whose upstream bills per turn and can
// report that charge after the answer has been produced. It is separate from
// Credits because a charge is not a balance: settling a turn never rewrites the
// account's remaining credit.
type TurnUsage interface {
	// TurnCredits returns the credit charge of one completed turn, together
	// with a marker naming where the number came from. An upstream that has not
	// settled the turn yet must report an error rather than zero, because a
	// settled zero and an unsettled turn are different facts.
	TurnCredits(ctx context.Context, acc *account.Account, turnID string) (float64, string, error)

	// UsedCredits reports the credits the account has consumed in total, which
	// is what a single turn's charge is measured against when the upstream has
	// no per-turn bill to hand. It is a read-only observation: settling a turn
	// must never write the result back into the account's balance, or an
	// observation would end up steering routing. ok is false when the number
	// cannot be read, which is not the same as a balance of zero.
	UsedCredits(ctx context.Context, acc *account.Account) (float64, bool)
}

// registry is swapped as a whole rather than mutated: the console can rebuild
// an upstream when its settings change, and requests read the registry
// concurrently. A copy-on-write map keeps that from becoming a data race.
var registry atomic.Pointer[map[Source]Upstream]

func init() { registry.Store(&map[Source]Upstream{}) }

// Register makes an upstream available to the router. Registering a source
// again replaces it, which is how a settings change reaches the running
// gateway without a restart.
func Register(u Upstream) {
	next := make(map[Source]Upstream)
	for s, v := range *registry.Load() {
		next[s] = v
	}
	next[u.Source()] = u
	registry.Store(&next)
}

// Unregister removes an upstream from the router.
func Unregister(s Source) {
	next := make(map[Source]Upstream)
	for k, v := range *registry.Load() {
		if k != s {
			next[k] = v
		}
	}
	registry.Store(&next)
}

// Get returns the registered upstream for a source.
func Get(s Source) (Upstream, bool) {
	u, ok := (*registry.Load())[s]
	return u, ok
}

// Sources lists the currently registered sources in a stable order.
func Sources() []Source {
	reg := *registry.Load()
	out := make([]Source, 0, len(reg))
	for s := range reg {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
