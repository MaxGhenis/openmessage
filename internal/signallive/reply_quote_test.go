package signallive

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/rs/zerolog"
)

const replyQuoteTestAccount = "+15551230000"

func TestQuoteArgsRules(t *testing.T) {
	const aci = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	resolve := func(value string) string {
		if value == aci {
			return "+15557654321"
		}
		return value
	}
	tests := []struct {
		name    string
		target  ReplyTarget
		want    []string
		wantErr string
	}{
		{
			name: "own outbox send quotes the transport timestamp, not the submit time",
			target: ReplyTarget{
				RemoteID: "1700000000555",
				SentAt:   time.UnixMilli(1_700_000_000_000),
				Text:     "sent through the outbox",
			},
			want: quoteArgs("1700000000555", replyQuoteTestAccount, "sent through the outbox"),
		},
		{
			name: "own message under a migrated local alias quotes its occurred time",
			target: ReplyTarget{
				RemoteID: "local:c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735",
				SentAt:   time.UnixMilli(1_700_000_000_321),
				Text:     "from the phone",
			},
			want: quoteArgs("1700000000321", replyQuoteTestAccount, "from the phone"),
		},
		{
			name: "own message authored as the account address",
			target: ReplyTarget{
				RemoteID: "1700000000999",
				AuthorID: " " + replyQuoteTestAccount + " ",
				SentAt:   time.UnixMilli(1_700_000_000_111),
				Text:     "me",
			},
			want: quoteArgs("1700000000999", replyQuoteTestAccount, "me"),
		},
		{
			name: "incoming message quotes its occurred time and author",
			target: ReplyTarget{
				RemoteID: "f48818f15483f503bb133d92360ca8e2fbd8287e",
				AuthorID: "+15551234567",
				SentAt:   time.UnixMilli(1_700_000_000_123),
				Text:     "  hello there  ",
			},
			want: quoteArgs("1700000000123", "+15551234567", "hello there"),
		},
		{
			name: "incoming message never takes a numeric remote ID as its timestamp",
			target: ReplyTarget{
				RemoteID: "1700000001000",
				AuthorID: "+15551234567",
				SentAt:   time.UnixMilli(1_700_000_000_456),
				Text:     "legacy numeric id",
			},
			want: quoteArgs("1700000000456", "+15551234567", "legacy numeric id"),
		},
		{
			name: "incoming ACI author resolves to its number",
			target: ReplyTarget{
				RemoteID: "f48818f15483f503bb133d92360ca8e2fbd8287e",
				AuthorID: aci,
				SentAt:   time.UnixMilli(1_700_000_000_123),
				Text:     "from an ACI",
			},
			want: quoteArgs("1700000000123", "+15557654321", "from an ACI"),
		},
		{
			name: "body-less photo quotes a photo placeholder",
			target: ReplyTarget{
				RemoteID: "1700000000555", SentAt: time.UnixMilli(1), HasAttachment: true, AttachmentMIME: "image/png",
			},
			want: quoteArgs("1700000000555", replyQuoteTestAccount, "[Photo]"),
		},
		{
			name: "attachment of unknown type quotes the generic placeholder",
			target: ReplyTarget{
				AuthorID: "+15551234567", SentAt: time.UnixMilli(1_700_000_000_123), Text: "   ", HasAttachment: true,
			},
			want: quoteArgs("1700000000123", "+15551234567", "[Attachment]"),
		},
		{
			name:   "body-less message without an attachment",
			target: ReplyTarget{AuthorID: "+15551234567", SentAt: time.UnixMilli(1_700_000_000_123)},
			want:   quoteArgs("1700000000123", "+15551234567", "Attachment"),
		},
		{
			name:    "no timestamp at all",
			target:  ReplyTarget{RemoteID: "f48818f15483f503bb133d92360ca8e2fbd8287e", AuthorID: "+15551234567"},
			wantErr: "signal reply target timestamp is unavailable",
		},
		{
			name:    "own message whose request ID is not a timestamp and no occurred time",
			target:  ReplyTarget{RemoteID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"},
			wantErr: "signal reply target timestamp is unavailable",
		},
		{
			name:    "pre-epoch occurred time",
			target:  ReplyTarget{AuthorID: "+15551234567", SentAt: time.UnixMilli(-5)},
			wantErr: "signal reply target timestamp is unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := QuoteArgs(test.target, replyQuoteTestAccount, resolve)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("QuoteArgs() = %q, %v; want error %q", got, err, test.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("QuoteArgs() = %q, %v; want %q", got, err, test.want)
			}
		})
	}

	if _, err := QuoteArgs(ReplyTarget{SentAt: time.UnixMilli(1)}, "", resolve); err == nil ||
		err.Error() != "signal reply target author is unavailable" {
		t.Fatalf("QuoteArgs() without an account = %v, want author unavailable", err)
	}
}

