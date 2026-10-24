package traecn

// Daily check-in and its risk-control handling.
//
// The check-in API lives on its own host and answers with a business code
// inside an HTTP 200 body, so two rules drive this file:
//
//   - Only code 0 is a successful claim. In particular code 9095 means "this
//     device already checked in" — it is device-scoped, so the same device may
//     have claimed for a different account, and it must never be read as "this
//     account is done".
//   - Code 9074 ("too many users") is scoped to the derived device id, not the
//     account. A fresh id claims successfully, so a 9074 rotates the id once
//     and retries; a second 9074 backs off on a doubling schedule rather than
//     hammering the window, which would only extend it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Check-in endpoints and the two business codes that need special handling.
const (
	checkinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	checkinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
	entitlementPath   = "/trae/api/v2/pay/ide_user_ent_usage"

	// codeOK is the only success code.
	codeOK = 0
	// codeRateLimited is upstream "too many users, retry later". It is scoped
	// to the device id, so a rotated id often succeeds immediately.
	codeRateLimited = 9074
	// codeAlreadyOnDevice means the device already claimed. It is deliberately
	// not treated as success: only the account status endpoint can establish
	// whether this account is checked in.
	codeAlreadyOnDevice = 9095

	checkinTimeout = 30 * time.Second
	payTimeout     = 30 * time.Second
)

// The adapter opts into account maintenance without claiming a growth surface.
var _ upstream.Maintenance = (*Client)(nil)

// CheckinConfig tunes the daily check-in and its risk-control handling.
type CheckinConfig struct {
	// Enabled decides whether this source takes part in the daily sweep. Nil
	// means enabled, which is what a config written before the switch existed
	// gets — otherwise upgrading would silently stop every check-in.
	Enabled *bool
	// IntervalSeconds is the minimum spacing between two claims. The upstream
	// throttles a source by device, and every account here shares this machine,
	// so the gate is process-wide rather than per account.
	IntervalSeconds int
	// RetryAfterSeconds is the base of the 9074 backoff.
	RetryAfterSeconds int
	// MaxBackoffSeconds caps that backoff.
	MaxBackoffSeconds int
	// BackoffExponentCap bounds the doubling so a long 9074 streak settles at
	// minutes rather than parking on the ceiling: 9074 is a capacity signal,
	// not a penalty for this account.
	BackoffExponentCap int
	// RotateDeviceID lets a 9074 rotate the derived device id and retry once.
	// Nil means enabled.
	RotateDeviceID *bool
	// DeviceIDs pins a device id per identity, which disables rotation for it.
	DeviceIDs map[string]string
}

func (c CheckinConfig) normalize() CheckinConfig {
	if c.IntervalSeconds <= 0 {
		c.IntervalSeconds = 60
	}
	if c.RetryAfterSeconds <= 0 {
		c.RetryAfterSeconds = 60
	}
	if c.MaxBackoffSeconds <= 0 {
		c.MaxBackoffSeconds = 3600
	}
	if c.BackoffExponentCap < 0 {
		c.BackoffExponentCap = 0
	}
	if c.BackoffExponentCap == 0 {
		c.BackoffExponentCap = 3
	}
	if c.RotateDeviceID == nil {
		c.RotateDeviceID = boolPointer(true)
	}
	return c
}

// checkinEnabled reports whether this source joins the daily sweep.
func (c CheckinConfig) checkinEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c CheckinConfig) rotationEnabled() bool {
	return c.RotateDeviceID == nil || *c.RotateDeviceID
}

// pinnedDeviceID returns an operator-configured device id for an identity, if
// one is set. Rotating past a pinned id is not the gateway's call.
func (c CheckinConfig) pinnedDeviceID(identity, uid string) string {
	for _, key := range []string{identity, uid} {
		if key == "" {
			continue
		}
		if v := strings.TrimSpace(c.DeviceIDs[key]); v != "" {
			return v
		}
	}
	return ""
}

