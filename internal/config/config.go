// Package config loads and persists the gateway configuration from a single
// JSON file, generating a default file (with a random api_key) on first run.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the root configuration object.
type Config struct {
	Listen   string `json:"listen"`
	APIKey   string `json:"api_key"`
	AuthDir  string `json:"auth_dir"`
	DataDir  string `json:"data_dir"`
	LogLevel string `json:"log_level"`

	Pool      PoolConfig      `json:"pool"`
	Router    RouterConfig    `json:"router"`
	Upstreams UpstreamsConfig `json:"upstreams"`
}

// PoolConfig controls account selection and resilience.
type PoolConfig struct {
	CreditFloor     int `json:"credit_floor"`
	CooldownSeconds int `json:"cooldown_seconds"`
	MaxInFlight     int `json:"max_in_flight"`
}

// RouterConfig controls model naming and cross-source selection.
type RouterConfig struct {
	Naming         string `json:"naming"`
	SameNamePolicy string `json:"same_name_policy"`
}

// UpstreamsConfig holds one section per source.
type UpstreamsConfig struct {
	WorkBuddy WorkBuddyConfig `json:"workbuddy"`
	TraeCN    TraeCNConfig    `json:"trae-cn"`
}

// WorkBuddyConfig configures the WorkBuddy / CodeBuddy upstream.
type WorkBuddyConfig struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	UserAgent      string `json:"user_agent"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// TraeCNConfig configures the Trae CN upstream.
type TraeCNConfig struct {
	Enabled      bool   `json:"enabled"`
	AuthSource   string `json:"auth_source"`
	UpstreamMode string `json:"upstream_mode"`
	AutoRoute    bool   `json:"auto_route"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:   "127.0.0.1:6828",
		AuthDir:  "./auths",
		DataDir:  "./data",
		LogLevel: "info",
		Pool:     PoolConfig{CreditFloor: 100, CooldownSeconds: 600, MaxInFlight: 3},
		Router:   RouterConfig{Naming: "flat", SameNamePolicy: "auto_healthy"},
		Upstreams: UpstreamsConfig{
			WorkBuddy: WorkBuddyConfig{Enabled: true, TimeoutSeconds: 120},
			TraeCN:    TraeCNConfig{Enabled: false, AuthSource: "auto", UpstreamMode: "remote", AutoRoute: true},
		},
	}
}

// Load reads config from path. When the file is absent it writes a default file
// (including a fresh random api_key) and returns it; the caller is expected to
// surface that key once. Environment variables CODENEXUS_LISTEN and
// CODENEXUS_API_KEY override the file when non-empty.
func Load(path string) (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		merged := Default()
		if err := json.Unmarshal(b, &merged); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
		cfg = merged
	case os.IsNotExist(err):
		key, kerr := randomKey()
		if kerr != nil {
			return nil, kerr
		}
		cfg.APIKey = key
		if werr := writeAtomic(path, cfg); werr != nil {
			return nil, werr
		}
	default:
		return nil, err
	}
	cfg.applyEnv()
	cfg.normalize()
	return &cfg, nil
}

// Save writes cfg to path atomically.
func Save(path string, cfg *Config) error { return writeAtomic(path, *cfg) }

func (c *Config) applyEnv() {
	if v := os.Getenv("CODENEXUS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CODENEXUS_API_KEY"); v != "" {
		c.APIKey = v
	}
}

func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:6828"
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Pool.MaxInFlight <= 0 {
		c.Pool.MaxInFlight = 3
	}
	if c.Router.Naming == "" {
		c.Router.Naming = "flat"
	}
	if c.Router.SameNamePolicy == "" {
		c.Router.SameNamePolicy = "auto_healthy"
	}
	if c.Upstreams.WorkBuddy.TimeoutSeconds <= 0 {
		c.Upstreams.WorkBuddy.TimeoutSeconds = 120
	}
	if c.Upstreams.TraeCN.AuthSource == "" {
		c.Upstreams.TraeCN.AuthSource = "auto"
	}
	if c.Upstreams.TraeCN.UpstreamMode == "" {
		c.Upstreams.TraeCN.UpstreamMode = "remote"
	}
}

func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cnx-" + hex.EncodeToString(b), nil
}
