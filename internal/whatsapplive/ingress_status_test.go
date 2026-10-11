package whatsapplive

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/whatsmeow"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
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

func statusTestMessage(chat watypes.JID, id watypes.MessageID, message *waE2E.Message) *waevents.Message {
	return &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
			ID:            id,
			Timestamp:     time.UnixMilli(1775006200000),
		},
		Message: message,
	}
}

// TestRedeliveryDoesNotUndoWhatLegacyAppliedInBetween: the legacy store took
// a message on a delivery v2 did not commit, then an edit (or a revoke) of it
// was applied; the redelivery of the original must not write the original
// body back (or bring the revoked message back). A resend with different
// content (a repaired undecryptable message, say) is a different frame and
// still updates the row.
func TestRedeliveryDoesNotUndoWhatLegacyAppliedInBetween(t *testing.T) {
	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	original := statusTestMessage(chat, "original", &waE2E.Message{Conversation: proto.String("original text")})

	for _, change := range []string{"edit", "revoke"} {
		t.Run(change, func(t *testing.T) {
			store, err := db.New(":memory:")
			if err != nil {
				t.Fatalf("db.New(): %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			bridge := &Bridge{store: store}
			bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)() // durable, now gone

			if bridge.handleClientEvent(nil, 0, original) {
				t.Fatal("the first delivery was reported handled with no observing generation")
			}
			protocol := &waE2E.ProtocolMessage{Key: &waCommon.MessageKey{ID: proto.String("original")}}
			if change == "edit" {
				protocol.Type = waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()
				protocol.EditedMessage = &waE2E.Message{Conversation: proto.String("edited text")}
			} else {
				protocol.Type = waE2E.ProtocolMessage_REVOKE.Enum()
			}
			bridge.handleClientEvent(nil, 0, statusTestMessage(chat, "protocol-"+watypes.MessageID(change), &waE2E.Message{ProtocolMessage: protocol}))

			bridge.handleClientEvent(nil, 0, original) // redelivery, still uncommitted
			bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)
			if !bridge.handleClientEvent(nil, 0, original) { // redelivery, committed
				t.Fatal("the committed redelivery was reported not handled")
			}

			stored, err := store.GetMessageByID("whatsapp:original")
			if err != nil {
				t.Fatalf("GetMessageByID(): %v", err)
			}
			switch change {
			case "edit":
				if stored == nil || stored.Body != "edited text" {
					t.Fatalf("legacy row after redelivery = %+v, want the edit kept", stored)
				}
			case "revoke":
				if stored != nil {
					t.Fatalf("legacy row after redelivery = %+v, want the revoked message to stay deleted", stored)
				}
			}
		})
	}

	t.Run("resend with different content", func(t *testing.T) {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("db.New(): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		bridge := &Bridge{store: store}
		bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()
		bridge.handleClientEvent(nil, 0, original)
		bridge.handleClientEvent(nil, 0, statusTestMessage(chat, "original", &waE2E.Message{Conversation: proto.String("resent text")}))
		if stored, err := store.GetMessageByID("whatsapp:original"); err != nil || stored == nil || stored.Body != "resent text" {
			t.Fatalf("legacy row after a different-content resend = %+v, %v; want it updated", stored, err)
		}
	})
}

// legacyTakesModel is the reference semantics of legacyTakesDelivery: per
// frame, the legacy store takes a delivery iff no uncommitted delivery of the
// frame is pending (one taken since the frame's last committed delivery, no
// older than the retention); every uncommitted delivery leaves one pending,
// and a committed one clears it. Pending entries are pruned only when an
// uncommitted delivery records one, as in the implementation.
type legacyTakesModel struct {
	pending map[string]time.Time
}

func (m *legacyTakesModel) deliver(key string, committed bool, now time.Time) bool {
	if key == "" {
		return true
	}
	_, isPending := m.pending[key]
	if committed {
		delete(m.pending, key)
		return !isPending
	}
	if isPending {
		return false
	}
	for k, t := range m.pending {
		if now.Sub(t) > legacyAheadRetention {
			delete(m.pending, k)
		}
	}
	m.pending[key] = now
	return true
}

// TestLegacyTakesDeliveryMatchesItsModel is a differential property test:
// over random delivery sequences (a small key space, both outcomes, time
// steps that cross the retention), the implementation and the model agree on
// every delivery.
func TestLegacyTakesDeliveryMatchesItsModel(t *testing.T) {
	type step struct {
		Key       uint8
		Committed bool
		AdvanceH  uint16
	}
	property := func(steps []step) bool {
		bridge := &Bridge{}
		model := &legacyTakesModel{pending: map[string]time.Time{}}
		now := time.Unix(1775000000, 0)
		for _, s := range steps {
			now = now.Add(time.Duration(s.AdvanceH%400) * time.Hour)
			key := ""
			if s.Key%5 != 0 {
				key = fmt.Sprintf("msg:k%d:00000000", s.Key%5)
			}
			if got, want := bridge.legacyTakesDelivery(key, s.Committed, now), model.deliver(key, s.Committed, now); got != want {
				return false
			}
		}
		return len(bridge.legacyAhead) == len(model.pending)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261011))}); err != nil {
		t.Fatal(err)
	}
}
