package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/configschema"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
)

// poolPaths are the settings the pool absorbs without a restart.
var poolPaths = []string{
	"pool.max_in_flight", "pool.credit_floor", "pool.cooldown_seconds",
	"pool.cooldown_max_seconds", "pool.breaker_threshold", "pool.breaker_seconds",
	"pool.wait_max_seconds", "pool.weight_credit", "pool.weight_idle",
	"pool.weight_success", "pool.weight_expiring",
	"session_sticky.enabled", "session_sticky.ttl_seconds",
}

// upstreamPaths are the settings that only reach the wire through a rebuilt
// upstream client, so a change to any of them re-registers the sources.
var upstreamPaths = []string{
	"upstreams.trae-cn.enabled", "upstreams.trae-cn.upstream_mode",
	"upstreams.trae-cn.auto_route", "upstreams.trae-cn.timeout_seconds",
	"upstreams.trae-cn.max_tokens", "upstreams.trae-cn.debug_upstream",
	"upstreams.trae-cn.base_url", "upstreams.trae-cn.remote_base_url",
	"upstreams.trae-cn.max_mode.enabled", "upstreams.trae-cn.max_mode.models",
	// The remaining protocol endpoints and the device identity the adapter
	// presents. They are advanced settings, but "advanced" is not "inert": a
	// self-hosted relay is exactly the case where they change.
	"upstreams.trae-cn.auth_base_url", "upstreams.trae-cn.ug_base_url",
	"upstreams.trae-cn.pay_base_url", "upstreams.trae-cn.usage_base_url",
	"upstreams.trae-cn.authorize_url", "upstreams.trae-cn.client_id",
	"upstreams.trae-cn.web_login_port",
	"upstreams.trae-cn.device_brand", "upstreams.trae-cn.device_type",
	"upstreams.trae-cn.os_version", "upstreams.trae-cn.app_version",
	"upstreams.trae-cn.ide_version", "upstreams.trae-cn.app_id",
	"upstreams.trae-cn.cli.command",
	// The check-in tuning lives inside the client (its spacing gate and 9074
	// backoff are constructed with it), so it needs the same rebuild.
	"upstreams.trae-cn.checkin.enabled", "upstreams.trae-cn.checkin.interval_seconds",
	"upstreams.trae-cn.checkin.retry_after_seconds", "upstreams.trae-cn.checkin.max_backoff_seconds",
	"upstreams.trae-cn.checkin.backoff_exponent_cap", "upstreams.trae-cn.checkin.rotate_device_id",
	"upstreams.workbuddy.enabled", "upstreams.workbuddy.timeout_seconds",
	"upstreams.workbuddy.base_url", "upstreams.workbuddy.user_agent",
	"features.sanitize_fingerprints", "features.repair_tool_markup",
}

func changedPool(before, after *config.Config) bool {
	return configschema.Changed(before, after, poolPaths...)
}

func changedUpstream(before, after *config.Config) bool {
	return configschema.Changed(before, after, upstreamPaths...)
}

// poolOptions mirrors buildPool but without the one-time wiring, so a hot
// reload can hand the pool fresh tuning without touching its state path or
// error handler.
func poolOptions(cfg *config.Config) pool.Options {
	return pool.Options{
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
	}
}

// restartProcess replaces the running process with a fresh one, which is the
// only way a change to the listener, the data directory or credential
// encryption can take effect.
//
// The port has to be released before the replacement binds it, so the order
// matters: shut down, then spawn. If spawning fails the caller is given the
// chance to listen again, because a restart that leaves no process behind is
// worse than a change that did not apply.
func restartProcess(addr string, current func() *http.Server, listenAgain func()) func() error {
	return func() error {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("定位可执行文件失败：%w", err)
		}
		if srv := current(); srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		// The port is free now, which is the only moment the new address can be
		// proven bindable. Finding out after the old process is gone would
		// leave nothing serving.
		if ln, err := net.Listen("tcp", addr); err != nil {
			listenAgain()
			return fmt.Errorf("新监听地址 %s 无法绑定：%w", addr, err)
		} else {
			_ = ln.Close()
		}
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if wd, err := os.Getwd(); err == nil {
			cmd.Dir = wd
		}
		detachProcess(cmd)
		if err := cmd.Start(); err != nil {
			listenAgain()
			return fmt.Errorf("启动新进程失败：%w", err)
		}
		logx.Infof("restarting into a new process (pid %d)", cmd.Process.Pid)
		// Give the response a moment to reach the browser, then step aside so
		// the replacement owns the port.
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
		return nil
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
