package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/app"
)

type sendCommandDeps struct {
	mode       func() (v2RuntimeMode, error)
	client     *http.Client
	baseURL    string
	newKey     func() (string, error)
	output     io.Writer
	legacySend func(conversationID, message string) error
}

type sendGroupCommandDeps struct {
	mode       func() (v2RuntimeMode, error)
	client     *http.Client
	baseURL    string
	legacySend func(phones []string, message string) error
}

type outboxSubmission struct {
	OutboxID       string `json:"outbox_id"`
	State          string `json:"state"`
	ScheduledForMS int64  `json:"scheduled_for_ms"`
	Deduplicated   bool   `json:"deduplicated"`
}

type outboxDelivery struct {
	OutboxID        string `json:"outbox_id"`
	State           string `json:"state"`
	RemoteMessageID string `json:"remote_message_id"`
	ErrorClass      string `json:"error_class"`
	Warning         string `json:"warning"`
}

func defaultSendCommandDeps(legacy func(string, string) error) sendCommandDeps {
	return sendCommandDeps{
		mode: func() (v2RuntimeMode, error) {
			return resolveV2RuntimeMode(app.DemoMode(), app.DefaultDataDir())
		},
		client:     &http.Client{Timeout: 10 * time.Second},
		baseURL:    localAPIBaseURL(),
		newKey:     newCLIIdempotencyKey,
		output:     os.Stdout,
		legacySend: legacy,
	}
}

func defaultSendGroupCommandDeps(legacy func([]string, string) error) sendGroupCommandDeps {
	return sendGroupCommandDeps{
		mode: func() (v2RuntimeMode, error) {
			return resolveV2RuntimeMode(app.DemoMode(), app.DefaultDataDir())
		},
		client:     &http.Client{Timeout: 10 * time.Second},
		baseURL:    localAPIBaseURL(),
		legacySend: legacy,
	}
}

