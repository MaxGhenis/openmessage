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

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestSendReactionRequestMapsActionsAndTargetSender(t *testing.T) {
	const (
		group  = "whatsapp:120363019999999999@g.us"
		direct = "whatsapp:15557654321@s.whatsapp.net"
	)
	tests := []struct {
		name            string
		chat            string
		action          string
		targetAuthorID  string
		targetOutgoing  bool
		wantText        string
		wantFromMe      bool
		wantParticipant string
	}{
		{
			name:            "add from another group participant",
			chat:            group,
			action:          "add",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "switch overwrites with another emoji",
			chat:            group,
			action:          "switch",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "remove sends empty reaction text",
			chat:            group,
			action:          "remove",
			targetAuthorID:  "+15557654321",
			wantText:        "",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:           "own message in a group",
			chat:           group,
			action:         "add",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		{
			name:           "own message in a 1:1 chat",
			chat:           direct,
			action:         "add",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		// The direction decides, not the stored author: an own message
		// whose author names another number is still this account's.
		{
			name:           "own message whatever its stored author",
			chat:           group,
			action:         "add",
			targetAuthorID: "+15557654321",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		// An incoming 1:1 message stored with no sender is the other
		// party's, as the legacy sender had it. Addressed with an empty
		// sender it would name a message of this account's.
		{
			name:     "incoming 1:1 message with no stored sender",
			chat:     direct,
			action:   "add",
			wantText: "🔥",
		},
		{
			name:           "incoming 1:1 message with a stored sender",
			chat:           direct,
			action:         "add",
			targetAuthorID: "+15557654321",
			wantText:       "🔥",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := connectedMediaTestBridge()
			originalSend := sendTextMessage
			originalIsConnected := clientIsConnected
			originalAddressing := groupAddressingMode
			defer func() {
				sendTextMessage = originalSend
				clientIsConnected = originalIsConnected
				groupAddressingMode = originalAddressing
			}()
			clientIsConnected = func(*whatsmeow.Client) bool { return true }
			groupAddressingMode = func(*whatsmeow.Client, context.Context, watypes.JID) (watypes.AddressingMode, error) {
				return watypes.AddressingModePN, nil
			}

			var gotTo watypes.JID
			var gotMessage *waE2E.Message
			var gotRequestID watypes.MessageID
			sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				gotTo = to
				gotMessage = message
				if len(extra) != 1 {
					t.Fatalf("SendMessage extras = %d, want exactly one", len(extra))
				}
				gotRequestID = extra[0].ID
				return whatsmeow.SendResponse{}, nil
			}

			sentAt, err := bridge.SendReactionRequest(
				test.chat,
				"target-remote-id",
				test.targetAuthorID,
				test.targetOutgoing,
				"🔥",
				test.action,
				"outbox-request-123",
			)
			if err != nil {
				t.Fatalf("SendReactionRequest() error = %v", err)
			}
			wantTo := strings.TrimPrefix(test.chat, "whatsapp:")
			if gotTo.String() != wantTo {
				t.Fatalf("recipient = %q, want the chat %q", gotTo.String(), wantTo)
			}
			if gotRequestID != deriveWebMessageID("outbox-request-123") {
				t.Fatalf("request ID = %q, want derived ID %q", gotRequestID, deriveWebMessageID("outbox-request-123"))
			}
			reaction := extractReactionMessage(gotMessage)
			if reaction == nil {
				t.Fatal("expected outgoing reaction message")
			}
			// The returned time is the reaction's own sender timestamp, the
			// one the ingest path records for it.
			if ms := reaction.GetSenderTimestampMS(); ms <= 0 || sentAt.UnixMilli() != ms {
				t.Fatalf("returned time %d, want the reaction's sender timestamp %d", sentAt.UnixMilli(), ms)
			}
			if reaction.GetKey().GetID() != "target-remote-id" || reaction.GetKey().GetRemoteJID() != wantTo {
				t.Fatalf("target key = %+v, want target-remote-id in %q", reaction.GetKey(), wantTo)
			}
			if reaction.GetText() != test.wantText {
				t.Fatalf("reaction text = %q, want %q", reaction.GetText(), test.wantText)
			}
			if reaction.GetKey().GetFromMe() != test.wantFromMe {
				t.Fatalf("target FromMe = %t, want %t", reaction.GetKey().GetFromMe(), test.wantFromMe)
			}
			if reaction.GetKey().GetParticipant() != test.wantParticipant {
				t.Fatalf("target participant = %q, want %q", reaction.GetKey().GetParticipant(), test.wantParticipant)
			}
		})
	}
}

const (
	reactionTestGroup = "whatsapp:120363019999999999@g.us"
	reactionTestPhone = "15557654321"
	reactionTestLID   = "98765432109876"
)

// reactionTestLIDs is the account's LID map in reaction tests: the phone
// number each known LID stands for, by user part.
type reactionTestLIDs map[string]string

func (m reactionTestLIDs) PutManyLIDMappings(context.Context, []wastore.LIDMapping) error {
	return nil
}

func (m reactionTestLIDs) PutLIDMapping(context.Context, watypes.JID, watypes.JID) error {
	return nil
}

func (m reactionTestLIDs) GetPNForLID(_ context.Context, lid watypes.JID) (watypes.JID, error) {
	if pn, ok := m[lid.User]; ok {
		return watypes.NewJID(pn, watypes.DefaultUserServer), nil
	}
	return watypes.EmptyJID, nil
}

func (m reactionTestLIDs) GetLIDForPN(_ context.Context, pn watypes.JID) (watypes.JID, error) {
	for lid, number := range m {
		if number == pn.User {
			return watypes.NewJID(lid, watypes.HiddenUserServer), nil
		}
	}
	return watypes.EmptyJID, nil
}

func (m reactionTestLIDs) GetManyLIDsForPNs(ctx context.Context, pns []watypes.JID) (map[watypes.JID]watypes.JID, error) {
	result := make(map[watypes.JID]watypes.JID)
	for _, pn := range pns {
		if lid, _ := m.GetLIDForPN(ctx, pn); !lid.IsEmpty() {
			result[pn] = lid
		}
	}
	return result, nil
}

// reactionTestBridge is a connected test bridge whose account knows the LIDs
// in lids.
func reactionTestBridge(lids reactionTestLIDs) *Bridge {
	bridge := connectedMediaTestBridge()
	bridge.client.Store.LIDs = lids
	return bridge
}

// reactionTestTransport stands in for a connected WhatsApp transport: it
// records each reaction sent and answers the group addressing lookup with mode,
// or with lookupErr.
type reactionTestTransport struct {
	mode      watypes.AddressingMode
	lookupErr error
	lookups   []watypes.JID
	sent      []*waE2E.ReactionMessage
}

func newReactionTestTransport(t *testing.T) *reactionTestTransport {
	t.Helper()
	transport := &reactionTestTransport{mode: watypes.AddressingModePN}
	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	originalAddressing := groupAddressingMode
	t.Cleanup(func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
		groupAddressingMode = originalAddressing
	})
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	groupAddressingMode = func(_ *whatsmeow.Client, _ context.Context, group watypes.JID) (watypes.AddressingMode, error) {
		transport.lookups = append(transport.lookups, group)
		return transport.mode, transport.lookupErr
	}
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		transport.sent = append(transport.sent, extractReactionMessage(message))
		return whatsmeow.SendResponse{}, nil
	}
	return transport
}

