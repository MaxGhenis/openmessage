// Package localapi is the HTTP client for a running OpenMessage daemon's
// local API. CLI commands and the transportless MCP serve mode use it to
// detect the daemon ("daemon truth") and to route sends through the daemon's
// durable outbox instead of opening their own platform transports — two
// processes holding the same WhatsApp device credentials or signal-cli
// account kill each other's sessions, so exactly one process (the daemon)
// may own live connections.
package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ControlTokenFile is the file inside the daemon's data directory that holds
// the local control token. It mirrors web.ControlTokenFile; the two constants
// are asserted equal in cmd tests so they cannot drift.
const ControlTokenFile = "control.token"

// DefaultBaseURL returns the local daemon base URL, honoring OPENMESSAGES_PORT.
func DefaultBaseURL() string {
	port := strings.TrimSpace(os.Getenv("OPENMESSAGES_PORT"))
	if port == "" {
		port = "7007"
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}

// LoadControlToken reads the local control token from dataDir. A missing or
// unreadable token yields "" so callers degrade to unauthenticated requests
// (the daemon decides whether those are acceptable).
func LoadControlToken(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, ControlTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Client talks to one OpenMessage daemon. The zero value is not usable; call
// NewClient or populate BaseURL and HTTP explicitly.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

// NewClient returns a Client for baseURL with a conservative default timeout.
// An empty baseURL falls back to DefaultBaseURL().
func NewClient(baseURL, token string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL()
	}
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		Token:   token,
	}
}

// DaemonStatus is the subset of /api/status used for daemon-truth decisions.
type DaemonStatus struct {
	Connected bool `json:"connected"`
	V2Send    bool `json:"v2_send"`
	V2Primary bool `json:"v2_primary"`
	Auth      struct {
		DataDir string `json:"data_dir"`
	} `json:"auth"`
}

// SendsViaOutbox reports whether the daemon expects sends on the durable
// /api/v1/outbox surface rather than the legacy /api/send route.
func (s DaemonStatus) SendsViaOutbox() bool {
	return s.V2Send || s.V2Primary
}

// Status probes /api/status. The second result reports reachability: false
// means no daemon answered at all (connection refused/timeout), while true
// with a non-nil error means something answered but the response was not a
// usable status document.
func (c *Client) Status(ctx context.Context) (DaemonStatus, bool, error) {
	var status DaemonStatus
	reachable, err := c.getJSON(ctx, "/api/status", &status)
	return status, reachable, err
}

