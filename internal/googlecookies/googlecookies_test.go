package googlecookies

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

type dbCookie struct {
	host      string
	name      string
	encrypted []byte
}

// writeCookieDB builds a minimal Chrome-shaped cookies SQLite file.
func writeCookieDB(t *testing.T, path string, cookies []dbCookie) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open cookie db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table cookies (host_key text, name text, encrypted_value blob, value text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, c := range cookies {
		if _, err := db.Exec(
			`insert into cookies (host_key, name, encrypted_value, value) values (?, ?, ?, '')`,
			c.host, c.name, c.encrypted,
		); err != nil {
			t.Fatalf("insert cookie: %v", err)
		}
	}
}

// encryptCookie mirrors Chrome's v10 scheme so DecryptCookie can be tested
// without a live Chrome profile: AES-128-CBC, IV of 16 spaces, PKCS7 padding,
// optional SHA256(host) plaintext prefix (Chrome 130+).
func encryptCookie(t *testing.T, value, host string, key []byte, withHostHash bool) []byte {
	t.Helper()
	plain := []byte(value)
	if withHostHash {
		h := sha256.Sum256([]byte(host))
		plain = append(h[:], plain...)
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padLen; i++ {
		plain = append(plain, byte(padLen))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(out, plain)
	return append([]byte("v10"), out...)
}

func TestDeriveKeyChromePBKDF2Vectors(t *testing.T) {
	// Fixed Chrome vectors guard key derivation before Wave 1 changes credential storage.
	tests := []struct {
		iterations int
		want       string
	}{
		{iterations: 1, want: "d7d4df19d842591632e8dfb427ab3474"},
		{iterations: 1003, want: "01ab06dc67d036480129f3e40d53ca5f"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := hex.EncodeToString(DeriveKey([]byte("test-secret"), tt.iterations))
			if got != tt.want {
				t.Fatalf("DeriveKey(iterations=%d) = %s, want %s", tt.iterations, got, tt.want)
			}
		})
	}
}

func TestDecryptCookieRoundTrip(t *testing.T) {
	key := DeriveKey([]byte("test-secret"), 1003)
	for _, withHash := range []bool{false, true} {
		enc := encryptCookie(t, "cookie-value-123", ".google.com", key, withHash)
		got, err := DecryptCookie(enc, ".google.com", key)
		if err != nil {
			t.Fatalf("DecryptCookie(withHash=%v): %v", withHash, err)
		}
		if got != "cookie-value-123" {
			t.Fatalf("DecryptCookie(withHash=%v) = %q, want %q", withHash, got, "cookie-value-123")
		}
	}
}

func TestDecryptCookieV11(t *testing.T) {
	// Cookie refresh must keep accepting Chrome's v11 envelope through Wave 1.
	key := DeriveKey([]byte("test-secret"), 1003)
	encrypted := encryptCookie(t, "cookie-value-123", ".google.com", key, true)
	copy(encrypted[:3], "v11")

	got, err := DecryptCookie(encrypted, ".google.com", key)
	if err != nil {
		t.Fatalf("DecryptCookie(v11): %v", err)
	}
	if got != "cookie-value-123" {
		t.Fatalf("DecryptCookie(v11) = %q, want %q", got, "cookie-value-123")
	}
}

func TestDecryptCookieIgnoresUnencrypted(t *testing.T) {
	key := DeriveKey([]byte("test-secret"), 1003)
	got, err := DecryptCookie([]byte("plain"), ".google.com", key)
	if err != nil {
		t.Fatalf("DecryptCookie() unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("DecryptCookie() = %q, want empty for non-v10 value", got)
	}
}

func TestUpdateSessionCookiesPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	original := map[string]any{
		"phone_id": "abc123",
		"auth_data": map[string]any{
			"tachyon_token": "keep-me",
			"cookies":       map[string]any{"OLD": "stale"},
		},
	}
	raw, _ := json.Marshal(original)
	if err := os.WriteFile(sessionPath, raw, 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	newCookies := map[string]string{"SID": "fresh", "OSID": "fresh2"}
	if err := UpdateSessionCookies(sessionPath, newCookies); err != nil {
		t.Fatalf("UpdateSessionCookies(): %v", err)
	}

	updatedRaw, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("read updated session: %v", err)
	}
	var updated map[string]any
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatalf("parse updated session: %v", err)
	}
	if updated["phone_id"] != "abc123" {
		t.Fatalf("phone_id lost: %v", updated["phone_id"])
	}
	authData := updated["auth_data"].(map[string]any)
	if authData["tachyon_token"] != "keep-me" {
		t.Fatalf("tachyon_token lost: %v", authData["tachyon_token"])
	}
	cookies := authData["cookies"].(map[string]any)
	if cookies["SID"] != "fresh" || cookies["OSID"] != "fresh2" {
		t.Fatalf("cookies not replaced: %v", cookies)
	}
	if _, exists := cookies["OLD"]; exists {
		t.Fatalf("stale cookie survived replacement")
	}
}

