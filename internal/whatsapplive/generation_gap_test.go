package whatsapplive_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// These tests cover frames the bridge captures while no lifecycle generation
// can commit them: after one generation retired and before the next observes
// ingress. whatsmeow does not redeliver them. It downloads a history-sync
// chunk in a background loop that ignores the handler's result and then
// deletes the server copy (message.go handleHistorySyncNotificationLoop). It
// acknowledges a receipt before dispatching it. And a message whose
// redelivery decrypts under a different sender (a phone number that became a
// LID) or whose failure whatsmeow inverts (a placeholder resend nested in a
// protocol message) is never redelivered as plaintext either.
//
// Invariant: a frame captured in a generation gap reaches v2 ingest once the
// next generation observes ingress, under the dedupe key it was captured with.

// ownPhone is the account's primary phone (device 0 of our own number), with
// its own real Signal store. It sends history-sync notifications and
// placeholder resend responses.
type ownPhone struct {
	jid    watypes.JID
	device *store.Device
}

func (h *teardownHarness) newOwnPhone() *ownPhone {
	h.t.Helper()
	jid := watypes.JID{User: h.ourJID.User, Server: watypes.DefaultUserServer}
	device := whatsapplive.NewPeerDeviceForTeardownTest(h.t, filepath.Join(filepath.Dir(h.sessionPath), "phone-session.db"), jid)
	h.startSessionFrom(device)
	return &ownPhone{jid: jid, device: device}
}

// phoneMessage is one message from our phone to our device.
func (h *teardownHarness) phoneMessage(phone *ownPhone, id watypes.MessageID, message *waE2E.Message) *waBinary.Node {
	h.t.Helper()
	return stanza(waBinary.Attrs{"from": phone.jid}, id, h.encryptToUsFrom(phone.device, message))
}

// historyChunk is a history-sync chunk holding one incoming text message from
// the peer.
func (h *teardownHarness) historyChunk(id watypes.MessageID, text string) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(h.peerJID.String()),
			Messages: []*waHistorySync.HistorySyncMsg{{
				Message: &waWeb.WebMessageInfo{
					Key: &waCommon.MessageKey{
						RemoteJID: proto.String(h.peerJID.String()),
						FromMe:    proto.Bool(false),
						ID:        proto.String(string(id)),
					},
					Message:          &waE2E.Message{Conversation: proto.String(text)},
					MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
				},
			}},
		}},
	}
}

// historyNotification is the protocol message our phone sends for a chunk.
// The chunk travels inline (InitialHistBootstrapInlinePayload), so whatsmeow's
// DownloadHistorySync needs no media server.
func (h *teardownHarness) historyNotification(chunk *waHistorySync.HistorySync) *waE2E.Message {
	h.t.Helper()
	raw, err := proto.Marshal(chunk)
	if err != nil {
		h.t.Fatalf("marshal history chunk: %v", err)
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		h.t.Fatalf("compress history chunk: %v", err)
	}
	if err := writer.Close(); err != nil {
		h.t.Fatalf("compress history chunk: %v", err)
	}
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType:                          waE2E.HistorySyncType_RECENT.Enum(),
			InitialHistBootstrapInlinePayload: compressed.Bytes(),
		},
	}}
}

// historyFrames lists the committed history-sync frames that carry message id.
func (s *teardownSink) historyFrames(t *testing.T, id watypes.MessageID) []committedText {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var frames []committedText
	for _, record := range s.records {
		if !strings.HasPrefix(record.DedupeKey, "hist:") {
			continue
		}
		var envelope struct {
			Kind     string `json:"kind"`
			ProtoB64 []byte `json:"proto_b64"`
		}
		if err := json.Unmarshal(record.Payload, &envelope); err != nil {
			t.Fatalf("decode history payload: %v", err)
		}
		var chunk waHistorySync.HistorySync
		if err := proto.Unmarshal(envelope.ProtoB64, &chunk); err != nil {
			t.Fatalf("decode history chunk: %v", err)
		}
		for _, conversation := range chunk.GetConversations() {
			for _, item := range conversation.GetMessages() {
				if item.GetMessage().GetKey().GetID() == string(id) {
					frames = append(frames, committedText{generation: record.Generation, dedupeKey: record.DedupeKey})
				}
			}
		}
	}
	return frames
}

