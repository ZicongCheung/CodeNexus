package authstore

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// Environment variables that inject a WorkBuddy account without touching disk.
// This suits headless deployments where credentials come from the platform's
// secret manager rather than an interactive login.
const (
	EnvWorkBuddyToken        = "CODENEXUS_WORKBUDDY_TOKEN"
	EnvWorkBuddyRefreshToken = "CODENEXUS_WORKBUDDY_REFRESH_TOKEN"
	EnvWorkBuddyUID          = "CODENEXUS_WORKBUDDY_UID"
	EnvWorkBuddyDomain       = "CODENEXUS_WORKBUDDY_DOMAIN"
	EnvWorkBuddyExpiresAt    = "CODENEXUS_WORKBUDDY_EXPIRES_AT"
	EnvWorkBuddyExpiresIn    = "CODENEXUS_WORKBUDDY_EXPIRES_IN"
)

// AccountsFromEnv builds ephemeral accounts supplied through the environment.
// The returned accounts are not persisted; they live only for this process.
func AccountsFromEnv() []*account.Account {
	token := strings.TrimSpace(os.Getenv(EnvWorkBuddyToken))
	uid := strings.TrimSpace(os.Getenv(EnvWorkBuddyUID))
	if token == "" || uid == "" {
		return nil
	}
	acc := &account.Account{
		Source:       account.SourceWorkBuddy,
		UID:          uid,
		Label:        "env",
		Enabled:      true,
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(os.Getenv(EnvWorkBuddyRefreshToken)),
		Domain:       strings.TrimSpace(os.Getenv(EnvWorkBuddyDomain)),
		ExpiresAt:    envExpiry(),
	}
	return []*account.Account{acc}
}

func envExpiry() int64 {
	if v := strings.TrimSpace(os.Getenv(EnvWorkBuddyExpiresAt)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvWorkBuddyExpiresIn)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Now().Add(time.Duration(n) * time.Second).Unix()
		}
	}
	return 0
}
