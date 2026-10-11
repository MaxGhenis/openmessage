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
	observerNeverUsed ingressObserverOutcome = "no observer ever"
	observerCommits   ingressObserverOutcome = "observer commits"
	observerRejects   ingressObserverOutcome = "observer rejects"
	observerPanics    ingressObserverOutcome = "observer panics"
	observerRemoved   ingressObserverOutcome = "observer removed"
)

// TestHandledStatusTable checks, for every combination of observer state,
// observer durability and event kind, the status the bridge reports to
// whatsmeow and what the legacy store holds. The rule: only a message whose
// frame was not committed, for a reason a later delivery can clear (no
// observing generation, or the observer rejected it), while the last observer
// commits to a durable inbox, is reported not handled. The legacy store takes
// every message on every delivery.
func TestHandledStatusTable(t *testing.T) {
	outcomes := []ingressObserverOutcome{observerNeverUsed, observerCommits, observerRejects, observerPanics, observerRemoved}
	kinds := []string{"message", "receipt", "history_sync", "chat_presence"}
	for _, durable := range []bool{true, false} {
		for _, outcome := range outcomes {
			for _, kind := range kinds {
				if outcome == observerNeverUsed && !durable {
					continue // durability is a property of an observer
				}
				t.Run(fmt.Sprintf("durable=%v/%s/%s", durable, outcome, kind), func(t *testing.T) {
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
						bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil }, durable)
					case observerRejects:
						bridge.ObserveIngress(func(IngressFrame) error { observed++; return errors.New("stale generation") }, durable)
					case observerPanics:
						bridge.ObserveIngress(func(IngressFrame) error { observed++; panic("observer bug") }, durable)
					case observerRemoved:
						remove := bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil }, durable)
						remove()
					}

					chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
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
					wantHandled := !(kind == "message" && durable && (outcome == observerRejects || outcome == observerRemoved))
					if handled != wantHandled {
						t.Fatalf("handled = %v, want %v", handled, wantHandled)
					}
					if kind != "message" {
						return
					}
					stored, err := store.GetMessageByID("whatsapp:status-table")
					if err != nil {
						t.Fatalf("GetMessageByID(): %v", err)
					}
					if stored == nil || stored.Body != "status table" {
						t.Fatalf("legacy store message = %+v, want it stored on every delivery", stored)
					}
				})
			}
		}
	}
}

// TestUnhandledMessageIsCommittedOnRedelivery replays the redelivery: the
// first delivery finds no observing generation while ingress is durable, the
// second finds one. The legacy store holds one row throughout.
func TestUnhandledMessageIsCommittedOnRedelivery(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bridge := &Bridge{store: store}
	bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()

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
	if stored, err := store.GetMessageByID("whatsapp:redelivered"); err != nil || stored == nil {
		t.Fatalf("legacy store after the unhandled delivery = %+v, %v; want the message", stored, err)
	}

	var frames []IngressFrame
	bridge.ObserveIngress(func(frame IngressFrame) error { frames = append(frames, frame); return nil }, true)
	if !bridge.handleClientEvent(nil, 0, event) {
		t.Fatal("the redelivery to an observing generation was reported not handled")
	}
	if len(frames) != 1 {
		t.Fatalf("redelivery committed %d frames, want 1", len(frames))
	}
	messages, err := store.GetMessagesByConversation(waConversationID(chat), 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("legacy store holds %d rows after two deliveries, want 1", len(messages))
	}
}