// receiptFrames lists the committed receipt frames that name message id.
func (s *teardownSink) receiptFrames(t *testing.T, id watypes.MessageID) []committedText {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var frames []committedText
	for _, record := range s.records {
		if !strings.HasPrefix(record.DedupeKey, "rcpt:") {
			continue
		}
		var envelope struct {
			MessageIDs []string `json:"message_ids"`
		}
		if err := json.Unmarshal(record.Payload, &envelope); err != nil {
			t.Fatalf("decode receipt payload: %v", err)
		}
		for _, messageID := range envelope.MessageIDs {
			if messageID == string(id) {
				frames = append(frames, committedText{generation: record.Generation, dedupeKey: record.DedupeKey})
			}
		}
	}
	return frames
}

// requireFramesIn checks that a frame was committed, every commit of it
// carries one dedupe key, and every commit came from generation want.
func requireFramesIn(t *testing.T, frames []committedText, want bridge.Generation, what string) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatalf("%s never reached v2 ingest", what)
	}
	for _, frame := range frames {
		if frame.dedupeKey != frames[0].dedupeKey {
			t.Fatalf("%s committed under dedupe keys %q and %q", what, frames[0].dedupeKey, frame.dedupeKey)
		}
		if frame.generation != want {
			t.Fatalf("%s committed by generation %d, want %d", what, frame.generation, want)
		}
	}
}

func (h *teardownHarness) awaitLegacyMessage(id watypes.MessageID, text string) {
	h.t.Helper()
	if !awaitTeardownCondition(h.t, "the legacy store to take "+string(id), func() bool {
		stored, err := h.messages.GetMessageByID("whatsapp:" + string(id))
		return err == nil && stored != nil && stored.Body == text
	}) {
		h.t.FailNow()
	}
}

// TestHistoryChunkDispatchedBetweenGenerationsReachesV2 drives the loss path
// end to end. Our phone sends a history-sync notification while a generation
// is online; the notification itself commits and is acknowledged, so the
// server never redelivers it. whatsmeow's background loop downloads the chunk
// and dispatches it after that generation retired and before the next one
// observes ingress (a handler ahead of the bridge's retires the generation at
// that moment). whatsmeow then deletes the server copy whatever the handler
// returned. Before the fix the bridge captured the chunk only when an observer
// was installed, so it reached the legacy store but never v2.
func TestHistoryChunkDispatchedBetweenGenerationsReachesV2(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, _ := h.awaitGeneration()
	phone := h.newOwnPhone()

	const id watypes.MessageID = "3EB0HISTORYGAP"
	const text = "history chunk dispatched between generations"
	notified := make(chan struct{})
	var once sync.Once
	whatsapplive.RunBeforeBridgeHandlerForTeardownTest(h.host, func(raw any) {
		if _, ok := raw.(*waevents.HistorySync); !ok {
			return
		}
		once.Do(func() {
			// Let the notification's own message event finish in the online
			// generation first, as it does when the download takes longer
			// than the handler.
			select {
			case <-notified:
			case <-time.After(teardownTestTimeout):
				t.Error("the notification's handler never returned")
			}
			h.retireGeneration()
		})
	})
	h.deliver(first, h.phoneMessage(phone, "3EB0HISTORYNOTICE", h.historyNotification(h.historyChunk(id, text))))
	close(notified)

	// The legacy store takes the chunk after the bridge's capture of it.
	h.awaitLegacyMessage(id, text)
	_, generation := h.awaitGeneration()
	requireFramesIn(t, h.sink.historyFrames(t, id), generation, "history chunk dispatched between generations")
}

// TestFramesCapturedBetweenGenerationsReachV2 dispatches each frame kind
// whatsmeow will not redeliver after one generation retired and before the
// next observes ingress: a history-sync chunk (dispatched by whatsmeow's
// background loop, which then deletes the server copy) and a receipt
// (acknowledged before dispatch).
func TestFramesCapturedBetweenGenerationsReachV2(t *testing.T) {
	const id watypes.MessageID = "3EB0GAPFRAME"
	for _, kind := range []string{"history sync", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			h := newTeardownHarness(t, teardownMode{})
			h.awaitGeneration()
			h.retireGeneration()

			var event any
			switch kind {
			case "history sync":
				event = &waevents.HistorySync{Data: h.historyChunk(id, "history chunk in a generation gap")}
			case "receipt":
				event = &waevents.Receipt{
					MessageSource: watypes.MessageSource{Chat: h.peerJID, Sender: h.peerJID},
					MessageIDs:    []watypes.MessageID{id},
					Timestamp:     time.Now(),
					Type:          watypes.ReceiptTypeRead,
				}
			}
			h.cli.DangerousInternals().DispatchEvent(event)

			_, generation := h.awaitGeneration()
			switch kind {
			case "history sync":
				requireFramesIn(t, h.sink.historyFrames(t, id), generation, "history chunk")
			case "receipt":
				requireFramesIn(t, h.sink.receiptFrames(t, id), generation, "receipt")
			}
		})
	}
}

