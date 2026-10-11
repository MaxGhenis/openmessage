package whatsapplive

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
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
// whatsmeow and what the legacy store holds. The bridge has neither a spool
// nor a history-sync queue, as one whose runtime database is closed. The
// rule: only a message or history-sync chunk whose frame was not committed,
// for a reason a later delivery can clear (no observing generation, or the
// observer rejected it), while the last observer commits to a durable inbox,
// is reported not handled; and a history-sync notification is reported not
// handled whatever the observer did, since nothing can queue it. The legacy
// store takes every message on every delivery.
func TestHandledStatusTable(t *testing.T) {
	outcomes := []ingressObserverOutcome{observerNeverUsed, observerCommits, observerRejects, observerPanics, observerRemoved}
	kinds := []string{"message", "receipt", "history_sync", "history_notification", "chat_presence"}
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
					case "history_notification":
						event = spoolTestEvent("history_notification", "status-table")
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
					missed := durable && (outcome == observerRejects || outcome == observerRemoved)
					wantHandled := !((kind == "message" || kind == "history_sync") && missed) && kind != "history_notification"
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
	if len(bridge.legacyAhead) != 0 {
		t.Fatalf("legacy records after the committed redelivery = %v, want none", bridge.legacyAhead)
	}
	fresh := statusTestMessage(chat, "fresh", &waE2E.Message{Conversation: proto.String("committed first time")})
	if !bridge.handleClientEvent(nil, 0, fresh) || len(bridge.legacyAhead) != 0 {
		t.Fatalf("a message committed on its first delivery left legacy records %v", bridge.legacyAhead)
	}
	messages, err := store.GetMessagesByConversation(waConversationID(chat), 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("legacy store holds %d rows after two deliveries of one message and one of another, want 2", len(messages))
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

// legacyTakesModel is the reference semantics of legacyTakesDelivery plus
// legacyTook, as handleMessage drives them: per frame, the legacy store takes
// a delivery iff no stored uncommitted delivery of the frame is pending (one
// recorded since the frame's last committed delivery, within the retention);
// an uncommitted delivery whose legacy write succeeded is recorded, and a
// committed delivery clears the record.
type legacyTakesModel struct {
	pending map[string]time.Time
}

func (m *legacyTakesModel) deliver(key string, committed, writeOK bool, now time.Time) bool {
	if key == "" {
		return true
	}
	at, isPending := m.pending[key]
	if isPending && now.Sub(at) > legacyAheadRetention {
		delete(m.pending, key)
		isPending = false
	}
	if committed {
		delete(m.pending, key)
	}
	take := !isPending
	if take && writeOK && !committed {
		for k, t := range m.pending {
			if now.Sub(t) > legacyAheadRetention {
				delete(m.pending, k)
			}
		}
		m.pending[key] = now
	}
	return take
}

type legacyDelivery struct {
	key                string
	committed, writeOK bool
	at                 time.Time
	took               bool
}

// legacyTakesFromHistory states the rule from the delivery history alone,
// independent of any state machine: delivery i takes the legacy store unless
// an earlier delivery j of the same frame took it, stored it, was uncommitted,
// lies within the retention of i, and no committed delivery of the frame came
// between j and i.
func legacyTakesFromHistory(history []legacyDelivery, i int) bool {
	d := history[i]
	if d.key == "" {
		return true
	}
	for j := i - 1; j >= 0; j-- {
		e := history[j]
		if e.key != d.key {
			continue
		}
		if e.committed {
			return true
		}
		if e.took && e.writeOK && d.at.Sub(e.at) <= legacyAheadRetention {
			return false
		}
	}
	return true
}

// TestLegacyTakesDeliveryMatchesItsModel is a differential property test:
// over random delivery sequences (a small key space, committed or not, legacy
// writes that fail, time steps that cross the retention), the implementation
// agrees with the reference model and with the history rule on every
// delivery, and holds no more records than the model.
func TestLegacyTakesDeliveryMatchesItsModel(t *testing.T) {
	type step struct {
		Key       uint8
		Committed bool
		WriteFail uint8
		AdvanceH  uint16
	}
	property := func(steps []step) bool {
		bridge := &Bridge{}
		model := &legacyTakesModel{pending: map[string]time.Time{}}
		now := time.Unix(1775000000, 0)
		var history []legacyDelivery
		for _, s := range steps {
			now = now.Add(time.Duration(s.AdvanceH%400) * time.Hour)
			key := ""
			if s.Key%5 != 0 {
				key = fmt.Sprintf("msg:k%d:00000000", s.Key%5)
			}
			writeOK := s.WriteFail%4 != 0
			took := bridge.legacyTakesDelivery(key, s.Committed, now)
			if took && writeOK && !s.Committed {
				bridge.legacyTook(key, now)
			}
			if took != model.deliver(key, s.Committed, writeOK, now) {
				return false
			}
			history = append(history, legacyDelivery{key: key, committed: s.Committed, writeOK: writeOK, at: now, took: took})
			if took != legacyTakesFromHistory(history, len(history)-1) {
				return false
			}
		}
		return len(bridge.legacyAhead) == len(model.pending)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261011))}); err != nil {
		t.Fatal(err)
	}
}

