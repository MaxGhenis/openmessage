package whatsapplive

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/whatsmeow"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestMarkReadRequestMapsTransportArguments(t *testing.T) {
	readAt := time.Date(2026, time.July, 15, 14, 3, 2, 123, time.UTC)
	tests := []struct {
		name           string
		conversationID string
		messageIDs     []string
		senderID       string
		wantChat       string
		wantSender     string
	}{
		{
			name:           "group messages from participant",
			conversationID: "whatsapp:120363019999999999@g.us",
			messageIDs:     []string{"message-one", "message-two"},
			senderID:       "+15557654321",
			wantChat:       "120363019999999999@g.us",
			wantSender:     "15557654321@s.whatsapp.net",
		},
		{
			name:           "empty sender remains self",
			conversationID: "whatsapp:15551234567@s.whatsapp.net",
			messageIDs:     []string{"message-three"},
			wantChat:       "15551234567@s.whatsapp.net",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := connectedMediaTestBridge()
			originalMarkRead := markReadMessages
			originalIsConnected := clientIsConnected
			originalAddressing := groupAddressingMode
			defer func() {
				markReadMessages = originalMarkRead
				clientIsConnected = originalIsConnected
				groupAddressingMode = originalAddressing
			}()
			clientIsConnected = func(*whatsmeow.Client) bool { return true }
			groupAddressingMode = func(*whatsmeow.Client, context.Context, watypes.JID) (watypes.AddressingMode, error) {
				return watypes.AddressingModePN, nil
			}

			var (
				gotClient    *whatsmeow.Client
				gotContext   context.Context
				gotIDs       []watypes.MessageID
				gotReadAt    time.Time
				gotChat      watypes.JID
				gotSender    watypes.JID
				gotExtra     []watypes.ReceiptType
				transportHit int
			)
			markReadMessages = func(
				cli *whatsmeow.Client,
				ctx context.Context,
				ids []watypes.MessageID,
				timestamp time.Time,
				chat, sender watypes.JID,
				extra ...watypes.ReceiptType,
			) error {
				transportHit++
				gotClient = cli
				gotContext = ctx
				gotIDs = append([]watypes.MessageID(nil), ids...)
				gotReadAt = timestamp
				gotChat = chat
				gotSender = sender
				gotExtra = append([]watypes.ReceiptType(nil), extra...)
				return nil
			}

			err := host.MarkReadRequest(
				test.conversationID,
				test.messageIDs,
				test.senderID,
				readAt,
			)
			if err != nil {
				t.Fatalf("MarkReadRequest() error = %v", err)
			}
			if transportHit != 1 {
				t.Fatalf("MarkRead transport calls = %d, want one", transportHit)
			}
			if gotClient != host.client {
				t.Fatalf("MarkRead client = %p, want host client %p", gotClient, host.client)
			}
			if gotContext == nil {
				t.Fatal("MarkRead context is nil")
			}
			if len(gotIDs) != len(test.messageIDs) {
				t.Fatalf("MarkRead IDs = %q, want %q", gotIDs, test.messageIDs)
			}
			for i, want := range test.messageIDs {
				if gotIDs[i] != watypes.MessageID(want) {
					t.Fatalf("MarkRead ID[%d] = %q, want %q", i, gotIDs[i], want)
				}
			}
			if !gotReadAt.Equal(readAt) {
				t.Fatalf("MarkRead timestamp = %s, want %s", gotReadAt, readAt)
			}
			if gotChat.String() != test.wantChat {
				t.Fatalf("MarkRead chat = %q, want %q", gotChat.String(), test.wantChat)
			}
			if test.wantSender == "" {
				if !gotSender.IsEmpty() {
					t.Fatalf("MarkRead sender = %q, want EmptyJID for self", gotSender.String())
				}
			} else if gotSender.String() != test.wantSender {
				t.Fatalf("MarkRead sender = %q, want %q", gotSender.String(), test.wantSender)
			}
			if len(gotExtra) != 0 {
				t.Fatalf("MarkRead receipt extras = %v, want none so whatsmeow defaults to read", gotExtra)
			}
		})
	}
}