// checkinRecord is the persisted risk-control bookkeeping of one identity.
type checkinRecord struct {
	DeviceGeneration int   `json:"device_generation,omitempty"`
	Retry9074Count   int   `json:"retry_9074_count,omitempty"`
	RetryBackoff     int64 `json:"retry_backoff,omitempty"`
	RetryUpdatedAt   int64 `json:"retry_updated_at,omitempty"`
}

// checkinState keeps that bookkeeping, optionally backed by a file so a
// restart does not immediately reuse a device id the upstream just throttled.
type checkinState struct {
	mu      sync.Mutex
	path    string
	records map[string]checkinRecord
	loaded  bool
	// claimAt is when the next claim may be sent, shared by every account:
	// the upstream throttles a source by device, and one machine is one device
	// family. It is process-local on purpose — a restart may claim at once.
	claimAt time.Time
}

// interval returns the configured spacing between claims.
func (c CheckinConfig) interval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

// waitForClaimSlot blocks until the spacing allows a claim, returning false if
// the context ends first. A caller that is not claiming must not call it.
func (s *checkinState) waitForClaimSlot(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return ctx.Err() == nil
	}
	s.mu.Lock()
	wait := time.Until(s.claimAt)
	s.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	s.claimAt = time.Now().Add(interval)
	s.mu.Unlock()
	return true
}

func newCheckinState(path string) *checkinState {
	return &checkinState{path: path, records: map[string]checkinRecord{}}
}

// load reads the backing file once. A missing or unreadable file is not an
// error: it only costs the first claim a redundant rotation.
func (s *checkinState) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	records := map[string]checkinRecord{}
	if err := json.Unmarshal(raw, &records); err != nil {
		return
	}
	for key, record := range records {
		if record.DeviceGeneration < 0 {
			record.DeviceGeneration = 0
		}
		s.records[key] = record
	}
}

// save writes the backing file. Failures are reported to the caller, which
// logs them: losing the bookkeeping must not fail a check-in.
func (s *checkinState) save() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// get returns one identity's record.
func (s *checkinState) get(identity string) checkinRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.records[identity]
}

// update mutates one identity's record under the lock and persists it.
func (s *checkinState) update(identity string, fn func(*checkinRecord)) error {
	s.mu.Lock()
	s.load()
	record := s.records[identity]
	fn(&record)
	if record == (checkinRecord{}) {
		delete(s.records, identity)
	} else {
		s.records[identity] = record
	}
	s.mu.Unlock()
	return s.save()
}

// generation is the current device-id generation of an identity.
func (s *checkinState) generation(identity string) int {
	if identity == "" {
		return 0
	}
	return s.get(identity).DeviceGeneration
}

// rotate advances the generation, returning false when the identity is empty.
func (s *checkinState) rotate(identity string) bool {
	if identity == "" {
		return false
	}
	_ = s.update(identity, func(r *checkinRecord) { r.DeviceGeneration++ })
	return true
}

