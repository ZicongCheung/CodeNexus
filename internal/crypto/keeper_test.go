package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestFileKeeperPersistsKey(t *testing.T) {
	dir := t.TempDir()
	k1, err := NewKeeper(dir, WithBackend(BackendFile))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	if k1.Name() != "file" {
		t.Fatalf("name = %q", k1.Name())
	}
	a, err := k1.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(a) != KeySize {
		t.Fatalf("key size = %d", len(a))
	}

	k2, _ := NewKeeper(dir, WithBackend(BackendFile))
	b, err := k2.Key()
	if err != nil {
		t.Fatalf("Key (reload): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("key not stable across keeper instances")
	}
}

func TestNewKeeperRequiresDir(t *testing.T) {
	if _, err := NewKeeper(""); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestNewKeeperUnknownBackend(t *testing.T) {
	if _, err := NewKeeper(t.TempDir(), WithBackend("nope")); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestNewKeeperOSUnsupported(t *testing.T) {
	// Force the "os" backend; assert the error path is sane either way.
	_, err := NewKeeper(t.TempDir(), WithBackend(BackendOS))
	if err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// failingKeeper always fails, to exercise the fallback path.
type failingKeeper struct{}

func (failingKeeper) Name() string         { return "failing" }
func (failingKeeper) Key() ([]byte, error) { return nil, errors.New("nope") }

func TestFallbackKeeperUsesFallbackOnError(t *testing.T) {
	file, err := newFileKeeper(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fk := &fallbackKeeper{primary: failingKeeper{}, fallback: file}
	k, err := fk.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(k) != KeySize {
		t.Fatalf("key size = %d", len(k))
	}
	if fk.Name() != "file" {
		t.Fatalf("resolved backend = %q", fk.Name())
	}
	// Second call is served from cache.
	if k2, _ := fk.Key(); !bytes.Equal(k, k2) {
		t.Fatal("cached key changed")
	}
}

func TestMemoryKeeper(t *testing.T) {
	k := NewMemoryKeeper()
	a, err := k.Key()
	if err != nil || len(a) != KeySize {
		t.Fatalf("Key: %v len=%d", err, len(a))
	}
	b, _ := k.Key()
	if !bytes.Equal(a, b) {
		t.Fatal("memory keeper key changed")
	}
}