func TestMarkReadRequestRejectsEmptyIDsBeforeTransport(t *testing.T) {
	originalMarkRead := markReadMessages
	defer func() { markReadMessages = originalMarkRead }()

	var transportCalls int
	markReadMessages = func(
		*whatsmeow.Client,
		context.Context,
		[]watypes.MessageID,
		time.Time,
		watypes.JID,
		watypes.JID,
		...watypes.ReceiptType,
	) error {
		transportCalls++
		return nil
	}

	err := connectedMediaTestBridge().MarkReadRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		nil,
		"",
		time.Now(),
	)
	if err == nil || !errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("MarkReadRequest(empty IDs) error = %v, want not-dispatched marker", err)
	}
	if transportCalls != 0 {
		t.Fatalf("MarkRead transport calls = %d, want zero", transportCalls)
	}
}

func TestMarkReadRequestMarksOtherPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		err := connectedMediaTestBridge().MarkReadRequest(
			"invalid",
			[]string{"message-one"},
			"",
			time.Now(),
		)
		if err == nil || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("MarkReadRequest(invalid conversation) error = %v, want not-dispatched marker", err)
		}
	})

	t.Run("transport disconnected before call", func(t *testing.T) {
		err := (&Bridge{}).MarkReadRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			[]string{"message-one"},
			"",
			time.Now(),
		)
		if !errors.Is(err, whatsmeow.ErrNotConnected) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("MarkReadRequest(disconnected) error = %v, want not-connected cause and not-dispatched marker", err)
		}
	})
}

func TestMarkReadRequestTransportErrorIsWrappedAndReported(t *testing.T) {
	host := connectedMediaTestBridge()
	originalMarkRead := markReadMessages
	originalIsConnected := clientIsConnected
	defer func() {
		markReadMessages = originalMarkRead
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

	want := whatsmeow.ErrNotConnected
	var transportCalls int
	markReadMessages = func(
		*whatsmeow.Client,
		context.Context,
		[]watypes.MessageID,
		time.Time,
		watypes.JID,
		watypes.JID,
		...watypes.ReceiptType,
	) error {
		transportCalls++
		return want
	}
	var reported error
	host.callbacks.OnConnectionError = func(err error) { reported = err }

	err := host.MarkReadRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		[]string{"message-one"},
		"",
		time.Now(),
	)
	if !errors.Is(err, want) {
		t.Fatalf("MarkReadRequest() error = %v, want wrapped transport cause", err)
	}
	if err == want {
		t.Fatalf("MarkReadRequest() returned raw transport error %v, want contextual wrapper", err)
	}
	if errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("MarkReadRequest() error = %v, transport-call failure must not carry predispatch marker", err)
	}
	if transportCalls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want one", transportCalls)
	}
	if !errors.Is(reported, want) {
		t.Fatalf("retained lifecycle report = %v, want transport cause %v", reported, want)
	}
}

// readReceiptTestCall is one receipt whatsmeow's MarkRead was asked to send.
type readReceiptTestCall struct {
	ctx    context.Context
	ids    []watypes.MessageID
	chat   watypes.JID
	sender watypes.JID
}

// readReceiptTestTransport stands in for a connected WhatsApp transport in
// read-receipt tests: it records each receipt sent and answers the group
// addressing lookup with mode, or with lookupErr.
type readReceiptTestTransport struct {
	mode       watypes.AddressingMode
	lookupErr  error
	lookups    []watypes.JID
	lookupCtxs []context.Context
	receipts   []readReceiptTestCall
}

func newReadReceiptTestTransport(t *testing.T) *readReceiptTestTransport {
	t.Helper()
	transport := &readReceiptTestTransport{mode: watypes.AddressingModePN}
	originalMarkRead := markReadMessages
	originalIsConnected := clientIsConnected
	originalAddressing := groupAddressingMode
	t.Cleanup(func() {
		markReadMessages = originalMarkRead
		clientIsConnected = originalIsConnected
		groupAddressingMode = originalAddressing
	})
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	groupAddressingMode = func(_ *whatsmeow.Client, ctx context.Context, group watypes.JID) (watypes.AddressingMode, error) {
		transport.lookups = append(transport.lookups, group)
		transport.lookupCtxs = append(transport.lookupCtxs, ctx)
		return transport.mode, transport.lookupErr
	}
	markReadMessages = func(_ *whatsmeow.Client, ctx context.Context, ids []watypes.MessageID, _ time.Time, chat, sender watypes.JID, _ ...watypes.ReceiptType) error {
		transport.receipts = append(transport.receipts, readReceiptTestCall{
			ctx:    ctx,
			ids:    append([]watypes.MessageID(nil), ids...),
			chat:   chat,
			sender: sender,
		})
		return nil
	}
	return transport
}

