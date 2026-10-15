package authstore

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// Environment variables that inject an account without touching disk. This
// suits headless deployments where credentials come from the platform's secret
// manager rather than an interactive login.

// WorkBuddy variables.
const (
	EnvWorkBuddyToken        = "CODENEXUS_WORKBUDDY_TOKEN"
	EnvWorkBuddyRefreshToken = "CODENEXUS_WORKBUDDY_REFRESH_TOKEN"
	EnvWorkBuddyUID          = "CODENEXUS_WORKBUDDY_UID"
	EnvWorkBuddyDomain       = "CODENEXUS_WORKBUDDY_DOMAIN"
	EnvWorkBuddyExpiresAt    = "CODENEXUS_WORKBUDDY_EXPIRES_AT"
	EnvWorkBuddyExpiresIn    = "CODENEXUS_WORKBUDDY_EXPIRES_IN"
)

// Trae CN variables. They are namespaced rather than reusing the names the
// Trae IDE itself exports, so running CodeNexus next to the client cannot pick
// up a credential by accident.
const (
	EnvTraeToken        = "CODENEXUS_TRAE_TOKEN"
	EnvTraeRefreshToken = "CODENEXUS_TRAE_REFRESH_TOKEN"
	EnvTraeUID          = "CODENEXUS_TRAE_UID"
	EnvTraeExpiresAt    = "CODENEXUS_TRAE_EXPIRES_AT"
	EnvTraeExpiresIn    = "CODENEXUS_TRAE_EXPIRES_IN"
	EnvTraeLabel        = "CODENEXUS_TRAE_LABEL"
)

// AccountsFromEnv builds ephemeral accounts supplied through the environment.
// The returned accounts are not persisted; they live only for this process.
func AccountsFromEnv() []*account.Account {
	var out []*account.Account
	if acc := workBuddyFromEnv(); acc != nil {
		out = append(out, acc)
	}
	if acc := traeFromEnv(); acc != nil {
		out = append(out, acc)
	}
	return out
}

func workBuddyFromEnv() *account.Account {
	token := strings.TrimSpace(os.Getenv(EnvWorkBuddyToken))
	uid := strings.TrimSpace(os.Getenv(EnvWorkBuddyUID))
	if token == "" || uid == "" {
		return nil
	}
	return &account.Account{
		Source:       account.SourceWorkBuddy,
		UID:          uid,
		Label:        "env",
		Enabled:      true,
		Ephemeral:    true,
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(os.Getenv(EnvWorkBuddyRefreshToken)),
		Domain:       strings.TrimSpace(os.Getenv(EnvWorkBuddyDomain)),
		ExpiresAt:    envExpiry(EnvWorkBuddyExpiresAt, EnvWorkBuddyExpiresIn),
	}
}

func traeFromEnv() *account.Account {
	token := strings.TrimSpace(os.Getenv(EnvTraeToken))
	if token == "" {
		return nil
	}
	uid := strings.TrimSpace(os.Getenv(EnvTraeUID))
	label := strings.TrimSpace(os.Getenv(EnvTraeLabel))
	if label == "" {
		label = "env"
	}
	return &account.Account{
		Source:       account.SourceTraeCN,
		UID:          uid,
		Label:        label,
		Enabled:      true,
		Ephemeral:    true,
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(os.Getenv(EnvTraeRefreshToken)),
		ExpiresAt:    envExpiry(EnvTraeExpiresAt, EnvTraeExpiresIn),
	}
}

func envExpiry(atVar, inVar string) int64 {
	if v := strings.TrimSpace(os.Getenv(atVar)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	if v := strings.TrimSpace(os.Getenv(inVar)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Now().Add(time.Duration(n) * time.Second).Unix()
		}
	}
	return 0
}
