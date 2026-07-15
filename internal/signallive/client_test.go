package signallive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestQRCodeRendersDataURL(t *testing.T) {
	bridge := &Bridge{
		qr: QRSnapshot{
			URI: "sgnl://linkdevice?uuid=test",
		},
	}
	snap, err := bridge.QRCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap.PNGDataURL, "data:image/png;base64,") {
		t.Fatalf("unexpected QR data url: %q", snap.PNGDataURL)
	}
}

func TestParseSignalAccountsIgnoresSignalCLIErrorOutput(t *testing.T) {
	raw := []byte("WARN  MultiAccountManager - Ignoring +15551230000: User is not registered. (NotRegisteredException)\nUser +15551230000 is not registered.\n")
	if got := parseSignalAccounts(raw); len(got) != 0 {
		t.Fatalf("parseSignalAccounts() = %#v, want no accounts from error output", got)
	}
}

func TestParseSignalAccountsAcceptsJSONPhoneNumbers(t *testing.T) {
	got := parseSignalAccounts([]byte(`[{"number":"+15551230000"}]`))
	if len(got) != 1 || got[0] != "+15551230000" {
		t.Fatalf("parseSignalAccounts() = %#v, want +15551230000", got)
	}
}

func TestBridgeSendTextRunsSignalCLI(t *testing.T) {
	t.Setenv("OPENMESSAGES_MY_NAME", "")
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     nil,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	originalNow := now
	defer func() {
		runSignalCLI = originalRun
		now = originalNow
	}()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}
	now = func() time.Time { return time.UnixMilli(1700000000123) }

	msg, err := bridge.SendText("signal:+15551234567", "hello from signal", "signal:quoted")
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	wantArgs := []string{
		"-a", "+15551230000",
		"send",
		"-m", "hello from signal",
		"+15551234567",
	}
	if !slices.Equal(captured[1:], wantArgs) {
		t.Fatalf("legacy signal-cli args = %q, want %q", captured[1:], wantArgs)
	}
	if msg.MessageID != "signal:local:ff9f0accc4bd325d662c082170f55eea5c1f550d" ||
		msg.SourceID != "local:ff9f0accc4bd325d662c082170f55eea5c1f550d" ||
		msg.ConversationID != "signal:+15551234567" ||
		msg.SenderName != "Me" ||
		msg.SenderNumber != "+15551230000" ||
		msg.Body != "hello from signal" ||
		msg.TimestampMS != 1700000000123 ||
		msg.Status != "sent" ||
		!msg.IsFromMe ||
		msg.ReplyToID != "signal:quoted" ||
		msg.SourcePlatform != "signal" {
		t.Fatalf("legacy SendText() message = %+v, want fabricated local identity and unchanged db.Message form", msg)
	}
}

func TestBridgeSendTextRequestRunsJSONSendAndReturnsSignalTimestamp(t *testing.T) {
	success, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read success fixture: %v", err)
	}
	tests := []struct {
		name           string
		conversationID string
		wantTargetArgs []string
	}{
		{
			name:           "recipient",
			conversationID: "signal:+15551234567",
			wantTargetArgs: []string{"+15551234567"},
		},
		{
			name:           "group",
			conversationID: "signal-group:test-group",
			wantTargetArgs: []string{"--group-id", "test-group"},
		},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &Bridge{
				account:   "+15551230000",
				connected: true,
				configDir: t.TempDir(),
				logger:    zerolog.Nop(),
			}
			var captured []string
			runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
				captured = append([]string{}, args...)
				return success, nil
			}

			timestamp, err := bridge.SendTextRequest(tc.conversationID, "  hello from durable Signal  ", "")
			if err != nil {
				t.Fatalf("SendTextRequest(): %v", err)
			}
			if timestamp != 1700000000123 {
				t.Fatalf("SendTextRequest() timestamp = %d, want 1700000000123", timestamp)
			}
			wantArgs := []string{
				"--output=json",
				"-a", "+15551230000",
				"send",
				"-m", "hello from durable Signal",
			}
			wantArgs = append(wantArgs, tc.wantTargetArgs...)
			if !slices.Equal(captured, wantArgs) {
				t.Fatalf("signal-cli args = %q, want %q", captured, wantArgs)
			}
		})
	}
}

func TestBridgeSendTextRequestIncludesSignalQuoteArguments(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:reply-1",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "quoted body",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed reply target: %v", err)
	}
	success, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read success fixture: %v", err)
	}
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{}, args...)
		return success, nil
	}

	if _, err := bridge.SendTextRequest(conversationID, "replying", "signal:reply-1"); err != nil {
		t.Fatalf("SendTextRequest(): %v", err)
	}
	wantArgs := []string{
		"--output=json",
		"-a", "+15551230000",
		"send",
		"-m", "replying",
		"--quote-timestamp", "1700000000123",
		"--quote-author", "+15551234567",
		"--quote-message", "quoted body",
		"+15551234567",
	}
	if !slices.Equal(captured, wantArgs) {
		t.Fatalf("signal-cli args = %q, want %q", captured, wantArgs)
	}
}

func TestBridgeSendTextRequestClassifiesStructuredAndOpaqueFailures(t *testing.T) {
	allFailed, err := os.ReadFile(filepath.Join("testdata", "send-media-all-recipients-failed.json"))
	if err != nil {
		t.Fatalf("read all-failed fixture: %v", err)
	}
	malformed, err := os.ReadFile(filepath.Join("testdata", "send-media-unparseable.txt"))
	if err != nil {
		t.Fatalf("read malformed fixture: %v", err)
	}
	tests := []struct {
		name              string
		output            []byte
		commandErr        error
		wantNotDispatched bool
		wantDiagnostics   []string
	}{
		{
			name:              "all recipients failed with nonzero exit",
			output:            allFailed,
			commandErr:        errors.New("exit status 1"),
			wantNotDispatched: true,
			wantDiagnostics:   []string{"exit status 1", "UNREGISTERED_FAILURE"},
		},
		{
			name:            "malformed successful output",
			output:          malformed,
			wantDiagnostics: []string{"decode Signal media send JSON"},
		},
		{
			name:            "opaque nonzero exit",
			output:          []byte("signal-cli transport failed"),
			commandErr:      errors.New("exit status 2"),
			wantDiagnostics: []string{"exit status 2", "signal-cli transport failed"},
		},
		{
			name:            "timeout despite all-failed output",
			output:          allFailed,
			commandErr:      context.DeadlineExceeded,
			wantDiagnostics: []string{"context deadline exceeded", "UNREGISTERED_FAILURE"},
		},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &Bridge{
				account:   "+15551230000",
				connected: true,
				configDir: t.TempDir(),
				logger:    zerolog.Nop(),
			}
			runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
				return tc.output, tc.commandErr
			}

			_, err := bridge.SendTextRequest("signal:+15551234567", "hello", "")
			if err == nil || !IsCommandError(err) {
				t.Fatalf("SendTextRequest() error = %v (%T), want CommandError", err, err)
			}
			if got := IsSendNotDispatchedError(err); got != tc.wantNotDispatched {
				t.Fatalf("IsSendNotDispatchedError() = %v, want %v for %v", got, tc.wantNotDispatched, err)
			}
			for _, diagnostic := range tc.wantDiagnostics {
				if !strings.Contains(err.Error(), diagnostic) {
					t.Fatalf("SendTextRequest() error = %q, want diagnostic %q", err, diagnostic)
				}
			}
		})
	}
}

func TestBridgeSendTextRequestSucceedsOnMixedRecipientResults(t *testing.T) {
	mixed, err := os.ReadFile(filepath.Join("testdata", "send-media-mixed-results.json"))
	if err != nil {
		t.Fatalf("read mixed-results fixture: %v", err)
	}
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		return mixed, nil
	}

	timestamp, err := bridge.SendTextRequest("signal-group:test-group", "hello group", "")
	if err != nil {
		t.Fatalf("SendTextRequest() on mixed results = %v, want success", err)
	}
	if timestamp != 1700000000678 {
		t.Fatalf("SendTextRequest() timestamp = %d, want 1700000000678", timestamp)
	}
}

