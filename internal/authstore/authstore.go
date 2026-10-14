// Package authstore persists account credentials to disk.
//
// Files are written atomically with 0600 permissions inside a 0700 directory.
// When a crypto.Keeper is supplied the payload is sealed with envelope
// encryption (see internal/crypto); otherwise it is stored as plain JSON for
// tests and for reading pre-encryption files. Legacy plaintext files are
// upgraded transparently by Migrate.
package authstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/crypto"
)

// ErrInvalid is returned when an account is missing required fields.
var ErrInvalid = errors.New("authstore: invalid account")

// encKey is the envelope marker inside an encrypted credential file.
const encKey = "codenexus_encrypted"

// Store reads and writes credential files in a directory.
type Store struct {
	dir    string
	keeper crypto.Keeper
	logf   func(string, ...any)
}

// Option customizes a Store.
type Option func(*Store)

// WithKeeper enables envelope encryption using k.
func WithKeeper(k crypto.Keeper) Option { return func(s *Store) { s.keeper = k } }

// WithLogger installs a warning logger.
func WithLogger(fn func(string, ...any)) Option { return func(s *Store) { s.logf = fn } }

// New returns a store rooted at dir.
func New(dir string, opts ...Option) *Store {
	s := &Store{dir: dir}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// Dir returns the credential directory.
func (s *Store) Dir() string { return s.dir }

// Encrypted reports whether the store seals credentials at rest.
func (s *Store) Encrypted() bool { return s.keeper != nil }

func (s *Store) warn(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Load reads every credential file in the directory. A missing directory is not
// an error; malformed or invalid files are skipped with a warning.
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
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			s.warn("authstore: read %s: %v", e.Name(), err)
			continue
		}
		acc, _, err := s.decode(raw)
		if err != nil {
			s.warn("authstore: decode %s: %v", e.Name(), err)
			continue
		}
		if acc.Source == "" {
			acc.Source = sourceFromName(e.Name())
		}
		if Validate(acc) != nil {
			s.warn("authstore: skip invalid %s", e.Name())
			continue
		}
		out = append(out, acc)
	}
	return out, nil
}

// Save writes acc to disk atomically.
func (s *Store) Save(acc *account.Account) error {
	if err := Validate(acc); err != nil {
		return err
	}
	return s.saveTo(filepath.Join(s.dir, acc.FileName()), acc)
}

// Migrate re-seals any legacy plaintext credential files in place and returns
// how many were upgraded. It is a no-op without a keeper.
func (s *Store) Migrate() (int, error) {
	if s.keeper == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	migrated := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		acc, encrypted, err := s.decode(raw)
		if err != nil || encrypted {
			continue
		}
		if acc.Source == "" {
			acc.Source = sourceFromName(e.Name())
		}
		if Validate(acc) != nil {
			continue
		}
		if err := s.saveTo(path, acc); err != nil {
			s.warn("authstore: migrate %s: %v", e.Name(), err)
			continue
		}
		migrated++
	}
	return migrated, nil
}

// Delete removes the credential file for acc.
func (s *Store) Delete(acc *account.Account) error {
	err := os.Remove(filepath.Join(s.dir, acc.FileName()))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeleteFile removes a credential file by name.
func (s *Store) DeleteFile(name string) error {
	err := os.Remove(filepath.Join(s.dir, filepath.Base(name)))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) saveTo(path string, acc *account.Account) error {
	b, err := s.encode(acc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// encode renders acc as either a sealed envelope or plain JSON. A snapshot is
// serialized so a concurrent token refresh cannot produce a torn record.
func (s *Store) encode(acc *account.Account) ([]byte, error) {
	snap := acc.Snapshot()
	raw, err := json.Marshal(&snap)
	if err != nil {
		return nil, err
	}
	if s.keeper == nil {
		return pretty(raw), nil
	}
	kek, err := s.keeper.Key()
	if err != nil {
		return nil, fmt.Errorf("authstore: key unavailable: %w", err)
	}
	env, err := crypto.Seal(kek, raw)
	if err != nil {
		return nil, err
	}
	doc := map[string]json.RawMessage{encKey: env}
	return prettyJSON(doc)
}

// decode parses a credential file, reporting whether it was sealed.
func (s *Store) decode(blob []byte) (*account.Account, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, false, err
	}
	if env, ok := doc[encKey]; ok && len(env) > 0 {
		if s.keeper == nil {
			return nil, true, errors.New("authstore: encrypted credential but no key available")
		}
		kek, err := s.keeper.Key()
		if err != nil {
			return nil, true, fmt.Errorf("authstore: key unavailable: %w", err)
		}
		raw, err := crypto.Open(kek, env)
		if err != nil {
			return nil, true, err
		}
		var acc account.Account
		if err := json.Unmarshal(raw, &acc); err != nil {
			return nil, true, err
		}
		return &acc, true, nil
	}
	var acc account.Account
	if err := json.Unmarshal(blob, &acc); err != nil {
		return nil, false, err
	}
	return &acc, false, nil
}

func pretty(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return append(raw, '\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

func prettyJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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

// BundleVersion is the export bundle format version.
const BundleVersion = 1

// bundle is the plaintext structure sealed into an export file.
type bundle struct {
	Version   int                `json:"version"`
	CreatedAt string             `json:"created_at"`
	Accounts  []*account.Account `json:"accounts"`
}

// Export seals accounts into a portable, passphrase-protected JSON document.
func Export(accts []*account.Account, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("authstore: export requires a passphrase")
	}
	payload, err := json.Marshal(bundle{
		Version:   BundleVersion,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Accounts:  accts,
	})
	if err != nil {
		return nil, err
	}
	env, err := crypto.SealWithPassphrase(passphrase, payload)
	if err != nil {
		return nil, err
	}
	return prettyJSON(map[string]json.RawMessage{encKey: env})
}

// Import opens a bundle produced by Export and returns its accounts.
func Import(blob []byte, passphrase string) ([]*account.Account, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("authstore: invalid bundle: %w", err)
	}
	env, ok := doc[encKey]
	if !ok || len(env) == 0 {
		return nil, errors.New("authstore: bundle is not an encrypted export")
	}
	payload, err := crypto.OpenWithPassphrase(passphrase, env)
	if err != nil {
		return nil, err
	}
	var b bundle
	if err := json.Unmarshal(payload, &b); err != nil {
		return nil, err
	}
	if b.Version != BundleVersion {
		return nil, fmt.Errorf("authstore: unsupported bundle version %d", b.Version)
	}
	out := make([]*account.Account, 0, len(b.Accounts))
	for _, a := range b.Accounts {
		if Validate(a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
