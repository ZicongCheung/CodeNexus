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
	"strings"
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
	UsageStore    UsageStoreConfig    `json:"usage_store"`
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

	// CheckinRetryEnabled re-runs the check-in for accounts that have not
	// completed today's, so an account the upstream throttled at the slot still
	// gets in later the same day. The adapter's own backoff is what keeps the
	// retries from hammering upstream.
	CheckinRetryEnabled bool `json:"checkin_retry_enabled"`
	CheckinRetrySeconds int  `json:"checkin_retry_seconds"`

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

// UsageStoreConfig controls the persisted usage history.
type UsageStoreConfig struct {
	// Enabled turns the SQLite usage history on. Off keeps the console on the
	// in-memory ring alone and writes nothing to disk.
	Enabled bool `json:"enabled"`
	// Path is the database file. Empty puts it in DataDir.
	Path string `json:"path"`
	// MaxRecords bounds how many rows are kept; the oldest are pruned.
	MaxRecords int `json:"max_records"`
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
	// Naming selects the model-name surface: "flat" exposes bare model ids and
	// resolves them across sources, "prefixed" exposes source/model only.
	Naming string `json:"naming"`
	// SameNamePolicy decides what happens when several sources serve the same
	// bare model id: "auto_healthy" picks the healthiest account across all of
	// them, "first" pins the first registered source.
	SameNamePolicy string `json:"same_name_policy"`
	// AllowPrefix keeps the "source/model" form usable as an explicit channel
	// even when Naming is flat, so a client can pin a source without changing
	// global naming.
	AllowPrefix bool `json:"allow_prefix"`
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
	Enabled bool `json:"enabled"`
	// AuthSource records how accounts are expected to arrive: manual, env,
	// web-login or auto (the local client's store). It is informational — every
	// registered path stays available in the console.
	AuthSource string `json:"auth_source"`
	// UpstreamMode pins the executor: remote, solo, ide, work-agent, cli, or
	// auto-route (tools → IDE Agent, plain chat → Remote). The cli mode runs a
	// local installation instead of calling the HTTP upstreams.
	UpstreamMode string `json:"upstream_mode"`
	// DebugUpstream dumps every raw Trae event into the debug log.
	DebugUpstream bool `json:"debug_upstream"`
	// AutoRoute enables the tools/chat split when UpstreamMode is auto-route.
	AutoRoute bool `json:"auto_route"`

	// BaseURL is the model gateway host serving the IDE Agent / Solo endpoint.
	BaseURL string `json:"base_url"`
	// RemoteBaseURL is the web/remote chat base (chat_sessions + events).
	RemoteBaseURL string `json:"remote_base_url"`
	// AuthBaseURL serves the OAuth token exchange.
	AuthBaseURL string `json:"auth_base_url"`
	// UGBaseURL serves the daily check-in API.
	UGBaseURL string `json:"ug_base_url"`
	// PayBaseURL serves the entitlement-usage (credit balance) API.
	PayBaseURL string `json:"pay_base_url"`
	// UsageBaseURL serves the per-turn commercial usage API.
	UsageBaseURL string `json:"usage_base_url"`
	// AuthorizeURL is the page the interactive login opens.
	AuthorizeURL string `json:"authorize_url"`
	// ClientID is sent with the token exchange.
	ClientID string `json:"client_id"`

	TimeoutSeconds int `json:"timeout_seconds"`
	// MaxTokens caps a single completion.
	MaxTokens int `json:"max_tokens"`
	// WebLoginPort pins the loopback port of the interactive authorization
	// helper. Zero picks a free port, which is the friendlier default.
	WebLoginPort int `json:"web_login_port"`

	// Device identity presented to the upstream. Defaults mirror the desktop
	// client so the requests look like a real installation.
	DeviceBrand string `json:"device_brand"`
	DeviceType  string `json:"device_type"`
	OSVersion   string `json:"os_version"`
	AppVersion  string `json:"app_version"`
	IDEVersion  string `json:"ide_version"`
	AppID       string `json:"app_id"`

	// LocalDataDirs overrides where the desktop client's store is looked for
	// when importing from this machine. Empty derives the platform defaults.
	LocalDataDirs []string `json:"local_data_dirs,omitempty"`
	// CLI configures the local-CLI executor (UpstreamMode: cli).
	CLI TraeCLIConfig `json:"cli"`
	// Checkin configures the daily check-in and its risk-control handling.
	Checkin CheckinConfig `json:"checkin"`
}

