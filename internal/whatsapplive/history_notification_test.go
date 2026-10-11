package whatsapplive_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	watypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// These tests drive our phone's history-sync notifications through
// whatsmeow's real message handler (real Signal ciphertext from the phone's
// own store), the bridge, the lifecycle adapter and the supervisor, with the
// history-sync pump's WhatsApp effects recorded instead of sent.
//
// Invariant: a notification is acknowledged (whatsmeow's stanza receipt,
// then the history-sync receipt) only once it is durable, and the
// history-sync receipt and the server copy's deletion come only after the
// chunk reached v2.

// whatsmeowAcknowledgedProtocolMessage reports whether whatsmeow tried to
// send a protocol message's own receipt. The harness has no socket, so every
// attempt logs this warning (message.go handleProtocolMessage).
func (h *teardownHarness) whatsmeowAcknowledgedProtocolMessage(id watypes.MessageID) bool {
	return strings.Contains(h.logs.String(), "Failed to send acknowledgement for protocol message "+string(id))
}

type historyEffects struct {
	mu      sync.Mutex
	effects []string
}

func (e *historyEffects) add(effect string) {
	e.mu.Lock()
	e.effects = append(e.effects, effect)
	e.mu.Unlock()
}

func (e *historyEffects) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.effects...)
}

func TestHistoryNotificationIsAcknowledgedOnlyAfterItsChunkReachesV2(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, generation := h.awaitGeneration()
	phone := h.newOwnPhone()
	if manual, noAutomaticReceipt := whatsapplive.ClientHistorySyncFlagsForTest(h.host); !manual || !noAutomaticReceipt {
		t.Fatalf("whatsmeow downloads history itself (manual %v) or sends its receipt (automatic receipt disabled %v)", manual, noAutomaticReceipt)
	}

	const inlineID, mediaID watypes.MessageID = "3EB0HISTINLINE", "3EB0HISTMEDIA"
	const inlineNotice, mediaNotice watypes.MessageID = "3EB0NOTICEINLINE", "3EB0NOTICEMEDIA"
	const directPath = "/v/t62.15575-24/history-media"
	chunkOf := map[watypes.MessageID]watypes.MessageID{inlineNotice: inlineID, mediaNotice: mediaID}
	mediaChunk := h.historyChunk(mediaID, "history chunk on the media server")
	reachedV2 := func(chunk watypes.MessageID) bool { return len(h.sink.historyFrames(t, chunk)) > 0 }
	recorded := &historyEffects{}
	whatsapplive.SetHistorySyncEffectsForTest(h.host, whatsapplive.HistorySyncEffectsForTest{
		Download: func(ctx context.Context, cli *whatsmeow.Client, notification *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
			if notification.GetDirectPath() == directPath {
				// The media server, stubbed.
				return mediaChunk, nil
			}
			return whatsapplive.DownloadHistorySyncChunkForTest(ctx, cli, notification)
		},
		SendReceipt: func(_ context.Context, _ *whatsmeow.Client, id watypes.MessageID) error {
			if !reachedV2(chunkOf[id]) {
				t.Errorf("history-sync receipt for %s sent before its chunk reached v2", id)
			}
			recorded.add("receipt:" + string(id))
			return nil
		},
		DeleteMedia: func(_ context.Context, _ *whatsmeow.Client, notification *waE2E.HistorySyncNotification) error {
			if !reachedV2(mediaID) {
				t.Errorf("server copy %s deleted before its chunk reached v2", notification.GetDirectPath())
			}
			recorded.add("delete:" + notification.GetDirectPath())
			return nil
		},
	})

	media := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType:      waE2E.HistorySyncType_RECENT.Enum(),
			DirectPath:    proto.String(directPath),
			MediaKey:      []byte("media key"),
			FileEncSHA256: []byte("enc sha256"),
		},
	}}
	h.deliver(first, h.phoneMessage(phone, inlineNotice, h.historyNotification(h.historyChunk(inlineID, "inline history chunk"))))
	h.deliver(first, h.phoneMessage(phone, mediaNotice, media))
	if !awaitTeardownCondition(t, "the pump to finish both notifications", func() bool {
		return whatsapplive.QueuedHistorySyncForTest(h.host) == 0 && len(recorded.list()) == 3
	}) {
		t.FailNow()
	}

	want := []string{"receipt:" + string(inlineNotice), "receipt:" + string(mediaNotice), "delete:" + directPath}
	if got := recorded.list(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pump effects = %v, want %v", got, want)
	}
	requireFramesIn(t, h.sink.historyFrames(t, inlineID), generation, "inline history chunk")
	requireFramesIn(t, h.sink.historyFrames(t, mediaID), generation, "media history chunk")
	h.awaitLegacyMessage(inlineID, "inline history chunk")
	h.awaitLegacyMessage(mediaID, "history chunk on the media server")
	for _, notice := range []watypes.MessageID{inlineNotice, mediaNotice} {
		if h.whatsmeowAcknowledgedProtocolMessage(notice) {
			t.Fatalf("whatsmeow sent %s's history-sync receipt itself", notice)
		}
	}
	if n := whatsapplive.RetainedPlaintextsForTeardownTest(t, h.sessionPath); n != 0 {
		t.Fatalf("whatsmeow still buffers %d plaintexts after both notifications were handled", n)
	}
}