func TestParseSignalSendResultPreservesMediaFixtures(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		want          int64
		wantErr       string
		wantAllFailed bool
	}{
		{
			name:    "success",
			fixture: "send-media-success.json",
			want:    1700000000123,
		},
		{
			name:          "all recipients failed",
			fixture:       "send-media-all-recipients-failed.json",
			want:          1700000000456,
			wantErr:       "Signal media send failed for recipient 0: UNREGISTERED_FAILURE, recipient 1: NETWORK_FAILURE",
			wantAllFailed: true,
		},
		{
			name:          "all recipients failed with invalid pre-key",
			fixture:       "send-media-invalid-pre-key.json",
			want:          1700000000555,
			wantErr:       "Signal media send failed for recipient 0: INVALID_PRE_KEY_FAILURE",
			wantAllFailed: true,
		},
		{
			// Mixed results return the timestamp ALONGSIDE the recipient error so
			// callers can honor signal-cli's exit-0 semantics (>=1 success means
			// the message went out).
			name:    "mixed success and failure",
			fixture: "send-media-mixed-results.json",
			want:    1700000000678,
			wantErr: "Signal media send failed for recipient 1: UNREGISTERED_FAILURE",
		},
		{
			// signal-cli WARN/JVM stderr noise rides CombinedOutput around the JSON
			// on fully successful sends; the decoder must fall back to a per-line
			// scan instead of failing the whole send.
			name:    "stderr noise around success JSON",
			fixture: "send-media-stderr-noise.txt",
			want:    1700000000123,
		},
		{
			name:    "zero timestamp",
			fixture: "send-media-zero-timestamp.json",
			wantErr: "Signal media send result has no valid timestamp",
		},
		{
			name:    "missing timestamp",
			fixture: "send-media-missing-timestamp.json",
			wantErr: "Signal media send result has no valid timestamp",
		},
		{
			name:    "empty recipient results",
			fixture: "send-media-empty-results.json",
			wantErr: "Signal media send result has no recipient results",
		},
		{
			name:    "unparseable output",
			fixture: "send-media-unparseable.txt",
			wantErr: "decode Signal media send JSON: invalid character 's' looking for beginning of value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			got, err := parseSignalSendResult(output)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("parseSignalSendResult() error = %v, want %q", err, tc.wantErr)
				}
				if got := signalSendAllRecipientsFailed(err); got != tc.wantAllFailed {
					t.Fatalf("signalSendAllRecipientsFailed() = %v, want %v for %v", got, tc.wantAllFailed, err)
				}
				if got != tc.want {
					t.Fatalf("parseSignalSendResult() timestamp = %d, want %d alongside error", got, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSignalSendResult() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseSignalSendResult() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBridgeSendMediaRequestStreamsAttachmentAndRemovesTempFileOnSuccess(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}
	content := []byte("streamed-png-bytes")
	output, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read send result fixture: %v", err)
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	var attachmentPath string
	var attachmentContent []byte
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{}, args...)
		for _, arg := range args {
			if strings.HasPrefix(arg, "--attachment=") {
				attachmentPath = strings.TrimPrefix(arg, "--attachment=")
				attachmentContent, err = os.ReadFile(attachmentPath)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		return output, nil
	}

	timestamp, err := bridge.SendMediaRequest(
		"signal:a1a98e48-7fa6-402e-9f62-b687098fed68",
		bytes.NewBuffer(content),
		int64(len(content)),
		"photo.png",
		"image/png",
		"signal photo",
		"",
	)
	if err != nil {
		t.Fatalf("SendMediaRequest(): %v", err)
	}
	if timestamp != 1700000000123 {
		t.Fatalf("SendMediaRequest() timestamp = %d, want 1700000000123", timestamp)
	}
	if !bytes.Equal(attachmentContent, content) {
		t.Fatalf("streamed attachment = %q, want %q", attachmentContent, content)
	}
	if attachmentPath == "" {
		t.Fatal("signal-cli received no anchored attachment path")
	}
	if _, err := os.Stat(attachmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary attachment still exists after success: %v", err)
	}
	wantArgs := []string{
		"--output=json",
		"-a", "+15551230000",
		"send",
		"-m", "signal photo",
		"--attachment=" + attachmentPath,
		"a1a98e48-7fa6-402e-9f62-b687098fed68",
	}
	if !slices.Equal(captured, wantArgs) {
		t.Fatalf("signal-cli args = %q, want %q", captured, wantArgs)
	}
}

func TestBridgeSendMediaRequestClassifiesStructuredAndOpaqueFailures(t *testing.T) {
	allFailed, err := os.ReadFile(filepath.Join("testdata", "send-media-all-recipients-failed.json"))
	if err != nil {
		t.Fatalf("read all-failed fixture: %v", err)
	}
	malformed, err := os.ReadFile(filepath.Join("testdata", "send-media-unparseable.txt"))
	if err != nil {
		t.Fatalf("read malformed fixture: %v", err)
	}
	tests := []struct {
		name              string
		output            []byte
		commandErr        error
		wantNotDispatched bool
		wantDiagnostics   []string
	}{
		{
			name:              "all recipients failed with nonzero exit",
			output:            allFailed,
			commandErr:        errors.New("exit status 1"),
			wantNotDispatched: true,
			wantDiagnostics:   []string{"exit status 1", "UNREGISTERED_FAILURE"},
		},
		{
			name:            "malformed successful output",
			output:          malformed,
			wantDiagnostics: []string{"decode Signal media send JSON"},
		},
		{
			name:            "opaque nonzero exit",
			output:          []byte("signal-cli transport failed"),
			commandErr:      errors.New("exit status 2"),
			wantDiagnostics: []string{"exit status 2", "signal-cli transport failed"},
		},
		{
			name:            "timeout despite all-failed output",
			output:          allFailed,
			commandErr:      context.DeadlineExceeded,
			wantDiagnostics: []string{"context deadline exceeded", "UNREGISTERED_FAILURE"},
		},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &Bridge{
				account:   "+15551230000",
				connected: true,
				configDir: t.TempDir(),
				logger:    zerolog.Nop(),
			}
			var attachmentPath string
			runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
				for _, arg := range args {
					if strings.HasPrefix(arg, "--attachment=") {
						attachmentPath = strings.TrimPrefix(arg, "--attachment=")
						break
					}
				}
				return tc.output, tc.commandErr
			}

			_, err := bridge.SendMediaRequest(
				"signal:+15551234567",
				strings.NewReader("png-bytes"),
				int64(len("png-bytes")),
				"photo.png",
				"image/png",
				"",
				"",
			)
			if err == nil || !IsCommandError(err) {
				t.Fatalf("SendMediaRequest() error = %v (%T), want CommandError", err, err)
			}
			if got := IsSendNotDispatchedError(err); got != tc.wantNotDispatched {
				t.Fatalf("IsSendNotDispatchedError() = %v, want %v for %v", got, tc.wantNotDispatched, err)
			}
			for _, diagnostic := range tc.wantDiagnostics {
				if !strings.Contains(err.Error(), diagnostic) {
					t.Fatalf("SendMediaRequest() error = %q, want diagnostic %q", err, diagnostic)
				}
			}
			if attachmentPath == "" {
				t.Fatal("signal-cli received no attachment path")
			}
			if _, statErr := os.Stat(attachmentPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("temporary attachment still exists after error: %v", statErr)
			}
		})
	}
}

