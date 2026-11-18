// Package telemetry sends anonymous, opt-in usage heartbeats so the project
// can answer basic questions like "how many people are actually running this?"
// and "what platforms do they have paired?".
//
// Design principles:
//   - Off by default. The only switch is the OPENMESSAGE_TELEMETRY environment
//     variable set to exactly "1" for `openmessage serve` (cmd/serve.go), which
//     also skips telemetry in demo mode and in transportless processes. There
//     is no UI toggle or config file.
//   - No message content, contact info, or anything that could identify a user.
//   - At most one heartbeat per Client every 24 hours. That limit lives in
//     memory and serve builds a new Client on each launch, so each launch
//     tries to send one heartbeat, however recently the last launch sent one.
//   - Stable anonymous install ID: random, cached at $dataDir/telemetry-id and
//     read back on every later launch that uses the same data dir. No
//     IP-based identity.
//   - Failure is silent — telemetry must never block app startup.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	defaultEndpoint = "https://openmessage.ai/api/heartbeat"
	heartbeatPeriod = 24 * time.Hour
	requestTimeout  = 5 * time.Second
)

// Client sends heartbeat events.
type Client struct {
	endpoint string
	dataDir  string
	enabled  bool
	version  string
	httpc    *http.Client
	mu       sync.Mutex
	lastSent time.Time
}

// PlatformStatus is the minimum info needed to count active installs by route.
type PlatformStatus struct {
	GoogleMessages bool `json:"google_messages,omitempty"`
	WhatsApp       bool `json:"whatsapp,omitempty"`
	Signal         bool `json:"signal,omitempty"`
}

type payload struct {
	InstallID  string         `json:"install_id"`
	Version    string         `json:"version"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	Platforms  PlatformStatus `json:"platforms"`
	SentAt     string         `json:"sent_at"`
	SchemaVer  int            `json:"schema_version"`
}

// New constructs a Client. enabled is the user's opt-in preference; if false,
// every method becomes a no-op.
func New(dataDir, version string, enabled bool) *Client {
	return &Client{
		endpoint: defaultEndpoint,
		dataDir:  dataDir,
		enabled:  enabled,
		version:  version,
		httpc:    &http.Client{Timeout: requestTimeout},
	}
}

// SetEndpoint overrides the default endpoint (mostly for tests).
func (c *Client) SetEndpoint(url string) {
	c.endpoint = url
}

// MaybeSend sends a heartbeat if telemetry is enabled and the period has
// elapsed since the last send. Never blocks the caller meaningfully —
// network calls are bounded by requestTimeout.
func (c *Client) MaybeSend(ctx context.Context, status PlatformStatus) {
	if c == nil || !c.enabled {
		return
	}

	c.mu.Lock()
	if !c.lastSent.IsZero() && time.Since(c.lastSent) < heartbeatPeriod {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	id, err := c.installID()
	if err != nil {
		return
	}

	body, err := json.Marshal(payload{
		InstallID: id,
		Version:   c.version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Platforms: status,
		SentAt:    time.Now().UTC().Format(time.RFC3339),
		SchemaVer: 1,
	})
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("openmessage/%s (%s/%s)", c.version, runtime.GOOS, runtime.GOARCH))

	resp, err := c.httpc.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()

	c.mu.Lock()
	c.lastSent = time.Now()
	c.mu.Unlock()
}

// installID returns this install's anonymous ID, which stays the same across
// launches. The first call generates 16 random bytes, hex-encodes them and
// writes them to $dataDir/telemetry-id; later calls, in this process or a
// later launch, read that file back. If the file can't be read, or doesn't
// hold 32 characters after trimming whitespace, it generates a fresh ID and
// overwrites the file. If the write fails it returns an error and MaybeSend
// sends nothing, so no heartbeat carries an ID that isn't saved. Never tied
// to any user identifier.
func (c *Client) installID() (string, error) {
	path := filepath.Join(c.dataDir, "telemetry-id")
	if data, err := os.ReadFile(path); err == nil {
		id := string(bytes.TrimSpace(data))
		if len(id) == 32 {
			return id, nil
		}
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}
