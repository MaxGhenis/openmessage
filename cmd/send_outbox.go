package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/localapi"
)

type sendCommandDeps struct {
	mode         func() (v2RuntimeMode, error)
	client       *http.Client
	baseURL      string
	newKey       func() (string, error)
	output       io.Writer
	legacySend   func(conversationID, message string) error
	controlToken string
}

type sendGroupCommandDeps struct {
	mode         func() (v2RuntimeMode, error)
	client       *http.Client
	baseURL      string
	legacySend   func(phones []string, message string) error
	controlToken string
}

// outboxSubmission and outboxDelivery are the CLI's names for the shared
// local API wire types.
type (
	outboxSubmission = localapi.Submission
	outboxDelivery   = localapi.Delivery
	v2DaemonStatus   = localapi.DaemonStatus
)

func defaultSendCommandDeps(legacy func(string, string) error) sendCommandDeps {
	return sendCommandDeps{
		mode: func() (v2RuntimeMode, error) {
			return resolveV2RuntimeMode(app.DemoMode(), app.DefaultDataDir())
		},
		client:       &http.Client{Timeout: 10 * time.Second},
		baseURL:      localAPIBaseURL(),
		newKey:       newCLIIdempotencyKey,
		output:       os.Stdout,
		legacySend:   legacy,
		controlToken: loadCLIControlToken(app.DefaultDataDir()),
	}
}

func defaultSendGroupCommandDeps(legacy func([]string, string) error) sendGroupCommandDeps {
	return sendGroupCommandDeps{
		mode: func() (v2RuntimeMode, error) {
			return resolveV2RuntimeMode(app.DemoMode(), app.DefaultDataDir())
		},
		client:       &http.Client{Timeout: 10 * time.Second},
		baseURL:      localAPIBaseURL(),
		legacySend:   legacy,
		controlToken: loadCLIControlToken(app.DefaultDataDir()),
	}
}

func localAPIBaseURL() string {
	return localapi.DefaultBaseURL()
}

func (deps sendCommandDeps) daemonClient() *localapi.Client {
	return &localapi.Client{
		BaseURL: deps.baseURL,
		HTTP:    deps.client,
		Token:   deps.controlToken,
	}
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

	daemon := deps.daemonClient()
	status, reachable, err := daemon.Status(ctx)
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
	daemon.Token = controlTokenFromDaemonStatus(status, deps.controlToken)
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
	submission, err := daemon.SubmitText(ctx, localapi.TextSubmission{
		ConversationID: conversationID,
		Body:           message,
		IdempotencyKey: key,
		NotBeforeMS:    notBeforeMS,
	})
	if err != nil {
		if localapi.IsDeterministicRejection(err) {
			return deterministicRejectionError(err)
		}
		return ambiguousSendError(key, err)
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
	delivery, err := daemon.Delivery(ctx, submission.OutboxID)
	if err != nil {
		if !scheduled {
			fmt.Fprintf(deps.output, "queued; app continues in the background. Do not resend. Replay-check with the same idempotency key: %s\n", key)
		}
		return nil
	}
	return writeCLIDelivery(deps.output, delivery, key, scheduled)
}

func controlTokenFromDaemonStatus(status v2DaemonStatus, fallback string) string {
	if strings.TrimSpace(status.Auth.DataDir) == "" {
		return fallback
	}
	return loadCLIControlToken(status.Auth.DataDir)
}

func deterministicRejectionError(err error) error {
	if responseErr, ok := localapi.AsResponseError(err); ok && responseErr.StatusCode == http.StatusConflict {
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
		fmt.Fprintln(output, "transmitted: the transport accepted the message. Transport acceptance is not delivery — verify in-thread before reporting it as sent.")
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

func loadCLIControlToken(dataDir string) string {
	return localapi.LoadControlToken(dataDir)
}

func runSendGroupWithDeps(deps sendGroupCommandDeps, phones []string, message string) error {
	if deps.client == nil {
		deps.client = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.baseURL == "" {
		deps.baseURL = localAPIBaseURL()
	}
	daemon := &localapi.Client{BaseURL: deps.baseURL, HTTP: deps.client, Token: deps.controlToken}
	status, reachable, err := daemon.Status(context.Background())
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
