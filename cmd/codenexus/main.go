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
	"syscall"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/api"
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

	p := pool.New()
	accts, err := account.LoadDir(cfg.AuthDir)
	if err != nil {
		logx.Warnf("load accounts: %v", err)
	}
	p.Replace(accts)
	logx.Infof("loaded %d account(s) from %s", len(accts), cfg.AuthDir)

	if cfg.Upstreams.WorkBuddy.Enabled {
		upstream.Register(workbuddy.New(workbuddy.Config{
			BaseURL:        cfg.Upstreams.WorkBuddy.BaseURL,
			UserAgent:      cfg.Upstreams.WorkBuddy.UserAgent,
			TimeoutSeconds: cfg.Upstreams.WorkBuddy.TimeoutSeconds,
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