// signal-cli exits 0 on mixed per-recipient results (it only throws when no
// recipient succeeded), and the pre-M2b legacy path treated exit 0 as a
// successful send. A group send that reached most members must not surface as
// a failure — per-recipient delivery gaps are M5 reconciliation's job.
func TestBridgeSendMediaRequestSucceedsOnMixedRecipientResults(t *testing.T) {
	mixed, err := os.ReadFile(filepath.Join("testdata", "send-media-mixed-results.json"))
	if err != nil {
		t.Fatalf("read mixed-results fixture: %v", err)
	}
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var attachmentPath string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--attachment=") {
				attachmentPath = strings.TrimPrefix(arg, "--attachment=")
				break
			}
		}
		return mixed, nil // exit 0: signal-cli only throws when no recipient succeeded
	}

	timestamp, err := bridge.SendMediaRequest(
		"signal:group-id",
		strings.NewReader("png-bytes"),
		int64(len("png-bytes")),
		"photo.png",
		"image/png",
		"group photo",
		"",
	)
	if err != nil {
		t.Fatalf("SendMediaRequest() on mixed results = %v, want success", err)
	}
	if timestamp != 1700000000678 {
		t.Fatalf("SendMediaRequest() timestamp = %d, want 1700000000678", timestamp)
	}
	if attachmentPath == "" {
		t.Fatal("signal-cli received no attachment path")
	}
	if _, statErr := os.Stat(attachmentPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary attachment still exists after mixed-result success: %v", statErr)
	}
}

// Web-sent media rows persist the transport timestamp as their MessageID
// ("signal:<ts>"). When history sync later replays the same send as a
// SyncMessage.SentMessage transcript, the exact-identity match must dedupe it —
// otherwise every web-sent media message appears twice after a re-pair.
func TestSentMessageSyncDoesNotDuplicateWebSentMedia(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
	}

	success, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read success fixture: %v", err)
	}
	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return success, nil
	}

	// Legacy web send with no caption: the persisted body is the attachment
	// placeholder — the case the old body+drift heuristic couldn't be trusted on.
	msg, err := bridge.SendMedia("signal:+15551234567", []byte("png-bytes"), "photo.png", "image/png", "", "")
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	if msg.MessageID != "signal:1700000000123" {
		t.Fatalf("web-sent media MessageID = %q, want signal:1700000000123", msg.MessageID)
	}
	// The app layer persists the returned message; mirror it.
	if err := store.UpsertMessage(msg); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	// History sync replays the same send as a SentMessage transcript with the
	// same sender timestamp.
	payload := `{"account":"+15551230000","envelope":{"source":"+15551230000","sourceNumber":"+15551230000","timestamp":1700000000123,"syncMessage":{"sentMessage":{"destinationNumber":"+15551234567","timestamp":1700000000123,"message":"","attachments":[{"contentType":"image/png","id":"remote-attachment-1"}]}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	fromMe := 0
	for _, m := range msgs {
		if m != nil && m.IsFromMe {
			fromMe++
			if m.MessageID != msg.MessageID {
				t.Fatalf("deduped MessageID = %q, want original %q", m.MessageID, msg.MessageID)
			}
		}
	}
	if fromMe != 1 {
		t.Fatalf("outgoing rows after history-sync replay = %d, want exactly 1 (duplicate on re-pair)", fromMe)
	}
}

func TestBridgeSendMediaRequestRejectsOversizeReaderBeforeCommand(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	called := false
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	_, err := bridge.SendMediaRequest(
		"signal:+15551234567",
		strings.NewReader("one byte too long"),
		int64(len("one byte too lon")),
		"attachment.bin",
		"application/octet-stream",
		"",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds declared size") {
		t.Fatalf("SendMediaRequest() error = %v, want declared-size error", err)
	}
	if called {
		t.Fatal("signal-cli was called for an oversize reader")
	}
	entries, readErr := os.ReadDir(filepath.Join(bridge.configDir, "outgoing-attachments"))
	if readErr != nil {
		t.Fatalf("read outgoing attachment directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("oversize reader left temporary attachments: %v", entries)
	}
}

func TestSignalCLIExecutableFallsBackToHomebrewPath(t *testing.T) {
	originalOverride := os.Getenv("OPENMESSAGES_SIGNAL_CLI")
	originalLookPath := signalCLILookPath
	originalStat := signalCLIStat
	defer func() {
		if originalOverride == "" {
			_ = os.Unsetenv("OPENMESSAGES_SIGNAL_CLI")
		} else {
			_ = os.Setenv("OPENMESSAGES_SIGNAL_CLI", originalOverride)
		}
		signalCLILookPath = originalLookPath
		signalCLIStat = originalStat
	}()

	_ = os.Unsetenv("OPENMESSAGES_SIGNAL_CLI")
	signalCLILookPath = func(file string) (string, error) {
		return "", os.ErrNotExist
	}
	signalCLIStat = func(name string) (os.FileInfo, error) {
		if name == "/opt/homebrew/bin/signal-cli" {
			return fakeFileInfo{name: "signal-cli"}, nil
		}
		return nil, os.ErrNotExist
	}

	if got := signalCLIExecutable(); got != "/opt/homebrew/bin/signal-cli" {
		t.Fatalf("signalCLIExecutable() = %q, want /opt/homebrew/bin/signal-cli", got)
	}
}

func TestFirstSignalAccountParsesWrappedAccountsJSON(t *testing.T) {
	raw := []byte(`{"accounts":[{"number":"+16506303657","uuid":"abc"}],"version":2}`)
	if got := firstSignalAccount(raw); got != "+16506303657" {
		t.Fatalf("firstSignalAccount() = %q, want +16506303657", got)
	}
}

func TestParseSignalAccountsParsesWrappedAccountsJSON(t *testing.T) {
	raw := []byte(`{"accounts":[{"number":"+16506303657"},{"number":"+15551230000"}],"version":2}`)
	got := parseSignalAccounts(raw)
	want := []string{"+15551230000", "+16506303657"}
	if len(got) != len(want) {
		t.Fatalf("parseSignalAccounts() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseSignalAccounts()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestParseSignalGroupsParsesCLIOutput(t *testing.T) {
	raw := []byte("INFO  AccountHelper - The Signal protocol expects that incoming messages are regularly received.\nId: L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE= Name: Neighborhood Planning Group  Active: true Blocked: false\nId: tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ= Name: Signal Sandbox  Active: true Blocked: false\n")
	got := parseSignalGroups(raw)
	if got["L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="] != "Neighborhood Planning Group" {
		t.Fatalf("first parsed group = %q", got["L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="])
	}
	if got["tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ="] != "Signal Sandbox" {
		t.Fatalf("second parsed group = %q", got["tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ="])
	}
}

func TestSignalHistorySyncStatusTracksProgressAndCompletion(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalNow := now
	defer func() { now = originalNow }()

	current := time.UnixMilli(1700000000000)
	now = func() time.Time { return current }

	bridge.beginHistorySync()
	snap := bridge.Status()
	if snap.HistorySync == nil || !snap.HistorySync.Running {
		t.Fatalf("expected running history sync snapshot, got %+v", snap.HistorySync)
	}
	if snap.HistorySync.ImportedConversations != 0 || snap.HistorySync.ImportedMessages != 0 {
		t.Fatalf("unexpected initial history sync counts: %+v", snap.HistorySync)
	}

	bridge.recordHistorySyncProgress(true, true)
	snap = bridge.Status()
	if snap.HistorySync == nil {
		t.Fatal("expected history sync snapshot after progress")
	}
	if snap.HistorySync.ImportedConversations != 1 || snap.HistorySync.ImportedMessages != 1 {
		t.Fatalf("unexpected imported counts: %+v", snap.HistorySync)
	}

	current = current.Add(historySyncQuietAfter + time.Second)
	snap = bridge.Status()
	if snap.HistorySync == nil || snap.HistorySync.Running {
		t.Fatalf("expected completed history sync snapshot, got %+v", snap.HistorySync)
	}
	if snap.HistorySync.CompletedAt == 0 {
		t.Fatalf("expected completed_at on %+v", snap.HistorySync)
	}
}

func TestStartReceiveLoopDropsSignalConnectionAfterRepeatedReceiveFailures(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	originalVersionProbe := probeSignalCLIVersion
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		probeSignalCLIVersion = originalVersionProbe
	}()
	probeSignalCLIVersion = func(context.Context) ([]byte, error) {
		return []byte("signal-cli 0.14.5"), nil
	}

	var receiveCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " listAccounts") {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		if strings.Contains(strings.Join(args, " "), " receive ") {
			receiveCalls.Add(1)
			return []byte("WARN  ReceiveHelper - Connection closed unexpectedly, reconnecting in 100 ms"), context.DeadlineExceeded
		}
		return []byte{}, nil
	}

	go bridge.startReceiveLoop("+15551230000", false)

	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return !status.Connected &&
			!status.Connecting &&
			status.Paired &&
			strings.Contains(status.LastError, "Connection closed unexpectedly")
	})
	if calls := receiveCalls.Load(); calls < int32(receiveFailureLimit) {
		t.Fatalf("receive called %d times, want at least %d", calls, receiveFailureLimit)
	}
}

func TestStartReceiveLoopIgnoresIdleReceiveTimeouts(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	originalVersionProbe := probeSignalCLIVersion
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		probeSignalCLIVersion = originalVersionProbe
	}()
	probeSignalCLIVersion = func(context.Context) ([]byte, error) {
		return []byte("signal-cli 0.14.5"), nil
	}

	var receiveCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " listAccounts") {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		if strings.Contains(strings.Join(args, " "), " receive ") {
			calls := receiveCalls.Add(1)
			if calls >= int32(receiveFailureLimit+1) {
				go bridge.Close()
			}
			return []byte{}, context.DeadlineExceeded
		}
		return []byte{}, nil
	}

	go bridge.startReceiveLoop("+15551230000", false)

	waitForCondition(t, 2*time.Second, func() bool {
		return receiveCalls.Load() >= int32(receiveFailureLimit+1)
	})
	waitForCondition(t, 2*time.Second, func() bool {
		return !bridge.Status().Connected
	})

	status := bridge.Status()
	if status.LastError != "" {
		t.Fatalf("expected idle timeouts to avoid last_error, got %+v", status)
	}
	if calls := receiveCalls.Load(); calls < int32(receiveFailureLimit+1) {
		t.Fatalf("receive called %d times, want at least %d", calls, receiveFailureLimit+1)
	}
}

func TestReceiveWALIsClearedAfterSuccessfulBatch(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	configDir := t.TempDir()
	bridge := &Bridge{store: store, logger: zerolog.Nop(), configDir: configDir}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from wal"}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	// WAL must be gone after a successful batch.
	if _, err := os.Stat(bridge.receiveWALPath()); !os.IsNotExist(err) {
		t.Fatalf("expected WAL to be removed, got err=%v", err)
	}

	msgs, _ := store.GetMessagesByConversation("signal:+15551234567", 10)
	if len(msgs) != 1 || msgs[0].Body != "hi from wal" {
		t.Fatalf("batch not ingested: %+v", msgs)
	}
}

func TestDrainReceiveWALRecoversPendingBatch(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	configDir := t.TempDir()
	bridge := &Bridge{store: store, logger: zerolog.Nop(), configDir: configDir}

	// Simulate a crash: WAL contains an unprocessed payload (two lines),
	// no DB row exists yet, and drainReceiveWAL runs on the next startup.
	payload1 := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000111,"dataMessage":{"timestamp":1700000000111,"message":"first"}}}`
	payload2 := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000222,"dataMessage":{"timestamp":1700000000222,"message":"second"}}}`
	if err := appendReceiveWAL(bridge.receiveWALPath(), "+15551230000", []byte(payload1+"\n"+payload2+"\n")); err != nil {
		t.Fatalf("appendReceiveWAL(): %v", err)
	}

	bridge.drainReceiveWAL("+15551230000")

	// WAL is removed after drain.
	if _, err := os.Stat(bridge.receiveWALPath()); !os.IsNotExist(err) {
		t.Fatalf("expected WAL to be removed after drain, got err=%v", err)
	}

	msgs, _ := store.GetMessagesByConversation("signal:+15551234567", 10)
	if len(msgs) != 2 {
		t.Fatalf("want 2 replayed messages, got %d: %+v", len(msgs), msgs)
	}
}

func TestHandleReceiveOutputStoresIncomingSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from signal","mentions":[{"number":"+15551230000"}]}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.Name != "Taylor" || convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "hi from signal" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
	if !msgs[0].MentionsMe {
		t.Fatalf("expected mentions_me on %+v", msgs[0])
	}
}

func TestHandleReceiveOutputStoresIncomingSignalMessageFromSourceServiceID(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:        store,
		logger:       zerolog.Nop(),
		configDir:    t.TempDir(),
		contactByACI: map[string]string{"9f4b50e3-ebf2-413c-a856-161756a6161a": "+15551234567"},
	}

	payload := `{"account":"+15551230000","envelope":{"sourceName":"Taylor","sourceServiceId":"9f4b50e3-ebf2-413c-a856-161756a6161a","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from service id"}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo == nil {
		t.Fatal("expected direct conversation")
	}
	if convo.Name != "Taylor" || convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].SenderNumber != "+15551234567" {
		t.Fatalf("sender number = %q, want +15551234567", msgs[0].SenderNumber)
	}
	if msgs[0].Body != "hi from service id" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
}

