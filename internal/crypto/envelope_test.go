package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	kek, err := randomBytes(KeySize)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"uid":"u1","access_token":"secret"}`)

	blob, err := Seal(kek, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, []byte("secret")) {
		t.Fatal("ciphertext leaks plaintext")
	}

	got, err := Open(kek, blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestSealIsRandomized(t *testing.T) {
	kek, _ := randomBytes(KeySize)
	a, _ := Seal(kek, []byte("same"))
	b, _ := Seal(kek, []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of identical plaintext must differ (nonce/DEK)")
	}
}

func TestOpenWrongKey(t *testing.T) {
	kek, _ := randomBytes(KeySize)
	other, _ := randomBytes(KeySize)
	blob, _ := Seal(kek, []byte("x"))

	if _, err := Open(other, blob); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestSealRejectsBadKeySize(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestPassphraseRoundTrip(t *testing.T) {
	plaintext := []byte("bundle payload")
	blob, err := SealWithPassphraseIter("correct horse", plaintext, 1000)
	if err != nil {
		t.Fatalf("SealWithPassphrase: %v", err)
	}
	got, err := OpenWithPassphrase("correct horse", blob)
	if err != nil {
		t.Fatalf("OpenWithPassphrase: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("mismatch: %q", got)
	}
	if _, err := OpenWithPassphrase("wrong horse", blob); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want ErrDecrypt on wrong passphrase, got %v", err)
	}
}

func TestPassphraseEnvelopeRejectedByKeeperOpen(t *testing.T) {
	kek, _ := randomBytes(KeySize)
	blob, _ := SealWithPassphraseIter("pw", []byte("x"), 1000)
	if _, err := Open(kek, blob); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("want ErrPassphrase, got %v", err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, blob := range [][]byte{nil, []byte("not json"), []byte(`{"v":1}`), []byte(`{"v":99,"alg":"x","n":"a","ct":"b"}`)} {
		if _, err := Parse(blob); err == nil {
			t.Fatalf("expected ErrFormat for %q", blob)
		}
	}
}
