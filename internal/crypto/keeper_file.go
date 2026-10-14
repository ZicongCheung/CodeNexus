package crypto

import (
	"os"
	"path/filepath"
)

// fileKeeper stores the raw KEK in a 0600 file. It is the portable fallback
// used when no OS keystore is reachable; it protects against casual reads but
// not against a local attacker with the same file access.
type fileKeeper struct{ path string }

func newFileKeeper(dir string) (Keeper, error) {
	if dir == "" {
		return nil, ErrUnsupported
	}
	return &fileKeeper{path: filepath.Join(dir, "master.key")}, nil
}

func (k *fileKeeper) Name() string { return "file" }

func (k *fileKeeper) Key() ([]byte, error) {
	if b, err := os.ReadFile(k.path); err == nil && len(b) == KeySize {
		return b, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	key, err := randomBytes(KeySize)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return nil, err
	}
	if err := writeAtomicFile(k.path, key); err != nil {
		return nil, err
	}
	return key, nil
}

// writeAtomicFile writes b to path atomically with 0600 permissions.
func writeAtomicFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
