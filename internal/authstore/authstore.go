// Package authstore persists account credentials to disk.
//
// Credential files are written atomically with 0600 permissions inside a 0700
// directory. Envelope encryption via the OS keystore is deferred to a later
// phase; keeping a single Store seam means that change stays local.
package authstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// ErrInvalid is returned when an account is missing required fields.
var ErrInvalid = errors.New("authstore: invalid account")

// Store reads and writes credential files in a directory.
type Store struct {
	dir string
}

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir returns the credential directory.
func (s *Store) Dir() string { return s.dir }

// Load reads every credential file in the directory. A missing directory is not
// an error; malformed or invalid files are skipped.
func (s *Store) Load() ([]*account.Account, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*account.Account, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var acc account.Account
		if err := json.Unmarshal(b, &acc); err != nil {
			continue
		}
		if acc.Source == "" {
			acc.Source = sourceFromName(e.Name())
		}
		if Validate(&acc) != nil {
			continue
		}
		out = append(out, &acc)
	}
	return out, nil
}

// Save writes acc to disk atomically with 0600 permissions.
func (s *Store) Save(acc *account.Account) error {
	if err := Validate(acc); err != nil {
		return err
	}
	b, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	name := acc.FileName()
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

// Delete removes the credential file for acc.
func (s *Store) Delete(acc *account.Account) error {
	err := os.Remove(filepath.Join(s.dir, acc.FileName()))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Validate reports whether acc has the fields required to be usable.
func Validate(acc *account.Account) error {
	if acc == nil {
		return fmt.Errorf("%w: nil account", ErrInvalid)
	}
	switch acc.Source {
	case account.SourceWorkBuddy, account.SourceTraeCN:
	default:
		return fmt.Errorf("%w: unknown source %q", ErrInvalid, acc.Source)
	}
	if strings.TrimSpace(acc.UID) == "" {
		return fmt.Errorf("%w: empty uid", ErrInvalid)
	}
	return nil
}

func sourceFromName(name string) account.Source {
	switch {
	case strings.HasPrefix(name, string(account.SourceTraeCN)+"-"):
		return account.SourceTraeCN
	case strings.HasPrefix(name, string(account.SourceWorkBuddy)+"-"):
		return account.SourceWorkBuddy
	default:
		return ""
	}
}