// RawStatus returns the daemon's /api/status document verbatim, for callers
// that surface daemon state to a human or agent rather than branching on it.
func (c *Client) RawStatus(ctx context.Context) (map[string]any, error) {
	raw := map[string]any{}
	if _, err := c.getJSON(ctx, "/api/status", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// TextSubmission is a durable text send routed at POST /api/v1/outbox/messages.
type TextSubmission struct {
	ConversationID string `json:"conversation_id"`
	Body           string `json:"body"`
	ReplyToID      string `json:"reply_to_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	NotBeforeMS    *int64 `json:"not_before_ms,omitempty"`
}

// MediaSubmission is a durable media send routed at POST /api/v1/outbox/media.
// Content is streamed; the daemon enforces its own size cap.
type MediaSubmission struct {
	ConversationID string
	Filename       string
	MIME           string
	Caption        string
	ReplyToID      string
	IdempotencyKey string
	NotBeforeMS    *int64
	Content        io.Reader
}

// Submission mirrors the daemon's v1 submission response.
type Submission struct {
	OutboxID       string `json:"outbox_id"`
	LocalMessageID string `json:"local_message_id"`
	State          string `json:"state"`
	ScheduledForMS int64  `json:"scheduled_for_ms"`
	Deduplicated   bool   `json:"deduplicated"`
}

// Delivery mirrors the daemon's v1 delivery response.
type Delivery struct {
	OutboxID        string `json:"outbox_id"`
	State           string `json:"state"`
	LocalMessageID  string `json:"local_message_id"`
	RemoteMessageID string `json:"remote_message_id"`
	ErrorClass      string `json:"error_class"`
	ErrorCode       string `json:"error_code"`
	Warning         string `json:"warning"`
}

// Settled reports whether the delivery reached a state the dispatcher will
// not advance on its own without new information: everything except the
// in-flight queued/dispatching states. not_dispatched counts as settled here
// because the daemon schedules its own retries; callers report it as an
// auto-retrying outcome rather than polling forever.
func (d Delivery) Settled() bool {
	switch d.State {
	case "", "queued", "dispatching":
		return false
	default:
		return true
	}
}

// SubmitText enqueues a text send on the daemon's durable outbox.
func (c *Client) SubmitText(ctx context.Context, submission TextSubmission) (Submission, error) {
	body, err := json.Marshal(submission)
	if err != nil {
		return Submission{}, fmt.Errorf("encode outbox submission: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/outbox/messages", bytes.NewReader(body))
	if err != nil {
		return Submission{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return Submission{}, err
	}
	var result Submission
	if err := decodeResponse(response, &result); err != nil {
		return Submission{}, err
	}
	if result.OutboxID == "" {
		return Submission{}, fmt.Errorf("submission response omitted outbox_id")
	}
	return result, nil
}

// SubmitMedia enqueues a media send on the daemon's durable outbox, streaming
// Content as a multipart upload.
func (c *Client) SubmitMedia(ctx context.Context, submission MediaSubmission) (Submission, error) {
	reader, contentType := multipartMediaBody(submission)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/outbox/media", reader)
	if err != nil {
		_ = reader.Close()
		return Submission{}, err
	}
	request.Header.Set("Content-Type", contentType)
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return Submission{}, err
	}
	var result Submission
	if err := decodeResponse(response, &result); err != nil {
		return Submission{}, err
	}
	if result.OutboxID == "" {
		return Submission{}, fmt.Errorf("submission response omitted outbox_id")
	}
	return result, nil
}

func multipartMediaBody(submission MediaSubmission) (io.ReadCloser, string) {
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	go func() {
		fail := func(err error) {
			_ = pipeWriter.CloseWithError(err)
		}
		fields := map[string]string{
			"conversation_id": submission.ConversationID,
			"idempotency_key": submission.IdempotencyKey,
			"caption":         submission.Caption,
			"reply_to_id":     submission.ReplyToID,
		}
		if submission.NotBeforeMS != nil {
			fields["not_before_ms"] = fmt.Sprintf("%d", *submission.NotBeforeMS)
		}
		for name, value := range fields {
			if value == "" {
				continue
			}
			if err := writer.WriteField(name, value); err != nil {
				fail(err)
				return
			}
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(
			`form-data; name="file"; filename=%q`, submission.Filename,
		))
		mimeType := strings.TrimSpace(submission.MIME)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		header.Set("Content-Type", mimeType)
		part, err := writer.CreatePart(header)
		if err != nil {
			fail(err)
			return
		}
		if _, err := io.Copy(part, submission.Content); err != nil {
			fail(err)
			return
		}
		if err := writer.Close(); err != nil {
			fail(err)
			return
		}
		_ = pipeWriter.Close()
	}()
	return pipeReader, writer.FormDataContentType()
}

// Delivery fetches the current durable state of one outbox item.
func (c *Client) Delivery(ctx context.Context, outboxID string) (Delivery, error) {
	var delivery Delivery
	if _, err := c.getJSON(ctx, "/api/v1/outbox/"+outboxID, &delivery); err != nil {
		return Delivery{}, err
	}
	return delivery, nil
}

// WaitDelivery polls the outbox item until it settles, ctx ends, or
// settleTimeout elapses. It returns the last observed delivery; the bool
// reports whether that delivery settled. A non-nil error means the state
// could never be read — the send itself remains durably queued on the daemon.
func (c *Client) WaitDelivery(ctx context.Context, outboxID string, settleTimeout time.Duration) (Delivery, bool, error) {
	deadline := time.Now().Add(settleTimeout)
	var last Delivery
	var lastErr error
	observed := false
	for {
		delivery, err := c.Delivery(ctx, outboxID)
		if err == nil {
			observed = true
			last = delivery
			lastErr = nil
			if delivery.Settled() {
				return delivery, true, nil
			}
		} else {
			lastErr = err
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				if observed {
					return last, false, nil
				}
				return Delivery{}, false, lastErr
			}
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			if observed {
				return last, false, nil
			}
			return Delivery{}, false, lastErr
		}
		select {
		case <-ctx.Done():
			if observed {
				return last, false, nil
			}
			return Delivery{}, false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// LegacySendResult is the daemon's legacy /api/send response.
type LegacySendResult struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	Success   bool   `json:"success"`
}

// LegacySendText routes a text send through the daemon's legacy /api/send
// surface. Only valid against daemons running with v2 send disabled; a
// v2-primary daemon rejects this route deterministically.
func (c *Client) LegacySendText(ctx context.Context, conversationID, message, replyToID, idempotencyKey string) (LegacySendResult, error) {
	payload := struct {
		ConversationID string `json:"conversation_id"`
		Message        string `json:"message"`
		ReplyToID      string `json:"reply_to_id,omitempty"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}{conversationID, message, replyToID, idempotencyKey}
	var result LegacySendResult
	if err := c.postJSON(ctx, "/api/send", payload, &result); err != nil {
		return LegacySendResult{}, err
	}
	return result, nil
}

// LegacySendMedia routes a media send through the daemon's legacy
// /api/send-media surface (multipart, same field names as the v1 outbox
// media route). Only valid against daemons running with v2 send disabled.
func (c *Client) LegacySendMedia(ctx context.Context, submission MediaSubmission) (LegacySendResult, error) {
	reader, contentType := multipartMediaBody(submission)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/send-media", reader)
	if err != nil {
		_ = reader.Close()
		return LegacySendResult{}, err
	}
	request.Header.Set("Content-Type", contentType)
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return LegacySendResult{}, err
	}
	var result LegacySendResult
	if err := decodeResponse(response, &result); err != nil {
		return LegacySendResult{}, err
	}
	return result, nil
}