func TestHandleReceiveOutputQuarantinesMalformedSignalPayloadAndRequestsRecoverySync(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	originalNow := now
	defer func() {
		runSignalCLI = originalRun
		now = originalNow
	}()

	var syncCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		syncCalls.Add(1)
		return []byte("ok"), nil
	}
	now = func() time.Time { return time.UnixMilli(1700000000123) }

	if err := bridge.handleReceiveOutput("+15551230000", []byte("{not json}\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return syncCalls.Load() == 1
	})

	recoveryLog, err := os.ReadFile(filepath.Join(bridge.configDir, "signal-receive-recovery.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile(recovery): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recoveryLog)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d recovery records, want 1", len(lines))
	}
	var record signalReceiveRecoveryRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("json.Unmarshal(recovery): %v", err)
	}
	if record.Reason != "unmarshal_failed" {
		t.Fatalf("reason = %q, want unmarshal_failed", record.Reason)
	}
	if record.Account != "+15551230000" {
		t.Fatalf("account = %q, want +15551230000", record.Account)
	}
	if strings.TrimSpace(record.Raw) != "{not json}" {
		t.Fatalf("raw = %q, want original line", record.Raw)
	}
	if record.Error == "" {
		t.Fatal("expected recovery record to include unmarshal error")
	}
	if snap := bridge.Status(); snap.HistorySync == nil || !snap.HistorySync.Running {
		t.Fatalf("expected running history sync after recovery request, got %+v", snap.HistorySync)
	}
}