// whatsmeow's MarkRead puts the sender in a group receipt's participant
// attribute as it is given (receipt.go), and its own delivery receipt for a
// group message copies the participant the message arrived with
// (buildBaseReceipt): the member's LID when the group addressed it by LID, its
// phone number otherwise. A read receipt names the sender the same way a
// reaction does. Before, a stored LID went out as the participant
// "<digits>@lid@s.whatsapp.net", which names no member, and a stored phone
// number went out as the phone number in a group that addresses members by
// LID.
func TestMarkReadRequestNamesAGroupSenderInTheFormItsMessagesArrivedWith(t *testing.T) {
	const (
		lidJID   = senderTestLID + "@lid"
		phoneJID = senderTestPhone + "@s.whatsapp.net"
	)
	lookupFailed := errors.New("group info query timed out")
	tests := []struct {
		name      string
		lids      senderTestLIDs
		mode      watypes.AddressingMode
		lookupErr error
		sender    string
		want      string
	}{
		{
			name:   "LID-only sender in a group that addresses by LID",
			lids:   senderTestLIDs{},
			mode:   watypes.AddressingModeLID,
			sender: lidJID,
			want:   lidJID,
		},
		// The messages arrived from the LID; a phone number learned since
		// does not change who sent them.
		{
			name:   "LID sender whose number became known later",
			lids:   senderTestLIDs{senderTestLID: senderTestPhone},
			mode:   watypes.AddressingModeLID,
			sender: lidJID,
			want:   lidJID,
		},
		{
			name:   "LID sender in a group that now reads phone-number addressing",
			lids:   senderTestLIDs{senderTestLID: senderTestPhone},
			mode:   watypes.AddressingModePN,
			sender: lidJID,
			want:   lidJID,
		},
		{
			name:   "phone-number sender in a group that addresses by LID",
			lids:   senderTestLIDs{senderTestLID: senderTestPhone},
			mode:   watypes.AddressingModeLID,
			sender: "+" + senderTestPhone,
			want:   lidJID,
		},
		{
			name:   "phone-number sender whose LID is unknown",
			lids:   senderTestLIDs{},
			mode:   watypes.AddressingModeLID,
			sender: "+" + senderTestPhone,
			want:   phoneJID,
		},
		{
			name:   "phone-number sender in a group that addresses by phone number",
			lids:   senderTestLIDs{senderTestLID: senderTestPhone},
			mode:   watypes.AddressingModePN,
			sender: "+" + senderTestPhone,
			want:   phoneJID,
		},
		// whatsmeow's SendMessage treats any addressing but "lid" as phone
		// numbers.
		{
			name:   "group that reports no addressing",
			lids:   senderTestLIDs{senderTestLID: senderTestPhone},
			mode:   "",
			sender: "+" + senderTestPhone,
			want:   phoneJID,
		},
		// MarkRead does not read the group cache, so the receipt goes out
		// naming the stored sender rather than failing.
		{
			name:      "group addressing lookup fails",
			lids:      senderTestLIDs{senderTestLID: senderTestPhone},
			mode:      watypes.AddressingModeLID,
			lookupErr: lookupFailed,
			sender:    "+" + senderTestPhone,
			want:      phoneJID,
		},
		{
			name:   "phone number stored without a plus",
			lids:   senderTestLIDs{},
			mode:   watypes.AddressingModePN,
			sender: senderTestPhone,
			want:   phoneJID,
		},
		{
			name:   "hosted sender",
			lids:   senderTestLIDs{},
			mode:   watypes.AddressingModeLID,
			sender: senderTestPhone + "@hosted",
			want:   senderTestPhone + "@hosted",
		},
		{
			name:   "bot sender",
			lids:   senderTestLIDs{},
			mode:   watypes.AddressingModeLID,
			sender: "867051314767696@bot",
			want:   "867051314767696@bot",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newReadReceiptTestTransport(t)
			transport.mode = test.mode
			transport.lookupErr = test.lookupErr

			err := senderTestBridge(test.lids).MarkReadRequest(
				senderTestGroup,
				[]string{"message-one", "message-two"},
				test.sender,
				time.Now(),
			)
			if err != nil {
				t.Fatalf("MarkReadRequest() error = %v", err)
			}
			if len(transport.receipts) != 1 {
				t.Fatalf("receipts sent = %d, want one", len(transport.receipts))
			}
			receipt := transport.receipts[0]
			if receipt.sender.String() != test.want {
				t.Fatalf("receipt participant = %q, want %q", receipt.sender.String(), test.want)
			}
			if receipt.chat.String() != strings.TrimPrefix(senderTestGroup, "whatsapp:") {
				t.Fatalf("receipt chat = %q, want the group", receipt.chat.String())
			}
			if len(receipt.ids) != 2 || receipt.ids[0] != "message-one" || receipt.ids[1] != "message-two" {
				t.Fatalf("receipt IDs = %q, want both messages under the one sender", receipt.ids)
			}
			// Only a phone number has another form to look up.
			wantLookups := 0
			if !strings.Contains(test.sender, "@") {
				wantLookups = 1
			}
			if len(transport.lookups) != wantLookups {
				t.Fatalf("group addressing lookups = %v, want %d", transport.lookups, wantLookups)
			}
		})
	}
}