// React routes a reaction through the daemon's /api/react surface, which
// works in every daemon mode. Action is "add", "remove", or "switch".
func (c *Client) React(ctx context.Context, conversationID, messageID, emoji, action string) error {
	payload := struct {
		ConversationID string `json:"conversation_id"`
		MessageID      string `json:"message_id"`
		Emoji          string `json:"emoji"`
		Action         string `json:"action"`
	}{conversationID, messageID, emoji, action}
	return c.postJSON(ctx, "/api/react", payload, &map[string]any{})
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode local API request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	return decodeResponse(response, target)
}

// getJSON performs a GET and decodes the response. The bool reports whether
// anything answered the request at all.
func (c *Client) getJSON(ctx context.Context, path string, target any) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return false, err
	}
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return false, err
	}
	if err := decodeResponse(response, target); err != nil {
		return true, err
	}
	return true, nil
}

func (c *Client) authorize(request *http.Request) {
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func decodeResponse(response *http.Response, target any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &ResponseError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode local API response: %w", err)
	}
	return nil
}

// ResponseError is a non-2xx local API response.
type ResponseError struct {
	StatusCode int
	Body       string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("local API returned HTTP %d: %s", e.StatusCode, e.Body)
}

// AsResponseError unwraps err into a *ResponseError when it is one.
func AsResponseError(err error) (*ResponseError, bool) {
	var responseErr *ResponseError
	if errors.As(err, &responseErr) {
		return responseErr, true
	}
	return nil, false
}

// IsDeterministicRejection reports whether err is a daemon response that
// proves the request was received and refused — safe to surface as a hard
// failure with no replay ambiguity.
func IsDeterministicRejection(err error) bool {
	responseErr, ok := AsResponseError(err)
	if !ok {
		return false
	}
	switch responseErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// IsAuthError reports whether err is the daemon refusing our control token.
func IsAuthError(err error) bool {
	responseErr, ok := AsResponseError(err)
	if !ok {
		return false
	}
	return responseErr.StatusCode == http.StatusUnauthorized || responseErr.StatusCode == http.StatusForbidden
}