// cooldownRemaining reports how long is left of the persisted 9074 backoff.
// The deadline is stored as wall-clock time, so a restart still respects it.
func (s *checkinState) cooldownRemaining(identity string, now time.Time) time.Duration {
	record := s.get(identity)
	if record.RetryBackoff <= 0 || record.RetryUpdatedAt <= 0 {
		return 0
	}
	remaining := time.Unix(record.RetryUpdatedAt+record.RetryBackoff, 0).Sub(now)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// startCooldown records one 9074 and returns the backoff it started.
func (s *checkinState) startCooldown(identity string, cfg CheckinConfig, now time.Time) time.Duration {
	backoff := cfg.backoffFor(s.get(identity).Retry9074Count, now)
	_ = s.update(identity, func(r *checkinRecord) {
		r.Retry9074Count++
		r.RetryBackoff = int64(backoff / time.Second)
		r.RetryUpdatedAt = now.Unix()
	})
	return backoff
}

// clearRetry retires the backoff after a successful claim. The device
// generation survives: it records which id is in use, not a failure.
func (s *checkinState) clearRetry(identity string) {
	_ = s.update(identity, func(r *checkinRecord) {
		r.RetryBackoff = 0
		r.Retry9074Count = 0
	})
}

// backoffFor is the doubling schedule: base * 2^min(count, cap), clamped to
// the ceiling.
func (c CheckinConfig) backoffFor(count int, _ time.Time) time.Duration {
	base := c.RetryAfterSeconds
	if base <= 0 {
		base = 60
	}
	exponent := count
	if exponent > c.BackoffExponentCap {
		exponent = c.BackoffExponentCap
	}
	if exponent < 0 {
		exponent = 0
	}
	seconds := base << exponent
	maximum := c.MaxBackoffSeconds
	if maximum < base {
		maximum = base
	}
	if seconds > maximum {
		seconds = maximum
	}
	return time.Duration(seconds) * time.Second
}

// Scope implements upstream.Maintenance. Trae CN has a daily check-in and a
// credit balance, and none of the WorkBuddy growth families.
//
// Checkin is switchable per source: an operator who keeps Trae strictly for
// traffic can stop it from participating in the daily sweep without losing the
// balance refresh that powers the credits page.
func (c *Client) Scope(acc *account.Account) upstream.TaskScope {
	if c.cfg.UpstreamMode == CLIMode || acc == nil {
		return upstream.TaskScope{}
	}
	// The CLI placeholder has no credential, and therefore nothing to check in
	// or bill.
	if acc.Local || strings.TrimSpace(acc.Token()) == "" {
		return upstream.TaskScope{}
	}
	return upstream.TaskScope{Checkin: c.cfg.Checkin.checkinEnabled(), Balance: true}
}

// Checkin performs the daily check-in. A repeated check-in is reported as
// Already rather than as an error.
func (c *Client) Checkin(ctx context.Context, acc *account.Account) (*upstream.CheckinResult, error) {
	identity, err := c.checkinTarget(acc)
	if err != nil {
		return nil, err
	}
	// A persisted backoff is honoured before anything is sent: a 9074 window is
	// extended by retrying inside it.
	if wait := c.ck.cooldownRemaining(identity, time.Now()); wait > 0 {
		return nil, rateLimitedError(wait)
	}

	status, err := c.checkinPost(ctx, acc, checkinStatusPath)
	if err != nil {
		return nil, err
	}
	if checkedIn, _ := status["checked_in"].(bool); checkedIn {
		c.ck.clearRetry(identity)
		return &upstream.CheckinResult{Already: true, Credit: checkinCredit(status)}, nil
	}

	// Space the claims out before the first request, not after: two accounts
	// checked back to back would otherwise burn the upstream's device throttle.
	if !c.ck.waitForClaimSlot(ctx, c.cfg.Checkin.interval()) {
		return nil, upstream.Classify(ctx.Err())
	}

	claim, err := c.checkinPost(ctx, acc, checkinClaimPath)
	if err != nil {
		return nil, err
	}
	if businessCode(claim) == codeRateLimited && c.rotateDevice(identity, acc) {
		if retried, retryErr := c.checkinPost(ctx, acc, checkinClaimPath); retryErr == nil {
			claim = retried
		}
	}

	switch code := businessCode(claim); {
	case code == codeRateLimited:
		wait := c.ck.startCooldown(identity, c.cfg.Checkin, time.Now())
		return nil, rateLimitedError(wait)
	case code == codeAlreadyOnDevice:
		// Deliberately not "already checked in": the device may have claimed for
		// another account, so this account's state is still unknown.
		return nil, upstream.NewError(upstream.KindServer, 0,
			"trae: check-in refused for this device (9095); the account's own status does not confirm a claim")
	case code != codeOK:
		return nil, upstream.NewError(upstream.KindServer, 0,
			fmt.Sprintf("trae: check-in failed (code %d): %s", code, messageOf(claim)))
	}
	c.ck.clearRetry(identity)
	return &upstream.CheckinResult{Credit: checkinCredit(claim)}, nil
}

// Credits reads the account's entitlement packs and reports the merged balance.
// Trae CN pools every pack into one general credit balance, so no work/total
// split is derived.
func (c *Client) Credits(ctx context.Context, acc *account.Account, _ time.Duration) (*upstream.Credits, error) {
	if _, err := c.checkinTarget(acc); err != nil {
		return nil, err
	}
	raw, err := c.payPost(ctx, acc, entitlementPath, map[string]any{
		"require_usage": true,
		"req_source":    0,
	})
	if err != nil {
		return nil, err
	}
	// The entitlement endpoint reports failures with a business code too, and a
	// body without one is a success.
	if code := businessCode(raw); code != codeOK {
		return nil, upstream.NewError(upstream.KindServer, 0,
			fmt.Sprintf("trae: credit lookup failed (code %d): %s", code, messageOf(raw)))
	}
	if c.cfg.DebugUpstream {
		logx.Debugf("trae: entitlements %s: %s", shortUID(acc.UID), describeEntitlements(raw))
	}
	return parseEntitlements(raw), nil
}

// checkinTarget validates that an account can be checked in and returns its
// stable identity.
func (c *Client) checkinTarget(acc *account.Account) (string, error) {
	if c.cfg.UpstreamMode == CLIMode {
		return "", errors.New("trae: the local CLI has no account check-in")
	}
	if acc == nil || strings.TrimSpace(acc.Token()) == "" {
		return "", upstream.NewError(upstream.KindAuth, 0, "trae: no credential")
	}
	identity := strings.TrimSpace(TokenUserID(acc.Token()))
	if identity == "" {
		identity = strings.TrimSpace(acc.UID)
	}
	if identity == "" {
		return "", errors.New("trae: account has no usable identity for the check-in device id")
	}
	return identity, nil
}

// rotateDevice moves the identity onto a fresh device id after a 9074. It
// reports whether the retry should be attempted.
func (c *Client) rotateDevice(identity string, acc *account.Account) bool {
	if !c.cfg.Checkin.rotationEnabled() {
		return false
	}
	if c.cfg.Checkin.pinnedDeviceID(identity, acc.UID) != "" {
		// Rotating past an operator-pinned id would ignore their choice.
		return false
	}
	return c.ck.rotate(identity)
}

// checkinPost sends one check-in request and returns the decoded body.
func (c *Client) checkinPost(ctx context.Context, acc *account.Account, path string) (map[string]any, error) {
	return c.postJSON(ctx, c.cfg.UGBaseURL+path, acc, map[string]any{}, checkinTimeout, c.checkinHeaders(acc))
}

// payPost sends one entitlement request.
func (c *Client) payPost(ctx context.Context, acc *account.Account, path string, body map[string]any) (map[string]any, error) {
	return c.postJSON(ctx, c.cfg.PayBaseURL+path, acc, body, payTimeout, c.checkinHeaders(acc))
}

// postJSON performs a small JSON POST and returns the decoded object.
func (c *Client) postJSON(ctx context.Context, url string, acc *account.Account, body map[string]any, timeout time.Duration, headers map[string]string) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, upstream.Classify(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstream.ClassifyHTTP(resp.StatusCode, string(text))
	}
	var doc map[string]any
	if err := json.Unmarshal(text, &doc); err != nil {
		return nil, upstream.NewError(upstream.KindServer, resp.StatusCode,
			"trae: upstream returned invalid JSON")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, upstream.Classify(ctxErr)
	}
	return doc, nil
}