// WhatsApp keys a group message by the sender it arrived from (whatsmeow's
// MessageInfo.Sender): the member's LID when the message was addressed by LID,
// its phone number otherwise. Ingest stores "<digits>@lid" for a LID whose
// number it did not know, and the phone number whenever it did. Before, a
// stored LID went out as the participant "<digits>@lid@s.whatsapp.net", a JID
// no message has, and a stored phone number went out as the phone number in a
// group that addresses members by LID.
func TestSendReactionRequestNamesAGroupSenderByTheKeyItsMessageArrivedWith(t *testing.T) {
	const (
		lidJID   = reactionTestLID + "@lid"
		phoneJID = reactionTestPhone + "@s.whatsapp.net"
	)
	lookupFailed := errors.New("group info query timed out")
	tests := []struct {
		name      string
		lids      reactionTestLIDs
		mode      watypes.AddressingMode
		lookupErr error
		author    string
		want      string
	}{
		{
			name:   "LID-only sender in a group that addresses by LID",
			lids:   reactionTestLIDs{},
			mode:   watypes.AddressingModeLID,
			author: lidJID,
			want:   lidJID,
		},
		// The message was keyed by the LID; a phone number learned since
		// does not change its key.
		{
			name:   "LID sender whose number became known later",
			lids:   reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:   watypes.AddressingModeLID,
			author: lidJID,
			want:   lidJID,
		},
		{
			name:   "LID sender in a group that now reads phone-number addressing",
			lids:   reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:   watypes.AddressingModePN,
			author: lidJID,
			want:   lidJID,
		},
		{
			name:   "phone-number sender in a group that addresses by LID",
			lids:   reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:   watypes.AddressingModeLID,
			author: "+" + reactionTestPhone,
			want:   lidJID,
		},
		{
			name:   "phone-number sender whose LID is unknown",
			lids:   reactionTestLIDs{},
			mode:   watypes.AddressingModeLID,
			author: "+" + reactionTestPhone,
			want:   phoneJID,
		},
		{
			name:   "phone-number sender in a group that addresses by phone number",
			lids:   reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:   watypes.AddressingModePN,
			author: "+" + reactionTestPhone,
			want:   phoneJID,
		},
		// whatsmeow's SendMessage treats any addressing but "lid" as phone
		// numbers.
		{
			name:   "group that reports no addressing",
			lids:   reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:   "",
			author: "+" + reactionTestPhone,
			want:   phoneJID,
		},
		// SendMessage reads the same cache and reports the failure; the
		// lookup only decides the form.
		{
			name:      "group addressing lookup fails",
			lids:      reactionTestLIDs{reactionTestLID: reactionTestPhone},
			mode:      watypes.AddressingModeLID,
			lookupErr: lookupFailed,
			author:    "+" + reactionTestPhone,
			want:      phoneJID,
		},
		{
			name:   "phone number stored without a plus",
			lids:   reactionTestLIDs{},
			mode:   watypes.AddressingModePN,
			author: reactionTestPhone,
			want:   phoneJID,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newReactionTestTransport(t)
			transport.mode = test.mode
			transport.lookupErr = test.lookupErr

			_, err := reactionTestBridge(test.lids).SendReactionRequest(
				reactionTestGroup,
				"target-remote-id",
				test.author,
				false,
				"🔥",
				"add",
				"request-1",
			)
			if err != nil {
				t.Fatalf("SendReactionRequest() error = %v", err)
			}
			if len(transport.sent) != 1 || transport.sent[0] == nil {
				t.Fatalf("reactions sent = %d, want one", len(transport.sent))
			}
			key := transport.sent[0].GetKey()
			if key.GetParticipant() != test.want {
				t.Fatalf("target participant = %q, want %q", key.GetParticipant(), test.want)
			}
			if key.GetFromMe() {
				t.Fatal("target FromMe = true, want another member's message")
			}
			if key.GetRemoteJID() != strings.TrimPrefix(reactionTestGroup, "whatsapp:") || key.GetID() != "target-remote-id" {
				t.Fatalf("target key = %+v, want target-remote-id in the group", key)
			}
		})
	}
}