// TraeCLIConfig configures the local-CLI executor. Zero values take the
// built-in defaults the adapter ships.
type TraeCLIConfig struct {
	// Command overrides the executable. Empty searches the built-in names.
	Command string `json:"command,omitempty"`
	// Args replaces the built-in base arguments (default "-p").
	Args []string `json:"args,omitempty"`
	// WorkDir isolates the child process.
	WorkDir string `json:"work_dir,omitempty"`
	// ModelMode is how the model reaches the CLI: default, config, flag, none.
	ModelMode string `json:"model_mode,omitempty"`
	// OutputMode is "json" (structured, default) or "text".
	OutputMode string `json:"output_mode,omitempty"`
	// PromptStdin sends the prompt on stdin instead of as an argument.
	PromptStdin bool `json:"prompt_stdin,omitempty"`
	// MaxConcurrency caps simultaneous child processes.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// TimeoutSeconds bounds the silence between two reads from the child.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// DisableTools denies the built-in tool set. Nil means enabled, which is the
	// safe default for a proxy that would otherwise run commands on this host.
	DisableTools *bool `json:"disable_tools,omitempty"`
	// DisallowedTools adds to the built-in deny list.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	// MaxMessages and MaxPromptChars bound the serialized transcript.
	MaxMessages    int `json:"max_messages,omitempty"`
	MaxPromptChars int `json:"max_prompt_chars,omitempty"`
}

// CheckinConfig configures the daily check-in and its risk-control handling.
type CheckinConfig struct {
	// Enabled turns the daily sweep on.
	Enabled bool `json:"enabled"`
	// At is the local time of day the sweep runs, "HH:MM".
	At string `json:"at"`
	// IntervalSeconds spaces out the claims of one sweep. The upstream throttles
	// a source by device, so back-to-back claims from one machine are unwise.
	IntervalSeconds int `json:"interval_seconds"`
	// RetryAfterSeconds is the base of the 9074 backoff.
	RetryAfterSeconds int `json:"retry_after_seconds"`
	// MaxBackoffSeconds caps that backoff.
	MaxBackoffSeconds int `json:"max_backoff_seconds"`
	// BackoffExponentCap bounds the doubling so a long 9074 streak settles at
	// minutes rather than parking on the ceiling.
	BackoffExponentCap int `json:"backoff_exponent_cap"`
	// RotateDeviceID lets a 9074 rotate the derived device id and retry once.
	RotateDeviceID *bool `json:"rotate_device_id,omitempty"`
	// DeviceIDs pins a device id per account, disabling rotation when set.
	DeviceIDs map[string]string `json:"device_ids,omitempty"`
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
		Router:        RouterConfig{Naming: "flat", SameNamePolicy: "auto_healthy", AllowPrefix: true},
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
			CheckinRetryEnabled:   true,
			CheckinRetrySeconds:   60,
			CatchUpOnStart:        true,
			AccountDelayMS:        1500,
			ExpiringSoonHours:     168,
		},
		ReqLog:     ReqLogConfig{RecentLimit: 100, Stdout: true},
		UsageStore: UsageStoreConfig{Enabled: true, MaxRecords: 100000},
		Upstreams: UpstreamsConfig{
			WorkBuddy: WorkBuddyConfig{Enabled: true, TimeoutSeconds: 120},
			TraeCN: TraeCNConfig{
				Enabled:        true,
				AuthSource:     "web-login",
				UpstreamMode:   "auto-route",
				AutoRoute:      true,
				BaseURL:        "https://trae-api-cn.mchost.guru",
				RemoteBaseURL:  "https://trae-api-cn.mchost.guru/api/remote/v1",
				AuthBaseURL:    "https://api.trae.cn",
				UGBaseURL:      "https://api.trae.cn",
				PayBaseURL:     "https://api.trae.cn",
				UsageBaseURL:   "https://api5-normal.mchost.guru",
				AuthorizeURL:   "https://www.trae.cn/authorization",
				ClientID:       "ono9krqynydwx5",
				TimeoutSeconds: 120,
				MaxTokens:      64000,
				DeviceBrand:    "ASUS TUF Gaming A15 FA507RM_FA507RM",
				DeviceType:     "windows",
				OSVersion:      "Windows 10 Pro",
				AppVersion:     "3.3.65",
				IDEVersion:     "3.3.67",
				AppID:          "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
				CLI: TraeCLIConfig{
					Args:           []string{"-p"},
					ModelMode:      "default",
					OutputMode:     "json",
					MaxConcurrency: 2,
					TimeoutSeconds: 300,
					DisableTools:   boolPtr(true),
					MaxMessages:    60,
					MaxPromptChars: 20000,
				},
				Checkin: CheckinConfig{
					Enabled:            true,
					At:                 "08:30",
					IntervalSeconds:    60,
					RetryAfterSeconds:  60,
					MaxBackoffSeconds:  3600,
					BackoffExponentCap: 3,
					RotateDeviceID:     boolPtr(true),
				},
			},
		},
	}
}

