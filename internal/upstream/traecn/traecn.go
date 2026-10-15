package traecn

import (
	"context"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// emitTracker records whether anything actually reached the client. It is what
// separates "the attempt failed" from "the attempt started answering and then
// failed": only the former may be retried, on this account or another.
type emitTracker struct {
	emit    upstream.EmitFunc
	emitted bool
}

func (t *emitTracker) write(d upstream.StreamDelta) error {
	if d.Content == "" && d.ReasoningContent == "" && len(d.ToolCalls) == 0 && d.Usage == nil {
		return nil
	}
	t.emitted = true
	if t.emit == nil {
		return nil
	}
	return t.emit(d)
}

// Chat implements upstream.Upstream. The upstream only streams, so a
// non-streaming request is assembled locally.
func (c *Client) Chat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	t := &emitTracker{}
	resp, err := c.chat(ctx, acc, req, t)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ChatStream implements upstream.Upstream.
func (c *Client) ChatStream(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	t := &emitTracker{emit: emit}
	_, err := c.chat(ctx, acc, req, t)
	return err
}

// chat routes a request to its surface and applies the one fallback the
// upstream needs: a model the agent surface refuses is usually served by the
// remote surface, so the request is retried there once, on the same account and
// only while nothing has been emitted yet.
func (c *Client) chat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, t *emitTracker) (*upstream.ChatResponse, error) {
	if req == nil {
		return nil, upstream.NewError(upstream.KindBadRequest, 0, "trae: empty request")
	}
	if acc == nil || strings.TrimSpace(acc.Token()) == "" {
		return nil, upstream.NewError(upstream.KindAuth, 0, "trae: no credential")
	}

	switch c.mode(req) {
	case "ide", "solo":
		resp, err := c.agentChat(ctx, acc, req, t.write)
		if err == nil {
			return resp, nil
		}
		if t.emitted || !upstream.Classify(err).Retryable() {
			return resp, err
		}
		fallback, ferr := c.remoteChat(ctx, acc, req, t.write)
		if ferr == nil {
			return fallback, nil
		}
		// Report the original failure: it is the one that explains why the
		// preferred surface was not used.
		return nil, err
	default:
		return c.remoteChat(ctx, acc, req, t.write)
	}
}
