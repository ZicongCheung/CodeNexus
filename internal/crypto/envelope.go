// Package crypto provides envelope encryption for credentials and a pluggable
// key keeper that binds the wrapping key to the host.
//
// The on-disk envelope uses a per-record data key (DEK) that is wrapped by a
// key-encryption key (KEK). Keeping the payload behind an indirection means the
// KEK can later be rotated (or re-wrapped for another platform) without
// re-encrypting every payload.
//
// Two sealing modes exist:
//
//   - Seal/Open use a KEK obtained from a Keeper (bound to this machine).
//   - SealWithPassphrase/OpenWithPassphrase derive the KEK from a user
//     passphrase, which is what the portable export bundle uses.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// EnvelopeVersion is the current on-disk format version.
	EnvelopeVersion = 1
	// Algorithm names the payload cipher.
	Algorithm = "AES-256-GCM"
	// KDFPBKDF2SHA256 names the passphrase key derivation function.
	KDFPBKDF2SHA256 = "pbkdf2-sha256"

	// KeySize is the length of the KEK and DEK in bytes (AES-256).
	KeySize = 32
	// DefaultIterations is the PBKDF2 iteration count for passphrase sealing.
	DefaultIterations = 600_000

	nonceSize = 12
	saltSize  = 16
)

// Errors returned by this package.
var (
	// ErrDecrypt means the envelope could not be authenticated/decrypted.
	ErrDecrypt = errors.New("crypto: decryption failed")
	// ErrFormat means the blob is not a recognizable envelope.
	ErrFormat = errors.New("crypto: unrecognized envelope")
	// ErrPassphrase means a passphrase-protected envelope was opened without one.
	ErrPassphrase = errors.New("crypto: envelope requires a passphrase")
)

// KDFParams records the key derivation used for a passphrase-sealed envelope.
type KDFParams struct {
	Name string `json:"name"`
	Iter int    `json:"iter"`
	Salt string `json:"salt"`
}

// Envelope is the JSON on-disk representation of one sealed record.
type Envelope struct {
	V   int    `json:"v"`
	Alg string `json:"alg"`
	// WN/WK wrap the per-record DEK with the KEK.
	WN string `json:"wn,omitempty"`
	WK string `json:"wk,omitempty"`
	// N/CT are the payload nonce and ciphertext.
	N  string `json:"n"`
	CT string `json:"ct"`
	// KDF is present only for passphrase-sealed records.
	KDF *KDFParams `json:"kdf,omitempty"`
}

// Seal encrypts plaintext under a KEK held by the caller (machine-bound).
func Seal(kek, plaintext []byte) ([]byte, error) { return seal(kek, nil, plaintext) }

// Open decrypts a KEK-sealed envelope. It rejects passphrase-sealed records.
func Open(kek, blob []byte) ([]byte, error) {
	env, err := Parse(blob)
	if err != nil {
		return nil, err
	}
	if env.KDF != nil {
		return nil, ErrPassphrase
	}
	return openWithKEK(kek, env)
}

// SealWithPassphrase encrypts plaintext under a key derived from passphrase
// using DefaultIterations PBKDF2 rounds.
func SealWithPassphrase(passphrase string, plaintext []byte) ([]byte, error) {
	return SealWithPassphraseIter(passphrase, plaintext, DefaultIterations)
}

// SealWithPassphraseIter is SealWithPassphrase with an explicit iteration count.
func SealWithPassphraseIter(passphrase string, plaintext []byte, iter int) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("crypto: empty passphrase")
	}
	if iter <= 0 {
		iter = DefaultIterations
	}
	salt, err := randomBytes(saltSize)
	if err != nil {
		return nil, err
	}
	kek, err := derive(passphrase, salt, iter)
	if err != nil {
		return nil, err
	}
	return seal(kek, &KDFParams{Name: KDFPBKDF2SHA256, Iter: iter, Salt: b64(salt)}, plaintext)
}

// OpenWithPassphrase decrypts a passphrase-sealed envelope.
func OpenWithPassphrase(passphrase string, blob []byte) ([]byte, error) {
	env, err := Parse(blob)
	if err != nil {
		return nil, err
	}
	if env.KDF == nil {
		return nil, fmt.Errorf("%w: not passphrase-protected", ErrFormat)
	}
	if env.KDF.Name != KDFPBKDF2SHA256 {
		return nil, fmt.Errorf("%w: unsupported kdf %q", ErrFormat, env.KDF.Name)
	}
	salt, err := unb64(env.KDF.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: bad salt", ErrFormat)
	}
	kek, err := derive(passphrase, salt, env.KDF.Iter)
	if err != nil {
		return nil, err
	}
	return openWithKEK(kek, env)
}

// Parse decodes and validates the envelope structure without decrypting it.
func Parse(blob []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(blob, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if env.V != EnvelopeVersion || env.Alg != Algorithm || env.CT == "" || env.N == "" {
		return nil, ErrFormat
	}
	return &env, nil
}

func seal(kek []byte, kdf *KDFParams, plaintext []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, fmt.Errorf("crypto: kek must be %d bytes, got %d", KeySize, len(kek))
	}
	dek, err := randomBytes(KeySize)
	if err != nil {
		return nil, err
	}
	wrapNonce, err := randomBytes(nonceSize)
	if err != nil {
		return nil, err
	}
	wrapped, err := gcmSeal(kek, wrapNonce, dek)
	if err != nil {
		return nil, err
	}
	nonce, err := randomBytes(nonceSize)
	if err != nil {
		return nil, err
	}
	ct, err := gcmSeal(dek, nonce, plaintext)
	if err != nil {
		return nil, err
	}
	env := Envelope{
		V:   EnvelopeVersion,
		Alg: Algorithm,
		WN:  b64(wrapNonce),
		WK:  b64(wrapped),
		N:   b64(nonce),
		CT:  b64(ct),
		KDF: kdf,
	}
	return json.Marshal(env)
}

func openWithKEK(kek []byte, env *Envelope) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, fmt.Errorf("crypto: kek must be %d bytes, got %d", KeySize, len(kek))
	}
	wrapNonce, err := unb64(env.WN)
	if err != nil {
		return nil, fmt.Errorf("%w: bad wrap nonce", ErrFormat)
	}
	wrapped, err := unb64(env.WK)
	if err != nil {
		return nil, fmt.Errorf("%w: bad wrapped key", ErrFormat)
	}
	dek, err := gcmOpen(kek, wrapNonce, wrapped)
	if err != nil {
		return nil, err
	}
	nonce, err := unb64(env.N)
	if err != nil {
		return nil, fmt.Errorf("%w: bad payload nonce", ErrFormat)
	}
	ct, err := unb64(env.CT)
	if err != nil {
		return nil, fmt.Errorf("%w: bad ciphertext", ErrFormat)
	}
	return gcmOpen(dek, nonce, ct)
}

func derive(passphrase string, salt []byte, iter int) ([]byte, error) {
	if iter <= 0 {
		return nil, errors.New("crypto: invalid iteration count")
	}
	return pbkdf2.Key(sha256.New, passphrase, salt, iter, KeySize)
}

func gcmSeal(key, nonce, plaintext []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, nil), nil
}

func gcmOpen(key, nonce, ciphertext []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	out, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }
