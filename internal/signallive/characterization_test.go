package signallive

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestSignalIncomingSourceIDUsesStableSenderTimestampIdentity(t *testing.T) {
	const (
		conversationID = "signal:+15551234567"
		sender         = "+15557654321"
		timestamp      = int64(1700000000123)
	)

	base := signalIncomingSourceID(conversationID, sender, timestamp, "before edit")
	if got := signalIncomingSourceID("  "+conversationID+"  ", "  "+sender+"  ", timestamp, "after edit"); got != base {
		t.Fatalf("body/whitespace changed stable source ID: got %q, want %q", got, base)
	}

	changed := []struct {
		name           string
		conversationID string
		sender         string
		timestamp      int64
	}{
		{name: "conversation", conversationID: "signal:+15550000000", sender: sender, timestamp: timestamp},
		{name: "sender", conversationID: conversationID, sender: "+15550000000", timestamp: timestamp},
		{name: "timestamp", conversationID: conversationID, sender: sender, timestamp: timestamp + 1},
	}
	for _, tc := range changed {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalIncomingSourceID(tc.conversationID, tc.sender, tc.timestamp, "before edit"); got == base {
				t.Fatalf("changing %s did not change source ID %q", tc.name, got)
			}
		})
	}
}

func TestParseSignalContactsRecordedOutput(t *testing.T) {
	raw := []byte("INFO  AccountHelper - refreshing contacts\n" +
		"Number: +15551234567 ACI: d91b5024-f3db-4c82-98f8-2691974d6a9b Name:  Profile name: Michael Thorning Username:  Color:  Blocked: false Message expiration: disabled\n" +
		"Number: +15559876543 ACI: 9f4b50e3-ebf2-413c-a856-161756a6161a Name: Taylor Profile name: Taylor Username:  Color:  Blocked: false\n" +
		"Number: +15550000000 Name: missing ACI\n")

	got := parseSignalContacts(raw)
	want := map[string]string{
		"d91b5024-f3db-4c82-98f8-2691974d6a9b": "+15551234567",
		"9f4b50e3-ebf2-413c-a856-161756a6161a": "+15559876543",
	}
	if len(got) != len(want) {
		t.Fatalf("parseSignalContacts() = %#v, want %#v", got, want)
	}
	for aci, number := range want {
		if got[aci] != number {
			t.Fatalf("parseSignalContacts()[%q] = %q, want %q", aci, got[aci], number)
		}
	}
}

