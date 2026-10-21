package signallive

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestParseAttachmentDownloadFailuresReadsBothSignalCLIFormats(t *testing.T) {
	output := strings.Join([]string{
		"INFO  AccountHelper - The Signal protocol expects that incoming messages are regularly received.",
		// signal-cli 0.14.9: the exception follows the WARN, stack frames and all.
		"WARN  AttachmentHelper - Failed to download attachment (AAAAbbbbCCCCdddd1234), ignoring",
		"java.io.IOException: Connection reset",
		"\tat org.whispersystems.signalservice.internal.push.PushServiceSocket.retrieveAttachment(PushServiceSocket.java:1301)",
		"Caused by: java.net.SocketException: Connection reset",
		"\tat java.base/sun.nio.ch.NioSocketImpl.implRead(NioSocketImpl.java:318)",
		"\t... 14 common frames omitted",
		`{"envelope":{"source":"+15551234567","timestamp":1,"dataMessage":{"timestamp":1,"attachments":[{"id":"AAAAbbbbCCCCdddd1234.html"}]}}}`,
		// signal-cli 0.14.8 and earlier: the message on the same line.
		"WARN  AttachmentHelper - Failed to download attachment (9876543210), ignoring: Attachment pointer has no digest.",
		// The preview WARN is about a thumbnail, not the attachment.
		"WARN  AttachmentHelper - Failed to download attachment preview, ignoring: timeout",
		// Verbose layout, exception text interrupted by the next log line.
		"2026-10-21T22:36:50.123Z [main] WARN  AttachmentHelper - Failed to download attachment (with/slash+plus), ignoring",
		"java.io.InterruptedIOException: interrupted",
		"2026-10-21T22:36:50.125Z [main] INFO  ReceiveHelper - next",
		"WARN  AttachmentHelper - Failed to download attachment (silent), ignoring",
		"",
	}, "\n")

	got := parseAttachmentDownloadFailures([]byte(output))
	want := attachmentDownloadFailures{
		"AAAAbbbbCCCCdddd1234": "java.io.IOException: Connection reset <- java.net.SocketException: Connection reset",
		"9876543210":           "Attachment pointer has no digest.",
		"with_slash_plus":      "java.io.InterruptedIOException: interrupted",
		"silent":               "signal-cli reported no cause",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAttachmentDownloadFailures() =\n%#v\nwant\n%#v", got, want)
	}
}

func TestParseAttachmentDownloadFailuresIgnoresCleanOutput(t *testing.T) {
	output := `{"envelope":{"source":"+15551234567","timestamp":1,"dataMessage":{"timestamp":1,"attachments":[{"id":"x.png"}]}}}` + "\n" +
		"WARN  AttachmentHelper - Failed to download attachment preview, ignoring: timeout\n"
	if got := parseAttachmentDownloadFailures([]byte(output)); got != nil {
		t.Fatalf("parseAttachmentDownloadFailures(clean) = %#v, want nil", got)
	}
}

func TestParseAttachmentDownloadFailuresBoundsTheCause(t *testing.T) {
	output := "WARN  AttachmentHelper - Failed to download attachment (big), ignoring: " + strings.Repeat("x", 5000)
	cause := parseAttachmentDownloadFailures([]byte(output))["big"]
	if len(cause) > maxAttachmentFailureText+len("…") || !strings.HasSuffix(cause, "…") {
		t.Fatalf("cause has %d bytes (suffix %q), want at most %d plus an ellipsis", len(cause), cause[len(cause)-3:], maxAttachmentFailureText)
	}
}

