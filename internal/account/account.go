// Package account defines the unified account model shared by the pool and the
// upstream adapters. Persistence lives in internal/authstore.
package account

import (
	"fmt"
	"strings"
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
}

// Key returns a stable identifier across sources.
func (a *Account) Key() string { return string(a.Source) + ":" + a.UID }

// FileName returns the on-disk name for the account, namespaced by source.
func (a *Account) FileName() string {
	return fmt.Sprintf("%s-%s.json", a.Source, sanitize(a.UID))
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