// checkinHeaders builds the headers the check-in API expects. The device id is
// derived from the account so the upstream keeps seeing one installation, and
// it is independent of the chat path's device identity.
func (c *Client) checkinHeaders(acc *account.Account) map[string]string {
	identity := strings.TrimSpace(TokenUserID(acc.Token()))
	if identity == "" {
		identity = strings.TrimSpace(acc.UID)
	}
	return map[string]string{
		"Authorization":  TokenScheme + " " + acc.Token(),
		"Content-Type":   "application/json",
		"x-device-id":    c.checkinDeviceID(identity, acc.UID),
		"x-device-brand": c.cfg.DeviceBrand,
		"x-device-type":  c.cfg.DeviceType,
	}
}

// checkinDeviceID returns the stable, account-bound 16-digit device id.
func (c *Client) checkinDeviceID(identity, uid string) string {
	if pinned := c.cfg.Checkin.pinnedDeviceID(identity, uid); pinned != "" {
		return pinned
	}
	generation := c.ck.generation(identity)
	material := identity
	if generation > 0 {
		material = fmt.Sprintf("%s#gen%d", identity, generation)
	}
	sum := sha256.Sum256([]byte(material))
	return decimalID(sum[:])
}

// businessCode reads the upstream business code. A missing code is reported as
// codeOK so a plain success body is not mistaken for an error.
func businessCode(doc map[string]any) int {
	value, ok := doc["code"]
	if !ok {
		return codeOK
	}
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return codeOK
		}
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := parseCode(v); err == nil {
			return n
		}
	}
	return codeOK
}

func parseCode(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// messageOf extracts a human-readable message from an upstream body.
func messageOf(doc map[string]any) string {
	for _, key := range []string{"message", "msg", "error"} {
		if s, ok := doc[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "no message"
}

// checkinCredit reads the credit bonus from a check-in body. A non-finite or
// non-positive value is reported as zero rather than as a real number, matching
// the rule the status endpoint documents.
func checkinCredit(doc map[string]any) int64 {
	value, ok := doc["credits"]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return 0
		}
		return int64(v)
	case json.Number:
		if n, err := v.Float64(); err == nil && n > 0 && !math.IsInf(n, 0) {
			return int64(n)
		}
	}
	return 0
}

// parseEntitlements merges the entitlement packs into one balance. Only packs
// that carry a credit limit count: a feature-flag pack has no limit and would
// otherwise look like an empty one.
func parseEntitlements(raw map[string]any) *upstream.Credits {
	out := &upstream.Credits{}
	packs, _ := raw["user_entitlement_pack_list"].([]any)
	for _, entry := range packs {
		pack, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		info, _ := pack["entitlement_base_info"].(map[string]any)
		limit, ok := creditLimitOf(info)
		if !ok {
			continue
		}
		usage, _ := pack["usage"].(map[string]any)
		amount, _ := creditNumber(usage["credits_amount"])

		switch {
		case limit < 0:
			out.Unlimited = true
		default:
			out.Total += limit
		}
		// Consumption is summed independently of the grant so a partially
		// consumed pack still reports what it used, and an unmetered pack keeps
		// its usage visible.
		out.Used += amount
		if amount > 0 {
			out.Remain -= amount
		}
	}
	out.Packs = packsOf(raw)
	if out.Unlimited {
		out.Total = 0
		out.Remain = 0
		return out
	}
	out.Remain += out.Total
	if out.Remain < 0 {
		out.Remain = 0
	}
	return out
}

// creditLimitOf reads one pack's credit grant. It is stated in one of two
// places — beside the other quota flags, or nested inside the product extra —
// and the two spellings are the same grant, so the nested copy is only
// consulted when the flat one is absent. Reading the first spelling keeps the
// totals and the per-pack detail in agreement.
func creditLimitOf(info map[string]any) (int64, bool) {
	if quota, ok := info["quota"].(map[string]any); ok {
		if n, ok := creditNumber(quota["credits_limit"]); ok {
			return n, true
		}
	}
	extra, _ := info["product_extra"].(map[string]any)
	pkg, _ := extra["package_extra"].(map[string]any)
	quota, _ := pkg["quota"].(map[string]any)
	if n, ok := creditNumber(quota["credits_limit"]); ok {
		return n, true
	}
	return 0, false
}

// packsOf reads the per-batch breakdown the entitlement response carries.
//
// The batches are what make "when do these credits go away?" answerable — the
// balance is a sum over packs that lapse on different dates, and the sum cannot
// be taken apart again. The list is reported as the upstream stated it: a pack
// with no lapse time keeps a zero, because inventing a date is worse than
// leaving the console to say it does not know.
//
// The response also carries feature entitlements (Solo capability flags) that
// grant no credits; they are skipped rather than shown as empty packs.
//
// Note that these detail fields are for display only. Routing reads expiring
// credits from pool.SetCreditsDetail, which an explicit balance probe writes,
// and this parser must not quietly become a second writer of routing state.
func packsOf(raw map[string]any) []upstream.CreditPack {
	entries, _ := raw["user_entitlement_pack_list"].([]any)
	out := make([]upstream.CreditPack, 0, len(entries))
	for _, entry := range entries {
		pack, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		info, _ := pack["entitlement_base_info"].(map[string]any)
		limit, ok := creditLimitOf(info)
		if !ok {
			continue
		}
		usage, _ := pack["usage"].(map[string]any)
		used, _ := creditNumber(usage["credits_amount"])
		if limit == 0 && used == 0 {
			// A pack that neither grants nor has consumed anything is a
			// container, not a batch worth listing.
			continue
		}
		item := upstream.CreditPack{
			// The pack's human name is the upstream's own description of the
			// grant ("每月登录赠送"), which is what makes two 150-credit packs
			// distinguishable in the list.
			Name: firstString(pack, "display_desc", "displayDesc", "name", "title"),
			Used: used,
		}
		if item.Name == "" {
			item.Name = firstString(info, "name", "title", "package_name", "entitlement_name")
		}
		// An unmetered grant states a negative limit; reporting it as a size
		// would put a nonsense negative on the console, so it is left at zero
		// and only its consumption shows.
		if limit > 0 {
			item.Size = limit
			item.Remain = limit - used
			if item.Remain < 0 {
				item.Remain = 0
			}
		}
		if at, text, ok := packExpiry(info, pack); ok {
			item.ExpiresAt = at
			item.ExpiresText = text
		}
		out = append(out, item)
	}
	return out
}

// packExpiryKeys are the lapse-time spellings the pack list carries, most
// specific first. The deduction window is the one that matters — it is when
// unspent credits actually become unusable — so the looser package and cycle
// end times are only reached when it is absent, and a cycle boundary is a
// weaker claim than a deduction end.
var packExpiryKeys = []string{
	"deduction_end_time", "deductionEndTime",
	"expired_time", "expiredTime",
	"expire_time", "expireTime",
	"package_end_time", "packageEndTime",
	"cycle_end_time", "cycleEndTime",
	"end_time", "endTime",
}

// packExpiry finds a pack's lapse time, searching the containers it has been
// seen under. It reports the Unix seconds and the upstream's own text so the
// console can show both a sortable value and the string a human can check.
func packExpiry(containers ...map[string]any) (int64, string, bool) {
	for _, key := range packExpiryKeys {
		for _, c := range containers {
			if c == nil {
				continue
			}
			raw, present := c[key]
			if !present || raw == nil {
				continue
			}
			if at, ok := packTime(raw); ok {
				return at, packTimeText(at), true
			}
		}
	}
	return 0, "", false
}

// packTime reads a lapse time in any of the shapes the endpoint uses: a Unix
// count in seconds or milliseconds, or a wall-clock string. A stray small
// number is rejected rather than read as 1970, so a misidentified key degrades
// to "no expiry" instead of a wrong one.
func packTime(v any) (int64, bool) {
	switch t := v.(type) {
	case string:
		text := strings.TrimSpace(t)
		if text == "" {
			return 0, false
		}
		for _, layout := range packTimeLayouts {
			if parsed, err := time.ParseInLocation(layout, text, time.UTC); err == nil {
				return parsed.Unix(), true
			}
		}
		if n, ok := creditNumber(text); ok && n > 0 {
			return normalizeEpoch(n), true
		}
		return 0, false
	default:
		n, ok := creditNumber(v)
		if !ok || n <= 0 {
			return 0, false
		}
		return normalizeEpoch(n), true
	}
}

// packTimeLayouts are the wall-clock shapes the upstream has used. UTC is the
// assumed zone: these times are displayed rather than compared, and guessing a
// local zone would shift them for no benefit.
var packTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05.000",
	"2006/01/02 15:04:05",
	"2006-01-02",
}

