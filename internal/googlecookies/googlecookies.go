// Package googlecookies refreshes the Google account cookies inside
// OpenMessage's session.json from a local Chrome profile.
//
// Google-account libgm sessions authenticate with five .google.com account
// cookies (SID, HSID, SSID, APISID, and SAPISID) plus SAPISIDHASH. The cookies
// rotate roughly every 30 minutes. When the backend has been offline long
// enough (laptop asleep, travel), the stored cookies expire and every token
// refresh returns HTTP 401 — the session looks dead, but the phone-side device
// link is usually still intact. Rewriting auth_data.cookies with fresh values
// from the user's signed-in Chrome profile and reconnecting revives it with no
// re-pairing. A messages.google.com OSID cookie exists only when the user has
// used Messages for web in that Chrome profile; it is carried when present but
// is never required.
//
// This is the native (in-process) equivalent of
// scripts/refresh-google-session-cookies-{linux,macos}.py, so app installs
// self-heal without any external script configured.
package googlecookies

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/pbkdf2"
	_ "modernc.org/sqlite"
)

type requiredCookie struct {
	host string
	name string
}

var requiredCookies = []requiredCookie{
	{".google.com", "SID"},
	{".google.com", "HSID"},
	{".google.com", "SSID"},
	{".google.com", "APISID"},
	{".google.com", "SAPISID"},
}

var hostPriority = map[string]int{
	"messages.google.com": 0,
	".google.com":         1,
	"accounts.google.com": 2,
}