// whatsmeow's MarkRead sends no participant in a 1:1 chat with a phone number
// or a LID, so the stored sender only has to parse there. The group's
// addressing is not looked up, and a sender that names no one is left out as
// a missing one is, rather than refused.
func TestMarkReadRequestNamesADirectChatSenderOfEitherForm(t *testing.T) {
	for _, test := range []struct {
		chat   string
		sender string
		want   string
	}{
		{chat: "whatsapp:" + senderTestPhone + "@s.whatsapp.net", sender: senderTestLID + "@lid", want: senderTestLID + "@lid"},
		{chat: "whatsapp:" + senderTestLID + "@lid", sender: senderTestLID + "@lid", want: senderTestLID + "@lid"},
		{chat: "whatsapp:" + senderTestPhone + "@s.whatsapp.net", sender: "+" + senderTestPhone, want: senderTestPhone + "@s.whatsapp.net"},
		{chat: "whatsapp:" + senderTestPhone + "@s.whatsapp.net", sender: "", want: ""},
		{chat: "whatsapp:" + senderTestPhone + "@s.whatsapp.net", sender: senderTestLID + "@lid@s.whatsapp.net", want: ""},
		{chat: "whatsapp:" + senderTestPhone + "@s.whatsapp.net", sender: "Me", want: ""},
	} {
		t.Run(test.chat+" "+test.sender, func(t *testing.T) {
			transport := newReadReceiptTestTransport(t)
			transport.mode = watypes.AddressingModeLID
			lids := senderTestLIDs{senderTestLID: senderTestPhone}
			if strings.HasSuffix(test.chat, "@lid") {
				// A LID chat whose number is known is first merged into
				// the phone-number chat, and this bridge has no store.
				lids = senderTestLIDs{}
			}
			if err := senderTestBridge(lids).MarkReadRequest(test.chat, []string{"message-one"}, test.sender, time.Now()); err != nil {
				t.Fatalf("MarkReadRequest() error = %v", err)
			}
			if len(transport.receipts) != 1 {
				t.Fatalf("receipts sent = %d, want one", len(transport.receipts))
			}
			if got := transport.receipts[0].sender; got.String() != test.want {
				t.Fatalf("receipt sender = %q, want %q", got.String(), test.want)
			}
			if len(transport.lookups) != 0 {
				t.Fatalf("group addressing lookups = %v, want none for a 1:1 chat", transport.lookups)
			}
		})
	}
}

// In a chat that is neither a group nor a 1:1 chat, such as a bot's, MarkRead
// does send a participant. A sender's JID goes out as stored (before, a bot
// went out as "<id>@bot@s.whatsapp.net"), and a sender that names no one is
// left out as a missing one is.
func TestMarkReadRequestNamesABotChatSenderAsStored(t *testing.T) {
	const bot = "867051314767696@bot"
	for _, test := range []struct {
		sender string
		want   string
	}{
		{sender: bot, want: bot},
		{sender: "", want: ""},
		{sender: bot + "@s.whatsapp.net", want: ""},
	} {
		t.Run(test.sender, func(t *testing.T) {
			transport := newReadReceiptTestTransport(t)
			if err := senderTestBridge(senderTestLIDs{}).MarkReadRequest("whatsapp:"+bot, []string{"message-one"}, test.sender, time.Now()); err != nil {
				t.Fatalf("MarkReadRequest() error = %v", err)
			}
			if len(transport.receipts) != 1 || transport.receipts[0].sender.String() != test.want {
				t.Fatalf("receipts = %+v, want one naming %q", transport.receipts, test.want)
			}
			if len(transport.lookups) != 0 {
				t.Fatalf("group addressing lookups = %v, want none outside a group", transport.lookups)
			}
		})
	}
}

