// Package account defines the unified account model shared by the pool and the
// upstream adapters.
package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// LoadDir reads every *.json credential file in dir. A missing directory is not
// an error; malformed files are skipped.
func LoadDir(dir string) ([]*Account, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var a Account
		if err := json.Unmarshal(b, &a); err != nil {
			continue
		}
		if a.Source == "" {
			a.Source = sourceFromName(e.Name())
		}
		if a.Source == "" || a.UID == "" {
			continue
		}
		out = append(out, &a)
	}
	return out, nil
}

func sourceFromName(name string) Source {
	switch {
	case strings.HasPrefix(name, string(SourceTraeCN)+"-"):
		return SourceTraeCN
	case strings.HasPrefix(name, string(SourceWorkBuddy)+"-"):
		return SourceWorkBuddy
	default:
		return ""
	}
}
