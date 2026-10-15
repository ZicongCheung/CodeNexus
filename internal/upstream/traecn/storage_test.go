package traecn

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// pkcs7Pad is the inverse of the unpad the reader applies; the test needs to
// build a blob the same way the desktop client does.
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// buildStorageBlob encrypts payload the way the client stores it, so the
// decryptor can be checked against a blob this test made itself rather than
// against a captured fixture.
func buildStorageBlob(t *testing.T, header []byte, salt [64]byte, payload string) string {
	t.Helper()
	random := bytes.Repeat([]byte{7}, storageRandomLen)
	key, iv := deriveStorageKeyIV(random, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	sum := sha512.Sum512([]byte(payload))
	body := append(sum[:], pkcs7Pad([]byte(payload), aes.BlockSize)...)
	sealed := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, body)

	blob := make([]byte, 0, len(header)+len(random)+len(sealed))
	blob = append(blob, header...)
	blob = append(blob, random...)
	blob = append(blob, sealed...)
	return base64.StdEncoding.EncodeToString(blob)
}

func TestStorageDecryptRoundTrip(t *testing.T) {
	payload := `{"token":"abc","userId":"u-1"}`
	encoded := buildStorageBlob(t, headerStorage, xorSalt(saltA, saltB), payload)

	got, err := decryptStorageValue(encoded)
	if err != nil {
		t.Fatalf("decryptStorageValue: %v", err)
	}
	if got != payload {
		t.Fatalf("plaintext %q, want %q", got, payload)
	}
}

func TestStorageDecryptPrivateHeaderUsesItsOwnSaltPair(t *testing.T) {
	payload := `{"token":"abc"}`
	encoded := buildStorageBlob(t, headerPrivate, xorSalt(saltC, saltD), payload)

	if got, err := decryptStorageValue(encoded); err != nil || got != payload {
		t.Fatalf("private envelope: got %q err %v", got, err)
	}
}

func TestStorageDecryptRejectsWrongSalt(t *testing.T) {
	// The header promises the "tc" salt pair; the blob was sealed with the
	// private pair. The self-describing digest is the only thing that catches
	// this, so it must fail loudly rather than yield garbage.
	encoded := buildStorageBlob(t, headerStorage, xorSalt(saltC, saltD), `{"token":"abc"}`)

	if _, err := decryptStorageValue(encoded); err == nil {
		t.Fatal("a mismatched salt must fail the integrity check")
	}
}

func TestStorageDecryptRejectsUnknownHeader(t *testing.T) {
	encoded := buildStorageBlob(t, []byte{1, 2, 3, 4, 5, 6}, xorSalt(saltA, saltB), `{"token":"abc"}`)

	if _, err := decryptStorageValue(encoded); err == nil {
		t.Fatal("expected an unknown-format error")
	}
}

func TestStorageDecryptRejectsGarbage(t *testing.T) {
	if _, err := decryptStorageValue("not-base64-@@@"); err == nil {
		t.Fatal("expected a base64 error")
	}
	if _, err := decryptStorageValue(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected a too-short error")
	}
}

// writeStore lays out a storage.json the way the desktop client does, using the
// plain-JSON branch so the test does not depend on the encryptor.
func writeStore(t *testing.T, dir string, value string) {
	t.Helper()
	path := filepath.Join(dir, "globalStorage")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := map[string]any{storageKey: value}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "storage.json"), b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReadStorageAuthParsesPlainJSON(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir, `{"token":"`+fakeToken("u-9")+`","refreshToken":"rt","userId":"u-9","expiredAt":"2030-01-01T00:00:00Z"}`)

	auth, err := readStorageAuth(dir)
	if err != nil {
		t.Fatalf("readStorageAuth: %v", err)
	}
	if auth.token != fakeToken("u-9") || auth.refreshToken != "rt" || auth.userID != "u-9" {
		t.Fatalf("parsed %+v", auth)
	}
	if !auth.stillValid() {
		t.Fatalf("a 2030 expiry should still be valid: %+v", auth)
	}
}

func TestReadStorageAuthDetectsEncryptedValue(t *testing.T) {
	dir := t.TempDir()
	payload := `{"token":"` + fakeToken("u-enc") + `","userId":"u-enc"}`
	writeStore(t, dir, buildStorageBlob(t, headerStorage, xorSalt(saltA, saltB), payload))

	auth, err := readStorageAuth(dir)
	if err != nil {
		t.Fatalf("readStorageAuth: %v", err)
	}
	if auth.userID != "u-enc" {
		t.Fatalf("parsed %+v", auth)
	}
}

func TestReadStorageAuthMissingFile(t *testing.T) {
	_, err := readStorageAuth(t.TempDir())
	if !os.IsNotExist(err) {
		t.Fatalf("want os.ErrNotExist for a missing file, got %v", err)
	}
}