// normalizeEpoch folds a millisecond count onto seconds: the same endpoint has
// shipped both, and a millisecond value read as seconds lands in the year
// 56000, which is unmistakable but still wrong.
func normalizeEpoch(n int64) int64 {
	if n > 1e12 {
		return n / 1000
	}
	return n
}

func packTimeText(at int64) string {
	return time.Unix(at, 0).UTC().Format(time.RFC3339)
}

// describeEntitlements summarizes the raw entitlement response for the debug
// log. Without it, "does this account's grant actually itemise its packs, and
// under which keys?" is unanswerable from the outside, and the credit view
// would have to guess at field names that may not exist.
func describeEntitlements(raw map[string]any) string {
	entries, _ := raw["user_entitlement_pack_list"].([]any)
	var b strings.Builder
	fmt.Fprintf(&b, "packs=%d", len(entries))
	if len(entries) == 0 {
		return b.String()
	}
	pack, ok := entries[0].(map[string]any)
	if !ok {
		return b.String()
	}
	fmt.Fprintf(&b, " packKeys=%v", sortedKeys(pack))
	if info, ok := pack["entitlement_base_info"].(map[string]any); ok {
		fmt.Fprintf(&b, " baseKeys=%v", sortedKeys(info))
		if quota, ok := info["quota"].(map[string]any); ok {
			fmt.Fprintf(&b, " quotaKeys=%v", sortedKeys(quota))
		}
	}
	if usage, ok := pack["usage"].(map[string]any); ok {
		fmt.Fprintf(&b, " usageKeys=%v", sortedKeys(usage))
	}
	// The first credit-bearing pack is the interesting one: the list also holds
	// feature entitlements whose quota carries only capability flags.
	for _, entry := range entries {
		p, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		info, _ := p["entitlement_base_info"].(map[string]any)
		quota, _ := info["quota"].(map[string]any)
		if _, ok := quota["credits_limit"]; !ok {
			continue
		}
		fmt.Fprintf(&b, " creditPack: display_desc=%q source_id=%v product_type=%v product_id=%v expire_time=%v yearly_expire_time=%v end_time=%v usage=%v product_extra=%s",
			truncate(firstString(p, "display_desc"), 40),
			p["source_id"], info["product_type"], info["product_id"],
			p["expire_time"], p["yearly_expire_time"], info["end_time"],
			p["usage"], truncate(fmt.Sprint(info["product_extra"]), 160))
		break
	}
	return b.String()
}

// truncate shortens a diagnostic value so one log line stays readable.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// creditNumber reads a non-negative credit amount. A boolean is rejected
// explicitly, because JSON true would otherwise convert to 1.
func creditNumber(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		n, err := v.Float64()
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int64(n), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

// rateLimitedError reports a 9074 as a retryable, rate-limited failure so the
// scheduler backs off instead of treating it as a hard error.
func rateLimitedError(wait time.Duration) *upstream.Error {
	err := upstream.NewError(upstream.KindRateLimited, 0,
		fmt.Sprintf("trae: check-in rate limited (9074); retry after %s", wait.Round(time.Second)))
	err.RetryAfter = wait
	return err
}

func boolPointer(v bool) *bool { return &v }