// Property: a failure block never swallows an envelope line, so every
// envelope the receive output carries still reaches processReceiveLine, and
// every failure block yields exactly its sanitized remote id.
func TestParseAttachmentDownloadFailuresProperty(t *testing.T) {
	fragments := []string{
		"java.io.IOException: boom",
		"\tat a.b.C.d(C.java:1)",
		"Caused by: java.net.SocketTimeoutException: timeout",
		"\t... 3 common frames omitted",
		"INFO  AccountHelper - noise",
		"",
		"plain text that is neither",
	}
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		var lines []string
		want := map[string]bool{}
		envelopes := 0
		for block := 0; block < 1+rng.Intn(6); block++ {
			switch rng.Intn(3) {
			case 0:
				id := fmt.Sprintf("id%d-%c", block, "a/b+c_d.e"[rng.Intn(9)])
				lines = append(lines, "WARN  AttachmentHelper - Failed to download attachment ("+id+"), ignoring")
				want[sanitizeSignalAttachmentID(id)] = true
			case 1:
				lines = append(lines, fmt.Sprintf(`{"envelope":{"timestamp":%d}}`, block))
				envelopes++
			default:
				lines = append(lines, fragments[rng.Intn(len(fragments))])
			}
			for extra := rng.Intn(3); extra > 0; extra-- {
				lines = append(lines, fragments[rng.Intn(len(fragments))])
			}
		}
		output := []byte(strings.Join(lines, "\n"))
		got := parseAttachmentDownloadFailures(output)
		if len(got) != len(want) {
			return false
		}
		for id := range want {
			if got[id] == "" {
				return false
			}
		}
		for _, cause := range got {
			if strings.Contains(cause, `{"envelope"`) || strings.Contains(cause, "\tat ") {
				return false
			}
		}
		return receiveOutputEnvelopeCount(output) == envelopes
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

func receiveOutputEnvelopeCount(output []byte) int {
	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "{") {
			count++
		}
	}
	return count
}

func TestAttachmentDownloadFailuresMatchEnvelopeAttachmentIDs(t *testing.T) {
	failures := attachmentDownloadFailures{
		"Q3x9KpLmN2vB7cRtYw4z": "cause-html",
		"plain":                "cause-plain",
		"edit1":                "cause-edit",
		"sent1":                "cause-sent",
		"dotted.remote":        "cause-dotted",
	}
	env := &signalEnvelope{
		DataMessage: &signalDataMessage{Attachments: []signalAttachment{
			{ID: "Q3x9KpLmN2vB7cRtYw4z.html"},
			{ID: "plain"},
			{ID: "plainer.jpg"},
			{ID: "dotted.remote.png"},
		}},
		EditMessage: &signalEditMessage{DataMessage: &signalDataMessage{Attachments: []signalAttachment{{ID: "edit1.gif"}}}},
		SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{Attachments: []signalAttachment{{ID: "sent1.pdf"}}}},
	}
	got := failures.forEnvelope(env)
	want := map[string]string{
		"Q3x9KpLmN2vB7cRtYw4z.html": "cause-html",
		"plain":                     "cause-plain",
		"dotted.remote.png":         "cause-dotted",
		"edit1.gif":                 "cause-edit",
		"sent1.pdf":                 "cause-sent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forEnvelope() = %#v, want %#v", got, want)
	}
	if got := failures.forEnvelope(&signalEnvelope{DataMessage: &signalDataMessage{}}); got != nil {
		t.Fatalf("forEnvelope(no attachments) = %#v, want nil", got)
	}
	if got := attachmentDownloadFailures(nil).forEnvelope(env); got != nil {
		t.Fatalf("nil failures forEnvelope() = %#v, want nil", got)
	}
}