// TestQuoteArgsInvariants checks QuoteArgs over seeded random targets:
//
//   - Shape: success yields exactly --quote-timestamp, --quote-author and
//     --quote-message, with a positive decimal timestamp and non-empty author
//     and text.
//   - Timestamp source: a self-authored target whose RemoteID is a positive
//     decimal uses it; every other target uses SentAt. QuoteArgs fails exactly
//     when the chosen source is not positive.
//   - Author: self-authored targets (empty AuthorID or the account) quote the
//     account; others quote resolveAuthor(trimmed AuthorID).
//   - Text: the trimmed body, else the attachment placeholder when the target
//     has an attachment, else "Attachment".
//   - Determinism: the same target always yields the same arguments.
func TestQuoteArgsInvariants(t *testing.T) {
	random := rand.New(rand.NewSource(20261009))
	resolve := func(value string) string { return "resolved:" + value }
	authors := []string{"", " ", replyQuoteTestAccount, " " + replyQuoteTestAccount, "+15551234567", "9f4b50e3-ebf2-413c-a856-161756a6161a"}
	remoteIDs := []string{
		"", "0", "-17", "+17", "1700000000555", " 1700000000556 ", "99999999999999999999",
		"f48818f15483f503bb133d92360ca8e2fbd8287e", "local:c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735",
		"0f1e2d3c4b5a69788796a5b4c3d2e1f0", "17000000005x5",
	}
	bodies := []string{"", "  ", "hi", "  padded  ", "multi\nline"}
	mimes := []string{"", "image/png", "VIDEO/MP4", "audio/aac", "application/pdf"}
	for iteration := 0; iteration < 5000; iteration++ {
		target := ReplyTarget{
			RemoteID:       remoteIDs[random.Intn(len(remoteIDs))],
			AuthorID:       authors[random.Intn(len(authors))],
			Text:           bodies[random.Intn(len(bodies))],
			HasAttachment:  random.Intn(2) == 0,
			AttachmentMIME: mimes[random.Intn(len(mimes))],
		}
		switch random.Intn(4) {
		case 0:
		case 1:
			target.SentAt = time.UnixMilli(-random.Int63n(1_000_000))
		default:
			target.SentAt = time.UnixMilli(1 + random.Int63n(2_000_000_000_000))
		}

		got, err := QuoteArgs(target, replyQuoteTestAccount, resolve)
		again, againErr := QuoteArgs(target, replyQuoteTestAccount, resolve)
		if !slices.Equal(got, again) || (err == nil) != (againErr == nil) {
			t.Fatalf("iteration %d: QuoteArgs(%+v) is not deterministic", iteration, target)
		}

		author := strings.TrimSpace(target.AuthorID)
		self := author == "" || author == replyQuoteTestAccount
		wantAuthor := replyQuoteTestAccount
		if !self {
			wantAuthor = "resolved:" + author
		}
		var wantTimestamp int64
		if trimmed := strings.TrimSpace(target.RemoteID); self && trimmed != "" &&
			strings.Trim(trimmed, "0123456789") == "" {
			if parsed, parseErr := strconv.ParseInt(trimmed, 10, 64); parseErr == nil && parsed > 0 {
				wantTimestamp = parsed
			}
		}
		if wantTimestamp == 0 && !target.SentAt.IsZero() {
			wantTimestamp = target.SentAt.UnixMilli()
		}
		if wantTimestamp <= 0 {
			if err == nil || err.Error() != "signal reply target timestamp is unavailable" {
				t.Fatalf("iteration %d: QuoteArgs(%+v) = %q, %v; want unavailable timestamp", iteration, target, got, err)
			}
			continue
		}
		wantText := strings.TrimSpace(target.Text)
		if wantText == "" && target.HasAttachment {
			wantText = signalAttachmentPlaceholder([]signalAttachment{{ContentType: target.AttachmentMIME}})
		}
		if wantText == "" {
			wantText = "Attachment"
		}
		want := quoteArgs(strconv.FormatInt(wantTimestamp, 10), wantAuthor, wantText)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("iteration %d: QuoteArgs(%+v) = %q, %v; want %q", iteration, target, got, err, want)
		}
	}
}

