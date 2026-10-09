package signallive

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"testing/quick"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

const quoteTestAccount = "+15551230000"

func newQuoteTestBridge(t *testing.T, messages ...*db.Message) *Bridge {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, message := range messages {
		if err := store.UpsertMessage(message); err != nil {
			t.Fatalf("UpsertMessage(%q): %v", message.MessageID, err)
		}
	}
	return &Bridge{
		account:   quoteTestAccount,
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
	}
}

// TestSignalQuoteArgsResolvesV2RemoteIDs covers the IDs the durable outbox
// forwards as ReplyTo.RemoteID. v2 keys a Signal message by its legacy ID
// without the "signal:" prefix, so before the fallback every v2-native reply
// failed with "signal reply target not found".
func TestSignalQuoteArgsResolvesV2RemoteIDs(t *testing.T) {
	const conversationID = "signal:+15550003333"
	bridge := newQuoteTestBridge(t,
		&db.Message{MessageID: "signal:1700000000002", SourceID: "1700000000002", ConversationID: conversationID, SenderNumber: quoteTestAccount, Body: "sent", TimestampMS: 1_700_000_000_002, IsFromMe: true, SourcePlatform: "signal"},
		&db.Message{MessageID: "signal:5f0c1e", SourceID: "5f0c1e", ConversationID: conversationID, SenderNumber: "+15550003333", Body: "received", TimestampMS: 1_700_000_000_003, SourcePlatform: "signal"},
		&db.Message{MessageID: "signal:local:9a8b", SourceID: "local:9a8b", ConversationID: conversationID, Body: "local", TimestampMS: 1_700_000_000_004, IsFromMe: true, SourcePlatform: "signal"},
		// A non-Signal row whose ID is a bare Signal timestamp must not
		// shadow the Signal message it collides with.
		&db.Message{MessageID: "1700000000002", ConversationID: "google-thread", Body: "google", TimestampMS: 1_700_000_000_005, SourcePlatform: "sms"},
	)
	quote := func(timestampMS int64, author, body string) []string {
		return []string{"--quote-timestamp", strconv.FormatInt(timestampMS, 10), "--quote-author", author, "--quote-message", body}
	}
	for _, test := range []struct {
		replyToID string
		want      []string
	}{
		{replyToID: "", want: nil},
		{replyToID: "signal:1700000000002", want: quote(1_700_000_000_002, quoteTestAccount, "sent")},
		{replyToID: "1700000000002", want: quote(1_700_000_000_002, quoteTestAccount, "sent")},
		{replyToID: "signal:5f0c1e", want: quote(1_700_000_000_003, "+15550003333", "received")},
		{replyToID: "5f0c1e", want: quote(1_700_000_000_003, "+15550003333", "received")},
		{replyToID: "local:9a8b", want: quote(1_700_000_000_004, quoteTestAccount, "local")},
		{replyToID: " 1700000000002 ", want: quote(1_700_000_000_002, quoteTestAccount, "sent")},
	} {
		got, err := bridge.signalQuoteArgs(test.replyToID, quoteTestAccount)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("signalQuoteArgs(%q) = %q, %v; want %q", test.replyToID, got, err, test.want)
		}
	}
	for _, missing := range []string{"9999", "signal:9999", "signal:signal:1700000000002"} {
		if got, err := bridge.signalQuoteArgs(missing, quoteTestAccount); err == nil {
			t.Errorf("signalQuoteArgs(%q) = %q, want not found", missing, got)
		}
	}
}

// legacyExactQuoteArgs is signalQuoteArgs before the v2 fallback: exact legacy
// ID lookup only. It is the reference for the differential property below.
func legacyExactQuoteArgs(b *Bridge, replyToID, account string) ([]string, error) {
	target, err := b.store.GetMessageByID(replyToID)
	if err != nil {
		return nil, err
	}
	if target == nil || target.SourcePlatform != "signal" {
		return nil, errors.New("signal reply target not found")
	}
	exact := &Bridge{account: b.account, store: b.store, logger: b.logger}
	return exact.signalQuoteArgs(target.MessageID, account)
}

// TestSignalQuoteArgsFallbackOnlyAddsResolutions is a differential property
// over random legacy stores and reply IDs: every ID the exact lookup resolved
// still resolves to the same quote, and an ID that now resolves only through
// the fallback quotes exactly the message whose legacy ID is "signal:" + ID.
func TestSignalQuoteArgsFallbackOnlyAddsResolutions(t *testing.T) {
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		var messages []*db.Message
		var ids []string
		for index := range 1 + random.Intn(8) {
			suffix := strconv.FormatInt(1_700_000_000_000+int64(random.Intn(6)), 10)
			if random.Intn(3) == 0 {
				suffix = fmt.Sprintf("%x", random.Intn(6))
			}
			platform := "signal"
			messageID := "signal:" + suffix
			if random.Intn(4) == 0 {
				platform, messageID = "sms", suffix
			}
			messages = append(messages, &db.Message{
				MessageID: messageID, SourceID: suffix, ConversationID: "signal:+15550003333",
				SenderNumber: "+15550003333", Body: fmt.Sprintf("body %d", index),
				TimestampMS: 1_700_000_000_000 + int64(index), SourcePlatform: platform,
			})
			ids = append(ids, messageID, suffix, "signal:"+messageID)
		}
		bridge := newQuoteTestBridge(t, messages...)
		for _, replyToID := range ids {
			got, gotErr := bridge.signalQuoteArgs(replyToID, quoteTestAccount)
			want, wantErr := legacyExactQuoteArgs(bridge, replyToID, quoteTestAccount)
			if wantErr == nil {
				if gotErr != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("seed %d: signalQuoteArgs(%q) = %q, %v; exact lookup gave %q", seed, replyToID, got, gotErr, want)
					return false
				}
				continue
			}
			if gotErr != nil {
				continue
			}
			prefixed, prefixedErr := legacyExactQuoteArgs(bridge, "signal:"+replyToID, quoteTestAccount)
			if prefixedErr != nil || !reflect.DeepEqual(got, prefixed) {
				t.Errorf("seed %d: signalQuoteArgs(%q) = %q via fallback; exact lookup of the prefixed ID gave %q, %v",
					seed, replyToID, got, prefixed, prefixedErr)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 60, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}
