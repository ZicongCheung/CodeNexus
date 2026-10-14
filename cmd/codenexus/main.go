// Command codenexus runs the unified AI coding account gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/api"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/panel"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
	"github.com/ZicongCheung/CodeNexus/internal/upstream/workbuddy"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
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

	store := authstore.New(cfg.AuthDir)
	accts, err := store.Load()
	if err != nil {
		logx.Warnf("load accounts: %v", err)
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
	// Trae CN upstream is registered in a later phase.

	srv := api.NewServer(cfg, p, panel.Handler())
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
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
	})
	p.SetStatePath(filepath.Join(cfg.DataDir, "pool-state.json"))
	p.SetErrorHandler(func(err error) { logx.Warnf("flush pool state: %v", err) })
	return p
}