// TestLegacyQuoteLookupMatchesPreChangeImplementation is a differential check
// of the legacy path: for random legacy Signal rows, signalQuoteArgs (now built
// on QuoteArgs) returns exactly what the pre-change implementation, kept below
// verbatim, returned.
func TestLegacyQuoteLookupMatchesPreChangeImplementation(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	const aci = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	bridge := &Bridge{
		account:      replyQuoteTestAccount,
		connected:    true,
		configDir:    t.TempDir(),
		store:        store,
		logger:       zerolog.Nop(),
		contactByACI: map[string]string{aci: "+15557654321"},
	}
	stubSignalCLIForQuoteTest(t)

	senders := []string{"", " ", replyQuoteTestAccount, "+15551234567", " +15551234567 ", aci, "11111111-2222-3333-4444-555555555555"}
	bodies := []string{"", "  ", "quoted", "  padded quote  "}
	mimes := []string{"", "image/jpeg", "video/mp4", "audio/ogg", "application/zip"}
	for index := 0; index < 400; index++ {
		message := &db.Message{
			MessageID:      fmt.Sprintf("signal:legacy-%d", index),
			ConversationID: "signal:+15551234567",
			SenderNumber:   senders[random.Intn(len(senders))],
			Body:           bodies[random.Intn(len(bodies))],
			TimestampMS:    1 + random.Int63n(2_000_000_000_000),
			IsFromMe:       random.Intn(3) == 0,
			SourcePlatform: "signal",
		}
		if random.Intn(2) == 0 {
			message.MediaID = fmt.Sprintf("signalatt:%d", index)
			message.MimeType = mimes[random.Intn(len(mimes))]
		}
		if err := store.UpsertMessage(message); err != nil {
			t.Fatalf("UpsertMessage(): %v", err)
		}
		got, err := bridge.signalQuoteArgs(message.MessageID, replyQuoteTestAccount)
		want, wantErr := preChangeLegacyQuoteArgs(bridge, message, replyQuoteTestAccount)
		if !slices.Equal(got, want) || errorText(err) != errorText(wantErr) {
			t.Fatalf("row %+v: signalQuoteArgs = %q, %v; pre-change = %q, %v", message, got, err, want, wantErr)
		}
	}
}

func TestReplyQuoteArgsUsesTheDescriptionWithoutALegacyRow(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	for _, legacy := range []*db.Store{store, nil} {
		bridge := &Bridge{account: replyQuoteTestAccount, connected: true, store: legacy, logger: zerolog.Nop()}
		got, err := bridge.replyQuoteArgs(ReplyTarget{
			RemoteID: "1700000000555",
			SentAt:   time.UnixMilli(1_700_000_000_000),
			Text:     "only v2 holds this",
		}, replyQuoteTestAccount)
		want := quoteArgs("1700000000555", replyQuoteTestAccount, "only v2 holds this")
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("replyQuoteArgs(legacy store %v) = %q, %v; want %q", legacy != nil, got, err, want)
		}
	}

	bridge := &Bridge{account: replyQuoteTestAccount, connected: true, store: store, logger: zerolog.Nop()}
	if got, err := bridge.replyQuoteArgs(ReplyTarget{}, replyQuoteTestAccount); err != nil || got != nil {
		t.Fatalf("replyQuoteArgs(no reply) = %q, %v; want no quote", got, err)
	}
	// An undescribed target still needs its legacy row, as before.
	if _, err := bridge.replyQuoteArgs(ReplyTarget{RemoteID: "1700000000555"}, replyQuoteTestAccount); err == nil ||
		err.Error() != "signal reply target not found" {
		t.Fatalf("replyQuoteArgs(undescribed, no legacy row) = %v, want not found", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:legacy-quoted",
		ConversationID: "signal:+15551234567",
		SenderNumber:   "+15551234567",
		Body:           "legacy row",
		TimestampMS:    1_700_000_000_042,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}
	got, err := bridge.replyQuoteArgs(ReplyTarget{RemoteID: "signal:legacy-quoted"}, replyQuoteTestAccount)
	if want := quoteArgs("1700000000042", "+15551234567", "legacy row"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("replyQuoteArgs(legacy ID) = %q, %v; want %q", got, err, want)
	}
}

func TestReplyQuoteArgsQuotesALegacyIDFromItsLegacyRow(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	// The legacy-primary mirror keys its v2 copy of a reply target by the
	// legacy ID and records neither sender nor attachment.
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:1700000000700",
		ConversationID: "signal:+15551234567",
		SenderNumber:   "+15551234567",
		TimestampMS:    1_700_000_000_700,
		MediaID:        "signalatt:mirrored",
		MimeType:       "image/png",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}
	bridge := &Bridge{account: replyQuoteTestAccount, connected: true, store: store, logger: zerolog.Nop()}
	mirrored := ReplyTarget{RemoteID: "signal:1700000000700", SentAt: time.UnixMilli(1_700_000_000_700)}
	got, err := bridge.replyQuoteArgs(mirrored, replyQuoteTestAccount)
	if want := quoteArgs("1700000000700", "+15551234567", "[Photo]"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("replyQuoteArgs(mirrored legacy ID) = %q, %v; want the legacy row's %q", got, err, want)
	}

	// Without a legacy row the description is all there is.
	mirrored.RemoteID = "signal:1700000000701"
	mirrored.SentAt = time.UnixMilli(1_700_000_000_701)
	got, err = bridge.replyQuoteArgs(mirrored, replyQuoteTestAccount)
	if want := quoteArgs("1700000000701", replyQuoteTestAccount, "Attachment"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("replyQuoteArgs(legacy ID without a row) = %q, %v; want %q", got, err, want)
	}
}