// boolPtr is a helper for the optional booleans in the config, where absence
// has to be distinguishable from an explicit false.
func boolPtr(v bool) *bool { return &v }

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
		c.Upstreams.TraeCN.AuthSource = "web-login"
	}
	if c.Upstreams.TraeCN.UpstreamMode == "" {
		c.Upstreams.TraeCN.UpstreamMode = "auto-route"
	}
	c.normalizeTrae()
	c.normalizeSchedule()
}

// normalizeTrae fills the Trae CN section from the built-in defaults, which
// mirror the desktop client the upstream expects to see.
func (c *Config) normalizeTrae() {
	t := &c.Upstreams.TraeCN
	d := Default().Upstreams.TraeCN
	for _, f := range []struct {
		dst *string
		def string
	}{
		{&t.BaseURL, d.BaseURL},
		{&t.RemoteBaseURL, d.RemoteBaseURL},
		{&t.AuthBaseURL, d.AuthBaseURL},
		{&t.UGBaseURL, d.UGBaseURL},
		{&t.PayBaseURL, d.PayBaseURL},
		{&t.UsageBaseURL, d.UsageBaseURL},
		{&t.AuthorizeURL, d.AuthorizeURL},
		{&t.ClientID, d.ClientID},
		{&t.DeviceBrand, d.DeviceBrand},
		{&t.DeviceType, d.DeviceType},
		{&t.OSVersion, d.OSVersion},
		{&t.AppVersion, d.AppVersion},
		{&t.IDEVersion, d.IDEVersion},
		{&t.AppID, d.AppID},
	} {
		if strings.TrimSpace(*f.dst) == "" {
			*f.dst = f.def
		}
	}
	if t.TimeoutSeconds <= 0 {
		t.TimeoutSeconds = d.TimeoutSeconds
	}
	if t.MaxTokens <= 0 {
		t.MaxTokens = d.MaxTokens
	}
	if t.WebLoginPort < 0 || t.WebLoginPort > 65535 {
		t.WebLoginPort = 0
	}
	switch t.UpstreamMode {
	case "remote", "solo", "ide", "work-agent", "cli", "auto-route":
	default:
		t.UpstreamMode = "auto-route"
	}
	if t.UpstreamMode == "auto-route" {
		t.AutoRoute = true
	}
	c.normalizeTraeCLI()
	c.normalizeTraeCheckin()
}

// normalizeTraeCLI fills the local-CLI executor defaults and clamps its bounds.
func (c *Config) normalizeTraeCLI() {
	cli := &c.Upstreams.TraeCN.CLI
	d := Default().Upstreams.TraeCN.CLI
	if len(cli.Args) == 0 {
		cli.Args = d.Args
	}
	if strings.TrimSpace(cli.ModelMode) == "" {
		cli.ModelMode = d.ModelMode
	}
	if strings.TrimSpace(cli.OutputMode) == "" {
		cli.OutputMode = d.OutputMode
	}
	if cli.MaxConcurrency <= 0 {
		cli.MaxConcurrency = d.MaxConcurrency
	}
	if cli.TimeoutSeconds <= 0 {
		cli.TimeoutSeconds = d.TimeoutSeconds
	}
	if cli.DisableTools == nil {
		cli.DisableTools = d.DisableTools
	}
	if cli.MaxMessages <= 0 {
		cli.MaxMessages = d.MaxMessages
	}
	if cli.MaxPromptChars <= 0 {
		cli.MaxPromptChars = d.MaxPromptChars
	}
}

// normalizeTraeCheckin fills the check-in defaults, rejecting a malformed time
// so the sweep cannot be scheduled at an undefined moment.
func (c *Config) normalizeTraeCheckin() {
	ck := &c.Upstreams.TraeCN.Checkin
	d := Default().Upstreams.TraeCN.Checkin
	if _, err := parseClock(ck.At); err != nil {
		ck.At = d.At
	}
	if ck.IntervalSeconds <= 0 {
		ck.IntervalSeconds = d.IntervalSeconds
	}
	if ck.RetryAfterSeconds <= 0 {
		ck.RetryAfterSeconds = d.RetryAfterSeconds
	}
	if ck.MaxBackoffSeconds <= 0 {
		ck.MaxBackoffSeconds = d.MaxBackoffSeconds
	}
	if ck.MaxBackoffSeconds < ck.RetryAfterSeconds {
		ck.MaxBackoffSeconds = ck.RetryAfterSeconds
	}
	if ck.BackoffExponentCap <= 0 {
		ck.BackoffExponentCap = d.BackoffExponentCap
	}
	if ck.RotateDeviceID == nil {
		ck.RotateDeviceID = d.RotateDeviceID
	}
}

// parseClock validates an "HH:MM" time of day.
func parseClock(v string) (time.Time, error) {
	return time.Parse("15:04", strings.TrimSpace(v))
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
	if s.CheckinRetrySeconds <= 0 {
		s.CheckinRetrySeconds = 60
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