// In a 1:1 chat whatsmeow puts no participant in the key, and either form of
// the other party's identity marks the message as not this account's. The
// group's addressing is not looked up.
func TestSendReactionRequestNamesADirectChatSenderOfEitherForm(t *testing.T) {
	for _, test := range []struct {
		chat   string
		author string
	}{
		{chat: "whatsapp:" + reactionTestPhone + "@s.whatsapp.net", author: reactionTestLID + "@lid"},
		{chat: "whatsapp:" + reactionTestLID + "@lid", author: reactionTestLID + "@lid"},
		{chat: "whatsapp:" + reactionTestPhone + "@s.whatsapp.net", author: "+" + reactionTestPhone},
	} {
		t.Run(test.chat+" "+test.author, func(t *testing.T) {
			transport := newReactionTestTransport(t)
			_, err := reactionTestBridge(reactionTestLIDs{}).SendReactionRequest(
				test.chat,
				"target-remote-id",
				test.author,
				false,
				"🔥",
				"add",
				"request-1",
			)
			if err != nil {
				t.Fatalf("SendReactionRequest() error = %v", err)
			}
			if len(transport.sent) != 1 || transport.sent[0] == nil {
				t.Fatalf("reactions sent = %d, want one", len(transport.sent))
			}
			key := transport.sent[0].GetKey()
			if key.GetFromMe() || key.GetParticipant() != "" {
				t.Fatalf("target key = %+v, want another party's message with no participant", key)
			}
			if len(transport.lookups) != 0 {
				t.Fatalf("group addressing lookups = %v, want none for a 1:1 chat", transport.lookups)
			}
		})
	}
}