// TestHistoryNotificationTheQueueRefusesIsLeftForRedelivery fills the queue.
// The notification is reported not handled, so whatsmeow keeps its
// plaintext and acknowledges nothing. The server's redelivery, once the
// queue has room, is queued and its chunk reaches v2.
func TestHistoryNotificationTheQueueRefusesIsLeftForRedelivery(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, generation := h.awaitGeneration()
	phone := h.newOwnPhone()
	recorded := &historyEffects{}
	whatsapplive.SetHistorySyncEffectsForTest(h.host, whatsapplive.HistorySyncEffectsForTest{
		SendReceipt: func(_ context.Context, _ *whatsmeow.Client, id watypes.MessageID) error {
			recorded.add("receipt:" + string(id))
			return nil
		},
	})

	const id watypes.MessageID = "3EB0HISTREFUSED"
	const notice watypes.MessageID = "3EB0NOTICEREFUSED"
	const text = "history chunk whose notification the full queue refused"
	node := h.phoneMessage(phone, notice, h.historyNotification(h.historyChunk(id, text)))
	whatsapplive.SetHistorySyncQueueBoundForTest(h.host, 0)
	h.deliver(first, node)
	if queued := whatsapplive.QueuedHistorySyncForTest(h.host); queued != 0 {
		t.Fatalf("the full queue holds %d notifications", queued)
	}
	if n := whatsapplive.RetainedPlaintextsForTeardownTest(t, h.sessionPath); n != 1 {
		t.Fatalf("whatsmeow buffers %d plaintexts, want the refused notification's", n)
	}
	if got := recorded.list(); len(got) != 0 {
		t.Fatalf("pump effects for a refused notification: %v", got)
	}

	whatsapplive.SetHistorySyncQueueBoundForTest(h.host, 4096)
	h.deliver(first, node) // the server redelivers the unacknowledged stanza
	if !awaitTeardownCondition(t, "the redelivered notification's chunk to reach v2", func() bool {
		return len(h.sink.historyFrames(t, id)) > 0 && whatsapplive.QueuedHistorySyncForTest(h.host) == 0
	}) {
		t.FailNow()
	}
	requireFramesIn(t, h.sink.historyFrames(t, id), generation, "redelivered notification's chunk")
	h.awaitLegacyMessage(id, text)
	if got := recorded.list(); len(got) != 1 || got[0] != "receipt:"+string(notice) {
		t.Fatalf("pump effects = %v, want one receipt", got)
	}
	if n := whatsapplive.RetainedPlaintextsForTeardownTest(t, h.sessionPath); n != 0 {
		t.Fatalf("whatsmeow still buffers %d plaintexts after the redelivery was handled", n)
	}
}
