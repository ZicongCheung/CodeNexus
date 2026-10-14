package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCreatesDefaultWithKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey == "" {
		t.Fatal("expected a generated api_key")
	}
	if cfg.Listen != "127.0.0.1:6828" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.APIKey != cfg.APIKey {
		t.Fatalf("api_key not persisted: %q != %q", again.APIKey, cfg.APIKey)
	}
}

func TestEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CODENEXUS_LISTEN", "127.0.0.1:9999")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("env override ignored: %q", cfg.Listen)
	}
}