// A stored sender that is neither a JID nor a phone number names no member.
// In a group the reaction is refused before anything is sent, like one with no
// stored sender; before, it went out naming a participant no message has. In
// a 1:1 chat the other party is the sender, as with no stored sender.
func TestSendReactionRequestRefusesAGroupSenderThatIsNotAJIDOrNumber(t *testing.T) {
	for _, author := range []string{"@lid", reactionTestLID + "@", reactionTestLID + "@lid@s.whatsapp.net", "Me", "+", "+1-555-765-4321"} {
		t.Run(author, func(t *testing.T) {
			transport := newReactionTestTransport(t)
			_, err := reactionTestBridge(reactionTestLIDs{}).SendReactionRequest(
				reactionTestGroup,
				"target-remote-id",
				author,
				false,
				"🔥",
				"add",
				"request-1",
			)
			if !errors.Is(err, ErrReactionTargetAuthorUnknown) || !errors.Is(err, ErrSendNotDispatched) {
				t.Fatalf("SendReactionRequest() error = %v, want the unnamed-target refusal marked not dispatched", err)
			}
			if len(transport.sent) != 0 {
				t.Fatalf("reactions sent = %d, want none", len(transport.sent))
			}

			_, err = reactionTestBridge(reactionTestLIDs{}).SendReactionRequest(
				"whatsapp:"+reactionTestPhone+"@s.whatsapp.net",
				"target-remote-id",
				author,
				false,
				"🔥",
				"add",
				"request-1",
			)
			if err != nil {
				t.Fatalf("1:1 SendReactionRequest() error = %v", err)
			}
			if len(transport.sent) != 1 || transport.sent[0].GetKey().GetFromMe() {
				t.Fatalf("1:1 reactions sent = %d, want one naming the other party's message", len(transport.sent))
			}
		})
	}
}

