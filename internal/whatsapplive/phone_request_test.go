package whatsapplive_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/keys/identity"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"
	"go.mau.fi/libsignal/util/optional"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// TestUndecryptableMessageIsRequestedFromThePhoneOnce pins down a side
// effect of synchronous acks that OpenMessage accepts on purpose. When a
// message cannot be decrypted, whatsmeow sends the sender a retry receipt
// and, on the first one for that message, asks our own phone for the message
// (a placeholder resend request), whatever AutomaticMessageRerequestFromPhone
// says. It asks only once per message. The phone's answer reaches v2 ingest
// and the legacy store like any message.
func TestUndecryptableMessageIsRequestedFromThePhoneOnce(t *testing.T) {
	r := newTableRow(t)
	r.observe()
	r.connect(1)

	// Our phone: our account's device 0, with its own Signal store. We
	// already have a session with it, as a linked device does.
	ourLID := r.cli.Store.GetLID()
	phoneAddress := watypes.JID{User: ourLID.User, Device: 0, Server: watypes.HiddenUserServer}.SignalAddress()
	phone := whatsapplive.NewPeerDeviceForTeardownTest(t, filepath.Join(t.TempDir(), "phone-session.db"),
		watypes.JID{User: r.ourJID.User, Server: watypes.DefaultUserServer})
	processBundle(t, r.cli.Store, phone, phoneAddress)

	// A message from the peer that does not decrypt.
	r.startPeerSession()
	const id = watypes.MessageID("3EB0UNDECRYPTABLE")
	const text = "the phone has this message"
	broken := r.peerMessage(id, "the ciphertext we receive is damaged")
	enc := broken.GetChildren()[0]
	damaged := append([]byte{}, enc.Content.([]byte)...)
	damaged[len(damaged)-1] ^= 0xff
	enc.Content = damaged
	broken.Content = []waBinary.Node{enc}

	isPhoneRequest := func(received fakeReceived) bool {
		return received.node.Tag == "message" && fakeAttr(received.node, "category") == "peer"
	}
	isRetryReceipt := func(received fakeReceived) bool {
		return received.node.Tag == "receipt" && fakeAttr(received.node, "type") == string(watypes.ReceiptTypeRetry) &&
			fakeAttr(received.node, "id") == string(id)
	}

	r.server.deliver(broken)
	r.drain()
	if retries := r.server.receivedNodes(isRetryReceipt); len(retries) != 1 {
		t.Fatalf("sent %d retry receipts for %s, want 1", len(retries), id)
	}
	requests := r.server.receivedNodes(isPhoneRequest)
	if len(requests) != 1 {
		t.Fatalf("sent %d requests to the phone, want 1", len(requests))
	}
	ourAddress := r.ourJIDAsSeenByPhone()
	request := phoneDecrypts(t, phone, ourAddress, requests[0].node)
	operation := request.GetProtocolMessage().GetPeerDataOperationRequestMessage()
	if operation.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_PLACEHOLDER_MESSAGE_RESEND ||
		len(operation.GetPlaceholderMessageResendRequest()) != 1 ||
		operation.GetPlaceholderMessageResendRequest()[0].GetMessageKey().GetID() != string(id) {
		t.Fatalf("phone request = %v, want a placeholder resend request for %s", operation, id)
	}

	// The phone answers with the message.
	webMessage, err := proto.Marshal(&waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{
			RemoteJID: proto.String(r.peerJID.String()),
			FromMe:    proto.Bool(false),
			ID:        proto.String(string(id)),
		},
		Message:          &waE2E.Message{Conversation: proto.String(text)},
		MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
	})
	if err != nil {
		t.Fatalf("marshal web message: %v", err)
	}
	answer := phoneEncrypts(t, phone, ourAddress, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE.Enum(),
		PeerDataOperationRequestResponseMessage: &waE2E.PeerDataOperationRequestResponseMessage{
			PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_PLACEHOLDER_MESSAGE_RESEND.Enum(),
			StanzaID:                     proto.String(fakeAttr(requests[0].node, "id")),
			PeerDataOperationResult: []*waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult{{
				PlaceholderMessageResendResponse: &waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult_PlaceholderMessageResendResponse{
					WebMessageInfoBytes: webMessage,
				},
			}},
		},
	}})
	r.server.deliver(&waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{
		"from":     watypes.JID{User: ourLID.User, Device: 0, Server: watypes.HiddenUserServer},
		"id":       "3EB0PHONEANSWER",
		"t":        nowAttr(),
		"type":     "text",
		"category": "peer",
	}, Content: []waBinary.Node{answer}})
	r.drain()
	if frames := r.matching(messageFrame(id)); len(frames) != 1 {
		t.Fatalf("phone's answer committed %d frames for %s, want 1", len(frames), id)
	}
	if stored, err := r.messages.GetMessageByID("whatsapp:" + string(id)); err != nil || stored == nil || stored.Body != text {
		t.Fatalf("legacy store message %s = %+v, %v; want body %q", id, stored, err, text)
	}

	// The sender's retry fails too: a second retry receipt, and no second
	// request to the phone.
	r.server.deliver(broken)
	r.drain()
	if retries := r.server.receivedNodes(isRetryReceipt); len(retries) != 2 {
		t.Fatalf("sent %d retry receipts for %s, want 2", len(retries), id)
	}
	if requests := r.server.receivedNodes(isPhoneRequest); len(requests) != 1 {
		t.Fatalf("sent %d requests to the phone after a second failure, want still 1", len(requests))
	}
}