func TestDiscoverLocalImportsFromExplicitDirs(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir, `{"token":"`+fakeToken("u-local")+`","refreshToken":"rt","userId":"u-local"}`)

	c := New(Config{LocalDataDirs: []string{dir}})
	accounts, err := c.DiscoverLocal()
	if err != nil {
		t.Fatalf("DiscoverLocal: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("imported %d accounts, want 1", len(accounts))
	}
	acc := accounts[0]
	if acc.Source != account.SourceTraeCN || acc.UID != "u-local" {
		t.Fatalf("account %+v", acc)
	}
	if acc.Label != EditionCN.label() {
		t.Fatalf("label %q, want %q", acc.Label, EditionCN.label())
	}
	if acc.RefreshTokenValue() != "rt" {
		t.Fatalf("refresh token not carried over")
	}
}

func TestDiscoverLocalDeduplicatesByUID(t *testing.T) {
	token := fakeToken("dup")
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeStore(t, dir1, `{"token":"`+token+`","userId":"dup"}`)
	writeStore(t, dir2, `{"token":"`+token+`","userId":"dup"}`)

	c := New(Config{LocalDataDirs: []string{dir1, dir2}})
	accounts, err := c.DiscoverLocal()
	if err != nil {
		t.Fatalf("DiscoverLocal: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("imported %d accounts, want 1", len(accounts))
	}
}

func TestDiscoverLocalPrefersAStillValidSession(t *testing.T) {
	stale, live := t.TempDir(), t.TempDir()
	writeStore(t, stale, `{"token":"`+fakeToken("stale")+`","userId":"stale","expiredAt":"2020-01-01T00:00:00Z"}`)
	writeStore(t, live, `{"token":"`+fakeToken("live")+`","userId":"live","expiredAt":"`+time.Now().Add(24*time.Hour).Format(time.RFC3339)+`"}`)

	c := New(Config{LocalDataDirs: []string{stale, live}})
	accounts, err := c.DiscoverLocal()
	if err != nil {
		t.Fatalf("DiscoverLocal: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("imported %d accounts, want 2", len(accounts))
	}
	if accounts[0].UID != "live" {
		t.Fatalf("first account is %q, want the still-valid one", accounts[0].UID)
	}
}

func TestDiscoverLocalReportsWhenNothingIsFound(t *testing.T) {
	c := New(Config{LocalDataDirs: []string{t.TempDir()}})
	if _, err := c.DiscoverLocal(); err == nil {
		t.Fatal("expected ErrStorageNotFound")
	}
}

func TestDiscoverLocalCarriesAMismatchedHost(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir, `{"token":"`+fakeToken("u-host")+`","userId":"u-host","host":"https://other.example"}`)

	c := New(Config{LocalDataDirs: []string{dir}})
	accounts, err := c.DiscoverLocal()
	if err != nil {
		t.Fatalf("DiscoverLocal: %v", err)
	}
	if accounts[0].DomainValue() != "https://other.example" {
		t.Fatalf("host override not surfaced: %q", accounts[0].DomainValue())
	}
}

func TestDiscoverLocalDropsMismatchedHostWhenItMatches(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir, `{"token":"`+fakeToken("u-same")+`","userId":"u-same","host":"`+DefaultBaseURL+`"}`)

	c := New(Config{LocalDataDirs: []string{dir}})
	accounts, err := c.DiscoverLocal()
	if err != nil {
		t.Fatalf("DiscoverLocal: %v", err)
	}
	if accounts[0].DomainValue() != "" {
		t.Fatalf("a matching host must not be recorded as an override: %q", accounts[0].DomainValue())
	}
}

func TestAccountFromAuthRejectsMalformedToken(t *testing.T) {
	c := New(Config{})
	if _, err := c.accountFromAuth(&storageAuth{token: "short"}, EditionCN); err == nil {
		t.Fatal("expected a malformed-token error")
	}
	if _, err := c.accountFromAuth(&storageAuth{token: fakeToken("u")}, EditionCN); err != nil {
		t.Fatalf("a valid token with a derivable uid must be accepted: %v", err)
	}
}

func TestEditionMainlandScope(t *testing.T) {
	for _, e := range []Edition{EditionCN, EditionSoloCN} {
		if !e.mainland() {
			t.Fatalf("%s should be imported", e)
		}
	}
	for _, e := range []Edition{EditionSG, EditionSoloSG} {
		if e.mainland() {
			t.Fatalf("%s talks to a different host family and must not be imported", e)
		}
	}
}

func TestSaltPairsDiffer(t *testing.T) {
	if xorSalt(saltA, saltB) == xorSalt(saltC, saltD) {
		t.Fatal("the two salt pairs must not collapse into one")
	}
}