func TestHandleReceiveOutputIgnoresNonJSONSignalCLIOutput(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	if err := bridge.handleReceiveOutput("+15551230000", []byte("WARN  HikariPool - clock leap detected\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}
	if _, err := os.Stat(filepath.Join(bridge.configDir, "signal-receive-recovery.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("expected no recovery queue file, got err=%v", err)
	}
}

func TestHandleReceiveOutputQuarantinesMissingSignalSourceAndRateLimitsRecoverySync(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	originalNow := now
	defer func() {
		runSignalCLI = originalRun
		now = originalNow
	}()

	var syncCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		syncCalls.Add(1)
		return []byte("ok"), nil
	}
	now = func() time.Time { return time.UnixMilli(1700000000123) }

	payload := strings.Join([]string{
		`{"account":"+15551230000","envelope":{"sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"first"}}}`,
		`{"account":"+15551230000","envelope":{"timestamp":1700000001123,"dataMessage":{"timestamp":1700000001123,"message":"second"}}}`,
	}, "\n") + "\n"
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload)); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return syncCalls.Load() == 1
	})

	convos, err := store.ListConversationsByPlatform("signal", 10)
	if err != nil {
		t.Fatalf("ListConversationsByPlatform(): %v", err)
	}
	if len(convos) != 0 {
		t.Fatalf("unexpected stored conversations: %+v", convos)
	}
	recoveryLog, err := os.ReadFile(filepath.Join(bridge.configDir, "signal-receive-recovery.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile(recovery): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recoveryLog)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d recovery records, want 2", len(lines))
	}
	for i, line := range lines {
		var record signalReceiveRecoveryRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("json.Unmarshal(recovery[%d]): %v", i, err)
		}
		if record.Reason != "missing_data_message_source" {
			t.Fatalf("reason[%d] = %q, want missing_data_message_source", i, record.Reason)
		}
	}
}

func TestReplayReceiveRecoveryQueueStoresRecoveredSignalMessageAndClearsQueue(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:        store,
		logger:       zerolog.Nop(),
		configDir:    t.TempDir(),
		contactByACI: map[string]string{"9f4b50e3-ebf2-413c-a856-161756a6161a": "+15551234567"},
	}

	record := signalReceiveRecoveryRecord{
		TimestampMS: 1700000000123,
		Account:     "+15551230000",
		Reason:      "missing_data_message_source",
		Raw:         `{"account":"+15551230000","envelope":{"sourceName":"Taylor","sourceServiceId":"9f4b50e3-ebf2-413c-a856-161756a6161a","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"replayed from queue"}}}`,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(record): %v", err)
	}
	if err := os.WriteFile(bridge.receiveRecoveryPath(), append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(recovery): %v", err)
	}

	bridge.replayReceiveRecoveryQueue("+15551230000")

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.Name != "Taylor" || convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}
	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "replayed from queue" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
	if _, err := os.Stat(bridge.receiveRecoveryPath()); !os.IsNotExist(err) {
		t.Fatalf("expected recovery queue to be removed, got err=%v", err)
	}
}

func TestReplayReceiveRecoveryQueueKeepsUnresolvedSignalPayloads(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	record := signalReceiveRecoveryRecord{
		TimestampMS: 1700000000123,
		Account:     "+15551230000",
		Reason:      "missing_data_message_source",
		Raw:         `{"account":"+15551230000","envelope":{"timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"still unresolved"}}}`,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(record): %v", err)
	}
	if err := os.WriteFile(bridge.receiveRecoveryPath(), append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(recovery): %v", err)
	}

	bridge.replayReceiveRecoveryQueue("+15551230000")

	raw, err := os.ReadFile(bridge.receiveRecoveryPath())
	if err != nil {
		t.Fatalf("ReadFile(recovery): %v", err)
	}
	if strings.TrimSpace(string(raw)) != string(encoded) {
		t.Fatalf("unexpected retained recovery queue: %q", string(raw))
	}
	convos, err := store.ListConversationsByPlatform("signal", 10)
	if err != nil {
		t.Fatalf("ListConversationsByPlatform(): %v", err)
	}
	if len(convos) != 0 {
		t.Fatalf("unexpected stored conversations: %+v", convos)
	}
}

func TestReplayReceiveRecoveryQueueMaterializesMissingIncomingSignalEdit(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	record := signalReceiveRecoveryRecord{
		TimestampMS: 1700000001123,
		Account:     "+15551230000",
		Reason:      "handle_edit_message_failed",
		Raw:         `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000001123,"editMessage":{"targetSentTimestamp":1700000000123,"dataMessage":{"timestamp":1700000001123,"message":"after missing edit"}}}}`,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(record): %v", err)
	}
	if err := os.WriteFile(bridge.receiveRecoveryPath(), append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(recovery): %v", err)
	}

	bridge.replayReceiveRecoveryQueue("+15551230000")

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "after missing edit" {
		t.Fatalf("body = %q, want after missing edit", msgs[0].Body)
	}
	if msgs[0].TimestampMS != 1700000000123 {
		t.Fatalf("timestamp = %d, want 1700000000123", msgs[0].TimestampMS)
	}
	if msgs[0].IsFromMe {
		t.Fatal("expected synthesized incoming edit to be incoming")
	}
	if !strings.HasPrefix(msgs[0].SourceID, "missing-edit:") {
		t.Fatalf("source_id = %q, want missing-edit prefix", msgs[0].SourceID)
	}
	if _, err := os.Stat(bridge.receiveRecoveryPath()); !os.IsNotExist(err) {
		t.Fatalf("expected recovery queue to be removed, got err=%v", err)
	}
}

func TestReplayReceiveRecoveryQueueMaterializesMissingSentSignalEdit(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	record := signalReceiveRecoveryRecord{
		TimestampMS: 1700000002123,
		Account:     "+15551230000",
		Reason:      "handle_sent_message_failed",
		Raw:         `{"account":"+15551230000","envelope":{"source":"+15551230000","sourceName":"Me","timestamp":1700000002123,"syncMessage":{"sentMessage":{"destination":"+15551234567","editMessage":{"targetSentTimestamp":1700000000123,"dataMessage":{"timestamp":1700000002123,"message":"edited missing local send"}}}}}}`,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(record): %v", err)
	}
	if err := os.WriteFile(bridge.receiveRecoveryPath(), append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(recovery): %v", err)
	}

	bridge.replayReceiveRecoveryQueue("+15551230000")

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "edited missing local send" {
		t.Fatalf("body = %q, want edited missing local send", msgs[0].Body)
	}
	if msgs[0].TimestampMS != 1700000000123 {
		t.Fatalf("timestamp = %d, want 1700000000123", msgs[0].TimestampMS)
	}
	if !msgs[0].IsFromMe || msgs[0].Status != "sent" {
		t.Fatalf("unexpected synthesized sent edit %+v", msgs[0])
	}
	if msgs[0].SenderNumber != "+15551230000" {
		t.Fatalf("sender_number = %q, want account", msgs[0].SenderNumber)
	}
	if !strings.HasPrefix(msgs[0].SourceID, "missing-edit:") {
		t.Fatalf("source_id = %q, want missing-edit prefix", msgs[0].SourceID)
	}
	if _, err := os.Stat(bridge.receiveRecoveryPath()); !os.IsNotExist(err) {
		t.Fatalf("expected recovery queue to be removed, got err=%v", err)
	}
}

func TestHandleReceiveOutputMergesMissingIncomingSignalEditAliasIntoRealMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000001123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:" + signalMissingEditSourceID(conversationID, "+15551234567", 1700000000123),
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "after edit",
		TimestampMS:    1700000000123,
		Status:         "received",
		IsFromMe:       false,
		SourcePlatform: "signal",
		SourceID:       signalMissingEditSourceID(conversationID, "+15551234567", 1700000000123),
	}); err != nil {
		t.Fatalf("seed missing-edit alias: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000001123,"dataMessage":{"timestamp":1700000000123,"message":"before edit"}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "after edit" {
		t.Fatalf("body = %q, want after edit", msgs[0].Body)
	}
	if strings.HasPrefix(msgs[0].SourceID, signalMissingEditSourcePrefix) {
		t.Fatalf("source_id = %q, want canonical source id", msgs[0].SourceID)
	}
}