// ourJIDAsSeenByPhone is the address under which the phone keeps its
// session with this device.
func (r *tableRow) ourJIDAsSeenByPhone() *protocol.SignalAddress {
	lid := r.cli.Store.GetLID()
	return watypes.JID{User: lid.User, Device: r.ourJID.Device, Server: watypes.HiddenUserServer}.SignalAddress()
}

// processBundle builds ours' outgoing session with theirs from a prekey
// bundle of theirs, as a client does after fetching the bundle.
func processBundle(t *testing.T, ours, theirs *store.Device, theirAddress *protocol.SignalAddress) {
	t.Helper()
	ctx := context.Background()
	oneTime, err := theirs.PreKeys.GenOnePreKey(ctx)
	if err != nil {
		t.Fatalf("GenOnePreKey(): %v", err)
	}
	bundle := prekey.NewBundle(
		theirs.RegistrationID, theirAddress.DeviceID(),
		optional.NewOptionalUint32(oneTime.KeyID), theirs.SignedPreKey.KeyID,
		ecc.NewDjbECPublicKey(*oneTime.Pub), ecc.NewDjbECPublicKey(*theirs.SignedPreKey.Pub), *theirs.SignedPreKey.Signature,
		identity.NewKey(ecc.NewDjbECPublicKey(*theirs.IdentityKey.Pub)),
	)
	if err := session.NewBuilderFromSignal(ours, theirAddress, store.SignalProtobufSerializer).ProcessBundle(ctx, bundle); err != nil {
		t.Fatalf("ProcessBundle(): %v", err)
	}
}

// phoneDecrypts decrypts a peer message we sent to the phone.
func phoneDecrypts(t *testing.T, phone *store.Device, ourAddress *protocol.SignalAddress, stanza *waBinary.Node) *waE2E.Message {
	t.Helper()
	enc, ok := stanza.GetOptionalChildByTag("enc")
	if !ok {
		t.Fatalf("peer message %v has no enc child", stanza.Attrs["id"])
	}
	content, _ := enc.Content.([]byte)
	cipher := session.NewCipher(session.NewBuilderFromSignal(phone, ourAddress, store.SignalProtobufSerializer), ourAddress)
	var padded []byte
	var err error
	switch fakeAttr(&enc, "type") {
	case "pkmsg":
		var message *protocol.PreKeySignalMessage
		message, err = protocol.NewPreKeySignalMessageFromBytes(content, store.SignalProtobufSerializer.PreKeySignalMessage, store.SignalProtobufSerializer.SignalMessage)
		if err == nil {
			padded, err = cipher.DecryptMessage(context.Background(), message)
		}
	case "msg":
		var message *protocol.SignalMessage
		message, err = protocol.NewSignalMessageFromBytes(content, store.SignalProtobufSerializer.SignalMessage)
		if err == nil {
			padded, err = cipher.Decrypt(context.Background(), message)
		}
	default:
		t.Fatalf("peer message enc type %q", fakeAttr(&enc, "type"))
	}
	if err != nil {
		t.Fatalf("phone could not decrypt the peer message: %v", err)
	}
	var message waE2E.Message
	if err := proto.Unmarshal(padded[:len(padded)-int(padded[len(padded)-1])], &message); err != nil {
		t.Fatalf("unmarshal peer message: %v", err)
	}
	return &message
}

// phoneEncrypts encrypts one message from the phone to this device, on the
// session the phone took from our request.
func phoneEncrypts(t *testing.T, phone *store.Device, ourAddress *protocol.SignalAddress, message *waE2E.Message) waBinary.Node {
	t.Helper()
	cipher := session.NewCipher(session.NewBuilderFromSignal(phone, ourAddress, store.SignalProtobufSerializer), ourAddress)
	ciphertext, err := cipher.Encrypt(context.Background(), marshalForTeardownTest(t, message))
	if err != nil {
		t.Fatalf("phone Encrypt(): %v", err)
	}
	encType := "msg"
	if ciphertext.Type() == protocol.PREKEY_TYPE {
		encType = "pkmsg"
	}
	return waBinary.Node{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": encType}, Content: ciphertext.Serialize()}
}
