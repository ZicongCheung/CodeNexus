package traecn

// Local-client credential discovery.
//
// The Trae desktop client keeps its session in storage.json, under a single key
// whose value is a self-contained encrypted blob. Reading it lets a user add an
// account they are already signed into, with no token copying at all.
//
// Two facts make this safe and portable, and both were verified against the
// reference implementation rather than assumed:
//
//   - The key is derived only from bytes carried inside the blob plus two fixed
//     salts. No machine id, registry value or device fingerprint participates,
//     so the same storage.json decrypts on any machine.
//   - The plaintext is prefixed with its own SHA-512, so a wrong salt is
//     detectable instead of silently yielding garbage.
//
// Discovery is deliberately a single explicit action in the console, never a
// background scan: reading another application's credentials should be
// something the user asked for, not something that happens quietly.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// storageKey is the storage.json entry holding the encrypted session.
const storageKey = "iCubeAuthInfo://icube.cloudide"

// Blob layout: [6B header][32B random][N ciphertext].
const (
	storageHeaderLen = 6
	storageRandomLen = 32
	storageDigestLen = sha512.Size
)

// The two known envelope headers. The first is the common "tc" format, the
// second a private variant that takes a different salt pair.
var (
	headerStorage = []byte{'t', 'c', 0x05, 0x10, 0x00, 0x00}
	headerPrivate = []byte{18, 57, 32, 32, 2, 3}
)

// Fixed 64-byte salts. The key is derived from one XOR pair, so these are part
// of the wire format and must not be "tidied".
//
//nolint:gochecknoglobals // format constants
var (
	saltA = [64]byte{
		82, 9, 106, 213, 48, 54, 165, 56,
		191, 64, 163, 158, 129, 243, 215, 251,
		124, 227, 57, 130, 155, 47, 255, 135,
		52, 142, 67, 68, 196, 222, 233, 203,
		84, 123, 148, 50, 166, 194, 35, 61,
		238, 76, 149, 11, 66, 250, 195, 78,
		8, 46, 161, 102, 40, 217, 36, 178,
		118, 91, 162, 73, 109, 139, 209, 37,
	}
	saltB = [64]byte{
		31, 221, 168, 51, 136, 7, 199, 49,
		177, 18, 16, 89, 39, 128, 236, 95,
		96, 81, 127, 169, 25, 181, 74, 13,
		45, 229, 122, 159, 147, 201, 156, 239,
		160, 224, 59, 77, 174, 42, 245, 176,
		200, 235, 187, 60, 131, 83, 153, 97,
		23, 43, 4, 126, 186, 119, 214, 38,
		225, 105, 20, 99, 85, 33, 12, 125,
	}
	saltC = [64]byte{
		191, 192, 216, 250, 122, 246, 220, 97,
		31, 254, 98, 27, 8, 72, 71, 176,
		135, 99, 96, 18, 127, 101, 203, 104,
		211, 102, 191, 125, 37, 72, 150, 156,
		51, 229, 121, 35, 17, 153, 141, 177,
		110, 131, 150, 128, 172, 255, 254, 6,
		18, 140, 55, 62, 236, 249, 135, 64,
		135, 12, 117, 4, 89, 149, 168, 209,
	}
	saltD = [64]byte{
		246, 204, 26, 232, 232, 70, 129, 109,
		223, 146, 169, 242, 23, 241, 105, 145,
		50, 196, 165, 42, 254, 120, 3, 54,
		244, 207, 209, 85, 53, 6, 138, 106,
		175, 148, 31, 204, 186, 186, 165, 182,
		87, 142, 49, 10, 39, 110, 26, 154,
		86, 56, 173, 125, 18, 64, 198, 225,
		99, 99, 83, 82, 191, 134, 76, 170,
	}
)

// ErrStorageNotFound reports that no readable local client store was found.
var ErrStorageNotFound = errors.New("trae: no local client credential found")

// Edition identifies which desktop product a store belongs to. Only the two
// mainland editions map onto this adapter: the international editions talk to a
// different host family, so importing them here would route them wrongly.
type Edition string

const (
	// EditionCN is the mainland Trae client.
	EditionCN Edition = "cn"
	// EditionSoloCN is the mainland Trae SOLO client.
	EditionSoloCN Edition = "solo"
	// EditionSG and EditionSoloSG are international; detected but not imported.
	EditionSG     Edition = "sg"
	EditionSoloSG Edition = "solo-sg"
)

// mainland reports whether the edition belongs to this adapter's host family.
func (e Edition) mainland() bool { return e == EditionCN || e == EditionSoloCN }

// label is the user-facing name shown on the imported account.
func (e Edition) label() string {
	switch e {
	case EditionCN:
		return "本机 Trae CN"
	case EditionSoloCN:
		return "本机 Trae SOLO CN"
	case EditionSG:
		return "本机 Trae 国际版"
	case EditionSoloSG:
		return "本机 Trae SOLO 国际版"
	default:
		return "本机 Trae"
	}
}

