package authstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

func TestSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "tok", Enabled: true}

	if err := s.Save(acc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, acc.FileName())); err != nil {
		t.Fatalf("credential file missing: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].UID != "u1" || got[0].AccessToken != "tok" {
		t.Fatalf("loaded = %+v", got)
	}

	if err := s.Delete(acc); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ = s.Load()
	if len(got) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(got))
	}
}

func TestLoadMissingDirIsNil(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "nope"))
	got, err := s.Load()
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSourceInferredFromFileName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-x.json"), []byte(`{"uid":"x","enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Source != account.SourceWorkBuddy || got[0].UID != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(&account.Account{Source: "bogus", UID: "u"}); err == nil {
		t.Fatal("expected unknown source to fail")
	}
	if err := Validate(&account.Account{Source: account.SourceTraeCN}); err == nil {
		t.Fatal("expected empty uid to fail")
	}
	if err := Validate(&account.Account{Source: account.SourceTraeCN, UID: "u"}); err != nil {
		t.Fatalf("valid account rejected: %v", err)
	}
	if err := Validate(nil); err == nil {
		t.Fatal("expected nil to fail")
	}
}