func TestSignalEnvelopeAndSentTargetAliases(t *testing.T) {
	t.Run("envelope source", func(t *testing.T) {
		cases := []struct {
			name string
			env  *signalEnvelope
			want string
		}{
			{name: "nil", want: ""},
			{name: "source", env: &signalEnvelope{Source: " +15550000001 "}, want: "+15550000001"},
			{name: "source UUID", env: &signalEnvelope{SourceUUID: "uuid-source"}, want: "uuid-source"},
			{name: "source service ID", env: &signalEnvelope{SourceServiceID: "service-source"}, want: "service-source"},
			{name: "source number", env: &signalEnvelope{SourceNumber: "+15550000002"}, want: "+15550000002"},
			{
				name: "documented precedence",
				env: &signalEnvelope{
					Source:          "legacy-source",
					SourceUUID:      "uuid-source",
					SourceServiceID: "service-source",
					SourceNumber:    "+15550000003",
				},
				want: "+15550000003",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := signalEnvelopeSource(tc.env); got != tc.want {
					t.Fatalf("signalEnvelopeSource() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("sent target", func(t *testing.T) {
		cases := []struct {
			name string
			sent *signalSentMessage
			want string
		}{
			{name: "nil", want: ""},
			{name: "destination", sent: &signalSentMessage{Destination: " legacy-target "}, want: "legacy-target"},
			{name: "destination service ID", sent: &signalSentMessage{DestinationServiceID: "service-target"}, want: "service-target"},
			{name: "destination UUID", sent: &signalSentMessage{DestinationUUID: "uuid-target"}, want: "uuid-target"},
			{name: "destination E164", sent: &signalSentMessage{DestinationE164: "+15550000004"}, want: "+15550000004"},
			{name: "destination number", sent: &signalSentMessage{DestinationNumber: "+15550000005"}, want: "+15550000005"},
			{
				name: "documented precedence",
				sent: &signalSentMessage{
					Destination:          "legacy-target",
					DestinationServiceID: "service-target",
					DestinationUUID:      "uuid-target",
					DestinationE164:      "+15550000006",
					DestinationNumber:    "+15550000007",
				},
				want: "+15550000007",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := signalSentTarget(tc.sent); got != tc.want {
					t.Fatalf("signalSentTarget() = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

func TestSignalQuoteAndMentionNormalization(t *testing.T) {
	const (
		conversationID = "signal-group:test-group"
		timestamp      = int64(1700000000123)
	)

	t.Run("quotes", func(t *testing.T) {
		if got := signalQuoteReplyID(conversationID, nil); got != "" {
			t.Fatalf("nil quote reply ID = %q, want empty", got)
		}
		if got := signalQuoteReplyID(conversationID, &signalQuotedMessage{Author: "+15550000001"}); got != "" {
			t.Fatalf("zero-timestamp quote reply ID = %q, want empty", got)
		}

		quote := &signalQuotedMessage{
			Timestamp: timestamp,
			Author:    "+15550000001",
			AuthorACI: "quote-author-aci",
			Text:      "original quote text",
		}
		want := "signal:" + signalIncomingSourceID(conversationID, "quote-author-aci", timestamp, "")
		if got := signalQuoteReplyID(conversationID, quote); got != want {
			t.Fatalf("ACI quote reply ID = %q, want %q", got, want)
		}
		quote.Text = "edited quote text"
		if got := signalQuoteReplyID(conversationID, quote); got != want {
			t.Fatalf("quote text changed stable reply ID: got %q, want %q", got, want)
		}

		fallback := &signalQuotedMessage{Timestamp: timestamp, Author: "+15550000001"}
		fallbackWant := "signal:" + signalIncomingSourceID(conversationID, "+15550000001", timestamp, "")
		if got := signalQuoteReplyID(conversationID, fallback); got != fallbackWant {
			t.Fatalf("author fallback reply ID = %q, want %q", got, fallbackWant)
		}
	})

	t.Run("mentions", func(t *testing.T) {
		const account = "+15551230000"
		cases := []struct {
			name     string
			mentions []signalMention
			account  string
			want     bool
		}{
			{name: "number", mentions: []signalMention{{Number: account}}, account: account, want: true},
			{name: "recipient number", mentions: []signalMention{{RecipientNumber: " " + account + " "}}, account: account, want: true},
			{name: "recipient", mentions: []signalMention{{Recipient: account}}, account: account, want: true},
			{name: "case-insensitive service ID", mentions: []signalMention{{Recipient: "ACCOUNT-ACI"}}, account: "account-aci", want: true},
			{name: "different recipient", mentions: []signalMention{{Number: "+15550000000"}}, account: account, want: false},
			{name: "empty account", mentions: []signalMention{{Number: account}}, account: "", want: false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := signalMentionsMe(tc.mentions, tc.account); got != tc.want {
					t.Fatalf("signalMentionsMe() = %v, want %v", got, tc.want)
				}
			})
		}
	})
}

func TestSignalReactionNormalizationSupportsNestedAndLegacyTargets(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		account       string
		wantAuthor    string
		wantTimestamp int64
	}{
		{
			name:          "nested target",
			raw:           `{"target":{"authorAci":"nested-author-aci","timestamp":1700000000123},"targetAuthor":"legacy-author","targetSentTimestamp":0}`,
			account:       "+15551230000",
			wantAuthor:    "nested-author-aci",
			wantTimestamp: 1700000000123,
		},
		{
			name:          "legacy top-level target",
			raw:           `{"targetAuthorServiceId":"legacy-author-aci","targetSentTimestamp":1700000000456}`,
			account:       "+15551230000",
			wantAuthor:    "legacy-author-aci",
			wantTimestamp: 1700000000456,
		},
		{
			name:          "top-level timestamp wins",
			raw:           `{"target":{"author":"nested-author","timestamp":1700000000123},"targetSentTimestamp":1700000000789}`,
			account:       "+15551230000",
			wantAuthor:    "nested-author",
			wantTimestamp: 1700000000789,
		},
		{
			name:       "account author fallback",
			raw:        `{}`,
			account:    "+15551230000",
			wantAuthor: "+15551230000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reaction signalReaction
			if err := json.Unmarshal([]byte(tc.raw), &reaction); err != nil {
				t.Fatalf("json.Unmarshal(reaction): %v", err)
			}
			if got := signalReactionTargetAuthor(&reaction, tc.account); got != tc.wantAuthor {
				t.Fatalf("signalReactionTargetAuthor() = %q, want %q", got, tc.wantAuthor)
			}
			if got := signalReactionTargetTimestamp(&reaction); got != tc.wantTimestamp {
				t.Fatalf("signalReactionTargetTimestamp() = %d, want %d", got, tc.wantTimestamp)
			}
		})
	}

	if got := signalReactionTargetAuthor(nil, "+15551230000"); got != "" {
		t.Fatalf("nil reaction target author = %q, want empty", got)
	}
	if got := signalReactionTargetTimestamp(nil); got != 0 {
		t.Fatalf("nil reaction target timestamp = %d, want zero", got)
	}
}

func TestSignalUnsupportedContentPlaceholders(t *testing.T) {
	cases := []struct {
		name        string
		attachments []signalAttachment
		flags       signalUnsupportedContentFlags
		want        string
	}{
		{name: "default", want: signalUnsupportedMessagePlaceholder},
		{name: "photo", attachments: []signalAttachment{{ContentType: " image/jpeg "}}, want: "[Photo]"},
		{name: "video", attachments: []signalAttachment{{ContentType: "video/mp4"}}, want: "[Video]"},
		{name: "audio", attachments: []signalAttachment{{ContentType: "audio/ogg"}}, want: "[Audio]"},
		{name: "generic attachment", attachments: []signalAttachment{{ContentType: "application/pdf"}}, want: "[Attachment]"},
		{name: "sticker", flags: signalUnsupportedContentFlags{HasSticker: true}, want: "[Sticker]"},
		{name: "contact", flags: signalUnsupportedContentFlags{HasContacts: true}, want: "[Contact]"},
		{name: "payment", flags: signalUnsupportedContentFlags{HasPayment: true}, want: "[Payment]"},
		{name: "poll", flags: signalUnsupportedContentFlags{HasPollCreate: true}, want: "[Poll]"},
		{name: "poll vote", flags: signalUnsupportedContentFlags{HasPollVote: true}, want: "[Poll vote]"},
		{name: "poll closed", flags: signalUnsupportedContentFlags{HasPollTerminate: true}, want: "[Poll closed]"},
		{name: "remote delete", flags: signalUnsupportedContentFlags{HasRemoteDelete: true}, want: "[Deleted message]"},
		{name: "pin", flags: signalUnsupportedContentFlags{HasPinMessage: true}, want: "[Pinned message]"},
		{name: "unpin", flags: signalUnsupportedContentFlags{HasUnpinMessage: true}, want: "[Unpinned message]"},
		{name: "admin delete", flags: signalUnsupportedContentFlags{HasAdminDelete: true}, want: "[Deleted by admin]"},
		{name: "group update", flags: signalUnsupportedContentFlags{HasGroupUpdate: true}, want: "[Group updated]"},
		{name: "story reply", flags: signalUnsupportedContentFlags{HasStoryContext: true}, want: "[Story reply]"},
		{name: "expiration update", flags: signalUnsupportedContentFlags{IsExpirationUpdate: true}, want: "[Disappearing messages updated]"},
		{name: "view once", flags: signalUnsupportedContentFlags{ViewOnce: true}, want: "[View-once message]"},
		{name: "link preview", flags: signalUnsupportedContentFlags{HasPreview: true}, want: "[Link preview]"},
		{
			name:        "attachment takes precedence",
			attachments: []signalAttachment{{ContentType: "image/png"}},
			flags:       signalUnsupportedContentFlags{HasSticker: true, HasContacts: true, HasPayment: true},
			want:        "[Photo]",
		},
		{
			name: "flag precedence",
			flags: signalUnsupportedContentFlags{
				HasSticker:         true,
				HasContacts:        true,
				HasPayment:         true,
				HasRemoteDelete:    true,
				IsExpirationUpdate: true,
				ViewOnce:           true,
				HasPreview:         true,
			},
			want: "[Sticker]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalUnsupportedContentPlaceholder(tc.attachments, tc.flags); got != tc.want {
				t.Fatalf("signalUnsupportedContentPlaceholder() = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("message body takes precedence", func(t *testing.T) {
		data := &signalDataMessage{
			Message:     "  visible body  ",
			Attachments: []signalAttachment{{ContentType: "image/png"}},
			Sticker:     json.RawMessage(`{"packId":"pack-1"}`),
		}
		if got := data.displayBody(); got != "visible body" {
			t.Fatalf("data displayBody() = %q, want visible body", got)
		}
		sent := &signalSentMessage{
			Message:      "  sent body  ",
			Attachments:  []signalAttachment{{ContentType: "video/mp4"}},
			RemoteDelete: json.RawMessage(`{"targetSentTimestamp":1700000000000}`),
		}
		if got := sent.displayBody(); got != "sent body" {
			t.Fatalf("sent displayBody() = %q, want sent body", got)
		}
	})
}

func TestSignalAttachmentReferenceCodecs(t *testing.T) {
	t.Run("remote round trip", func(t *testing.T) {
		encoded := encodeSignalAttachmentRef("  attachment-123  ")
		if encoded != "signalatt:attachment-123" {
			t.Fatalf("encodeSignalAttachmentRef() = %q", encoded)
		}
		decoded, err := decodeSignalAttachmentRef("  " + encoded + "  ")
		if err != nil {
			t.Fatalf("decodeSignalAttachmentRef(): %v", err)
		}
		if decoded != "attachment-123" {
			t.Fatalf("decoded remote attachment = %q", decoded)
		}
		if got := encodeSignalAttachmentRef("  "); got != "" {
			t.Fatalf("empty remote attachment encoded as %q", got)
		}
	})

	t.Run("local round trip", func(t *testing.T) {
		const path = "/tmp/OpenMessage attachments/photo one.png"
		encoded := encodeSignalLocalAttachmentRef("  " + path + "  ")
		if !strings.HasPrefix(encoded, signalLocalAttachmentPrefix) {
			t.Fatalf("encodeSignalLocalAttachmentRef() = %q", encoded)
		}
		decoded, err := decodeSignalLocalAttachmentRef(encoded)
		if err != nil {
			t.Fatalf("decodeSignalLocalAttachmentRef(): %v", err)
		}
		if decoded != path {
			t.Fatalf("decoded local attachment = %q, want %q", decoded, path)
		}
		if got := encodeSignalLocalAttachmentRef("  "); got != "" {
			t.Fatalf("empty local attachment encoded as %q", got)
		}
	})

	malformed := []struct {
		name  string
		value string
		local bool
	}{
		{name: "remote wrong prefix", value: "other:attachment-123"},
		{name: "remote empty", value: "signalatt:"},
		{name: "local wrong prefix", value: "other:abc", local: true},
		{name: "local empty", value: "signallocal:", local: true},
		{name: "local invalid base64", value: "signallocal:not*base64", local: true},
		{name: "local whitespace path", value: "signallocal:ICA", local: true},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.local {
				_, err = decodeSignalLocalAttachmentRef(tc.value)
			} else {
				_, err = decodeSignalAttachmentRef(tc.value)
			}
			if err == nil {
				t.Fatalf("decoding malformed reference %q unexpectedly succeeded", tc.value)
			}
		})
	}
}

func TestSanitizeSignalOutput(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{name: "plain whitespace and carriage return", line: "  signal-cli ready\r  ", want: "signal-cli ready"},
		{name: "ANSI color", line: "\x1b[31mERROR\x1b[0m", want: "ERROR"},
		{name: "multiple ANSI sequences", line: "\x1b[1mSignal\x1b[0m \x1b[32mready\x1b[0m", want: "Signal ready"},
		{name: "unterminated ANSI sequence", line: "message\x1b[31", want: "message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSignalOutput(tc.line); got != tc.want {
				t.Fatalf("sanitizeSignalOutput(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestExtractSignalLinkURI(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{name: "missing", line: "Waiting for QR code", want: ""},
		{name: "bare URI", line: "sgnl://linkdevice?uuid=test", want: "sgnl://linkdevice?uuid=test"},
		{
			name: "prefixed recorded output",
			line: "Link device URI: sgnl://linkdevice?uuid=test&pub_key=abc  ",
			want: "sgnl://linkdevice?uuid=test&pub_key=abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSignalLinkURI(tc.line); got != tc.want {
				t.Fatalf("extractSignalLinkURI(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestCleanSignalCommandOutput(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		output string
		want   string
	}{
		{
			name: "sanitizes filters and deduplicates",
			err:  errors.New("exit status 1"),
			output: "\x1b[31mUser +15551230000 is not registered.\x1b[0m\n" +
				"████ QR block\n" +
				"User +15551230000 is not registered.\r\n",
			want: "exit status 1: User +15551230000 is not registered.",
		},
		{
			name:   "deduplicates error echoed on stdout",
			err:    errors.New("authorization failed"),
			output: "authorization failed\n",
			want:   "authorization failed",
		},
		{name: "empty", output: "\n████ QR block\n", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanSignalCommandOutput(tc.err, []byte(tc.output)); got != tc.want {
				t.Fatalf("cleanSignalCommandOutput() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsSignalAccountInvalid(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{name: "not registered output", output: "User +15551230000 is not registered.", want: true},
		{name: "authorization failed output", output: "AUTHORIZATION FAILED", want: true},
		{name: "invalid account error", err: errors.New("invalid account +15551230000"), want: true},
		{name: "transient receive failure", err: errors.New("exit status 1"), output: "Connection closed unexpectedly", want: false},
		{name: "poison envelope", err: errors.New("exit status 1"), output: "IncomingMessageHandler.getSender() content is null", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSignalAccountInvalid(tc.err, []byte(tc.output)); got != tc.want {
				t.Fatalf("isSignalAccountInvalid() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSignalEnvelopeRecoveryReason(t *testing.T) {
	group := &signalGroupInfo{GroupID: "group-1"}
	cases := []struct {
		name string
		env  *signalEnvelope
		want string
	}{
		{name: "nil envelope", want: ""},
		{name: "empty envelope", env: &signalEnvelope{}, want: ""},
		{
			name: "missing edit source",
			env:  &signalEnvelope{EditMessage: &signalEditMessage{DataMessage: &signalDataMessage{}}},
			want: "missing_edit_message_source",
		},
		{
			name: "edit source present",
			env: &signalEnvelope{
				SourceServiceID: "sender-aci",
				EditMessage:     &signalEditMessage{DataMessage: &signalDataMessage{}},
			},
			want: "",
		},
		{
			name: "source-less group edit allowed",
			env: &signalEnvelope{EditMessage: &signalEditMessage{DataMessage: &signalDataMessage{
				GroupInfo: group,
			}}},
			want: "",
		},
		{name: "missing data source", env: &signalEnvelope{DataMessage: &signalDataMessage{}}, want: "missing_data_message_source"},
		{
			name: "data source present",
			env:  &signalEnvelope{SourceNumber: "+15551234567", DataMessage: &signalDataMessage{}},
			want: "",
		},
		{
			name: "source-less group data allowed",
			env:  &signalEnvelope{DataMessage: &signalDataMessage{GroupInfo: group}},
			want: "",
		},
		{
			name: "missing sent target",
			env:  &signalEnvelope{SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{}}},
			want: "missing_sent_message_target",
		},
		{
			name: "sent target present",
			env: &signalEnvelope{SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{
				DestinationServiceID: "recipient-aci",
			}}},
			want: "",
		},
		{
			name: "target-less group sent message allowed",
			env: &signalEnvelope{SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{
				GroupInfo: group,
			}}},
			want: "",
		},
		{
			name: "target-less group sent edit allowed",
			env: &signalEnvelope{SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{
				EditMessage: &signalEditMessage{DataMessage: &signalDataMessage{GroupInfo: group}},
			}}},
			want: "",
		},
		{
			name: "first missing reason wins",
			env: &signalEnvelope{
				EditMessage: &signalEditMessage{DataMessage: &signalDataMessage{}},
				DataMessage: &signalDataMessage{},
				SyncMessage: &signalSyncMessage{SentMessage: &signalSentMessage{}},
			},
			want: "missing_edit_message_source",
		},
		{name: "typing does not require source", env: &signalEnvelope{TypingMessage: &signalTypingMessage{}}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalEnvelopeRecoveryReason(tc.env); got != tc.want {
				t.Fatalf("signalEnvelopeRecoveryReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProcessReceiveLineAcceptsResultWrappedEnvelope(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}
	payload := []byte(`{"result":{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"wrapped receive result","mentions":[{"recipient":"+15551230000"}]}}}}`)

	resolved, err := bridge.processReceiveLine("+15550000000", payload, true)
	if err != nil {
		t.Fatalf("processReceiveLine(): %v", err)
	}
	if !resolved {
		t.Fatal("result-wrapped payload was not resolved")
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Body != "wrapped receive result" || msgs[0].SenderNumber != "+15551234567" {
		t.Fatalf("unexpected wrapped message: %+v", msgs[0])
	}
	if !msgs[0].MentionsMe {
		t.Fatalf("result account was not used for mention normalization: %+v", msgs[0])
	}
	if status := bridge.receiveRecoveryStatus(); status != nil {
		t.Fatalf("valid result-wrapped payload was quarantined: %+v", status)
	}
}