func TestHandleReceiveOutputMergesMissingSentSignalEditAliasIntoRealMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const (
		account        = "+15551230000"
		conversationID = "signal:+15551234567"
		timestampMS    = int64(1700000000123)
	)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "+15551234567",
		LastMessageTS:  timestampMS,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:" + signalMissingEditSourceID(conversationID, account, timestampMS),
		ConversationID: conversationID,
		SenderName:     "Me",
		SenderNumber:   account,
		Body:           "edited outgoing",
		TimestampMS:    timestampMS,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
		SourceID:       signalMissingEditSourceID(conversationID, account, timestampMS),
	}); err != nil {
		t.Fatalf("seed missing-edit alias: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551230000","sourceName":"Me","timestamp":1700000001123,"syncMessage":{"sentMessage":{"destination":"+15551234567","timestamp":1700000000123,"message":"before edit"}}}}`
	if err := bridge.handleReceiveOutput(account, []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "edited outgoing" {
		t.Fatalf("body = %q, want edited outgoing", msgs[0].Body)
	}
	if strings.HasPrefix(msgs[0].SourceID, signalMissingEditSourcePrefix) {
		t.Fatalf("source_id = %q, want canonical source id", msgs[0].SourceID)
	}
	if !msgs[0].IsFromMe {
		t.Fatal("expected merged outgoing message to stay outgoing")
	}
}

func TestStatusIncludesReceiveRecoverySnapshot(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	first, err := json.Marshal(signalReceiveRecoveryRecord{
		TimestampMS: 1700000000123,
		Account:     "+15551230000",
		Reason:      "missing_data_message_source",
		Raw:         `{"bad":"first"}`,
	})
	if err != nil {
		t.Fatalf("json.Marshal(first): %v", err)
	}
	second, err := json.Marshal(signalReceiveRecoveryRecord{
		TimestampMS: 1700000001123,
		Account:     "+15551230000",
		Reason:      "handle_data_message_failed",
		Raw:         `{"bad":"second"}`,
	})
	if err != nil {
		t.Fatalf("json.Marshal(second): %v", err)
	}
	if err := os.WriteFile(bridge.receiveRecoveryPath(), append(append(first, '\n'), append(second, '\n')...), 0o600); err != nil {
		t.Fatalf("WriteFile(recovery): %v", err)
	}

	status := bridge.Status()
	if status.ReceiveRecovery == nil {
		t.Fatal("expected receive recovery snapshot")
	}
	if status.ReceiveRecovery.PendingCount != 2 {
		t.Fatalf("pending_count = %d, want 2", status.ReceiveRecovery.PendingCount)
	}
	if status.ReceiveRecovery.LastIssueAt != 1700000001123 {
		t.Fatalf("last_issue_at = %d, want 1700000001123", status.ReceiveRecovery.LastIssueAt)
	}
	if status.ReceiveRecovery.LastIssueReason != "handle_data_message_failed" {
		t.Fatalf("last_issue_reason = %q, want handle_data_message_failed", status.ReceiveRecovery.LastIssueReason)
	}
}

func TestHandleReceiveOutputAdvancesSignalHistorySyncCounts(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}
	bridge.beginHistorySync()

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from signal"}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	snap := bridge.Status()
	if snap.HistorySync == nil {
		t.Fatal("expected history sync snapshot")
	}
	if snap.HistorySync.ImportedConversations != 1 || snap.HistorySync.ImportedMessages != 1 {
		t.Fatalf("unexpected history sync counts: %+v", snap.HistorySync)
	}
}

func TestHandleReceiveOutputStoresIncomingSignalAttachmentID(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"attachments":[{"contentType":"image/png","id":"att-123"}]}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].MediaID != "signalatt:att-123" {
		t.Fatalf("media_id = %q, want signalatt:att-123", msgs[0].MediaID)
	}
	if msgs[0].MimeType != "image/png" {
		t.Fatalf("mime_type = %q, want image/png", msgs[0].MimeType)
	}
}

func TestHandleReceiveOutputStoresPlaceholderForUnsupportedIncomingSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Felix","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"sticker":{"packId":"pack-1"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "[Sticker]" {
		t.Fatalf("body = %q, want [Sticker]", msgs[0].Body)
	}
}

func TestHandleReceiveOutputUsesCachedSignalGroupNameWhenTitleMissing(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:      store,
		logger:     zerolog.Nop(),
		configDir:  t.TempDir(),
		groupNames: map[string]string{"L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE=": "Neighborhood Planning Group"},
	}

	payload := `{"account":"+15551230000","envelope":{"sourceName":"Michael Thorning","sourceUuid":"d91b5024-f3db-4c82-98f8-2691974d6a9b","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hello group","groupInfo":{"groupId":"L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal-group:L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE=")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if !convo.IsGroup {
		t.Fatalf("expected group conversation, got %+v", convo)
	}
	if convo.Name != "Neighborhood Planning Group" {
		t.Fatalf("group name = %q", convo.Name)
	}
}

func TestHandleReceiveOutputAppliesIncomingSignalReactionToTargetMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000001123,"dataMessage":{"timestamp":1700000001123,"reaction":{"emoji":"😂","targetAuthor":"+15551234567","targetSentTimestamp":1700000000123,"isRemove":false}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msg, err := store.GetMessageByID("signal:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected target message to exist")
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after add: %+v", reactions)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
}

func TestHandleReceiveOutputAppliesIncomingSignalEditToTargetMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000001123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "before edit",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000001123,"editMessage":{"targetSentTimestamp":1700000000123,"dataMessage":{"timestamp":1700000001123,"message":"after edit"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msg, err := store.GetMessageByID("signal:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected target message to exist")
	}
	if msg.Body != "after edit" {
		t.Fatalf("body = %q, want after edit", msg.Body)
	}
}

func TestHandleReceiveOutputAppliesIncomingSignalReactionToNearbyLocalOutgoingMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const (
		conversationID   = "signal:+15551234567"
		account          = "+15551230000"
		localTimestampMS = int64(1700000003000)
		targetTimestamp  = int64(1700000000123)
	)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  localTimestampMS,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:local:recent-outgoing",
		ConversationID: conversationID,
		SenderName:     "Me",
		SenderNumber:   account,
		Body:           "recent outgoing",
		TimestampMS:    localTimestampMS,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
		SourceID:       "local:recent-outgoing",
	}); err != nil {
		t.Fatalf("seed local outgoing: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000004123,"dataMessage":{"timestamp":1700000004123,"reaction":{"emoji":"👍","targetAuthor":"+15551230000","targetSentTimestamp":1700000000123,"isRemove":false}}}}`
	if err := bridge.handleReceiveOutput(account, []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msg, err := store.GetMessageByID("signal:local:recent-outgoing")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected outgoing message to exist")
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "👍" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after add: %+v", reactions)
	}
}

func TestHandleReceiveOutputAppliesSentSyncSignalGroupEditWithoutDestination(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal-group:kt4NmWvKe4VugsBqoZ3qPY0CRx8PiUVcNvi34jML/HQ="
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "social welfare optimizers",
		IsGroup:        true,
		LastMessageTS:  1776005928088,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:local:edit-target",
		ConversationID: conversationID,
		SenderName:     "Me",
		SenderNumber:   "+16506303657",
		Body:           "before group edit",
		TimestampMS:    1776005910475,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
		SourceID:       "local:edit-target",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		store:      store,
		logger:     zerolog.Nop(),
		configDir:  t.TempDir(),
		groupNames: map[string]string{"kt4NmWvKe4VugsBqoZ3qPY0CRx8PiUVcNvi34jML/HQ=": "social welfare optimizers"},
	}

	payload := `{"account":"+16506303657","envelope":{"source":"+16506303657","sourceNumber":"+16506303657","sourceUuid":"7a81fd95-20f1-4437-86e2-d5c93ba18851","sourceName":"Max Ghenis","sourceDevice":1,"timestamp":1776005928088,"syncMessage":{"sentMessage":{"destination":null,"destinationNumber":null,"destinationUuid":null,"editMessage":{"targetSentTimestamp":1776005910475,"dataMessage":{"timestamp":1776005928088,"message":"The UK has a ubi for people with mobility needs (PIP)","expiresInSeconds":0,"isExpirationUpdate":false,"viewOnce":false,"groupInfo":{"groupId":"kt4NmWvKe4VugsBqoZ3qPY0CRx8PiUVcNvi34jML/HQ=","groupName":"social welfare optimizers","revision":8,"type":"DELIVER"}}}}}}}`
	if err := bridge.handleReceiveOutput("+16506303657", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msg, err := store.GetMessageByID("signal:local:edit-target")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected edited message to exist")
	}
	if msg.Body != "The UK has a ubi for people with mobility needs (PIP)" {
		t.Fatalf("body = %q", msg.Body)
	}
}