// localStore is one candidate storage location.
type localStore struct {
	edition Edition
	dir     string
}

// defaultLocalDirs lists the desktop client's per-user data directories, in the
// order they should be tried. APPDATA is the Windows location; the home-relative
// fallback covers environments where it is unset.
func defaultLocalDirs() []localStore {
	base := strings.TrimSpace(os.Getenv("APPDATA"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "AppData", "Roaming")
		}
	}
	if base == "" {
		return nil
	}
	return []localStore{
		{EditionCN, filepath.Join(base, "Trae CN", "User")},
		{EditionSoloCN, filepath.Join(base, "TRAE SOLO CN", "User")},
		{EditionSG, filepath.Join(base, "Trae", "User")},
		{EditionSoloSG, filepath.Join(base, "TRAE SOLO", "User")},
	}
}

// localDirs resolves the candidate list, preferring an explicit override so a
// user can point at a copied store and so tests never touch a real profile.
func (c *Client) localDirs() []localStore {
	if len(c.cfg.LocalDataDirs) == 0 {
		return defaultLocalDirs()
	}
	out := make([]localStore, 0, len(c.cfg.LocalDataDirs))
	for _, dir := range c.cfg.LocalDataDirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			// An explicit directory is assumed to be a mainland store: the user
			// named it, so second-guessing the edition would only confuse.
			out = append(out, localStore{EditionCN, dir})
		}
	}
	return out
}

// storageAuth is the session the desktop client stored.
type storageAuth struct {
	token            string
	refreshToken     string
	userID           string
	expiresAt        string
	refreshExpiresAt string
	host             string
	clientID         string
}

// DiscoverLocal reads every readable local store and turns it into accounts
// ready for the pool. Stores it cannot use are reported, not silently skipped,
// so the console can explain what happened.
func (c *Client) DiscoverLocal() ([]*account.Account, error) {
	stores := c.localDirs()
	if len(stores) == 0 {
		return nil, ErrStorageNotFound
	}
	var (
		out     []*account.Account
		seen    = map[string]bool{}
		fresh   *account.Account
		reasons []string
	)
	for _, st := range stores {
		auth, err := readStorageAuth(st.dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				reasons = append(reasons, fmt.Sprintf("%s: %v", st.edition, err))
			}
			continue
		}
		if !st.edition.mainland() {
			reasons = append(reasons, fmt.Sprintf("%s: 国际版不属于 Trae CN 来源，未导入", st.edition))
			continue
		}
		acc, err := c.accountFromAuth(auth, st.edition)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", st.edition, err))
			continue
		}
		if seen[acc.UID] {
			continue
		}
		seen[acc.UID] = true
		out = append(out, acc)
		// The first still-valid session becomes the account the pool prefers,
		// because a stale token costs a refresh round trip before it is usable.
		if fresh == nil && auth.stillValid() {
			fresh = acc
		}
	}
	if len(out) == 0 {
		if len(reasons) > 0 {
			return nil, fmt.Errorf("%w (%s)", ErrStorageNotFound, strings.Join(reasons, "; "))
		}
		return nil, ErrStorageNotFound
	}
	// Put the account with a live session first; the pool orders by its own
	// weighting afterwards, but a fresh credential is the better default.
	if fresh != nil && out[0] != fresh {
		for i, acc := range out {
			if acc == fresh {
				out[0], out[i] = out[i], out[0]
				break
			}
		}
	}
	return out, nil
}

// accountFromAuth converts a decrypted session into a pool account.
func (c *Client) accountFromAuth(auth *storageAuth, edition Edition) (*account.Account, error) {
	if auth == nil || strings.TrimSpace(auth.token) == "" {
		return nil, errors.New("store carried no token")
	}
	if !LooksLikeToken(auth.token) {
		return nil, errors.New("store carried a malformed token")
	}
	uid := strings.TrimSpace(auth.userID)
	if uid == "" {
		uid = TokenUserID(auth.token)
	}
	if uid == "" {
		return nil, errors.New("store carried no user id and none derivable from the token")
	}
	acc := &account.Account{
		Source:       account.SourceTraeCN,
		UID:          uid,
		Label:        edition.label(),
		Enabled:      true,
		AccessToken:  auth.token,
		RefreshToken: auth.refreshToken,
		ExpiresAt:    parseTimestamp(auth.expiresAt),
	}
	// The store may name its own host. The gateway keeps one configured host per
	// source, so a differing value is surfaced rather than silently honoured:
	// a mismatched host would otherwise look like an auth failure.
	if h := strings.TrimSpace(auth.host); h != "" && !sameHost(h, c.cfg.BaseURL) {
		acc.Domain = h
	}
	return acc, nil
}

// sameHost compares two endpoints ignoring a trailing slash and the scheme's
// case, which is all the precision this check needs.
func sameHost(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.TrimSuffix(s, "/"))
		return strings.ToLower(s)
	}
	return norm(a) == norm(b)
}

