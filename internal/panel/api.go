package panel

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func sourceNames() []string {
	srcs := upstream.Sources()
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, string(s))
	}
	return out
}

// onboardingModes reports which sources support which way of adding an account,
// so the console can offer only the flows that will actually work.
func onboardingModes() (device, manual []string) {
	device = []string{}
	manual = []string{}
	for _, s := range upstream.Sources() {
		u, ok := upstream.Get(s)
		if !ok {
			continue
		}
		if _, ok := u.(upstream.DeviceAuth); ok {
			device = append(device, string(s))
		}
		if _, ok := u.(upstream.CredentialAcceptor); ok {
			manual = append(manual, string(s))
		}
	}
	return device, manual
}

// localSources lists the sources able to import an account that is already
// signed in on this machine.
func localSources() []string {
	out := []string{}
	for _, s := range upstream.Sources() {
		u, ok := upstream.Get(s)
		if !ok {
			continue
		}
		if _, ok := u.(upstream.LocalDiscoverer); ok {
			out = append(out, string(s))
		}
	}
	return out
}

// handleAuthLocal imports every usable credential found in the local desktop
// client's store. It is one explicit action rather than a background scan:
// reading another application's session should be something the user asked for.
func (c *Console) handleAuthLocal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	_ = decodeJSON(r, &req)
	src := account.Source(strings.TrimSpace(req.Source))
	if src == "" {
		src = account.SourceTraeCN
	}
	u, ok := upstream.Get(src)
	if !ok {
		writeErr(w, http.StatusBadRequest, "source not registered: "+string(src))
		return
	}
	discoverer, ok := u.(upstream.LocalDiscoverer)
	if !ok {
		writeErr(w, http.StatusBadRequest, "source does not support importing from this machine: "+string(src))
		return
	}
	accounts, err := discoverer.DiscoverLocal()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	added := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		if err := c.persist(acc); err != nil {
			writeErr(w, http.StatusInternalServerError, "save credential: "+err.Error())
			return
		}
		added = append(added, map[string]any{
			"uid":     acc.UID,
			"label":   acc.Label,
			"expires": acc.ExpiresAtValue(),
		})
	}
	c.warn("console: imported %d account(s) from the local %s client", len(added), src)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"source":   string(src),
		"imported": added,
	})
}

// accountView is the console's view of an account: the pool's status plus the
// one derived fact the pool cannot know on its own, whether today's check-in is
// done. The pool stores the day of the last successful check-in; "today" is the
// schedule's, not the host clock's.
type accountView struct {
	pool.AccountStatus
	CheckinDone bool `json:"checkin_done"`
	// CheckinApplicable is false for a source with no check-in programme, so
	// the console can say "not applicable" instead of implying a failure.
	CheckinApplicable bool `json:"checkin_applicable"`
}

// accountViews lists the pool enriched with today's check-in state.
func (c *Console) accountViews() []accountView {
	if c.pool == nil {
		return nil
	}
	statuses := c.pool.Describe()
	out := make([]accountView, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, accountView{
			AccountStatus:     st,
			CheckinDone:       c.checkinDone(st.Key),
			CheckinApplicable: c.checkinApplicable(st.Key),
		})
	}
	return out
}

// checkinDone reports whether today's check-in has completed for the account
// holding the given pool key.
func (c *Console) checkinDone(key string) bool {
	if c.sched == nil || c.pool == nil {
		return false
	}
	day := c.pool.CheckinDay(key)
	return day != "" && day == c.sched.LocalDay()
}

// checkinApplicable reports whether the account has a check-in programme at
// all, so the console can say "not applicable" instead of implying a failure.
// It asks the scheduler, which is the same resolver the sweep itself uses.
func (c *Console) checkinApplicable(key string) bool {
	if c.sched == nil || c.pool == nil {
		return false
	}
	acc, ok := c.pool.Get(key)
	if !ok || acc == nil {
		return false
	}
	return c.sched.CheckinScope(acc).Checkin
}

// handleAccounts lists the pool with runtime health, without credential data.
func (c *Console) handleAccounts(w http.ResponseWriter, _ *http.Request) {
	if c.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": c.accountViews()})
}

// handleRemoveAccount drops one account from the pool and from disk.
func (c *Console) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeErr(w, http.StatusBadRequest, "field 'key' is required")
		return
	}
	if c.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "account pool unavailable")
		return
	}
	acc, ok := c.pool.Get(req.Key)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown account")
		return
	}
	if c.store != nil && !acc.Ephemeral {
		if err := c.store.Delete(acc); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete credential: "+err.Error())
			return
		}
	}
	c.pool.Remove(req.Key)
	c.warn("console: removed account %s", req.Key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": req.Key})
}

