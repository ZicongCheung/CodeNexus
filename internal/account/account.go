// Package account defines the unified account model shared by the pool and the
// upstream adapters. Persistence lives in internal/authstore.
//
// Credential fields may be rewritten by a background token refresh, so they are
// guarded by an unexported mutex and reached through accessors. Identity fields
// (source, uid, label) are immutable after creation and may be read directly.
package account

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Source identifies which upstream an account belongs to.
type Source string

const (
	// SourceWorkBuddy covers the WorkBuddy / CodeBuddy family.
	SourceWorkBuddy Source = "workbuddy"
	// SourceTraeCN covers Trae CN.
	SourceTraeCN Source = "trae-cn"
)

// Account is the source-agnostic account record persisted under auth_dir.
type Account struct {
	Source  Source `json:"source"`
	UID     string `json:"uid"`
	Label   string `json:"label,omitempty"`
	Enabled bool   `json:"enabled"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Domain       string `json:"domain,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`

	// Ephemeral marks an account that must not be written to disk (for example
	// one injected through the environment).
	Ephemeral bool `json:"-"`

	mu sync.RWMutex
}

// Key returns a stable identifier across sources.
func (a *Account) Key() string { return string(a.Source) + ":" + a.UID }

// FileName returns the on-disk name for the account, namespaced by source.
func (a *Account) FileName() string {
	return fmt.Sprintf("%s-%s.json", a.Source, sanitize(a.UID))
}

// Token returns the current access token.
func (a *Account) Token() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue returns the current refresh token.
func (a *Account) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// DomainValue returns the current account domain.
func (a *Account) DomainValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Domain
}

// ExpiresAtValue returns the access token expiry as a unix timestamp (0 when
// unknown).
func (a *Account) ExpiresAtValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// NeedsRefresh reports whether the access token expires within the given window
// (or has no known expiry, in which case refreshing is left to the caller).
func (a *Account) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" {
		return true
	}
	if a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// SetCredentials atomically replaces the credential fields. Empty values leave
// the corresponding field unchanged, except expiry which is only updated when
// expiresAt is positive.
func (a *Account) SetCredentials(access, refresh, domain string, expiresAt int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = access
	}
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if domain != "" {
		a.Domain = domain
	}
	if expiresAt > 0 {
		a.ExpiresAt = expiresAt
	}
}

// Snapshot returns a copy with a consistent view of the credential fields. The
// copy carries no lock and is safe to serialize or compare.
func (a *Account) Snapshot() Account {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return Account{
		Source:       a.Source,
		UID:          a.UID,
		Label:        a.Label,
		Enabled:      a.Enabled,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		Domain:       a.Domain,
		ExpiresAt:    a.ExpiresAt,
		Ephemeral:    a.Ephemeral,
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