// stillValid reports whether the stored access token has meaningful life left.
// A five-minute buffer matches the refresh lead time used everywhere else, so a
// token that is about to expire is treated as stale here too.
func (a *storageAuth) stillValid() bool {
	if strings.TrimSpace(a.token) == "" {
		return false
	}
	exp := parseTimestamp(a.expiresAt)
	if exp == 0 {
		// No expiry recorded: assume usable rather than forcing a refresh.
		return true
	}
	return exp-time.Now().Unix() > 300
}

// readStorageAuth reads and decrypts one store. A missing file is reported as
// os.ErrNotExist so the caller can tell "not installed" from "unreadable".
func readStorageAuth(dir string) (*storageAuth, error) {
	path := filepath.Join(dir, "globalStorage", "storage.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("storage.json is not valid JSON: %w", err)
	}
	stored, _ := doc[storageKey].(string)
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return nil, fmt.Errorf("%s not present", storageKey)
	}
	plain := stored
	// The international client has been seen storing this value as plain JSON.
	if !strings.HasPrefix(stored, "{") {
		if plain, err = decryptStorageValue(stored); err != nil {
			return nil, err
		}
	}
	return parseStorageAuth(plain)
}

// decryptStorageValue decrypts one "tc" envelope into its plaintext JSON.
func decryptStorageValue(encoded string) (string, error) {
	blob, err := decodeBase64Loose(encoded)
	if err != nil {
		return "", fmt.Errorf("credential blob is not base64: %w", err)
	}
	if len(blob) < storageHeaderLen+storageRandomLen+aes.BlockSize {
		return "", errors.New("credential blob is too short")
	}
	header := blob[:storageHeaderLen]
	random := blob[storageHeaderLen : storageHeaderLen+storageRandomLen]
	body := blob[storageHeaderLen+storageRandomLen:]

	var salt [64]byte
	switch {
	case bytes.Equal(header, headerStorage):
		salt = xorSalt(saltA, saltB)
	case bytes.Equal(header, headerPrivate):
		salt = xorSalt(saltC, saltD)
	default:
		return "", fmt.Errorf("unknown credential format (header %x)", header)
	}

	key, iv := deriveStorageKeyIV(random, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	if len(body) == 0 || len(body)%aes.BlockSize != 0 {
		return "", errors.New("credential ciphertext is not block-aligned")
	}
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, body)

	// The plaintext carries its own digest, which is the only reliable way to
	// tell a correct decryption from a plausible-looking wrong one.
	if len(plain) < storageDigestLen {
		return "", errors.New("decrypted credential is too short")
	}
	digest, payload := plain[:storageDigestLen], pkcs7Unpad(plain[storageDigestLen:])
	sum := sha512.Sum512(payload)
	if !bytes.Equal(digest, sum[:]) {
		return "", errors.New("credential failed its integrity check")
	}
	return string(payload), nil
}

// deriveStorageKeyIV follows the documented two-step derivation: hash the
// per-blob random bytes, fold in the salt, then split the result.
func deriveStorageKeyIV(random []byte, salt [64]byte) (key, iv []byte) {
	first := sha512.Sum512(random)
	h := sha512.New()
	h.Write(first[:])
	h.Write(salt[:])
	final := h.Sum(nil)
	return final[:16], final[16:32]
}

func xorSalt(a, b [64]byte) [64]byte {
	var out [64]byte
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// pkcs7Unpad removes PKCS#7 padding, returning the input unchanged when the
// padding is not well formed. A wrong salt must fail the digest check, not be
// masked by a permissive unpad, so this stays deliberately strict.
func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

// decodeBase64Loose accepts the padded, unpadded and URL-safe alphabets, since
// the client has shipped more than one writer over time.
func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("unrecognized base64 alphabet")
}

// parseStorageAuth extracts the session fields, tolerating the alternate key
// spellings the client has used across releases.
func parseStorageAuth(plain string) (*storageAuth, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(plain)), &doc); err != nil {
		return nil, fmt.Errorf("decrypted credential is not JSON: %w", err)
	}
	auth := &storageAuth{
		token:            pickString(doc, "token", "Token"),
		refreshToken:     pickString(doc, "refreshToken", "RefreshToken"),
		userID:           pickString(doc, "userId", "UserID", "user_id", "id"),
		expiresAt:        pickString(doc, "expiredAt", "TokenExpireAt", "expiresAt"),
		refreshExpiresAt: pickString(doc, "refreshExpiredAt", "RefreshExpireAt"),
		host:             pickString(doc, "host", "Host"),
		clientID:         pickString(doc, "clientID", "ClientID", "clientId"),
	}
	if auth.token == "" && auth.refreshToken == "" {
		return nil, errors.New("decrypted credential carried no token or refresh token")
	}
	return auth, nil
}

// pickString returns the first non-empty value among keys, accepting numbers
// because the client stores some identities as JSON numbers.
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case json.Number:
			return v.String()
		}
	}
	return ""
}
