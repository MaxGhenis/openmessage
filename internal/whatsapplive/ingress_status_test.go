package whatsapplive

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/db"
)

type ingressObserverOutcome string

const (
	observerAbsent    ingressObserverOutcome = "no observer"
	observerCommits   ingressObserverOutcome = "observer commits"
	observerRejects   ingressObserverOutcome = "observer rejects"
	observerPanics    ingressObserverOutcome = "observer panics"
	observerRemoved   ingressObserverOutcome = "observer removed"
	observerNeverUsed ingressObserverOutcome = "no adapter ever observed"
)

// TestHandledStatusTable checks, for every combination of observer state and
// event kind, the status the bridge reports to whatsmeow and what the legacy
// store took. The rule: only a message or receipt whose frame was not
// committed while an adapter owns ingress, for a reason a later delivery can
// clear (no observing generation, or the observer rejected it), is reported
// not handled. The legacy store then does not take the message; a receipt's
// upgrade-only status change applies on every delivery.
func TestHandledStatusTable(t *testing.T) {
	outcomes := []ingressObserverOutcome{observerNeverUsed, observerCommits, observerRejects, observerPanics, observerRemoved}
	kinds := []string{"message", "receipt", "history_sync", "chat_presence"}
	for _, outcome := range outcomes {
		for _, kind := range kinds {
			t.Run(fmt.Sprintf("%s/%s", outcome, kind), func(t *testing.T) {
				store, err := db.New(":memory:")
				if err != nil {
					t.Fatalf("db.New(): %v", err)
				}
				t.Cleanup(func() { _ = store.Close() })
				own := watypes.NewJID("15550000000", watypes.DefaultUserServer)
				bridge := &Bridge{store: store, client: &whatsmeow.Client{Store: &wastore.Device{ID: &own}}}
				observed := 0
				switch outcome {
				case observerCommits:
					bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil })
				case observerRejects:
					bridge.ObserveIngress(func(IngressFrame) error { observed++; return errors.New("stale generation") })
				case observerPanics:
					bridge.ObserveIngress(func(IngressFrame) error { observed++; panic("observer bug") })
				case observerRemoved:
					remove := bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil })
					remove()
				}

				chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
				if kind == "receipt" {
					if err := store.UpsertMessage(&db.Message{
						MessageID:      "whatsapp:status-table",
						ConversationID: "whatsapp:" + chat.String(),
						Body:           "our message",
						TimestampMS:    1775005000000,
						Status:         "sent",
						IsFromMe:       true,
						SourcePlatform: "whatsapp",
						SourceID:       "status-table",
					}); err != nil {
						t.Fatalf("seed outgoing message: %v", err)
					}
				}
				var event any
				switch kind {
				case "message":
					event = &waevents.Message{
						Info: watypes.MessageInfo{
							MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
							ID:            "status-table",
							Timestamp:     time.UnixMilli(1775006000000),
						},
						Message: &waE2E.Message{Conversation: proto.String("status table")},
					}
				case "receipt":
					event = &waevents.Receipt{
						MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
						MessageIDs:    []watypes.MessageID{"status-table"},
						Timestamp:     time.UnixMilli(1775006000000),
						Type:          watypes.ReceiptTypeRead,
					}
				case "history_sync":
					event = &waevents.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}}
				case "chat_presence":
					event = &waevents.ChatPresence{MessageSource: watypes.MessageSource{Chat: chat, Sender: chat}}
				}

				handled := bridge.handleClientEvent(nil, 0, event)

				wantObserved := 0
				if (outcome == observerCommits || outcome == observerRejects || outcome == observerPanics) && kind != "chat_presence" {
					wantObserved = 1
				}
				if observed != wantObserved {
					t.Fatalf("observer saw %d frames, want %d", observed, wantObserved)
				}

				carriesFrame := kind == "message" || kind == "receipt"
				wantHandled := !(carriesFrame && (outcome == observerRejects || outcome == observerRemoved))
				if handled != wantHandled {
					t.Fatalf("handled = %v, want %v", handled, wantHandled)
				}
				if kind == "receipt" {
					stored, err := store.GetMessageByID("whatsapp:status-table")
					if err != nil || stored == nil || stored.Status != "read" {
						t.Fatalf("legacy status after the receipt = %+v, %v; want read on every delivery", stored, err)
					}
					return
				}
				if kind != "message" {
					return
				}
				stored, err := store.GetMessageByID("whatsapp:status-table")
				if err != nil {
					t.Fatalf("GetMessageByID(): %v", err)
				}
				if (stored != nil) != handled {
					t.Fatalf("legacy store holds the message = %v, want %v (it waits for the handled delivery)", stored != nil, handled)
				}
			})
		}
	}
}

// TestUnhandledMessageIsTakenOnceOnRedelivery replays the redelivery: the
// first delivery finds no observing generation, the second finds one.
func TestUnhandledMessageIsTakenOnceOnRedelivery(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	changes := 0
	bridge := &Bridge{store: store, callbacks: Callbacks{OnMessagesChange: func(string) { changes++ }}}
	bridge.ObserveIngress(func(IngressFrame) error { return nil })()

	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	event := &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
			ID:            "redelivered",
			Timestamp:     time.UnixMilli(1775006100000),
		},
		Message: &waE2E.Message{Conversation: proto.String("redelivered")},
	}
	if bridge.handleClientEvent(nil, 0, event) {
		t.Fatal("a message with no observing generation was reported handled")
	}
	if changes != 0 {
		t.Fatalf("legacy handling ran %d times for the unhandled delivery", changes)
	}

	var frames []IngressFrame
	bridge.ObserveIngress(func(frame IngressFrame) error { frames = append(frames, frame); return nil })
	if !bridge.handleClientEvent(nil, 0, event) {
		t.Fatal("the redelivery to an observing generation was reported not handled")
	}
	if len(frames) != 1 || changes != 1 {
		t.Fatalf("redelivery committed %d frames and ran legacy handling %d times, want 1 and 1", len(frames), changes)
	}
}