// DefaultChromeProfile returns the Chrome profile directory to read cookies
// from, honouring the same OPENMESSAGE_CHROME_PROFILE override as the
// standalone refresh scripts.
func DefaultChromeProfile() string {
	if p := strings.TrimSpace(os.Getenv("OPENMESSAGE_CHROME_PROFILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return defaultChromeProfileDir(home)
}

// Refresh reads Google cookies from the Chrome profile and rewrites
// auth_data.cookies in sessionPath. It never logs or returns cookie values.
func Refresh(ctx context.Context, profile, sessionPath string) error {
	if profile == "" {
		return fmt.Errorf("no Chrome profile directory")
	}
	secret, err := chromeSafeStorageSecret(ctx)
	if err != nil {
		return fmt.Errorf("chrome safe storage secret: %w", err)
	}
	cookies, err := LoadChromeCookies(profile, secret)
	if err != nil {
		return err
	}
	return UpdateSessionCookies(sessionPath, cookies)
}

// DeriveKey derives the AES-128 key Chrome uses for cookie encryption.
func DeriveKey(secret []byte, iterations int) []byte {
	return pbkdf2.Key(secret, []byte("saltysalt"), iterations, 16, sha1.New)
}

// DecryptCookie decrypts one Chrome encrypted_value. It returns ("", nil) for
// values without a v10/v11 prefix (unencrypted or unknown scheme).
func DecryptCookie(encrypted []byte, host string, key []byte) (string, error) {
	if len(encrypted) < 3 {
		return "", nil
	}
	prefix := string(encrypted[:3])
	if prefix != "v10" && prefix != "v11" {
		return "", nil
	}
	ciphertext := encrypted[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext length %d not a block multiple", len(ciphertext))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := []byte("                ") // 16 spaces
	padded := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(padded, ciphertext)

	padLen := int(padded[len(padded)-1])
	if padLen < 1 || padLen > aes.BlockSize || padLen > len(padded) {
		return "", fmt.Errorf("invalid cookie padding")
	}
	plain := padded[:len(padded)-padLen]

	// Chrome 130+ prepends SHA256(host_key) to the plaintext.
	hostHash := sha256.Sum256([]byte(host))
	if len(plain) >= 32 && strings.HasPrefix(string(plain), string(hostHash[:])) {
		plain = plain[32:]
	}
	if !utf8.Valid(plain) {
		return "", fmt.Errorf("decrypted cookie is not valid UTF-8")
	}
	return string(plain), nil
}

// LoadChromeCookies reads the profile's cookie DB and returns the decrypted
// Google cookies, trying both known PBKDF2 iteration counts and keeping the
// best-scoring result. The five .google.com account cookies are required;
// messages.google.com cookies such as OSID are carried only when present.
func LoadChromeCookies(profile string, secret []byte) (map[string]string, error) {
	dbCopy, cleanup, err := snapshotCookieDB(profile)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rows, err := readCookieRows(dbCopy)
	if err != nil {
		return nil, err
	}

	var best *attempt
	// 1003 is macOS, 1 is Linux/basic; trying both keeps the loader portable.
	for _, iterations := range []int{1003, 1} {
		key := DeriveKey(secret, iterations)
		att := &attempt{values: map[string]hostValue{}}
		for _, row := range rows {
			value := row.plainValue
			if value == "" && len(row.encrypted) > 0 {
				decrypted, err := DecryptCookie(row.encrypted, row.host, key)
				if err != nil {
					att.failed++
					continue
				}
				value = decrypted
			}
			if value == "" {
				continue
			}
			att.ok++
			prev, exists := att.values[row.name]
			if !exists || priorityOf(row.host) < priorityOf(prev.host) {
				att.values[row.name] = hostValue{host: row.host, value: value}
			}
		}
		for _, req := range requiredCookies {
			if hv, ok := att.values[req.name]; ok && hv.host == req.host {
				att.present++
			}
		}
		if best == nil || betterAttempt(att, best) {
			best = att
		}
	}

	var missing []string
	for _, req := range requiredCookies {
		if hv, ok := best.values[req.name]; !ok || hv.host != req.host {
			missing = append(missing, req.host+":"+req.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required cookies: %s", strings.Join(missing, ", "))
	}

	out := make(map[string]string, len(best.values))
	for name, hv := range best.values {
		out[name] = hv.value
	}
	return out, nil
}

type hostValue struct {
	host  string
	value string
}

type attempt struct {
	values  map[string]hostValue
	present int
	ok      int
	failed  int
}

type cookieRow struct {
	host       string
	name       string
	encrypted  []byte
	plainValue string
}

func priorityOf(host string) int {
	if p, ok := hostPriority[host]; ok {
		return p
	}
	return 10
}

func betterAttempt(a, b *attempt) bool {
	if a.present != b.present {
		return a.present > b.present
	}
	if a.ok != b.ok {
		return a.ok > b.ok
	}
	return a.failed < b.failed
}

// snapshotCookieDB copies the profile's cookie DB (with WAL/SHM sidecars) to
// a temp dir so we read the freshest values even while Chrome holds the live
// file — Google session cookies rotate ~30 minutes, so staleness matters.
func snapshotCookieDB(profile string) (string, func(), error) {
	candidates := []string{
		filepath.Join(profile, "Network", "Cookies"),
		filepath.Join(profile, "Cookies"),
	}
	var src string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			src = c
			break
		}
	}
	if src == "" {
		return "", nil, fmt.Errorf("chrome cookie DB not found under %s", profile)
	}

	tmpDir, err := os.MkdirTemp("", "om-cookie-refresh-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }
	dst := filepath.Join(tmpDir, "Cookies")
	if err := copyFile(src, dst); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		side := src + suffix
		if _, err := os.Stat(side); err == nil {
			if err := copyFile(side, dst+suffix); err != nil {
				cleanup()
				return "", nil, err
			}
		}
	}
	return dst, cleanup, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// readCookieRows opens the snapshot read-write so SQLite replays the WAL,
// making recently rotated cookies visible.
func readCookieRows(dbPath string) ([]cookieRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		select host_key, name, encrypted_value, value
		from cookies
		where host_key in ('.google.com','messages.google.com','accounts.google.com')
		   or host_key like '%.google.com'
		order by host_key, name`)
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	var out []cookieRow
	for rows.Next() {
		var row cookieRow
		if err := rows.Scan(&row.host, &row.name, &row.encrypted, &row.plainValue); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateSessionCookies rewrites auth_data.cookies in session.json atomically,
// preserving every other field.
func UpdateSessionCookies(sessionPath string, cookies map[string]string) error {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parse session: %w", err)
	}
	authData, ok := data["auth_data"].(map[string]any)
	if !ok {
		authData = map[string]any{}
		data["auth_data"] = authData
	}
	authData["cookies"] = cookies

	updated, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp := sessionPath + ".tmp"
	if err := os.WriteFile(tmp, append(updated, '\n'), 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("secure session: %w", err)
	}
	return os.Rename(tmp, sessionPath)
}