// A group receipt must name the sender of the messages it covers (whatsmeow's
// MarkRead requires one in a group), so a group message stored with no sender,
// or with one that is neither a user's JID nor a phone number, cannot be
// marked read. The receipt is refused before anything is sent, even while
// disconnected; before, it went out naming no participant, or one no message
// has. That includes the group's own JID, which the history decoder stores as
// the sender of a group message that names no participant. In a 1:1 chat the
// same receipt goes out with no sender, which MarkRead does not send anyway.
func TestMarkReadRequestRefusesAGroupSenderThatIsMissingOrNamesNoMember(t *testing.T) {
	for _, sender := range []string{
		"",
		"  ",
		"@lid",
		senderTestLID + "@",
		senderTestLID + "@lid@s.whatsapp.net",
		strings.TrimPrefix(senderTestGroup, "whatsapp:"),
		"status@broadcast",
		"120363000000000000@newsletter",
		senderTestPhone + "@c.us",
		"Me",
		"+",
		"+1-555-765-4321",
	} {
		t.Run(sender, func(t *testing.T) {
			transport := newReadReceiptTestTransport(t)
			err := senderTestBridge(senderTestLIDs{}).MarkReadRequest(senderTestGroup, []string{"message-one"}, sender, time.Now())
			if !errors.Is(err, ErrReadReceiptSenderUnknown) || !errors.Is(err, ErrSendNotDispatched) {
				t.Fatalf("MarkReadRequest() error = %v, want the unnamed-sender refusal marked not dispatched", err)
			}
			if len(transport.receipts) != 0 || len(transport.lookups) != 0 {
				t.Fatalf("receipts = %d, lookups = %d, want none", len(transport.receipts), len(transport.lookups))
			}

			err = (&Bridge{}).MarkReadRequest(senderTestGroup, []string{"message-one"}, sender, time.Now())
			if !errors.Is(err, ErrReadReceiptSenderUnknown) || errors.Is(err, whatsmeow.ErrNotConnected) {
				t.Fatalf("disconnected MarkReadRequest() error = %v, want the refusal, not a retryable disconnect", err)
			}

			err = senderTestBridge(senderTestLIDs{}).MarkReadRequest("whatsapp:"+senderTestPhone+"@s.whatsapp.net", []string{"message-one"}, sender, time.Now())
			if err != nil {
				t.Fatalf("1:1 MarkReadRequest() error = %v", err)
			}
			if len(transport.receipts) != 1 || !transport.receipts[0].sender.IsEmpty() {
				t.Fatalf("1:1 receipts = %+v, want one with no sender", transport.receipts)
			}
		})
	}
}

