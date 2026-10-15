package api

// State for the Responses facade: the tool-name bindings that let a response
// speak Responses names while the upstream speaks chat names, and the
// previous_response_id session cache that lets a client continue a conversation
// by reference.
//
// Nothing here is ever written to disk. The cache holds the conversation the
// client already sent us, bounded in both entry count and bytes, so a gateway
// restart simply forgets the chains (a client then resends its input).

import (
	"container/list"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// responsesBinding maps one caller-declared tool onto the name the upstream is
// told to call, and back.
type responsesBinding struct {
	ChatName     string
	ResponseType string // function | custom | tool_search
	Name         string
	Namespace    string
	Execution    string
}

// bindingKey is the Responses-side identity of a tool: (type, namespace, name).
type bindingKey [3]string

func (b responsesBinding) key() bindingKey {
	return bindingKey{b.ResponseType, b.Namespace, b.Name}
}

// responsesContext carries one in-flight Responses turn.
type responsesContext struct {
	responseID        string
	model             string
	createdAt         int64
	request           map[string]any
	messages          []any // the merged conversation, in OpenAI message shape
	bindings          map[string]responsesBinding
	callBindings      map[string]responsesBinding
	tools             []any
	upstreamSessionID string
	// thinking is the caller's explicit opt-in to a visible reasoning channel.
	thinking bool
	// toolChoice echoes what the client asked for, for the response object.
	toolChoice any
}

func (c *responsesContext) resolveBinding(chatName string) responsesBinding {
	if b, ok := c.bindings[chatName]; ok {
		return b
	}
	return responsesBinding{ChatName: chatName, ResponseType: "function", Name: chatName}
}

// ── session cache ───────────────────────────────────────────────────────────

// responsesSession is what a stored response is worth on the next turn: the
// conversation so far, the tool vocabulary, and the upstream session to keep.
type responsesSession struct {
	Messages          []any                       `json:"messages"`
	Bindings          map[string]responsesBinding `json:"bindings"`
	CallBindings      map[string]responsesBinding `json:"call_bindings"`
	Tools             []any                       `json:"tools"`
	UnavailableReason string                      `json:"unavailable_reason,omitempty"`
	UpstreamSessionID string                      `json:"upstream_session_id"`
}

type responsesCacheEntry struct {
	id      string
	expires time.Time
	session *responsesSession
}

// responsesCache is a bounded in-memory LRU.
type responsesCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxN     int
	maxBytes int
	maxMsgs  int
	entries  map[string]*list.Element
	order    *list.List // front is the most recently used
	now      func() time.Time
}

func newResponsesCache(ttl time.Duration, maxN, maxBytes, maxMsgs int) *responsesCache {
	return &responsesCache{
		ttl:      ttl,
		maxN:     max(1, maxN),
		maxBytes: max(1024, maxBytes),
		maxMsgs:  max(1, maxMsgs),
		entries:  map[string]*list.Element{},
		order:    list.New(),
		now:      time.Now,
	}
}

func (c *responsesCache) pruneLocked(now time.Time) {
	for id, el := range c.entries {
		if !el.Value.(*responsesCacheEntry).expires.After(now) {
			c.order.Remove(el)
			delete(c.entries, id)
		}
	}
}

// get returns a copy of the session, or nil when it is absent or expired.
func (c *responsesCache) get(id string) *responsesSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(c.now())
	el, ok := c.entries[id]
	if !ok {
		return nil
	}
	c.order.MoveToFront(el)
	session := cloneJSON(*el.Value.(*responsesCacheEntry).session)
	return &session
}

// put stores a session, bounded in messages and bytes. A session that exceeds
// the bounds is stored as an explicit "unavailable" marker rather than dropped,
// so the next turn reports why instead of looking like an ordinary miss.
func (c *responsesCache) put(id string, s *responsesSession) {
	snapshot := c.bounded(s)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	if el, ok := c.entries[id]; ok {
		c.order.Remove(el)
		delete(c.entries, id)
	}
	el := c.order.PushFront(&responsesCacheEntry{id: id, expires: now.Add(c.ttl), session: snapshot})
	c.entries[id] = el
	for len(c.entries) > c.maxN {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.entries, back.Value.(*responsesCacheEntry).id)
	}
}

