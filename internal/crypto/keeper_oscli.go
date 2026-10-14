package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// cliKeeper stores the KEK in an OS secret store reached through a command
// line helper (macOS `security`, Linux `secret-tool`). The key is held as
// base64 text by the helper.
type cliKeeper struct {
	name string
	get  func() (string, error)
	set  func(string) error

	mu  sync.Mutex
	key []byte
}

func (k *cliKeeper) Name() string { return k.name }

// Key reads the KEK from the OS store, creating it on first use.
func (k *cliKeeper) Key() ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, nil
	}
	if raw, err := k.get(); err == nil {
		if b, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(raw)); derr == nil && len(b) == KeySize {
			k.key = b
			return b, nil
		}
	}
	key, err := randomBytes(KeySize)
	if err != nil {
		return nil, err
	}
	if err := k.set(base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, err
	}
	k.key = key
	return key, nil
}

// newKeychainKeeper returns a keeper backed by the macOS Keychain, or nil when
// the `security` helper is unavailable.
func newKeychainKeeper() Keeper {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if _, err := exec.LookPath("security"); err != nil {
		return nil
	}
	return &cliKeeper{
		name: "keychain",
		get: func() (string, error) {
			out, err := exec.Command("security", "find-generic-password", "-s", "codenexus", "-a", "master", "-w").Output()
			return string(out), err
		},
		set: func(val string) error {
			return exec.Command("security", "add-generic-password", "-U", "-s", "codenexus", "-a", "master", "-w", val).Run()
		},
	}
}

// newSecretToolKeeper returns a keeper backed by the Linux Secret Service
// (libsecret), or nil when `secret-tool` is unavailable.
func newSecretToolKeeper() Keeper {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil
	}
	return &cliKeeper{
		name: "secret-service",
		get: func() (string, error) {
			out, err := exec.Command("secret-tool", "lookup", "service", "codenexus", "key", "master").Output()
			return string(out), err
		},
		set: func(val string) error {
			cmd := exec.Command("secret-tool", "store", "--label=CodeNexus master key", "service", "codenexus", "key", "master")
			cmd.Stdin = bytes.NewBufferString(val)
			if err := cmd.Run(); err != nil {
				return err
			}
			return nil
		},
	}
}

// memKeeper is an in-memory keeper used by tests.
type memKeeper struct{ key []byte }

// NewMemoryKeeper returns a keeper that never touches disk. Intended for tests.
func NewMemoryKeeper() Keeper {
	k, _ := randomBytes(KeySize)
	return &memKeeper{key: k}
}

func (k *memKeeper) Name() string { return "memory" }
func (k *memKeeper) Key() ([]byte, error) {
	if k.key == nil {
		return nil, errors.New("crypto: memory keeper has no key")
	}
	return k.key, nil
}