// handleAuthStart begins an interactive device authorization.
func (c *Console) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Realm  string `json:"realm"`
	}
	_ = decodeJSON(r, &req)
	src := account.Source(strings.TrimSpace(req.Source))
	if src == "" {
		src = account.SourceWorkBuddy
	}
	auth, err := c.deviceAuth(src)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	realm := strings.TrimSpace(req.Realm)
	if realm == "" {
		realm = "cn"
	}
	code, err := auth.DeviceStart(r.Context(), realm)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "device authorization start failed: "+err.Error())
		return
	}
	c.mu.Lock()
	c.dropExpiredLoginsLocked()
	c.logins[code.State] = loginSession{realm: realm, source: src, created: time.Now()}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"state":  code.State,
		"url":    code.URL,
		"realm":  realm,
		"source": string(src),
	})
}

// handleAuthPoll checks whether an authorization has completed.
func (c *Console) handleAuthPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	c.mu.Lock()
	sess, ok := c.logins[state]
	c.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown or expired authorization; start again")
		return
	}
	src := sess.source
	if src == "" {
		src = account.SourceWorkBuddy
	}
	auth, err := c.deviceAuth(src)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	acc, done, err := auth.DevicePoll(r.Context(), sess.realm, state)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": false})
		return
	}
	c.mu.Lock()
	delete(c.logins, state)
	c.mu.Unlock()

	if err := c.persist(acc); err != nil {
		writeErr(w, http.StatusInternalServerError, "save credential: "+err.Error())
		return
	}
	c.warn("console: account added uid=%s label=%q", acc.UID, acc.Label)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"done":    true,
		"uid":     acc.UID,
		"label":   acc.Label,
		"source":  string(acc.Source),
		"realm":   sess.realm,
		"expires": acc.ExpiresAtValue(),
	})
}

// authTokenRequest is a manually pasted credential. A user can supply a
// long-lived token, a refresh token, or both; the adapter fills in the gaps.
type authTokenRequest struct {
	Source       string `json:"source"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	UID          string `json:"uid"`
	Label        string `json:"label"`
}

// handleAuthToken onboards an account from a pasted credential, for sources
// whose authorization cannot be run in a browser popup.
func (c *Console) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req authTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	src := account.Source(strings.TrimSpace(req.Source))
	if src == "" {
		src = account.SourceWorkBuddy
	}
	acc, err := c.acceptCredential(src, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := c.persist(acc); err != nil {
		writeErr(w, http.StatusInternalServerError, "save credential: "+err.Error())
		return
	}
	c.warn("console: account added uid=%s label=%q source=%s", acc.UID, acc.Label, acc.Source)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     acc.UID,
		"label":   acc.Label,
		"source":  string(acc.Source),
		"expires": acc.ExpiresAtValue(),
	})
}

// acceptCredential routes a pasted credential to the adapter that understands
// it. Only sources implementing upstream.CredentialAcceptor accept one.
func (c *Console) acceptCredential(src account.Source, req authTokenRequest) (*account.Account, error) {
	u, ok := upstream.Get(src)
	if !ok {
		return nil, errors.New("source not registered: " + string(src))
	}
	acceptor, ok := u.(upstream.CredentialAcceptor)
	if !ok {
		return nil, errors.New("source does not accept a pasted credential: " + string(src))
	}
	return acceptor.AcceptCredential(req.Token, req.RefreshToken, req.UID, req.Label)
}

func (c *Console) dropExpiredLoginsLocked() {
	for s, sess := range c.logins {
		if time.Since(sess.created) > loginTTL {
			delete(c.logins, s)
		}
	}
}

// deviceAuth returns the interactive-login adapter for a source.
func (c *Console) deviceAuth(src account.Source) (upstream.DeviceAuth, error) {
	u, ok := upstream.Get(src)
	if !ok {
		return nil, errors.New("source not registered: " + string(src))
	}
	auth, ok := u.(upstream.DeviceAuth)
	if !ok {
		return nil, errors.New("source does not support interactive login: " + string(src))
	}
	return auth, nil
}

// persist seals the account to disk and adds it to the running pool.
func (c *Console) persist(acc *account.Account) error {
	if acc == nil {
		return errors.New("panel: nil account")
	}
	acc.Enabled = true
	if c.store != nil && !acc.Ephemeral {
		if err := c.store.Save(acc); err != nil {
			return err
		}
	}
	if c.pool != nil {
		c.pool.Add(acc)
	}
	return nil
}

// importRequest accepts one of three shapes: an inline account array, a
// server-side file path, or an encrypted export bundle.
type importRequest struct {
	Accounts   []importAccount `json:"accounts"`
	Path       string          `json:"path"`
	Bundle     string          `json:"bundle"`
	Passphrase string          `json:"passphrase"`
}

// importAccount is a permissive view of an externally produced credential
// record, covering the common export shapes.
type importAccount struct {
	Source       string `json:"source"`
	UID          string `json:"uid"`
	Label        string `json:"label"`
	Nickname     string `json:"nickname"`
	Email        string `json:"email"`
	EnterpriseID string `json:"enterprise_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	Enabled      *bool  `json:"enabled"`
}