func TestLoadChromeCookiesRequiresAllAccountCookies(t *testing.T) {
	// Exact host/name requirements guard Wave 1 from persisting partial credentials.
	secret := []byte("secret")
	key := DeriveKey(secret, 1003)

	for _, missing := range requiredCookies {
		missing := missing
		t.Run("missing "+missing.host+":"+missing.name, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "Default")
			if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
				t.Fatalf("mkdir profile: %v", err)
			}
			rows := make([]dbCookie, 0, len(requiredCookies)-1)
			for _, req := range requiredCookies {
				if req == missing {
					continue
				}
				rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "val-"+req.name, req.host, key, true)})
			}
			writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

			_, err := LoadChromeCookies(profile, secret)
			wantErr := "missing required cookies: " + missing.host + ":" + missing.name
			if err == nil || err.Error() != wantErr {
				t.Fatalf("LoadChromeCookies() error = %v, want %q", err, wantErr)
			}
		})
	}

	t.Run("required name on wrong host", func(t *testing.T) {
		profile := filepath.Join(t.TempDir(), "Default")
		if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
			t.Fatalf("mkdir profile: %v", err)
		}
		rows := make([]dbCookie, 0, len(requiredCookies))
		for _, req := range requiredCookies {
			if req.name == "SID" {
				continue
			}
			rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "val-"+req.name, req.host, key, true)})
		}
		rows = append(rows, dbCookie{
			host:      "accounts.google.com",
			name:      "SID",
			encrypted: encryptCookie(t, "wrong-host", "accounts.google.com", key, true),
		})
		writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

		_, err := LoadChromeCookies(profile, secret)
		const wantErr = "missing required cookies: .google.com:SID"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("LoadChromeCookies() error = %v, want %q", err, wantErr)
		}
	})
}

func TestLoadChromeCookiesDoesNotRequireMessagesOSID(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "Default")
	if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	secret := []byte("secret")
	key := DeriveKey(secret, 1003)
	rows := make([]dbCookie, 0, len(requiredCookies))
	for _, req := range requiredCookies {
		rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "val-"+req.name, req.host, key, true)})
	}
	writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

	got, err := LoadChromeCookies(profile, secret)
	if err != nil {
		t.Fatalf("LoadChromeCookies(): %v", err)
	}
	if _, exists := got["OSID"]; exists {
		t.Fatalf("OSID = %q, want absent when no Chrome host provides it", got["OSID"])
	}
}

func TestLoadChromeCookiesHappyPath(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "Default")
	if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	secret := []byte("secret")
	key := DeriveKey(secret, 1003)
	rows := make([]dbCookie, 0, len(requiredCookies)+3)
	for _, req := range requiredCookies {
		rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "val-"+req.name, req.host, key, true)})
	}
	// A duplicate SID on a lower-priority host must not shadow the .google.com one.
	rows = append(rows, dbCookie{"accounts.google.com", "SID", encryptCookie(t, "wrong", "accounts.google.com", key, true)})
	// A Messages OSID is preferred over a same-name account cookie when present.
	rows = append(rows,
		dbCookie{".google.com", "OSID", encryptCookie(t, "account-osid", ".google.com", key, true)},
		dbCookie{"messages.google.com", "OSID", encryptCookie(t, "messages-osid", "messages.google.com", key, true)},
	)
	writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

	got, err := LoadChromeCookies(profile, secret)
	if err != nil {
		t.Fatalf("LoadChromeCookies(): %v", err)
	}
	if got["SID"] != "val-SID" {
		t.Fatalf("SID = %q, want %q (host priority not applied)", got["SID"], "val-SID")
	}
	if got["OSID"] != "messages-osid" {
		t.Fatalf("OSID = %q, want %q", got["OSID"], "messages-osid")
	}
}

func TestLoadChromeCookiesEqualPriorityHostsUseLexicographicOrder(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "Default")
	if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	secret := []byte("secret")
	key := DeriveKey(secret, 1003)
	rows := make([]dbCookie, 0, len(requiredCookies)+2)
	for _, req := range requiredCookies {
		rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "val-"+req.name, req.host, key, true)})
	}
	// Insert mail first so SQLite's explicit ordering, not insertion order,
	// makes docs.google.com the stable winner at equal host priority.
	rows = append(rows,
		dbCookie{"mail.google.com", "OSID", encryptCookie(t, "mail-osid", "mail.google.com", key, true)},
		dbCookie{"docs.google.com", "OSID", encryptCookie(t, "docs-osid", "docs.google.com", key, true)},
	)
	writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

	for i := 0; i < 5; i++ {
		got, err := LoadChromeCookies(profile, secret)
		if err != nil {
			t.Fatalf("LoadChromeCookies() run %d: %v", i, err)
		}
		if got["OSID"] != "docs-osid" {
			t.Fatalf("LoadChromeCookies() run %d OSID = %q, want %q", i, got["OSID"], "docs-osid")
		}
	}
}

func TestLoadChromeCookiesSelectsLinuxPBKDF2Iterations(t *testing.T) {
	// Wave 1 credential handling must preserve selection of Chrome's Linux key.
	profile := filepath.Join(t.TempDir(), "Default")
	if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	secret := []byte("secret")
	key := DeriveKey(secret, 1)
	rows := make([]dbCookie, 0, len(requiredCookies))
	for _, req := range requiredCookies {
		rows = append(rows, dbCookie{req.host, req.name, encryptCookie(t, "linux-"+req.name, req.host, key, true)})
	}
	writeCookieDB(t, filepath.Join(profile, "Network", "Cookies"), rows)

	got, err := LoadChromeCookies(profile, secret)
	if err != nil {
		t.Fatalf("LoadChromeCookies(): %v", err)
	}
	for _, req := range requiredCookies {
		want := "linux-" + req.name
		if got[req.name] != want {
			t.Fatalf("%s = %q, want %q", req.name, got[req.name], want)
		}
	}
}