// Property: a failure for remote id R belongs to attachment id A exactly when
// A is R or R.<ext>; a different remote id that shares a prefix never matches.
func TestAttachmentDownloadFailureMatchingProperty(t *testing.T) {
	property := func(rawRemote, rawOther string, ext uint8) bool {
		remote := sanitizeSignalAttachmentID("r" + rawRemote)
		other := sanitizeSignalAttachmentID("o" + rawOther)
		extension := []string{"", ".png", ".html", ".jpg"}[ext%4]
		failures := attachmentDownloadFailures{remote: "cause"}
		id := remote + extension
		env := &signalEnvelope{DataMessage: &signalDataMessage{Attachments: []signalAttachment{
			{ID: id}, {ID: other + extension}, {ID: remote + "x" + extension},
		}}}
		got := failures.forEnvelope(env)
		if got[id] != "cause" {
			return false
		}
		// The o-prefixed id never equals the r-prefixed one, and R+"x" is a
		// different remote id that merely shares R's prefix.
		return len(got) == 1
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261022))}); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeSignalAttachmentIDMirrorsSignalCLI(t *testing.T) {
	for raw, want := range map[string]string{
		"AbC-_09.x":   "AbC-_09.x",
		"a/b+c=d":     "a_b_c_d",
		"ünïcode":     "_n_code",
		"1234567890":  "1234567890",
		"with space ": "with_space_",
	} {
		if got := sanitizeSignalAttachmentID(raw); got != want {
			t.Errorf("sanitizeSignalAttachmentID(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBridgeDownloadMediaRefReportsAttachmentSignalCLINeverStored(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		// signal-cli 0.14.9's exact answer for an attachment its receive never stored.
		return []byte("Could not find attachment with ID: Q3x9KpLmN2vB7cRtYw4z.html\n"), errors.New("exit status 1")
	}

	_, _, err := bridge.DownloadMediaRef(
		"+15551230000", "remote", "Q3x9KpLmN2vB7cRtYw4z.html", "", "0d6c1f9e-3b8a-4c2e-9f71-5a4e2b8c7d10", false,
	)
	if !errors.Is(err, ErrAttachmentNotStored) {
		t.Fatalf("DownloadMediaRef() error = %v, want ErrAttachmentNotStored", err)
	}
	if !IsCommandError(err) {
		t.Fatalf("DownloadMediaRef() error = %v, want it to stay a CommandError", err)
	}
	if want := "download Signal attachment: exit status 1: Could not find attachment with ID: Q3x9KpLmN2vB7cRtYw4z.html"; !strings.Contains(err.Error(), want) {
		t.Fatalf("DownloadMediaRef() error = %q, want it to keep %q", err, want)
	}
}

func TestBridgeDownloadMediaRefOtherFailuresAreNotNotStored(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("An error occurred reading attachment: x.png"), errors.New("exit status 3")
	}
	_, _, err := bridge.DownloadMediaRef("+15551230000", "remote", "x.png", "", "+15551234567", false)
	if err == nil || errors.Is(err, ErrAttachmentNotStored) || !IsCommandError(err) {
		t.Fatalf("DownloadMediaRef() error = %v, want a CommandError that is not ErrAttachmentNotStored", err)
	}
}

func TestBridgeDownloadMediaRefSkipsLogLinesBeforeThePayload(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	payload := base64.StdEncoding.EncodeToString([]byte("attachment bytes"))
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("WARN  AccountHelper - Messages have been last received 9 days ago. The Signal protocol expects that incoming messages are regularly received.\n" + payload + "\n"), nil
	}
	data, _, err := bridge.DownloadMediaRef("+15551230000", "remote", "x.bin", "", "+15551234567", false)
	if err != nil || string(data) != "attachment bytes" {
		t.Fatalf("DownloadMediaRef() = %q, %v; want the payload after the log line", data, err)
	}
}

func TestHandleReceiveOutputHandsAttachmentErrorsToTheirEnvelope(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bridge := &Bridge{store: store, configDir: t.TempDir(), logger: zerolog.Nop(), contactByACI: map[string]string{}}
	var captured []IngressLine
	unregister := bridge.ObserveIngressLine(func(line IngressLine) { captured = append(captured, line) })
	defer unregister()

	output := strings.Join([]string{
		"WARN  AttachmentHelper - Failed to download attachment (failedRemote), ignoring",
		"java.io.IOException: Connection reset",
		`{"envelope":{"source":"+15551234567","sourceNumber":"+15551234567","timestamp":2,"dataMessage":{"timestamp":2,"attachments":[{"id":"fineRemote.png"}]}},"account":"+15551230000"}`,
		`{"envelope":{"source":"+15551234567","sourceNumber":"+15551234567","timestamp":3,"dataMessage":{"timestamp":3,"attachments":[{"id":"failedRemote.html"},{"id":"other.png"}]}},"account":"+15551230000"}`,
	}, "\n")
	if err := bridge.handleReceiveOutput("+15551230000", []byte(output)); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("observed %d lines, want 2", len(captured))
	}
	if captured[0].AttachmentErrors != nil {
		t.Fatalf("first envelope attachment errors = %#v, want nil", captured[0].AttachmentErrors)
	}
	want := map[string]string{"failedRemote.html": "java.io.IOException: Connection reset"}
	if !reflect.DeepEqual(captured[1].AttachmentErrors, want) {
		t.Fatalf("second envelope attachment errors = %#v, want %#v", captured[1].AttachmentErrors, want)
	}
}

func TestObserveIngressStillServesFourArgumentObservers(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	var got []string
	unregister := bridge.ObserveIngress(func(account string, line []byte, source, destination string) {
		got = append(got, account, string(line), source, destination)
	})
	bridge.observeIngress(IngressLine{
		Account: "+1", Line: []byte("{}"), ResolvedSource: "src", ResolvedDestination: "dst",
		AttachmentErrors: map[string]string{"a": "b"},
	})
	unregister()
	bridge.observeIngress(IngressLine{Account: "+2", Line: []byte("{}")})
	if want := []string{"+1", "{}", "src", "dst"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy observer saw %q, want %q", got, want)
	}
}