func TestBridgeSendRequestsQuoteADescribedTargetTheLegacyStoreLacks(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	success, err := os.ReadFile(filepath.Join("testdata", "send-media-success.json"))
	if err != nil {
		t.Fatalf("read success fixture: %v", err)
	}
	const aci = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	bridge := &Bridge{
		account:      replyQuoteTestAccount,
		connected:    true,
		configDir:    t.TempDir(),
		store:        store,
		logger:       zerolog.Nop(),
		contactByACI: map[string]string{aci: "+15557654321"},
	}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{}, args...)
		return success, nil
	}

	ownSend := ReplyTarget{
		RemoteID: "1700000000555",
		SentAt:   time.UnixMilli(1_700_000_000_000),
		Text:     "sent through the v2 outbox",
	}
	if _, err := bridge.SendTextRequest("signal:+15551234567", "replying to myself", ownSend); err != nil {
		t.Fatalf("SendTextRequest(own send): %v", err)
	}
	wantText := []string{
		"--output=json", "-a", replyQuoteTestAccount, "send", "-m", "replying to myself",
		"--quote-timestamp", "1700000000555",
		"--quote-author", replyQuoteTestAccount,
		"--quote-message", "sent through the v2 outbox",
		"+15551234567",
	}
	if !slices.Equal(captured, wantText) {
		t.Fatalf("text args = %q, want %q", captured, wantText)
	}

	incoming := ReplyTarget{
		RemoteID:       "f48818f15483f503bb133d92360ca8e2fbd8287e",
		AuthorID:       aci,
		SentAt:         time.UnixMilli(1_700_000_000_123),
		HasAttachment:  true,
		AttachmentMIME: "image/jpeg",
	}
	if _, err := bridge.SendMediaRequest(
		"signal-group:group-id",
		strings.NewReader("png-bytes"),
		int64(len("png-bytes")),
		"reply.png",
		"image/png",
		"",
		incoming,
	); err != nil {
		t.Fatalf("SendMediaRequest(incoming): %v", err)
	}
	if len(captured) < 2 || !strings.HasPrefix(captured[4], "--attachment=") {
		t.Fatalf("media args = %q, want an attachment argument", captured)
	}
	wantMedia := []string{
		"--output=json", "-a", replyQuoteTestAccount, "send", captured[4],
		"--quote-timestamp", "1700000000123",
		"--quote-author", "+15557654321",
		"--quote-message", "[Photo]",
		"--group-id", "group-id",
	}
	if !slices.Equal(captured, wantMedia) {
		t.Fatalf("media args = %q, want %q", captured, wantMedia)
	}
}

func quoteArgs(timestamp, author, message string) []string {
	return []string{"--quote-timestamp", timestamp, "--quote-author", author, "--quote-message", message}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// stubSignalCLIForQuoteTest keeps resolveContactAddress's contact refresh
// from launching a real signal-cli.
func stubSignalCLIForQuoteTest(t *testing.T) {
	t.Helper()
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("signal-cli is not available in this test")
	}
}

// preChangeLegacyQuoteArgs is signalQuoteArgs's row-to-arguments logic as it
// stood before QuoteArgs, kept verbatim as the differential reference.
func preChangeLegacyQuoteArgs(b *Bridge, target *db.Message, account string) ([]string, error) {
	if target.TimestampMS == 0 {
		return nil, errors.New("signal reply target timestamp is unavailable")
	}
	author := normalizeSignalAddress(target.SenderNumber)
	if target.IsFromMe || addressesMatch(author, account) || author == "" {
		author = account
	} else {
		author = b.resolveContactAddress(author)
	}
	if author == "" {
		return nil, errors.New("signal reply target author is unavailable")
	}
	quoteBody := strings.TrimSpace(target.Body)
	if quoteBody == "" && target.MediaID != "" {
		quoteBody = signalAttachmentPlaceholder([]signalAttachment{{ContentType: target.MimeType}})
	}
	if quoteBody == "" {
		quoteBody = "Attachment"
	}
	return []string{
		"--quote-timestamp", strconv.FormatInt(target.TimestampMS, 10),
		"--quote-author", author,
		"--quote-message", quoteBody,
	}, nil
}
