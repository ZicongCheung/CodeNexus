// Command codenexus runs the unified AI coding account gateway.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/api"
	"github.com/ZicongCheung/CodeNexus/internal/authrefresh"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/crypto"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/panel"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/scheduler"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
	"github.com/ZicongCheung/CodeNexus/internal/upstream/traecn"
	"github.com/ZicongCheung/CodeNexus/internal/upstream/workbuddy"
)

// The WorkBuddy adapter implements interactive device login; the console relies
// on it being registered.
var _ upstream.DeviceAuth = (*workbuddy.Client)(nil)
var _ upstream.DeviceAuth = (*traecn.Client)(nil)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := runAuth(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "codenexus auth: %v\n", err)
			os.Exit(1)
		}
		return
	}

	configPath := flag.String("config", "config.json", "path to the config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("codenexus %s\n", version)
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "codenexus: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	_, statErr := os.Stat(configPath)
	fresh := os.IsNotExist(statErr)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logx.SetLevel(logx.ParseLevel(cfg.LogLevel))

	if fresh {
		logx.Infof("created %s with a new api_key (shown once): %s", configPath, cfg.APIKey)
	}

	p := buildPool(cfg)

	keeper, err := buildKeeper(cfg)
	if err != nil {
		return err
	}
	if keeper != nil {
		logx.Infof("credential encryption enabled (key backend: %s)", keeper.Name())
	} else {
		logx.Warnf("credential encryption disabled (security.encrypt_credentials=false)")
	}

	store := buildStore(cfg, keeper)
	accts, err := store.Load()
	if err != nil {
		logx.Warnf("load accounts: %v", err)
	}
	if n, merr := store.Migrate(); merr != nil {
		logx.Warnf("migrate credentials: %v", merr)
	} else if n > 0 {
		logx.Infof("upgraded %d credential file(s) to encrypted storage", n)
	}
	if env := authstore.AccountsFromEnv(); len(env) > 0 {
		env = identifyEnvAccounts(env)
		logx.Infof("loaded %d account(s) from environment variables", len(env))
		accts = append(accts, env...)
	}
	p.Replace(accts)
	if err := p.LoadState(); err != nil {
		logx.Warnf("load pool state: %v", err)
	}
	p.StartFlusher(5 * time.Second)
	defer func() {
		if err := p.Close(); err != nil {
			logx.Warnf("flush pool state: %v", err)
		}
	}()
	logx.Infof("loaded %d account(s) from %s", len(accts), cfg.AuthDir)

	if cfg.Upstreams.WorkBuddy.Enabled {
		upstream.Register(workbuddy.New(workbuddy.Config{
			BaseURL:             cfg.Upstreams.WorkBuddy.BaseURL,
			UserAgent:           cfg.Upstreams.WorkBuddy.UserAgent,
			TimeoutSeconds:      cfg.Upstreams.WorkBuddy.TimeoutSeconds,
			DisableSanitize:     !cfg.Features.SanitizeFingerprints,
			DisableMarkupRepair: !cfg.Features.RepairToolMarkup,
		}))
	}
	if cfg.Upstreams.TraeCN.Enabled {
		upstream.Register(traecn.New(traecn.Config{
			BaseURL:        cfg.Upstreams.TraeCN.BaseURL,
			RemoteBaseURL:  cfg.Upstreams.TraeCN.RemoteBaseURL,
			AuthBaseURL:    cfg.Upstreams.TraeCN.AuthBaseURL,
			AuthorizeURL:   cfg.Upstreams.TraeCN.AuthorizeURL,
			ClientID:       cfg.Upstreams.TraeCN.ClientID,
			TimeoutSeconds: cfg.Upstreams.TraeCN.TimeoutSeconds,
			MaxTokens:      cfg.Upstreams.TraeCN.MaxTokens,
			WebLoginPort:   cfg.Upstreams.TraeCN.WebLoginPort,
			DeviceBrand:    cfg.Upstreams.TraeCN.DeviceBrand,
			DeviceType:     cfg.Upstreams.TraeCN.DeviceType,
			OSVersion:      cfg.Upstreams.TraeCN.OSVersion,
			AppVersion:     cfg.Upstreams.TraeCN.AppVersion,
			IDEVersion:     cfg.Upstreams.TraeCN.IDEVersion,
			AppID:          cfg.Upstreams.TraeCN.AppID,
			UpstreamMode:   cfg.Upstreams.TraeCN.UpstreamMode,
			AutoRoute:      cfg.Upstreams.TraeCN.AutoRoute,
		}))
	}

	// Background credential refresh keeps tokens from expiring unnoticed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresher := authrefresh.New(p, store, authrefresh.Options{
		Logf: func(format string, args ...any) { logx.Warnf(format, args...) },
	})
	refresher.Start(ctx)

	// The scheduler runs the recurring account maintenance and the growth
	// task sweep. It shares the pool with the gateway, so a task and a live
	// request never fight over the same credential.
	statePath := filepath.Join(cfg.DataDir, "scheduler-state.json")
	sched := scheduler.New(scheduler.Options{
		Cfg:     cfg.Schedule,
		Pool:    p,
		Tasks:   upstream.TasksFor,
		Refresh: refresher,
		State:   scheduler.LoadState(statePath),
		Logf:    func(format string, args ...any) { logx.Infof(format, args...) },
	})
	go sched.Run(ctx)
	sched.StartBalanceRefresh(ctx)
	if cfg.Schedule.CheckinEnabled || cfg.Schedule.TravelEnabled || cfg.Schedule.ActivityEnabled ||
		cfg.Schedule.KeepaliveEnabled || cfg.Schedule.BlackcatEnabled || cfg.Schedule.GrowthEnabled {
		logx.Infof("scheduler active (timezone %s)", cfg.Schedule.Timezone)
	} else {
		logx.Warnf("scheduler inactive: every task family is disabled")
	}

	// The request log feeds the console's observability panels and is shared
	// with the HTTP layer that records every chat request.
	recorder := reqlog.New(cfg.ReqLog.RecentLimit, cfg.ReqLog.Stdout)

	console := panel.New(cfg, store, p,
		panel.WithLogger(func(format string, args ...any) { logx.Infof(format, args...) }),
		panel.WithScheduler(sched),
		panel.WithReqLog(recorder))
	srv := api.NewServer(cfg, p, console,
		api.WithRefresher(refresher),
		api.WithRecorder(recorder))
	// Model catalogues drive cross-source routing; loading them in the
	// background keeps the first request from paying for it.
	srv.WarmModels(ctx)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logx.Infof("codenexus %s listening on http://%s", version, cfg.Listen)
		logx.Infof("console: http://%s/panel/", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		logx.Infof("shutting down...")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// buildStore assembles the credential store, enabling encryption when a keeper
// is available.
func buildStore(cfg *config.Config, keeper crypto.Keeper) *authstore.Store {
	opts := []authstore.Option{
		authstore.WithLogger(func(format string, args ...any) { logx.Warnf(format, args...) }),
	}
	if keeper != nil {
		opts = append(opts, authstore.WithKeeper(keeper))
	}
	return authstore.New(cfg.AuthDir, opts...)
}

// buildKeeper resolves the credential key backend from configuration.
func buildKeeper(cfg *config.Config) (crypto.Keeper, error) {
	if !cfg.Security.EncryptCredentials {
		return nil, nil
	}
	dir := filepath.Join(cfg.DataDir, "keys")
	backend := cfg.Security.KeyProvider
	if backend == "" {
		backend = crypto.BackendAuto
	}
	keeper, err := crypto.NewKeeper(dir, crypto.WithBackend(backend))
	if err != nil {
		if errors.Is(err, crypto.ErrUnsupported) {
			logx.Warnf("key backend %q unavailable; falling back to a local key file", backend)
			return crypto.NewKeeper(dir, crypto.WithBackend(crypto.BackendFile))
		}
		return nil, err
	}
	return keeper, nil
}

// runAuth handles the `codenexus auth export|import` subcommands.
func runAuth(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: codenexus auth <export|import> [flags]")
	}
	op := args[0]
	fs := flag.NewFlagSet("auth "+op, flag.ContinueOnError)
	configPath := fs.String("config", "config.json", "path to the config file")
	pass := fs.String("passphrase", "", "bundle passphrase (else CODENEXUS_PASSPHRASE or prompt)")
	var in, out *string
	switch op {
	case "export":
		out = fs.String("out", "codenexus-auth-bundle.json", "output bundle path")
	case "import":
		in = fs.String("in", "", "input bundle path (required)")
	default:
		return fmt.Errorf("unknown auth command %q (want export or import)", op)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	keeper, err := buildKeeper(cfg)
	if err != nil {
		return err
	}
	store := buildStore(cfg, keeper)

	passphrase, err := resolvePassphrase(*pass)
	if err != nil {
		return err
	}

	switch op {
	case "export":
		accts, err := store.Load()
		if err != nil {
			return err
		}
		if len(accts) == 0 {
			return errors.New("no accounts to export")
		}
		blob, err := authstore.Export(accts, passphrase)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, blob, 0o600); err != nil {
			return err
		}
		fmt.Printf("exported %d account(s) to %s\n", len(accts), *out)
		return nil

	case "import":
		if strings.TrimSpace(*in) == "" {
			return errors.New("flag -in is required")
		}
		blob, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		accts, err := authstore.Import(blob, passphrase)
		if err != nil {
			return err
		}
		imported := 0
		for _, acc := range accts {
			if err := store.Save(acc); err != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", acc.UID, err)
				continue
			}
			imported++
		}
		fmt.Printf("imported %d/%d account(s) into %s\n", imported, len(accts), cfg.AuthDir)
		return nil
	}
	return nil
}

