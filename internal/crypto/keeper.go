package crypto

import (
	"errors"
	"runtime"
)

// Keeper supplies the machine-bound key-encryption key (KEK) used to wrap
// credential payloads. Implementations create the key on first use.
type Keeper interface {
	// Name identifies the backing store, e.g. "dpapi", "keychain",
	// "secret-service", "file".
	Name() string
	// Key returns the 32-byte KEK, generating and persisting it on first call.
	Key() ([]byte, error)
}

// Backend names accepted by WithBackend.
const (
	BackendAuto = "auto"
	BackendOS   = "os"
	BackendFile = "file"
)

// ErrUnsupported is returned when an explicitly requested backend is not
// available on this platform.
var ErrUnsupported = errors.New("crypto: requested key backend unavailable")

type keeperConfig struct {
	backend string
}

// KeeperOption customizes NewKeeper.
type KeeperOption func(*keeperConfig)

// WithBackend forces a backend: auto (default), os, or file.
func WithBackend(name string) KeeperOption {
	return func(c *keeperConfig) { c.backend = name }
}

// NewKeeper returns a keeper rooted at dir (used for the file fallback and for
// the DPAPI blob). With the default "auto" backend it prefers the OS keystore
// and silently falls back to a local key file, so the gateway never blocks on
// an unavailable keychain.
func NewKeeper(dir string, opts ...KeeperOption) (Keeper, error) {
	if dir == "" {
		return nil, errors.New("crypto: keeper directory required")
	}
	cfg := keeperConfig{backend: BackendAuto}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	file, ferr := newFileKeeper(dir)
	switch cfg.backend {
	case BackendFile:
		return file, ferr
	case BackendOS:
		os := osKeeper(dir)
		if os == nil {
			return nil, ErrUnsupported
		}
		return os, nil
	case BackendAuto, "":
		os := osKeeper(dir)
		if os == nil {
			return file, ferr
		}
		if ferr != nil {
			return os, nil
		}
		return &fallbackKeeper{primary: os, fallback: file}, nil
	default:
		return nil, errors.New("crypto: unknown key backend " + cfg.backend)
	}
}

// osKeeper returns the platform keystore keeper, or nil when unavailable.
func osKeeper(dir string) Keeper {
	switch runtime.GOOS {
	case "windows":
		return newDPAPIKeeper(dir)
	case "darwin":
		return newKeychainKeeper()
	case "linux":
		return newSecretToolKeeper()
	default:
		return nil
	}
}

// fallbackKeeper tries the platform keystore and, if it errors, uses the file
// keeper. Once a key has been read from either source it is remembered so the
// two never disagree within a process.
type fallbackKeeper struct {
	primary  Keeper
	fallback Keeper

	key  []byte
	used string
}

func (f *fallbackKeeper) Name() string {
	if f.used != "" {
		return f.used
	}
	return "auto(" + f.primary.Name() + "/" + f.fallback.Name() + ")"
}

func (f *fallbackKeeper) Key() ([]byte, error) {
	if f.key != nil {
		return f.key, nil
	}
	if k, err := f.primary.Key(); err == nil && len(k) == KeySize {
		f.key, f.used = k, f.primary.Name()
		return k, nil
	}
	k, err := f.fallback.Key()
	if err != nil {
		return nil, err
	}
	f.key, f.used = k, f.fallback.Name()
	return k, nil
}