// TestMarkReadRequestGroupParticipantProperty checks the participant rule over
// random senders in the two forms ingest stores ("+<digits>" and
// "<digits>@lid"), LID maps, group addressing and message counts. A group
// receipt always names a well-formed member JID (one "@", the phone-number or
// LID server, no device) and covers every message ID in request order. A
// stored LID is sent as stored, with no lookup. A stored phone number is sent
// as its LID exactly when the group addresses members by LID and the LID is
// known, and as the phone number otherwise, after at most one lookup.
func TestMarkReadRequestGroupParticipantProperty(t *testing.T) {
	type input struct {
		Phone     uint64
		LID       uint64
		StoredLID bool
		NoPlus    bool
		Known     bool
		Mode      uint8
		Messages  uint8
	}
	transport := newReadReceiptTestTransport(t)
	property := func(in input) bool {
		phone := strconv.FormatUint(in.Phone%1_000_000_000_000_000+1, 10)
		lid := strconv.FormatUint(in.LID%1_000_000_000_000_000+1, 10)
		lids := senderTestLIDs{}
		if in.Known {
			lids[lid] = phone
		}
		transport.receipts = nil
		transport.lookups = nil
		transport.lookupErr = nil
		switch in.Mode % 4 {
		case 0:
			transport.mode = watypes.AddressingModePN
		case 1:
			transport.mode = watypes.AddressingModeLID
		case 2:
			transport.mode = ""
		default:
			transport.mode = watypes.AddressingModeLID
			transport.lookupErr = errors.New("lookup failed")
		}
		ids := make([]string, int(in.Messages%5)+1)
		for i := range ids {
			ids[i] = "message-" + strconv.Itoa(i)
		}

		sender, want, maxLookups := "+"+phone, phone+"@s.whatsapp.net", 1
		if in.NoPlus {
			sender = phone
		}
		if in.StoredLID {
			sender, want, maxLookups = lid+"@lid", lid+"@lid", 0
		} else if transport.mode == watypes.AddressingModeLID && transport.lookupErr == nil && in.Known {
			want = lid + "@lid"
		}

		err := senderTestBridge(lids).MarkReadRequest(senderTestGroup, ids, sender, time.Now())
		if err != nil || len(transport.receipts) != 1 {
			t.Logf("sender %q: error %v, %d receipts", sender, err, len(transport.receipts))
			return false
		}
		receipt := transport.receipts[0]
		participant := receipt.sender.String()
		parsed, parseErr := watypes.ParseJID(participant)
		wellFormed := parseErr == nil && strings.Count(participant, "@") == 1 && parsed.Device == 0 &&
			(parsed.Server == watypes.DefaultUserServer || parsed.Server == watypes.HiddenUserServer)
		coversAll := len(receipt.ids) == len(ids)
		for i := 0; coversAll && i < len(ids); i++ {
			coversAll = string(receipt.ids[i]) == ids[i]
		}
		if !wellFormed || participant != want || !coversAll || len(transport.lookups) > maxLookups {
			t.Logf("sender %q, mode %q, lookup error %v, known %t: participant %q (want %q), ids %q, lookups %d",
				sender, transport.mode, transport.lookupErr, in.Known, participant, want, receipt.ids, len(transport.lookups))
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

// Like canonicalJID, the phone number's LID falls back to the session store's
// LID map when whatsmeow's LID store has none.
func TestMarkReadRequestFindsAGroupSendersLIDInTheSessionStore(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "whatsapp-session.db")
	sessionDB, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(session store): %v", err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT NOT NULL)`); err != nil {
		t.Fatalf("create lid map: %v", err)
	}
	if _, err := sessionDB.Exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)`, senderTestLID, senderTestPhone); err != nil {
		t.Fatalf("seed lid map: %v", err)
	}

	transport := newReadReceiptTestTransport(t)
	transport.mode = watypes.AddressingModeLID
	bridge := senderTestBridge(senderTestLIDs{})
	bridge.sessionPath = sessionPath
	if err := bridge.MarkReadRequest(senderTestGroup, []string{"message-one"}, "+"+senderTestPhone, time.Now()); err != nil {
		t.Fatalf("MarkReadRequest() error = %v", err)
	}
	if len(transport.receipts) != 1 {
		t.Fatalf("receipts sent = %d, want one", len(transport.receipts))
	}
	if got := transport.receipts[0].sender.String(); got != senderTestLID+"@lid" {
		t.Fatalf("receipt participant = %q, want the session store's LID %q", got, senderTestLID+"@lid")
	}
}

// The addressing lookup can fetch the group from WhatsApp, so it runs under
// the receipt's own deadline rather than an unbounded context.
func TestMarkReadRequestLooksUpGroupAddressingUnderTheReceiptsDeadline(t *testing.T) {
	transport := newReadReceiptTestTransport(t)
	if err := senderTestBridge(senderTestLIDs{}).MarkReadRequest(senderTestGroup, []string{"message-one"}, "+"+senderTestPhone, time.Now()); err != nil {
		t.Fatalf("MarkReadRequest() error = %v", err)
	}
	if len(transport.lookupCtxs) != 1 || len(transport.receipts) != 1 {
		t.Fatalf("lookups = %d, receipts = %d, want one of each", len(transport.lookupCtxs), len(transport.receipts))
	}
	if _, ok := transport.lookupCtxs[0].Deadline(); !ok {
		t.Fatal("group addressing lookup context has no deadline")
	}
	if transport.lookupCtxs[0] != transport.receipts[0].ctx {
		t.Fatal("group addressing lookup ran under a different context than the receipt")
	}
}