// handleImport adds accounts from an inline list, a local file, or a bundle.
func (c *Console) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var records []importAccount
	switch {
	case len(req.Accounts) > 0:
		records = req.Accounts
	case strings.TrimSpace(req.Path) != "":
		raw, err := os.ReadFile(strings.TrimSpace(req.Path))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
			return
		}
		parsed, err := parseImportFile(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		records = parsed
	case strings.TrimSpace(req.Bundle) != "":
		accs, err := decodeBundle(req.Bundle, req.Passphrase)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c.importAccounts(w, accs, nil)
		return
	default:
		writeErr(w, http.StatusBadRequest, "provide 'accounts', 'path', or 'bundle'")
		return
	}
	accs, skipped := normalizeImport(records)
	c.importAccounts(w, accs, skipped)
}

// importAccounts persists each valid account and reports a summary.
func (c *Console) importAccounts(w http.ResponseWriter, accs []*account.Account, skipped []string) {
	if len(accs) == 0 {
		writeErr(w, http.StatusBadRequest, "no importable accounts found")
		return
	}
	if skipped == nil {
		skipped = []string{}
	}
	imported := 0
	added := make([]string, 0, len(accs))
	for _, acc := range accs {
		if err := c.persist(acc); err != nil {
			skipped = append(skipped, acc.UID+": "+err.Error())
			continue
		}
		imported++
		added = append(added, string(acc.Source)+":"+acc.UID)
	}
	c.warn("console: imported %d/%d account(s)", imported, imported+len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       imported > 0,
		"total":    imported + len(skipped),
		"imported": imported,
		"skipped":  len(skipped),
		"errors":   skipped,
		"accounts": added,
	})
}

// parseImportFile accepts either a JSON array of records or a single record.
func parseImportFile(raw []byte) ([]importAccount, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("file is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []importAccount
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, errors.New("invalid JSON array: " + err.Error())
		}
		return arr, nil
	}
	var one importAccount
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, errors.New("invalid JSON object: " + err.Error())
	}
	return []importAccount{one}, nil
}

// decodeBundle accepts a raw or base64-encoded encrypted export.
func decodeBundle(bundle, passphrase string) ([]*account.Account, error) {
	if passphrase == "" {
		return nil, errors.New("bundle import requires a passphrase")
	}
	raw := []byte(strings.TrimSpace(bundle))
	if !json.Valid(raw) {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(bundle)); err == nil {
			raw = decoded
		}
	}
	return authstore.Import(raw, passphrase)
}

// normalizeImport validates and converts external records into accounts,
// returning the usable accounts and a reason for each rejected record.
func normalizeImport(records []importAccount) ([]*account.Account, []string) {
	out := make([]*account.Account, 0, len(records))
	var skipped []string
	seen := map[string]bool{}
	for i, rec := range records {
		uid := strings.TrimSpace(rec.UID)
		token := strings.TrimSpace(rec.AccessToken)
		if uid == "" || token == "" || len(uid) > 64 {
			skipped = append(skipped, fmt.Sprintf("record %d: missing or invalid uid/access_token", i+1))
			continue
		}
		src := account.Source(strings.TrimSpace(rec.Source))
		switch src {
		case account.SourceWorkBuddy, account.SourceTraeCN:
		case "":
			src = account.SourceWorkBuddy
		default:
			skipped = append(skipped, fmt.Sprintf("record %d: unknown source %q", i+1, rec.Source))
			continue
		}
		key := string(src) + ":" + uid
		if seen[key] {
			skipped = append(skipped, fmt.Sprintf("record %d: duplicate %s", i+1, key))
			continue
		}
		seen[key] = true

		label := firstNonEmpty(rec.Label, rec.Nickname, rec.Email, uid)
		enabled := true
		if rec.Enabled != nil {
			enabled = *rec.Enabled
		}
		acc := &account.Account{
			Source:       src,
			UID:          uid,
			Label:        label,
			Enabled:      enabled,
			EnterpriseID: strings.TrimSpace(rec.EnterpriseID),
		}
		acc.SetCredentials(token, strings.TrimSpace(rec.RefreshToken), strings.TrimSpace(rec.Domain), normalizeExpiry(rec))
		out = append(out, acc)
	}
	return out, skipped
}

// normalizeExpiry converts millisecond timestamps and relative lifetimes into
// an absolute unix timestamp in seconds.
func normalizeExpiry(rec importAccount) int64 {
	if rec.ExpiresAt > 0 {
		if rec.ExpiresAt > 1e12 { // milliseconds
			return rec.ExpiresAt / 1000
		}
		return rec.ExpiresAt
	}
	if rec.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(rec.ExpiresIn) * time.Second).Unix()
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("missing request body")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