func TestBridgeSendReactionRunsSignalCLIAndUpdatesLocalState(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}

	if err := bridge.SendReaction(conversationID, "signal:target-msg", "😂", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if got := strings.Join(captured[1:], " "); !strings.Contains(got, "-a +15551230000 sendReaction -e 😂 -a +15551234567 -t 1700000000123 +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}

	msg, err := store.GetMessageByID("signal:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after send: %+v", reactions)
	}
}

func TestBridgeSendReactionResolvesGroupTargetAuthorACIToNumber(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal-group:test-group"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Group",
		IsGroup:        true,
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Michael Thorning",
		SenderNumber:   "d91b5024-f3db-4c82-98f8-2691974d6a9b",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		account:      "+15551230000",
		connected:    true,
		configDir:    t.TempDir(),
		store:        store,
		logger:       zerolog.Nop(),
		callbacks:    Callbacks{},
		contactByACI: map[string]string{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var calls [][]string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "listContacts" {
			return []byte("Number: +15551234567 ACI: d91b5024-f3db-4c82-98f8-2691974d6a9b Name:  Profile name: Michael Thorning Username:  Color:  Blocked: false Message expiration: disabled\n"), nil
		}
		return []byte("ok"), nil
	}

	if err := bridge.SendReaction(conversationID, "signal:target-msg", "😂", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("expected listContacts and sendReaction calls, got %d", len(calls))
	}
	got := strings.Join(calls[len(calls)-1], " ")
	if !strings.Contains(got, "sendReaction -e 😂 -a +15551234567 -t 1700000000123 --group-id test-group") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
}

func TestBridgeSendTextIncludesSignalQuoteArguments(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:reply-1",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "quoted body",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed reply target: %v", err)
	}

	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}

	if _, err := bridge.SendText(conversationID, "replying", "signal:reply-1"); err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	got := strings.Join(captured[1:], " ")
	if !strings.Contains(got, "--quote-timestamp 1700000000123 --quote-author +15551234567 --quote-message quoted body") {
		t.Fatalf("unexpected quote args: %q", got)
	}
}

func TestLegacySendMediaCharacterizationPreservesTransportAndLocalAttachment(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	output, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read send result fixture: %v", err)
	}

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return output, nil
	}

	msg, err := bridge.SendMedia("signal:+15551234567", []byte("png-bytes"), "photo.png", "image/png", "signal photo", "")
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	got := strings.Join(captured[1:], " ")
	// The attachment flag must use `--attachment=path` (not `-a path`) so
	// signal-cli's greedy nargs='*' parsing doesn't consume the recipient
	// as a second attachment. See SendMedia for the full rationale.
	if !strings.Contains(got, "-a +15551230000 send -m signal photo --attachment=") ||
		!strings.Contains(got, " +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
	if strings.Contains(got, " -a "+string(os.PathSeparator)) || strings.Contains(got, "send -m signal photo -a /") {
		t.Fatalf("attachment passed as bare `-a path` — regression of the `No recipients given` bug: %q", got)
	}
	if len(captured) < 2 || captured[1] != "--output=json" {
		t.Fatalf("signal-cli global JSON flag = %v, want --output=json before account/send args", captured)
	}
	if msg.MessageID != "signal:1700000000123" ||
		msg.SourceID != "1700000000123" ||
		msg.TimestampMS != 1700000000123 {
		t.Fatalf("canonical legacy message identity = %+v, want signal-cli timestamp", msg)
	}
	if msg.SourcePlatform != "signal" {
		t.Fatalf("source platform = %q, want signal", msg.SourcePlatform)
	}
	if msg.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", msg.MimeType)
	}
	if !strings.HasPrefix(msg.MediaID, signalLocalAttachmentPrefix) {
		t.Fatalf("media id = %q, want local signal attachment ref", msg.MediaID)
	}
	attachmentPath, err := decodeSignalLocalAttachmentRef(msg.MediaID)
	if err != nil {
		t.Fatalf("decode retained local attachment: %v", err)
	}
	wantArgs := []string{
		bridge.configDir,
		"--output=json",
		"-a", "+15551230000",
		"send",
		"-m", "signal photo",
		"--attachment=" + attachmentPath,
		"+15551234567",
	}
	if !slices.Equal(captured, wantArgs) {
		t.Fatalf("legacy signal-cli call = %q, want %q", captured, wantArgs)
	}
	if msg.SourceID != strings.TrimPrefix(msg.MessageID, "signal:") {
		t.Fatalf("source_id = %q, want trimmed message id", msg.SourceID)
	}
	data, mimeType, err := bridge.DownloadMedia(msg)
	if err != nil {
		t.Fatalf("DownloadMedia(local): %v", err)
	}
	if string(data) != "png-bytes" {
		t.Fatalf("data = %q, want png-bytes", string(data))
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
}

// Regression: SendMedia to an ACI-only recipient (no phone number) previously
// failed with "No recipients given" because `-a path` let argparse's nargs='*'
// consume the following positional recipient as another attachment. The fix is
// to pass `--attachment=path` so the value is anchored to the flag.
func TestBridgeSendMediaToACIRecipientKeepsRecipientArg(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()
	output, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read send result fixture: %v", err)
	}

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{}, args...)
		return output, nil
	}

	const aci = "a1a98e48-7fa6-402e-9f62-b687098fed68"
	if _, err := bridge.SendMedia("signal:"+aci, []byte("png-bytes"), "photo.png", "image/png", "hello", ""); err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}

	// The ACI must appear as a standalone positional after all the flags.
	// Previously this token was being consumed by the preceding `-a` flag's
	// greedy attachment list.
	if len(captured) == 0 || captured[len(captured)-1] != aci {
		t.Fatalf("recipient ACI was not the final positional arg: %v", captured)
	}

	// No token may be a bare "-a" directly followed by a filesystem path —
	// that's the exact shape that triggers the bug.
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-a" && strings.HasPrefix(captured[i+1], string(os.PathSeparator)) {
			t.Fatalf("attachment still passed as bare `-a path` at index %d: %v", i, captured)
		}
	}
}

func TestBridgeDownloadMediaRunsSignalCLIAndDecodesAttachment(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     nil,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("aGVsbG8="), nil
	}

	data, mimeType, err := bridge.DownloadMedia(&db.Message{
		MessageID:      "signal:m1",
		ConversationID: "signal:+15551234567",
		SenderNumber:   "+15551234567",
		MimeType:       "image/png",
		MediaID:        "signalatt:att-123",
		SourcePlatform: "signal",
	})
	if err != nil {
		t.Fatalf("DownloadMedia(): %v", err)
	}
	if got := strings.Join(captured[1:], " "); !strings.Contains(got, "-a +15551230000 getAttachment --id att-123 --recipient +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", string(data))
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
}

type fakeFileInfo struct {
	name string
}