// resolvePassphrase reads the bundle passphrase from the flag, the environment,
// or standard input.
func resolvePassphrase(flagVal string) (string, error) {
	if p := strings.TrimSpace(flagVal); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv("CODENEXUS_PASSPHRASE")); p != "" {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "passphrase: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	p := strings.TrimSpace(line)
	if p == "" {
		return "", errors.New("empty passphrase")
	}
	return p, nil
}

// buildPool translates configuration into pool options.
func buildPool(cfg *config.Config) *pool.Pool {
	p := pool.New(pool.Options{
		MaxInFlight:      cfg.Pool.MaxInFlight,
		CreditFloor:      int64(cfg.Pool.CreditFloor),
		CooldownBase:     time.Duration(cfg.Pool.CooldownSeconds) * time.Second,
		CooldownMax:      time.Duration(cfg.Pool.CooldownMaxSeconds) * time.Second,
		BreakerThreshold: cfg.Pool.BreakerThreshold,
		BreakerFor:       time.Duration(cfg.Pool.BreakerSeconds) * time.Second,
		WaitMax:          time.Duration(cfg.Pool.WaitMaxSeconds) * time.Second,
		StickyEnabled:    cfg.SessionSticky.Enabled,
		StickyTTL:        time.Duration(cfg.SessionSticky.TTLSeconds) * time.Second,
		WeightCredit:     cfg.Pool.WeightCredit,
		WeightIdle:       cfg.Pool.WeightIdle,
		WeightSuccess:    cfg.Pool.WeightSuccess,
		WeightExpiring:   cfg.Pool.WeightExpiring,
	})
	p.SetStatePath(filepath.Join(cfg.DataDir, "pool-state.json"))
	p.SetErrorHandler(func(err error) { logx.Warnf("flush pool state: %v", err) })
	return p
}

// identifyEnvAccounts fills in the identity of environment-injected accounts
// and drops the ones whose identity cannot be determined, because an account
// without a uid cannot be keyed in the pool.
//
// A Trae token is a JWT that carries its own user id, so an operator only has
// to supply the credential; the identity is read back out of it.
func identifyEnvAccounts(in []*account.Account) []*account.Account {
	out := make([]*account.Account, 0, len(in))
	for _, acc := range in {
		if acc == nil {
			continue
		}
		if acc.UID == "" && acc.Source == account.SourceTraeCN {
			acc.UID = traecn.TokenUserID(acc.AccessToken)
			if acc.Label == "" || acc.Label == "env" {
				acc.Label = shortLabel(acc.UID)
			}
		}
		if acc.UID == "" {
			logx.Warnf("skipping environment %s account: no uid and none derivable from the token", acc.Source)
			continue
		}
		out = append(out, acc)
	}
	return out
}

// shortLabel renders a display label from an account id.
func shortLabel(uid string) string {
	if len(uid) > 12 {
		return uid[:12]
	}
	return uid
}