// TestLegacyFailureOnAnUncommittedDeliveryIsRetriedAndNotAcknowledged covers
// the two ways legacy handling can fail on a delivery v2 did not commit. A
// panic must not escape to whatsmeow, whose dispatcher would treat the event
// as handled and acknowledge it; the message stays not handled. A failed
// write is not recorded as taken, so the redelivery writes legacy again.
func TestLegacyFailureOnAnUncommittedDeliveryIsRetriedAndNotAcknowledged(t *testing.T) {
	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	event := statusTestMessage(chat, "fragile", &waE2E.Message{Conversation: proto.String("fragile text")})

	t.Run("panic before the write", func(t *testing.T) {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("db.New(): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		// With no store the legacy conversation upsert dereferences nil, a
		// panic before the message row is written.
		bridge := &Bridge{}
		bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()
		if bridge.handleClientEvent(nil, 0, event) {
			t.Fatal("an uncommitted message whose legacy handling panicked was reported handled")
		}
		if len(bridge.legacyAhead) != 0 {
			t.Fatalf("a legacy write that panicked was recorded as taken: %v", bridge.legacyAhead)
		}
		bridge.store = store
		bridge.handleClientEvent(nil, 0, event) // redelivery, still uncommitted
		if stored, err := store.GetMessageByID("whatsapp:fragile"); err != nil || stored == nil {
			t.Fatalf("legacy store after the redelivery = %+v, %v; want the panicked write retried", stored, err)
		}
	})

	t.Run("panic after the write", func(t *testing.T) {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("db.New(): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		panics := true
		bridge := &Bridge{store: store, callbacks: Callbacks{OnMessagesChange: func(string) {
			if panics {
				panics = false
				panic("legacy callback bug")
			}
		}}}
		bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()
		if bridge.handleClientEvent(nil, 0, event) {
			t.Fatal("an uncommitted message whose legacy handling panicked was reported handled")
		}
		var frames int
		bridge.ObserveIngress(func(IngressFrame) error { frames++; return nil }, true)
		if !bridge.handleClientEvent(nil, 0, event) || frames != 1 {
			t.Fatalf("redelivery: frames = %d, want the message committed", frames)
		}
	})

	t.Run("failed write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "messages.db")
		store, err := db.New(path)
		if err != nil {
			t.Fatalf("db.New(): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatalf("open raw store: %v", err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if _, err := raw.Exec(`CREATE TRIGGER fail_message_insert BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
			t.Fatalf("create failing trigger: %v", err)
		}
		bridge := &Bridge{store: store}
		bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()
		bridge.handleClientEvent(nil, 0, event)
		if stored, _ := store.GetMessageByID("whatsapp:fragile"); stored != nil {
			t.Fatal("the injected failure did not stop the legacy write")
		}
		if _, err := raw.Exec(`DROP TRIGGER fail_message_insert`); err != nil {
			t.Fatalf("drop failing trigger: %v", err)
		}
		bridge.handleClientEvent(nil, 0, event) // redelivery, still uncommitted
		if stored, err := store.GetMessageByID("whatsapp:fragile"); err != nil || stored == nil {
			t.Fatalf("legacy store after the redelivery = %+v, %v; want the failed write retried", stored, err)
		}
	})
}
