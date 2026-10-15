package traecn

// Per-turn credit settlement.
//
// The upstream bills a turn asynchronously: right after an answer there is
// often no charge to read yet, and it appears a moment later. Settlement is
// therefore a short poll of the commercial usage API, keyed on the turn id the
// conversation carried (the id of the user message that started it).
//
// Two rules keep this honest:
//
//   - A settled charge of zero is a value, not an absence. Only "no field at
//     all" means the upstream has not settled yet, and that is reported as an
//     error so the caller can mark the turn pending rather than free.
//   - Settlement never rewrites the account's remaining credit. A charge is not
//     a balance, and the balance endpoint is the only authority on that.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// sessionUsagePath is the commercial usage endpoint.
const sessionUsagePath = "/api/v1/commercial/get_session_usage"

// CreditsSourceSessionUsage marks a charge read from the per-turn API.
const CreditsSourceSessionUsage = "session_usage"

// sessionUsageTimeout bounds one lookup attempt.
const sessionUsageTimeout = 15 * time.Second

// sessionUsageSettle is the pause before the first poll. The upstream bills a
// moment after the answer ends, so opening with this window avoids a first
// attempt that is almost always too early.
var sessionUsageSettle = time.Second

// sessionUsageDelays is the polling schedule. The first attempt is immediate
// because the upstream often has the number by the time the answer ends, and
// the later attempts cover the slow path.
var sessionUsageDelays = []time.Duration{
	0,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	4 * time.Second,
}

// sessionCreditKeys are the per-turn charge field names, in priority order.
var sessionCreditKeys = []string{
	"credits_float",
	"creditsFloat",
	"credits_consumed",
	"consumed_credits",
	"credit_cost",
	"credits_cost",
	"amount_float",
	"amountFloat",
}

// sessionContainers are the billing-shaped wrappers the endpoint has shipped.
// They are searched before a generic walk so unrelated metadata cannot mask the
// session charge.
var sessionContainers = []string{
	"user_usage_group_by_session",
	"userUsageGroupBySession",
	"usage",
	"billing",
	"cost",
	"result",
	"data",
}

// TurnCredits implements upstream.TurnUsage. It polls until the upstream has
// settled the turn, and reports an error while it has not.
func (c *Client) TurnCredits(ctx context.Context, acc *account.Account, turnID string) (float64, string, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return 0, "", errors.New("trae: no turn id to settle")
	}
	if _, err := c.checkinTarget(acc); err != nil {
		return 0, "", err
	}
	if !waitCtx(ctx, sessionUsageSettle) {
		return 0, "", ctx.Err()
	}
	var lastErr error
	for _, delay := range sessionUsageDelays {
		if !waitCtx(ctx, delay) {
			return 0, "", ctx.Err()
		}
		credits, settled, err := c.sessionUsageOnce(ctx, acc, turnID)
		if err != nil {
			return 0, "", err
		}
		if settled {
			return credits, CreditsSourceSessionUsage, nil
		}
		lastErr = errors.New("trae: the upstream has not settled this turn yet")
	}
	if lastErr == nil {
		lastErr = errors.New("trae: no attempt was made")
	}
	return 0, "", lastErr
}

// UsedCredits implements upstream.TurnUsage. It reads the account's cumulative
// consumption straight from the adapter, which is the same number the balance
// refresh reads. It is deliberately not routed through the pool: a settlement
// is an observation, and writing it back would let a probe move an account's
// routing weight or lift a cooldown.
func (c *Client) UsedCredits(ctx context.Context, acc *account.Account) (float64, bool) {
	cr, err := c.Credits(ctx, acc, 0)
	if err != nil || cr == nil || cr.Unlimited || cr.Used < 0 {
		return 0, false
	}
	return float64(cr.Used), true
}

// sessionUsageOnce performs one lookup. It reports settled=false when the
// response carried no charge field at all.
func (c *Client) sessionUsageOnce(ctx context.Context, acc *account.Account, turnID string) (float64, bool, error) {
	doc, err := c.postJSON(ctx, c.cfg.UsageBaseURL+sessionUsagePath, acc,
		map[string]any{"session_id": turnID}, sessionUsageTimeout,
		map[string]string{
			"Authorization": TokenScheme + " " + acc.Token(),
			"Content-Type":  "application/json",
		})
	if err != nil {
		return 0, false, err
	}
	if code := businessCode(doc); code != codeOK {
		return 0, false, upstream.NewError(upstream.KindServer, 0,
			fmt.Sprintf("trae: session usage failed (code %d): %s", code, messageOf(doc)))
	}
	payload := any(doc)
	if data, ok := doc["data"]; ok {
		payload = data
	}
	credits, ok := findSessionCredit(payload, 0)
	if !ok {
		return 0, false, nil
	}
	return credits, true, nil
}

// findSessionCredit walks a billing-shaped payload for the first credit field.
// A negative value is discarded: a charge cannot be negative, so it is a
// container that happened to carry an unrelated number.
func findSessionCredit(value any, depth int) (float64, bool) {
	if depth > 6 {
		return 0, false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, key := range sessionCreditKeys {
			raw, present := v[key]
			if !present {
				continue
			}
			if n, ok := creditFloat(raw); ok && n >= 0 {
				return n, true
			}
		}
		for _, key := range sessionContainers {
			child, present := v[key]
			if !present {
				continue
			}
			if n, ok := findSessionCredit(child, depth+1); ok {
				return n, true
			}
		}
		for _, child := range v {
			if n, ok := findSessionCredit(child, depth+1); ok {
				return n, true
			}
		}
	case []any:
		for _, child := range v {
			if n, ok := findSessionCredit(child, depth+1); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// creditFloat reads a credit amount, accepting the numeric shapes a JSON body
// can produce. A boolean is rejected: JSON true would otherwise read as 1.
func creditFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case json.Number:
		n, err := v.Float64()
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int64:
		return float64(v), true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// waitCtx waits out a polling delay, reporting false when the context ends
// first. A zero delay returns immediately.
func waitCtx(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
