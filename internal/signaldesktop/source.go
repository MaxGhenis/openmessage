// Package signaldesktop reads Signal Desktop's local database for the v2
// history feed. It only reads: Signal Desktop is a separate linked device, and
// its archive is a second copy of the account's history that can fill gaps
// the live signal-cli bridge missed.
package signaldesktop

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

//go:embed read_helper.cjs
var readHelper []byte

const (
	helperTimeout   = 2 * time.Minute
	keychainTimeout = 10 * time.Second
	// maxHelperOutput bounds what one read may return. A full scan of a
	// several-year archive is tens of megabytes; this is far above that and
	// far below what would hurt the daemon.
	maxHelperOutput = 512 << 20
)

// Query is one read-only SELECT that runs inside a scan's single read
// transaction. An optional query that fails reports its error in Result
// instead of failing the scan, so a column an older Signal Desktop lacks can
// fall back to a simpler query.
type Query struct {
	Name     string `json:"name"`
	SQL      string `json:"sql"`
	Params   []any  `json:"params"`
	Optional bool   `json:"optional,omitempty"`
}

// Row is one result row, column name to JSON value.
type Row map[string]json.RawMessage

// Result holds one query's rows, or the error an optional query hit.
type Result struct {
	Rows  []Row  `json:"rows"`
	Error string `json:"error,omitempty"`
}

// Runner executes queries against one consistent snapshot of the Desktop
// database and never writes to it.
type Runner interface {
	Run(ctx context.Context, queries []Query) (map[string]Result, error)
}

// DefaultSupportDir is Signal Desktop's data folder on macOS.
func DefaultSupportDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Signal")
}

// DBPath is the SQLCipher database inside a Signal Desktop data folder.
func DBPath(supportDir string) string {
	return filepath.Join(supportDir, "sql", "db.sqlite")
}

// NodeRunner reads the SQLCipher database through Signal Desktop's own
// SQLCipher addon, loaded by a system Node.js. The key comes from Key on each
// run; callers cache it.
type NodeRunner struct {
	DBPath    string
	NodePath  string
	AddonPath string
	Key       func(context.Context) (string, error)
}