// TestSendReactionRequestGroupParticipantProperty checks the participant rule
// over random senders, LID maps and group addressing. The key always names a
// well-formed member JID (one "@", the phone-number or LID server, no device).
// A stored LID is sent as stored. A stored phone number is sent as its LID
// exactly when the group addresses members by LID and the LID is known, and as
// the phone number otherwise.
func TestSendReactionRequestGroupParticipantProperty(t *testing.T) {
	type input struct {
		Phone     uint64
		LID       uint64
		StoredLID bool
		NoPlus    bool
		Known     bool
		Mode      uint8
	}
	transport := newReactionTestTransport(t)
	property := func(in input) bool {
		phone := strconv.FormatUint(in.Phone%1_000_000_000_000_000+1, 10)
		lid := strconv.FormatUint(in.LID%1_000_000_000_000_000+1, 10)
		if phone == "15551230000" {
			return true // the test account's own number
		}
		lids := reactionTestLIDs{}
		if in.Known {
			lids[lid] = phone
		}
		transport.sent = nil
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

		author, want := "+"+phone, phone+"@s.whatsapp.net"
		if in.NoPlus {
			author = phone
		}
		if in.StoredLID {
			author, want = lid+"@lid", lid+"@lid"
		} else if transport.mode == watypes.AddressingModeLID && transport.lookupErr == nil && in.Known {
			want = lid + "@lid"
		}

		_, err := reactionTestBridge(lids).SendReactionRequest(reactionTestGroup, "target", author, false, "🔥", "add", "request")
		if err != nil || len(transport.sent) != 1 || transport.sent[0] == nil {
			t.Logf("author %q: error %v, %d sent", author, err, len(transport.sent))
			return false
		}
		participant := transport.sent[0].GetKey().GetParticipant()
		parsed, parseErr := watypes.ParseJID(participant)
		wellFormed := parseErr == nil && strings.Count(participant, "@") == 1 && parsed.Device == 0 &&
			(parsed.Server == watypes.DefaultUserServer || parsed.Server == watypes.HiddenUserServer)
		if !wellFormed || participant != want || transport.sent[0].GetKey().GetFromMe() {
			t.Logf("author %q, mode %q, lookup error %v, known %t: participant %q, want %q", author, transport.mode, transport.lookupErr, in.Known, participant, want)
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
func TestSendReactionRequestFindsAGroupSendersLIDInTheSessionStore(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "whatsapp-session.db")
	sessionDB, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(session store): %v", err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT NOT NULL)`); err != nil {
		t.Fatalf("create lid map: %v", err)
	}
	if _, err := sessionDB.Exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)`, reactionTestLID, reactionTestPhone); err != nil {
		t.Fatalf("seed lid map: %v", err)
	}

	transport := newReactionTestTransport(t)
	transport.mode = watypes.AddressingModeLID
	bridge := reactionTestBridge(reactionTestLIDs{})
	bridge.sessionPath = sessionPath
	if _, err := bridge.SendReactionRequest(reactionTestGroup, "target-remote-id", "+"+reactionTestPhone, false, "🔥", "add", "request-1"); err != nil {
		t.Fatalf("SendReactionRequest() error = %v", err)
	}
	if len(transport.sent) != 1 || transport.sent[0] == nil {
		t.Fatalf("reactions sent = %d, want one", len(transport.sent))
	}
	if got := transport.sent[0].GetKey().GetParticipant(); got != reactionTestLID+"@lid" {
		t.Fatalf("target participant = %q, want the session store's LID %q", got, reactionTestLID+"@lid")
	}
}

// An incoming group message stored with no sender cannot be named: WhatsApp
// keys it by its sender, and an empty one means this account. The reaction is
// refused before anything is sent rather than addressed to a message of this
// account's that does not exist.
func TestSendReactionRequestRefusesAnIncomingGroupTargetWithNoSender(t *testing.T) {
	bridge := connectedMediaTestBridge()
	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	sends := 0
	sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sends++
		return whatsmeow.SendResponse{}, nil
	}

	_, err := bridge.SendReactionRequest(
		"whatsapp:120363019999999999@g.us",
		"target-remote-id",
		"  ",
		false,
		"🔥",
		"add",
		"request-1",
	)
	if !errors.Is(err, ErrReactionTargetAuthorUnknown) || !errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("SendReactionRequest() error = %v, want the unnamed-target refusal marked not dispatched", err)
	}
	if sends != 0 {
		t.Fatalf("WhatsApp sends = %d, want none", sends)
	}
}

func TestSendReactionRequestMarksOnlyPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		_, err := connectedMediaTestBridge().SendReactionRequest(
			"invalid",
			"target-remote-id",
			"+15557654321",
			false,
			"🔥",
			"add",
			"request-1",
		)
		if err == nil || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, want not-dispatched marker", err)
		}
		if err.Error() != "invalid WhatsApp conversation id: invalid" {
			t.Fatalf("error text = %q, want parse error", err.Error())
		}
	})

	t.Run("transport disconnected before send", func(t *testing.T) {
		_, err := (&Bridge{}).SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			false,
			"🔥",
			"add",
			"request-1",
		)
		if !errors.Is(err, whatsmeow.ErrNotConnected) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, want not-connected cause and not-dispatched marker", err)
		}
	})

	t.Run("send-boundary failure is uncertain and reported by retained core", func(t *testing.T) {
		bridge := connectedMediaTestBridge()
		originalSend := sendTextMessage
		originalIsConnected := clientIsConnected
		defer func() {
			sendTextMessage = originalSend
			clientIsConnected = originalIsConnected
		}()
		clientIsConnected = func(*whatsmeow.Client) bool { return true }
		want := whatsmeow.ErrNotConnected
		sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
			return whatsmeow.SendResponse{}, want
		}
		var reported error
		bridge.callbacks.OnConnectionError = func(err error) { reported = err }

		_, err := bridge.SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			false,
			"🔥",
			"add",
			"request-1",
		)
		if !errors.Is(err, want) {
			t.Fatalf("SendReactionRequest() error = %v, want send cause", err)
		}
		if errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, send-boundary failure must stay uncertain", err)
		}
		if !errors.Is(reported, want) {
			t.Fatalf("retained lifecycle report = %v, want send cause", reported)
		}
	})
}
