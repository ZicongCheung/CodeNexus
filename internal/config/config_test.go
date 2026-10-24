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

// Retired keys must not stop an existing file from loading. The console's save
// path rejects unknown fields on purpose, but the boot path has to be more
// forgiving: an upgrade that refuses to start because of a key it no longer
// uses is worse than quietly dropping it.
func TestRetiredKeysAreIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
	  "listen": "127.0.0.1:7001",
	  "upstreams": {"trae-cn": {
	    "checkin": {"enabled": false, "at": "08:30"},
	    "cli": {"command": "trae", "args": ["-p"], "work_dir": "/tmp/x",
	            "model_mode": "flag", "output_mode": "text", "prompt_stdin": true,
	            "max_concurrency": 4, "timeout_seconds": 60,
	            "disable_tools": false, "disallowed_tools": ["Bash"],
	            "max_messages": 10, "max_prompt_chars": 4000}
	  }}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a retired key must not break the boot path: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7001" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.Upstreams.TraeCN.Checkin.Enabled {
		t.Fatal("checkin.enabled did not survive")
	}
	if cfg.Upstreams.TraeCN.CLI.Command != "trae" {
		t.Fatalf("cli.command = %q", cfg.Upstreams.TraeCN.CLI.Command)
	}
}