var runHelper = func(ctx context.Context, nodePath, helperPath string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, nodePath, helperPath)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr limitedBuffer
	stdout.limit = maxHelperOutput
	stderr.limit = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow {
		return nil, stderr.Bytes(), errors.New("Signal Desktop read returned more than the output limit")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (r *NodeRunner) Run(ctx context.Context, queries []Query) (map[string]Result, error) {
	if r == nil || r.Key == nil {
		return nil, errors.New("Signal Desktop reader is not configured")
	}
	key, err := r.Key(ctx)
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(struct {
		DB      string  `json:"db"`
		Key     string  `json:"key"`
		Addon   string  `json:"addon"`
		Queries []Query `json:"queries"`
	}{r.DBPath, key, r.AddonPath, queries})
	if err != nil {
		return nil, fmt.Errorf("encode Signal Desktop read request: %w", err)
	}
	helperPath, cleanup, err := writeHelper()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	runCtx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	stdout, stderr, err := runHelper(runCtx, r.NodePath, helperPath, request)
	if err != nil {
		// The helper reports SQL and addon errors on stderr. They carry no
		// message content, but trim them so a status field stays short.
		detail := strings.TrimSpace(string(stderr))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("read Signal Desktop database: %w: %s", err, detail)
	}
	var response struct {
		Results map[string]Result `json:"results"`
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode Signal Desktop read: %w", err)
	}
	if response.Results == nil {
		return nil, errors.New("Signal Desktop read returned no results")
	}
	return response.Results, nil
}

func writeHelper() (string, func(), error) {
	file, err := os.CreateTemp("", "openmessage-signal-desktop-read-*.cjs")
	if err != nil {
		return "", nil, fmt.Errorf("create Signal Desktop helper: %w", err)
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := file.Write(readHelper); err != nil {
		file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write Signal Desktop helper: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close Signal Desktop helper: %w", err)
	}
	return name, cleanup, nil
}

// SQLRunner reads a plain (unencrypted) SQLite file with the same queries. It
// backs tests and fixtures; production reads go through NodeRunner, because
// Go has no SQLCipher driver here.
type SQLRunner struct {
	DB *sql.DB
}

func (r SQLRunner) Run(ctx context.Context, queries []Query) (map[string]Result, error) {
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin Signal Desktop snapshot: %w", err)
	}
	defer tx.Rollback()
	results := make(map[string]Result, len(queries))
	for _, query := range queries {
		rows, err := runSQLQuery(ctx, tx, query)
		if err != nil {
			if !query.Optional {
				return nil, fmt.Errorf("query %s: %w", query.Name, err)
			}
			results[query.Name] = Result{Error: err.Error()}
			continue
		}
		results[query.Name] = Result{Rows: rows}
	}
	return results, tx.Commit()
}

func runSQLQuery(ctx context.Context, tx *sql.Tx, query Query) ([]Row, error) {
	rows, err := tx.QueryContext(ctx, query.SQL, query.Params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []Row{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(Row, len(columns))
		for i, column := range columns {
			value := values[i]
			if raw, ok := value.([]byte); ok {
				value = string(raw)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			row[column] = encoded
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

var (
	lookPath                     = exec.LookPath
	runSignalSafeStoragePassword = func(ctx context.Context) ([]byte, error) {
		return exec.CommandContext(ctx, "security", "find-generic-password", "-s", "Signal Safe Storage", "-w").Output()
	}
)

// NodePath finds a Node.js executable for the helper.
func NodePath() (string, error) {
	if resolved, err := lookPath("node"); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved, nil
	}
	for _, candidate := range []string{"/opt/homebrew/bin/node", "/usr/local/bin/node", "/opt/local/bin/node"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", errors.New("Node.js is required to read Signal Desktop's database")
}

// AddonPath finds the SQLCipher addon that ships inside Signal.app.
func AddonPath() (string, error) {
	arch := "darwin-arm64"
	if runtime.GOARCH == "amd64" {
		arch = "darwin-x64"
	}
	home, _ := os.UserHomeDir()
	for _, app := range []string{"/Applications/Signal.app", filepath.Join(home, "Applications", "Signal.app")} {
		candidate := filepath.Join(app, "Contents", "Resources", "app.asar.unpacked", "node_modules",
			"@signalapp", "sqlcipher", "prebuilds", arch, "@signalapp+sqlcipher.node")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", errors.New("Signal Desktop's SQLCipher addon was not found")
}

// SQLKey derives the database key from config.json's encryptedKey and the
// "Signal Safe Storage" keychain password, as Signal Desktop does on macOS.
// Reading the keychain item can show a macOS prompt, so callers cache the key
// and back off after a failure instead of asking again on every scan.
func SQLKey(ctx context.Context, supportDir string) (string, error) {
	rawConfig, err := os.ReadFile(filepath.Join(supportDir, "config.json"))
	if err != nil {
		return "", fmt.Errorf("read Signal config.json: %w", err)
	}
	var config struct {
		EncryptedKey string `json:"encryptedKey"`
	}
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return "", fmt.Errorf("decode Signal config.json: %w", err)
	}
	encrypted, err := hex.DecodeString(strings.TrimSpace(config.EncryptedKey))
	if err != nil || len(encrypted) <= 3 || string(encrypted[:3]) != "v10" {
		return "", errors.New("Signal config.json has no supported encryptedKey")
	}
	keychainCtx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	password, err := runSignalSafeStoragePassword(keychainCtx)
	if err != nil {
		return "", fmt.Errorf("read the Signal Safe Storage keychain item: %w", err)
	}
	secret := strings.TrimSpace(string(password))
	if secret == "" {
		return "", errors.New("the Signal Safe Storage keychain item is empty")
	}
	return decryptSQLKey(secret, encrypted[3:])
}

func decryptSQLKey(password string, ciphertext []byte) (string, error) {
	key := pbkdf2.Key([]byte(password), []byte("saltysalt"), 1003, 16, sha1.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("invalid Signal encrypted key payload")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte(" "), aes.BlockSize)).CryptBlocks(plaintext, ciphertext)
	padding := int(plaintext[len(plaintext)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plaintext) {
		return "", errors.New("invalid Signal key padding")
	}
	for _, b := range plaintext[len(plaintext)-padding:] {
		if int(b) != padding {
			return "", errors.New("invalid Signal key padding")
		}
	}
	key64 := strings.TrimSpace(string(plaintext[:len(plaintext)-padding]))
	if key64 == "" {
		return "", errors.New("Signal database key is empty")
	}
	for _, r := range key64 {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", errors.New("Signal database key is not hex")
		}
	}
	return key64, nil
}