func (c *responsesCache) bounded(s *responsesSession) *responsesSession {
	clone := cloneJSON(s)
	if len(clone.Messages) <= c.maxMsgs && c.size(clone) <= c.maxBytes {
		return clone
	}
	return &responsesSession{
		UnavailableReason: "previous response history exceeded the configured cache limit",
	}
}

func (c *responsesCache) size(s *responsesSession) int {
	b, err := json.Marshal(s)
	if err != nil {
		return 0
	}
	return len(b)
}

func (c *responsesCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(c.now())
	return len(c.entries)
}

// cloneJSON deep-copies a JSON-shaped value. Everything the cache and the
// item folders hold came from decoded JSON, so a round trip is a faithful copy.
func cloneJSON[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// ── tool normalization ──────────────────────────────────────────────────────

// normalizeResponsesTools folds the caller's tool declarations onto the chat
// vocabulary. It returns the chat tools, the chat-name bindings, and the
// reverse index used to translate a tool call back to its Responses name.
func normalizeResponsesTools(tools any, inherited map[string]responsesBinding) ([]any, map[string]responsesBinding, map[bindingKey]string, error) {
	bindings := map[string]responsesBinding{}
	reverse := map[bindingKey]string{}
	if tools == nil {
		return nil, bindings, reverse, nil
	}
	list, ok := tools.([]any)
	if !ok {
		return nil, nil, nil, anthropicErr("tools must be an array", "tools")
	}
	for name, b := range inherited {
		bindings[name] = b
		reverse[b.key()] = name
	}
	used := map[string]bool{}
	for name := range bindings {
		used[name] = true
	}
	inheritKey := map[bindingKey]string{}
	for name, b := range bindings {
		if _, ok := inheritKey[b.key()]; !ok {
			inheritKey[b.key()] = name
		}
	}

	chatTools := make([]any, 0, len(list))
	add := func(responseType, name string, tool map[string]any, namespace, execution string) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return anthropicErr("tool name must be non-empty", "tools")
		}
		base := name
		if namespace != "" {
			base = namespace + "__" + name
		}
		key := bindingKey{responseType, namespace, name}
		chatName, ok := inheritKey[key]
		if !ok {
			chatName = uniqueChatName(base, used)
		}
		var chatTool map[string]any
		if responseType == "custom" {
			chatTool = customChatTool(tool, chatName)
		} else {
			chatTool = functionChatTool(tool, chatName)
		}
		chatTools = append(chatTools, chatTool)
		bindings[chatName] = responsesBinding{
			ChatName: chatName, ResponseType: responseType, Name: name,
			Namespace: namespace, Execution: execution,
		}
		reverse[key] = chatName
		return nil
	}

	for i, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, nil, anthropicErr(fmt.Sprintf("tools.%d must be an object", i), fmt.Sprintf("tools.%d", i))
		}
		switch strings.ToLower(stringField(tool, "type")) {
		case "function":
			flat := tool
			if nested, ok := tool["function"].(map[string]any); ok {
				// Accept the chat-shaped nesting too.
				flat = map[string]any{"strict": tool["strict"]}
				for _, k := range []string{"name", "description", "parameters", "strict"} {
					if v, ok := nested[k]; ok {
						flat[k] = v
					}
				}
			}
			if err := add("function", stringField(flat, "name"), flat, "", ""); err != nil {
				return nil, nil, nil, err
			}
		case "custom":
			if err := add("custom", stringField(tool, "name"), tool, "", ""); err != nil {
				return nil, nil, nil, err
			}
		case "namespace":
			namespace := stringField(tool, "name")
			nested, _ := tool["tools"].([]any)
			if namespace == "" || nested == nil {
				return nil, nil, nil, anthropicErr(fmt.Sprintf("tools.%d namespace requires name and tools", i), fmt.Sprintf("tools.%d", i))
			}
			for j, rawNested := range nested {
				nt, ok := rawNested.(map[string]any)
				if !ok {
					return nil, nil, nil, anthropicErr(fmt.Sprintf("tools.%d.tools.%d must be an object", i, j), fmt.Sprintf("tools.%d.tools.%d", i, j))
				}
				ntype := strings.ToLower(stringField(nt, "type"))
				if ntype == "" {
					ntype = "function"
				}
				if ntype != "function" && ntype != "custom" {
					continue
				}
				if err := add(ntype, stringField(nt, "name"), nt, namespace, ""); err != nil {
					return nil, nil, nil, err
				}
			}
		case "tool_search":
			if strings.EqualFold(stringField(tool, "execution"), "server") {
				continue
			}
			synthetic := map[string]any{}
			for k, v := range tool {
				synthetic[k] = v
			}
			if stringField(synthetic, "name") == "" {
				synthetic["name"] = "tool_search"
			}
			if stringField(synthetic, "description") == "" {
				synthetic["description"] = "Search for deferred client tools."
			}
			if synthetic["parameters"] == nil {
				synthetic["parameters"] = map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": map[string]any{"type": "string"}},
					"required":   []any{"query"},
				}
			}
			execution := stringField(tool, "execution")
			if execution == "" {
				execution = "client"
			}
			if err := add("tool_search", "tool_search", synthetic, "", execution); err != nil {
				return nil, nil, nil, err
			}
		}
		// Server-hosted tools cannot run here and are dropped.
	}
	return chatTools, bindings, reverse, nil
}

var chatNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// uniqueChatName sanitizes a Responses tool name into one the chat surface
// accepts, and keeps it unique within the request.
func uniqueChatName(base string, used map[string]bool) string {
	name := strings.Trim(chatNameUnsafe.ReplaceAllString(base, "_"), "_")
	if name == "" {
		name = "tool"
	}
	if len(name) > 64 {
		sum := sha1.Sum([]byte(name))
		name = name[:53] + "_" + hex.EncodeToString(sum[:])[:10]
	}
	candidate := name
	for counter := 2; used[candidate]; counter++ {
		suffix := "_" + strconv.Itoa(counter)
		candidate = name[:min(len(name), 64-len(suffix))] + suffix
	}
	used[candidate] = true
	return candidate
}

func functionChatTool(tool map[string]any, chatName string) map[string]any {
	function := map[string]any{
		"name":        chatName,
		"description": stringField(tool, "description"),
		"parameters":  tool["parameters"],
	}
	if function["parameters"] == nil {
		function["parameters"] = map[string]any{}
	}
	if v, ok := tool["strict"]; ok {
		function["strict"] = v
	}
	return map[string]any{"type": "function", "function": function}
}

// customChatTool degrades a free-form custom tool into a function that takes the
// caller's free text in a single "input" field.
func customChatTool(tool map[string]any, chatName string) map[string]any {
	description := strings.TrimSpace(stringField(tool, "description"))
	if tool["format"] != nil {
		description += "\nReturn the custom tool input as the string field named input."
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        chatName,
			"description": description,
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"input": map[string]any{"type": "string"}},
				"required":             []any{"input"},
				"additionalProperties": false,
			},
		},
	}
}

// ── reasoning presentation ──────────────────────────────────────────────────

// reasoningRequested reports whether the caller explicitly opted into a visible
// reasoning channel. The upstream produces reasoning whether or not it was
// asked, so silence means "keep it private"; reasoning_effort only tunes
// generation and must not leak the chain.
func reasoningRequested(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on", "enabled", "enable", "summary":
			return true
		}
		return false
	case map[string]any:
		for _, key := range []string{"thinking", "include_reasoning", "enabled", "enable"} {
			if nested, ok := v[key]; ok {
				return reasoningRequested(nested)
			}
		}
		mode, _ := v["type"].(string)
		if mode == "" {
			mode, _ = v["mode"].(string)
		}
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case "1", "true", "yes", "on", "enabled", "enable", "thinking", "reasoning", "summary", "summarized":
			return true
		}
		return false
	}
	return false
}

// compactReasoningSummary bounds the reasoning a client sees: at most 6 unique
// lines and 800 characters.
func compactReasoningSummary(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	var lines []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, "\n") {
		line := strings.Join(strings.Fields(raw), " ")
		if line == "" {
			continue
		}
		fingerprint := strings.ToLower(line)
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		lines = append(lines, line)
	}
	if len(lines) > 6 {
		lines = append(lines[:5], lines[len(lines)-1])
	}
	summary := strings.Join(lines, "\n")
	if len(summary) > 800 {
		summary = strings.TrimRight(summary[:797], " ") + "..."
	}
	return summary
}