// TestRedeliveryUnderALIDDoesNotLoseTheMessage covers a #236 residual gap.
// A message decrypts under the peer's phone number and reaches the bridge
// after its generation retired. Before the next delivery a LID mapping for
// the peer arrives (a device notification or a history chunk carries one).
// The server's redelivery then decrypts under the LID: whatsmeow moves the
// session to the LID and keys its event buffer on the sender, so the buffered
// plaintext is missed, the advanced session fails with an old counter, and
// whatsmeow acknowledges the stanza.
func TestRedeliveryUnderALIDDoesNotLoseTheMessage(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, _ := h.awaitGeneration()
	h.startPeerSession()
	h.deliver(first, h.peerMessage("3EB0LIDSETUP", "opens the session"))
	h.replyToPeer()

	const id watypes.MessageID = "3EB0LIDMIGRATION"
	const text = "decrypted under the phone number, redelivered under the LID"
	node := h.peerMessage(id, text)
	if typ := node.GetChildren()[0].Attrs["type"]; typ != "msg" {
		t.Fatalf("message type = %v, want msg", typ)
	}
	var once sync.Once
	h.cli.Store.MsgSecrets = &msgSecretStoreHook{
		MsgSecretStore: h.cli.Store.MsgSecrets,
		beforePut:      func(context.Context) { once.Do(h.retireGeneration) },
	}
	h.deliver(first, node)

	lid := watypes.JID{User: "100000000000001", Server: watypes.HiddenUserServer}
	if err := h.cli.Store.LIDs.PutLIDMapping(context.Background(), lid, h.peerJID); err != nil {
		t.Fatalf("PutLIDMapping(): %v", err)
	}
	second, generation := h.awaitGeneration()
	h.deliver(second, node)
	requireCommittedOnlyIn(t, h.requireDelivered(id, text,
		"dispatched after its generation retired, then redelivered under the peer's LID"), generation)
}

// TestPlaceholderResendBetweenGenerationsReachesV2 covers the other #236
// residual gap. Our phone answers a placeholder resend request with the
// missing message nested in a protocol message. whatsmeow dispatches the
// nested message, and when the handler reports it not handled,
// handleDecryptedMessage returns "handler did not fail" for the enclosing
// stanza (processProtocolParts returns false, which it inverts), so whatsmeow
// clears the buffered plaintext and acknowledges the stanza. A redelivery
// finds the plaintext already cleared.
func TestPlaceholderResendBetweenGenerationsReachesV2(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, _ := h.awaitGeneration()
	phone := h.newOwnPhone()

	const id watypes.MessageID = "3EB0PLACEHOLDER"
	const text = "resent by the phone while generations changed"
	resent, err := proto.Marshal(&waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{
			RemoteJID: proto.String(h.peerJID.String()),
			FromMe:    proto.Bool(false),
			ID:        proto.String(string(id)),
		},
		Message:          &waE2E.Message{Conversation: proto.String(text)},
		MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
	})
	if err != nil {
		t.Fatalf("marshal resent message: %v", err)
	}
	response := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE.Enum(),
			PeerDataOperationRequestResponseMessage: &waE2E.PeerDataOperationRequestResponseMessage{
				PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_PLACEHOLDER_MESSAGE_RESEND.Enum(),
				StanzaID:                     proto.String("placeholder-request"),
				PeerDataOperationResult: []*waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult{{
					PlaceholderMessageResendResponse: &waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult_PlaceholderMessageResendResponse{
						WebMessageInfoBytes: resent,
					},
				}},
			},
		},
		// whatsmeow stores a message secret before it handles protocol parts:
		// the point at which the test retires the generation.
		MessageContextInfo: withSecret("").GetMessageContextInfo(),
	}
	node := h.phoneMessage(phone, "3EB0PLACEHOLDERRESPONSE", response)
	var once sync.Once
	h.cli.Store.MsgSecrets = &msgSecretStoreHook{
		MsgSecretStore: h.cli.Store.MsgSecrets,
		beforePut:      func(context.Context) { once.Do(h.retireGeneration) },
	}
	h.deliver(first, node)

	second, generation := h.awaitGeneration()
	h.deliver(second, node)
	requireCommittedOnlyIn(t, h.requireDelivered(id, text,
		"a placeholder resend dispatched after its generation retired"), generation)
}