func localAPIBaseURL() string {
	port := strings.TrimSpace(os.Getenv("OPENMESSAGES_PORT"))
	if port == "" {
		port = "7007"
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}

func runSendWithDeps(ctx context.Context, deps sendCommandDeps, conversationID, message string, notBeforeMS *int64) error {
	if deps.client == nil {
		deps.client = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.baseURL == "" {
		deps.baseURL = localAPIBaseURL()
	}
	if deps.output == nil {
		deps.output = os.Stdout
	}

	status, reachable, err := probeV2Status(ctx, deps.client, deps.baseURL)
	if err != nil {
		if reachable {
			return fmt.Errorf("check running OpenMessage mode: %w", err)
		}
		mode, modeErr := deps.mode()
		if modeErr != nil {
			return modeErr
		}
		if mode.Primary || mode.Send {
			return fmt.Errorf("OpenMessage isn't running; start it to send: %w", err)
		}
		return deps.legacySend(conversationID, message)
	}
	if !status.V2Send && !status.V2Primary {
		return deps.legacySend(conversationID, message)
	}
	if deps.newKey == nil {
		deps.newKey = newCLIIdempotencyKey
	}
	key, err := deps.newKey()
	if err != nil {
		return err
	}
	payload := struct {
		ConversationID string `json:"conversation_id"`
		Body           string `json:"body"`
		IdempotencyKey string `json:"idempotency_key"`
		NotBeforeMS    *int64 `json:"not_before_ms,omitempty"`
	}{conversationID, message, key, notBeforeMS}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox submission: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, deps.baseURL+"/api/v1/outbox/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := deps.client.Do(request)
	if err != nil {
		return ambiguousSendError(key, err)
	}
	var submission outboxSubmission
	if err := decodeCLIResponse(response, &submission); err != nil {
		if isDeterministicRejection(err) {
			return deterministicRejectionError(err)
		}
		return ambiguousSendError(key, err)
	}
	if submission.OutboxID == "" {
		return ambiguousSendError(key, fmt.Errorf("submission response omitted outbox_id"))
	}

	fmt.Fprintf(deps.output, "outbox_id=%s state=%s idempotency_key=%s deduplicated=%t", submission.OutboxID, submission.State, key, submission.Deduplicated)
	if submission.ScheduledForMS > 0 {
		fmt.Fprintf(deps.output, " scheduled_for_ms=%d", submission.ScheduledForMS)
	}
	fmt.Fprintln(deps.output)
	scheduled := submission.ScheduledForMS > time.Now().UnixMilli()
	if scheduled {
		when := time.UnixMilli(submission.ScheduledForMS).Format(time.RFC3339)
		fmt.Fprintf(deps.output, "scheduled; the app will send it at %s (%d ms). Do not resend.\n", when, submission.ScheduledForMS)
	}
	delivery, err := getOutboxDelivery(ctx, deps.client, deps.baseURL, submission.OutboxID)
	if err != nil {
		if !scheduled {
			fmt.Fprintf(deps.output, "queued; app continues in the background. Do not resend. Replay-check with the same idempotency key: %s\n", key)
		}
		return nil
	}
	return writeCLIDelivery(deps.output, delivery, key, scheduled)
}

type v2DaemonStatus struct {
	V2Send    bool `json:"v2_send"`
	V2Primary bool `json:"v2_primary"`
}

func probeV2Status(ctx context.Context, client *http.Client, baseURL string) (v2DaemonStatus, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/status", nil)
	if err != nil {
		return v2DaemonStatus{}, false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return v2DaemonStatus{}, false, err
	}
	var status v2DaemonStatus
	if err := decodeCLIResponse(response, &status); err != nil {
		return v2DaemonStatus{}, true, err
	}
	return status, true, nil
}

func getOutboxDelivery(ctx context.Context, client *http.Client, baseURL, id string) (outboxDelivery, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/outbox/"+id, nil)
	if err != nil {
		return outboxDelivery{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return outboxDelivery{}, err
	}
	var delivery outboxDelivery
	err = decodeCLIResponse(response, &delivery)
	return delivery, err
}

func decodeCLIResponse(response *http.Response, target any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &cliResponseError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode local API response: %w", err)
	}
	return nil
}

type cliResponseError struct {
	StatusCode int
	Body       string
}

func (e *cliResponseError) Error() string {
	return fmt.Sprintf("local API returned HTTP %d: %s", e.StatusCode, e.Body)
}

func isDeterministicRejection(err error) bool {
	var responseErr *cliResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func deterministicRejectionError(err error) error {
	var responseErr *cliResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("send rejected: %s; this key already carries different content; use a new key to send this message", responseErr.Body)
	}
	return fmt.Errorf("send rejected: %w", err)
}

type ambiguousCLIError struct {
	key   string
	cause error
}

func (e *ambiguousCLIError) Error() string {
	return fmt.Sprintf("send outcome is unknown (%v). %s", e.cause, e.OperatorInstruction())
}

func (e *ambiguousCLIError) OperatorInstruction() string {
	return fmt.Sprintf("Do not send with a new key. To replay-check this exact invocation, repeat it with the same idempotency key: %s", e.key)
}

func ambiguousSendError(key string, cause error) error {
	return &ambiguousCLIError{key: key, cause: cause}
}

func OperatorInstruction(err error) string {
	var ambiguous *ambiguousCLIError
	if errors.As(err, &ambiguous) {
		return ambiguous.OperatorInstruction()
	}
	return ""
}

func writeCLIDelivery(output io.Writer, delivery outboxDelivery, key string, scheduled bool) error {
	fmt.Fprintf(output, "outbox_id=%s state=%s idempotency_key=%s\n", delivery.OutboxID, delivery.State, key)
	switch delivery.State {
	case "confirmed":
		fmt.Fprintln(output, "message delivery confirmed")
	case "not_dispatched":
		if !scheduled {
			fmt.Fprintln(output, "queued; app retries automatically. Do not resend.")
		}
	case "queued", "dispatching":
		if !scheduled {
			fmt.Fprintln(output, "queued; app continues sending in the background. Do not resend.")
		}
	case "uncertain":
		fmt.Fprintln(output, "delivery is uncertain; the transport may have accepted it. Do not retry automatically.")
		// Exit zero because re-running an uncertain delivery risks a double-send.
	case "store_failed":
		fmt.Fprintln(output, "transport accepted the message; the local record is being repaired automatically. Do not resend.")
	case "rejected":
		fmt.Fprintln(output, "delivery was rejected; the app will not retry it")
		return fmt.Errorf("delivery was rejected; the app will not retry it")
	case "canceled":
		fmt.Fprintln(output, "delivery was canceled")
	default:
		fmt.Fprintln(output, "send is durably queued; check the outbox before taking further action")
	}
	return nil
}

func newCLIIdempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate CLI idempotency key: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(value[0:4]), hex.EncodeToString(value[4:6]),
		hex.EncodeToString(value[6:8]), hex.EncodeToString(value[8:10]),
		hex.EncodeToString(value[10:16]),
	}, "-"), nil
}

func runSendGroupWithDeps(deps sendGroupCommandDeps, phones []string, message string) error {
	if deps.client == nil {
		deps.client = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.baseURL == "" {
		deps.baseURL = localAPIBaseURL()
	}
	status, reachable, err := probeV2Status(context.Background(), deps.client, deps.baseURL)
	if err != nil {
		if reachable {
			return fmt.Errorf("check running OpenMessage mode: %w", err)
		}
		mode, modeErr := deps.mode()
		if modeErr != nil {
			return modeErr
		}
		if mode.Primary || mode.Send {
			return fmt.Errorf("OpenMessage isn't running; start it to send: %w", err)
		}
		return deps.legacySend(phones, message)
	}
	if status.V2Send || status.V2Primary {
		return fmt.Errorf("creating a new group isn't supported in v2 yet — send to an existing conversation")
	}
	return deps.legacySend(phones, message)
}
