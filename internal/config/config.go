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
	"sort"
	"time"
)

// Config is the root configuration object.
type Config struct {
	Listen   string `json:"listen"`
	APIKey   string `json:"api_key"`
	AuthDir  string `json:"auth_dir"`
	DataDir  string `json:"data_dir"`
	LogLevel string `json:"log_level"`

	Pool          PoolConfig          `json:"pool"`
	SessionSticky SessionStickyConfig `json:"session_sticky"`
	Features      FeaturesConfig      `json:"features"`
	Security      SecurityConfig      `json:"security"`
	Router        RouterConfig        `json:"router"`
	Schedule      ScheduleConfig      `json:"schedule"`
	ReqLog        ReqLogConfig        `json:"request_log"`
	Upstreams     UpstreamsConfig     `json:"upstreams"`
}

// ScheduleConfig controls the built-in daily task scheduler.
//
// Hours are local clock hours in Timezone. A task runs once at each listed
// hour, so listing two hours gives two daily chances. Enabled flags default to
// true: a config file that predates this section keeps every task on.
type ScheduleConfig struct {
	// Timezone is an IANA name used for day boundaries and slot hours. Sign-in
	// and streak rewards settle on the upstream's calendar, so the default
	// matches it rather than the host clock.
	Timezone string `json:"timezone"`

	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	BlackcatHours  []int `json:"blackcat_hours"`
	GrowthHours    []int `json:"growth_hours"`

	CheckinEnabled   bool `json:"checkin_enabled"`
	TravelEnabled    bool `json:"travel_enabled"`
	ActivityEnabled  bool `json:"activity_enabled"`
	KeepaliveEnabled bool `json:"keepalive_enabled"`
	BlackcatEnabled  bool `json:"blackcat_enabled"`
	GrowthEnabled    bool `json:"growth_enabled"`

	// BalanceRefreshEnabled keeps credit balances fresh on a short interval,
	// independent of the hourly schedule.
	BalanceRefreshEnabled bool `json:"balance_refresh_enabled"`
	BalanceRefreshMinutes int  `json:"balance_refresh_minutes"`

	// IncludeDisabledInTasks lets the credential-maintenance tasks (check-in,
	// activity, keep-alive, balance) also run for administratively disabled
	// accounts. Disabled accounts still never serve traffic.
	IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`

	// CatchUpOnStart runs one catch-up pass for the credential-maintenance
	// tasks when the process starts after today's slot already passed, so a
	// restart does not silently lose a day. Growth tasks are not caught up:
	// they reset daily and carry no continuity penalty.
	CatchUpOnStart bool `json:"catch_up_on_start"`

	// AccountDelayMS spaces out per-account upstream calls within one sweep.
	AccountDelayMS int `json:"account_delay_ms"`

	// ExpiringSoonHours marks credits that expire within this window. The pool
	// then prefers spending those accounts first. Zero disables the signal.
	ExpiringSoonHours int `json:"expiring_soon_hours"`
}

// ReqLogConfig controls the in-memory request log.
type ReqLogConfig struct {
	// RecentLimit is how many recent requests the console can show.
	RecentLimit int `json:"recent_limit"`
	// Stdout mirrors each record to the process log.
	Stdout bool `json:"stdout"`
}

// SecurityConfig controls how credentials are protected at rest.
type SecurityConfig struct {
	// KeyProvider selects the credential key backend: auto, os, or file.
	KeyProvider string `json:"key_provider"`
	// EncryptCredentials seals credential files with envelope encryption.
	EncryptCredentials bool `json:"encrypt_credentials"`
}

// FeaturesConfig toggles outbound rewrite protections. Both default to true.
type FeaturesConfig struct {
	// SanitizeFingerprints scrubs client prompt fingerprints that the upstream
	// content filter matches literally.
	SanitizeFingerprints bool `json:"sanitize_fingerprints"`
	// RepairToolMarkup restores native tool-call markup into structured calls.
	RepairToolMarkup bool `json:"repair_tool_markup"`
}

// PoolConfig controls account selection and resilience.
type PoolConfig struct {
	CreditFloor        int     `json:"credit_floor"`
	CooldownSeconds    int     `json:"cooldown_seconds"`
	CooldownMaxSeconds int     `json:"cooldown_max_seconds"`
	BreakerThreshold   int     `json:"breaker_threshold"`
	BreakerSeconds     int     `json:"breaker_seconds"`
	MaxInFlight        int     `json:"max_in_flight"`
	WaitMaxSeconds     int     `json:"wait_max_seconds"`
	FailoverAttempts   int     `json:"failover_attempts"`
	WeightCredit       float64 `json:"weight_credit"`
	WeightIdle         float64 `json:"weight_idle"`
	WeightSuccess      float64 `json:"weight_success"`
	// WeightExpiring tilts selection toward accounts holding credits that are
	// close to expiring, so they are spent before they lapse.
	WeightExpiring float64 `json:"weight_expiring"`
}

// SessionStickyConfig pins a conversation to one account for a while.
type SessionStickyConfig struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttl_seconds"`
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
		Pool: PoolConfig{
			CreditFloor:        100,
			CooldownSeconds:    30,
			CooldownMaxSeconds: 3600,
			BreakerThreshold:   5,
			BreakerSeconds:     120,
			MaxInFlight:        3,
			WaitMaxSeconds:     8,
			FailoverAttempts:   3,
			WeightCredit:       0.35,
			WeightIdle:         0.25,
			WeightSuccess:      0.25,
			WeightExpiring:     0.15,
		},
		SessionSticky: SessionStickyConfig{Enabled: true, TTLSeconds: 1800},
		Features:      FeaturesConfig{SanitizeFingerprints: true, RepairToolMarkup: true},
		Security:      SecurityConfig{KeyProvider: "auto", EncryptCredentials: true},
		Router:        RouterConfig{Naming: "flat", SameNamePolicy: "auto_healthy"},
		Schedule: ScheduleConfig{
			Timezone:              "Asia/Shanghai",
			CheckinHours:          []int{9, 21},
			TravelHours:           []int{9, 21},
			ActivityHours:         []int{10},
			KeepaliveHours:        []int{22},
			BlackcatHours:         []int{23},
			GrowthHours:           []int{1},
			CheckinEnabled:        true,
			TravelEnabled:         true,
			ActivityEnabled:       true,
			KeepaliveEnabled:      true,
			BlackcatEnabled:       true,
			GrowthEnabled:         true,
			BalanceRefreshEnabled: true,
			BalanceRefreshMinutes: 5,
			CatchUpOnStart:        true,
			AccountDelayMS:        1500,
			ExpiringSoonHours:     168,
		},
		ReqLog: ReqLogConfig{RecentLimit: 100, Stdout: true},
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
	if c.Pool.CooldownSeconds <= 0 {
		c.Pool.CooldownSeconds = 30
	}
	if c.Pool.CooldownMaxSeconds <= 0 {
		c.Pool.CooldownMaxSeconds = 3600
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 5
	}
	if c.Pool.BreakerSeconds <= 0 {
		c.Pool.BreakerSeconds = 120
	}
	if c.Pool.WaitMaxSeconds <= 0 {
		c.Pool.WaitMaxSeconds = 8
	}
	if c.Pool.FailoverAttempts <= 0 {
		c.Pool.FailoverAttempts = 3
	}
	if c.Pool.WeightCredit == 0 && c.Pool.WeightIdle == 0 && c.Pool.WeightSuccess == 0 && c.Pool.WeightExpiring == 0 {
		c.Pool.WeightCredit, c.Pool.WeightIdle, c.Pool.WeightSuccess, c.Pool.WeightExpiring = 0.35, 0.25, 0.25, 0.15
	}
	if c.Pool.WeightCredit < 0 || c.Pool.WeightIdle < 0 || c.Pool.WeightSuccess < 0 || c.Pool.WeightExpiring < 0 {
		c.Pool.WeightCredit, c.Pool.WeightIdle, c.Pool.WeightSuccess, c.Pool.WeightExpiring = 0.35, 0.25, 0.25, 0.15
	}
	if c.SessionSticky.TTLSeconds <= 0 {
		c.SessionSticky.TTLSeconds = 1800
	}
	switch c.Security.KeyProvider {
	case "auto", "os", "file":
	default:
		c.Security.KeyProvider = "auto"
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
	c.normalizeSchedule()
}

// normalizeSchedule fills defaults and validates the scheduler section. Hours
// are clamped into 0-23 and de-duplicated so a bad value cannot wedge the
// schedule loop into never firing.
func (c *Config) normalizeSchedule() {
	s := &c.Schedule
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		s.Timezone = "Asia/Shanghai"
	}
	s.CheckinHours = normalizeHours(s.CheckinHours, []int{9, 21})
	s.TravelHours = normalizeHours(s.TravelHours, []int{9, 21})
	s.ActivityHours = normalizeHours(s.ActivityHours, []int{10})
	s.KeepaliveHours = normalizeHours(s.KeepaliveHours, []int{22})
	s.BlackcatHours = normalizeHours(s.BlackcatHours, []int{23})
	s.GrowthHours = normalizeHours(s.GrowthHours, []int{1})
	if s.BalanceRefreshMinutes <= 0 {
		s.BalanceRefreshMinutes = 5
	}
	if s.AccountDelayMS < 0 {
		s.AccountDelayMS = 0
	}
	if s.ExpiringSoonHours < 0 {
		s.ExpiringSoonHours = 0
	}
	if c.ReqLog.RecentLimit <= 0 {
		c.ReqLog.RecentLimit = 100
	}
}

// normalizeHours de-duplicates and clamps configured hours, falling back to
// the defaults when nothing usable is left.
func normalizeHours(in, fallback []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, h := range in {
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return append([]int(nil), fallback...)
	}
	sort.Ints(out)
	return out
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