func (f fakeFileInfo) Name() string     { return f.name }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func TestHandleReceiveOutputStoresSentSyncMessageFromAnotherClient(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"message":"sent from phone","destinationNumber":"+15551234567"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !msgs[0].IsFromMe || msgs[0].Status != "sent" {
		t.Fatalf("unexpected outgoing sync message %+v", msgs[0])
	}
	if msgs[0].Body != "sent from phone" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
}

func TestHandleReceiveOutputStoresSentSyncMessageFromDestinationServiceID(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:        store,
		logger:       zerolog.Nop(),
		configDir:    t.TempDir(),
		contactByACI: map[string]string{"9f4b50e3-ebf2-413c-a856-161756a6161a": "+15551234567"},
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"message":"sent from phone","destinationServiceId":"9f4b50e3-ebf2-413c-a856-161756a6161a"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo == nil || convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !msgs[0].IsFromMe || msgs[0].Status != "sent" {
		t.Fatalf("unexpected outgoing sync message %+v", msgs[0])
	}
	if msgs[0].Body != "sent from phone" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
}

func TestHandleReceiveOutputStoresPlaceholderForUnsupportedSentSyncSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"destinationNumber":"+15551234567","remoteDelete":{"targetSentTimestamp":1700000000000}}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "[Deleted message]" {
		t.Fatalf("body = %q, want [Deleted message]", msgs[0].Body)
	}
	if !msgs[0].IsFromMe || msgs[0].Status != "sent" {
		t.Fatalf("unexpected outgoing sync message %+v", msgs[0])
	}
}

func TestHandleReceiveOutputDedupesMatchingLocalOutgoingSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	conversationID := "signal:+15551234567"
	timestamp := int64(1700000000222)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  timestamp - 1000,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	local := &db.Message{
		MessageID:      localOutgoingMessageID(conversationID, timestamp-500, "sent from openmessage"),
		ConversationID: conversationID,
		SenderName:     "Me",
		SenderNumber:   "+15551230000",
		Body:           "sent from openmessage",
		TimestampMS:    timestamp - 500,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
	}
	if err := store.UpsertMessage(local); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"message":"sent from openmessage","destinationNumber":"+15551234567"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].MessageID != local.MessageID {
		t.Fatalf("message id = %q, want %q", msgs[0].MessageID, local.MessageID)
	}
}

func TestMatchLocalOutgoingMessageWarnsOnDedupMiss(t *testing.T) {
	cases := []struct {
		name        string
		localBody   string
		localOffset time.Duration // offset from incoming timestamp
		wantHit     bool
		wantWarn    bool
	}{
		{"exact match within window", "hello signal", 500 * time.Millisecond, true, false},
		{"body mismatch", "hello signal extra", 500 * time.Millisecond, false, true},
		{"timestamp drift beyond 15s", "hello signal", 20 * time.Second, false, true},
		{"no local candidate at all", "", 0, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			store, err := db.New(filepath.Join(dataDir, "messages.db"))
			if err != nil {
				t.Fatalf("db.New(): %v", err)
			}
			defer store.Close()

			conversationID := "signal:+15551234567"
			incomingTS := int64(1700000000000)
			if err := store.UpsertConversation(&db.Conversation{
				ConversationID: conversationID,
				Name:           "Taylor",
				LastMessageTS:  incomingTS - 1000,
				SourcePlatform: "signal",
			}); err != nil {
				t.Fatalf("UpsertConversation(): %v", err)
			}

			if tc.localBody != "" {
				localTS := incomingTS - tc.localOffset.Milliseconds()
				local := &db.Message{
					MessageID:      localOutgoingMessageID(conversationID, localTS, tc.localBody),
					ConversationID: conversationID,
					SenderName:     "Me",
					SenderNumber:   "+15551230000",
					Body:           tc.localBody,
					TimestampMS:    localTS,
					Status:         "sent",
					IsFromMe:       true,
					SourcePlatform: "signal",
				}
				if err := store.UpsertMessage(local); err != nil {
					t.Fatalf("UpsertMessage(): %v", err)
				}
			}

			var buf bytes.Buffer
			bridge := &Bridge{store: store, logger: zerolog.New(&buf)}
			hit := bridge.matchLocalOutgoingMessage(conversationID, "hello signal", incomingTS)
			if tc.wantHit && hit == nil {
				t.Fatalf("expected match, got nil")
			}
			if !tc.wantHit && hit != nil {
				t.Fatalf("expected no match, got %+v", hit)
			}

			logged := buf.String()
			gotWarn := strings.Contains(logged, "Signal outgoing dedup missed")
			if gotWarn != tc.wantWarn {
				t.Fatalf("warn logged=%v want=%v (log=%q)", gotWarn, tc.wantWarn, logged)
			}
		})
	}
}

func TestConnectEmitsSignalQRCodeAndStoresPairedAccount(t *testing.T) {
	configDir := t.TempDir()
	bridge, err := New(configDir, nil, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	originalStartLink := startSignalLink
	originalRun := runSignalCLI
	originalVersionProbe := probeSignalCLIVersion
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		probeSignalCLIVersion = originalVersionProbe
		startSignalLink = originalStartLink
	}()
	probeSignalCLIVersion = func(context.Context) ([]byte, error) {
		return []byte("signal-cli 0.14.5"), nil
	}

	releaseWait := make(chan struct{})
	var callsMu sync.Mutex
	var calls [][]string
	startSignalLink = func(ctx context.Context, cfg string) (io.ReadCloser, func() error, error) {
		reader := io.NopCloser(strings.NewReader("sgnl://linkdevice?uuid=test\n"))
		wait := func() error {
			if err := os.MkdirAll(filepath.Join(cfg, "data"), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(cfg, "data", "accounts.json"), []byte(`[{"number":"+15551230000"}]`), 0600); err != nil {
				return err
			}
			<-releaseWait
			return nil
		}
		return reader, wait, nil
	}
	runSignalCLI = func(ctx context.Context, cfg string, args ...string) ([]byte, error) {
		callsMu.Lock()
		calls = append(calls, append([]string(nil), args...))
		callsMu.Unlock()
		if len(args) >= 3 && args[0] == "--output" && args[2] == "listAccounts" {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		return []byte{}, nil
	}

	if err := bridge.Connect(); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		return bridge.Status().QRAvailable
	})
	close(releaseWait)
	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return status.Paired && status.Account == "+15551230000"
	})
	waitForCondition(t, 2*time.Second, func() bool {
		callsMu.Lock()
		defer callsMu.Unlock()
		for _, args := range calls {
			if len(args) == 3 && args[0] == "-a" && args[1] == "+15551230000" && args[2] == "sendSyncRequest" {
				return true
			}
		}
		return false
	})

}

func TestCloseDuringSignalPairingSkipsPostCancelAccountProbe(t *testing.T) {
	configDir := t.TempDir()
	bridge, err := New(configDir, nil, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	originalStartLink := startSignalLink
	originalRun := runSignalCLI
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		startSignalLink = originalStartLink
	}()

	var probeCalls atomic.Int32
	startSignalLink = func(ctx context.Context, cfg string) (io.ReadCloser, func() error, error) {
		reader := io.NopCloser(strings.NewReader("sgnl://linkdevice?uuid=test\n"))
		wait := func() error {
			<-ctx.Done()
			return ctx.Err()
		}
		return reader, wait, nil
	}
	runSignalCLI = func(ctx context.Context, cfg string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "--output" && args[2] == "listAccounts" {
			probeCalls.Add(1)
		}
		return []byte{}, nil
	}

	if err := bridge.Connect(); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		return bridge.Status().QRAvailable
	})

	done := make(chan error, 1)
	go func() {
		done <- bridge.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close(): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after pairing cancellation")
	}
	if got := probeCalls.Load(); got != 0 {
		t.Fatalf("post-cancel account probe calls = %d, want 0", got)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
