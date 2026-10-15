package authstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/crypto"
)

func TestEncryptedSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, WithKeeper(crypto.NewMemoryKeeper()))
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "super-secret", Enabled: true}

	if err := s.Save(acc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, acc.FileName()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "super-secret") {
		t.Fatal("credential file leaks plaintext token")
	}
	if !strings.Contains(string(blob), encKey) {
		t.Fatalf("file is not an envelope: %s", blob)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].AccessToken != "super-secret" {
		t.Fatalf("loaded = %+v", got)
	}
}

func TestLoadEncryptedWithoutKeeperSkips(t *testing.T) {
	dir := t.TempDir()
	enc := New(dir, WithKeeper(crypto.NewMemoryKeeper()))
	if err := enc.Save(&account.Account{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "t", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A store without a keeper cannot read sealed files; it must skip, not crash.
	got, err := New(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 readable accounts, got %d", len(got))
	}
}

func TestMigratePlaintextCredentials(t *testing.T) {
	dir := t.TempDir()
	plain := New(dir) // no keeper: writes legacy plaintext
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "legacy", AccessToken: "plain-token", Enabled: true}
	if err := plain.Save(acc); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, acc.FileName())
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "plain-token") {
		t.Fatal("precondition: expected plaintext file")
	}

	enc := New(dir, WithKeeper(crypto.NewMemoryKeeper()))
	n, err := enc.Migrate()
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "plain-token") {
		t.Fatal("plaintext token survived migration")
	}
	// Idempotent: nothing left to migrate.
	if n, _ := enc.Migrate(); n != 0 {
		t.Fatalf("second Migrate = %d, want 0", n)
	}
	got, _ := enc.Load()
	if len(got) != 1 || got[0].AccessToken != "plain-token" {
		t.Fatalf("loaded = %+v", got)
	}
}

func TestMigrateWithoutKeeperIsNoop(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(&account.Account{Source: account.SourceWorkBuddy, UID: "u", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Migrate(); err != nil || n != 0 {
		t.Fatalf("Migrate = %d, %v; want 0, nil", n, err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	accts := []*account.Account{
		{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "t1", Enabled: true},
		{Source: account.SourceWorkBuddy, UID: "u2", AccessToken: "t2", Enabled: true},
	}
	blob, err := Export(accts, "hunter2")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(string(blob), "t1") {
		t.Fatal("export leaks plaintext")
	}
	got, err := Import(blob, "hunter2")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 2 || got[0].UID != "u1" || got[1].AccessToken != "t2" {
		t.Fatalf("imported = %+v", got)
	}
	if _, err := Import(blob, "wrong"); err == nil {
		t.Fatal("expected wrong passphrase to fail")
	}
	if _, err := Export(accts, ""); err == nil {
		t.Fatal("expected empty passphrase to fail")
	}
}

func TestImportRejectsPlaintextDocument(t *testing.T) {
	if _, err := Import([]byte(`{"uid":"u"}`), "pw"); err == nil {
		t.Fatal("expected non-envelope document to be rejected")
	}
}

func TestAccountsFromEnv(t *testing.T) {
	t.Setenv(EnvWorkBuddyToken, "tok")
	t.Setenv(EnvWorkBuddyUID, "u9")
	t.Setenv(EnvWorkBuddyRefreshToken, "rt")
	t.Setenv(EnvWorkBuddyExpiresIn, "3600")

	got := AccountsFromEnv()
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].UID != "u9" || got[0].AccessToken != "tok" || got[0].RefreshToken != "rt" {
		t.Fatalf("got %+v", got[0])
	}
	if got[0].ExpiresAt <= 0 {
		t.Fatal("expected expiry from EXPIRES_IN")
	}

	t.Setenv(EnvWorkBuddyToken, "")
	if len(AccountsFromEnv()) != 0 {
		t.Fatal("expected no account without a token")
	}
}

func TestTraeFromEnv(t *testing.T) {
	// Isolate from any ambient variables so the assertions below are exact.
	t.Setenv(EnvWorkBuddyToken, "")
	t.Setenv(EnvTraeToken, "")
	t.Setenv(EnvTraeRefreshToken, "")
	t.Setenv(EnvTraeUID, "")
	t.Setenv(EnvTraeLabel, "")
	t.Setenv(EnvTraeExpiresAt, "")
	t.Setenv(EnvTraeExpiresIn, "")

	t.Setenv(EnvTraeToken, "trae-token")
	t.Setenv(EnvTraeUID, "t7")
	t.Setenv(EnvTraeRefreshToken, "trae-refresh")
	t.Setenv(EnvTraeLabel, "my-trae")
	t.Setenv(EnvTraeExpiresIn, "120")

	got := AccountsFromEnv()
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	acc := got[0]
	if acc.Source != account.SourceTraeCN {
		t.Fatalf("source = %q", acc.Source)
	}
	if acc.UID != "t7" || acc.AccessToken != "trae-token" || acc.RefreshToken != "trae-refresh" {
		t.Fatalf("got %+v", acc)
	}
	if acc.Label != "my-trae" {
		t.Fatalf("label = %q", acc.Label)
	}
	if acc.ExpiresAt <= 0 {
		t.Fatal("expected expiry from EXPIRES_IN")
	}
	// An ephemeral account is usable but never written to disk.
	if !acc.Ephemeral {
		t.Fatal("env account should be ephemeral")
	}

	t.Setenv(EnvTraeToken, "")
	t.Setenv(EnvWorkBuddyToken, "")
	if len(AccountsFromEnv()) != 0 {
		t.Fatal("expected no account without a token")
	}
}
