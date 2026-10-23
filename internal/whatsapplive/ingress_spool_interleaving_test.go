package whatsapplive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"

	"github.com/maxghenis/openmessage/internal/db"
)

// gapStep is one event in TestEveryGapInterleavingCommitsEveryFrame.
type gapStep byte

const (
	gapCapture       gapStep = 'C' // a message arrives
	gapObserveCommit gapStep = 'O' // a generation begins observing and commits frames
	gapObserveRefuse gapStep = 'R' // a generation begins observing but refuses frames (it is retiring)
	gapObserveFlaky  gapStep = 'F' // a generation refuses the first frame offered, then commits
	gapUnobserve     gapStep = 'X' // the observing generation retires
)

var gapSteps = []gapStep{gapCapture, gapObserveCommit, gapObserveRefuse, gapObserveFlaky, gapUnobserve}

// TestEveryGapInterleavingCommitsEveryFrame checks the spool's invariants
// exhaustively over every sequence of up to five events (four under -race),
// with a spool that takes everything and one that holds a single frame:
//
//   - a message is reported handled iff it was committed or held, and a
//     message reported not handled is not held (whatsmeow redelivers it);
//   - the spool never holds more than its bound;
//   - once a generation that commits observes ingress, and whatsmeow
//     redelivers every message reported not handled, every message has been
//     committed, every commit of it carries the key it was captured with, the
//     spool is empty, and the legacy store holds it.
func TestEveryGapInterleavingCommitsEveryFrame(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// One in-memory database (a single connection keeps it alive) avoids a
	// file sync per step across the 7,810 interleavings.
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	t.Cleanup(func() { _ = database.Close() })
	own := watypes.NewJID("15550000000", watypes.DefaultUserServer)

	maxLength, wantSequences := 5, 3905 // 5 + 25 + 125 + 625 + 3125
	if raceDetectorEnabled {
		maxLength, wantSequences = 4, 780
	}
	var sequences [][]gapStep
	var extend func(prefix []gapStep)
	extend = func(prefix []gapStep) {
		if len(prefix) > 0 {
			sequences = append(sequences, append([]gapStep(nil), prefix...))
		}
		if len(prefix) == maxLength {
			return
		}
		for _, step := range gapSteps {
			extend(append(prefix, step))
		}
	}
	extend(nil)

	checked := 0
	for _, maxFrames := range []int64{16, 1} {
		for index, sequence := range sequences {
			label := fmt.Sprintf("bound %d, sequence %s", maxFrames, string(sequence))
			if _, err := database.Exec(`DROP TABLE IF EXISTS openmessage_whatsapp_ingress_spool`); err != nil {
				t.Fatalf("reset spool: %v", err)
			}
			spool, err := openIngressSpool(database, maxFrames, ingressSpoolMaxBytes)
			if err != nil {
				t.Fatalf("openIngressSpool(): %v", err)
			}
			now := spoolTestEpoch
			spool.now = func() time.Time { return now }
			bridge := &Bridge{store: store, client: &whatsmeow.Client{Store: &wastore.Device{ID: &own}}, spool: spool}
			// An adapter owns ingress, and no generation observes it yet.
			bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()

			committed := map[string][]string{} // message id -> keys it was committed under
			observe := func(refuseFirst, refuseAll bool) func(IngressFrame) error {
				refused := false
				return func(frame IngressFrame) error {
					if refuseAll || (refuseFirst && !refused) {
						refused = true
						return errors.New("refused")
					}
					id := frameMessageID(t, frame)
					committed[id] = append(committed[id], frame.DedupeKey)
					return nil
				}
			}
			var remove func()
			var captured []string
			unhandled := map[string]bool{}
			for step, event := range sequence {
				switch event {
				case gapCapture:
					id := fmt.Sprintf("gap-%d-%d-%d", maxFrames, index, step)
					captured = append(captured, id)
					handled := bridge.handleClientEvent(nil, 0, spoolTestEvent("message", id))
					var heldRows int
					if err := database.QueryRow(
						`SELECT COUNT(*) FROM openmessage_whatsapp_ingress_spool WHERE substr(dedupe_key, 1, ?) = ?`,
						len("msg:"+id+":"), "msg:"+id+":",
					).Scan(&heldRows); err != nil {
						t.Fatalf("%s: count held rows: %v", label, err)
					}
					held := heldRows > 0
					if !handled {
						unhandled[id] = true
						if held {
							t.Fatalf("%s: message %s reported not handled but held", label, id)
						}
					} else if !held && len(committed[id]) == 0 {
						t.Fatalf("%s: message %s reported handled but neither committed nor held", label, id)
					}
				case gapObserveCommit:
					remove = bridge.ObserveIngress(observe(false, false), true)
				case gapObserveRefuse:
					remove = bridge.ObserveIngress(observe(false, true), true)
				case gapObserveFlaky:
					remove = bridge.ObserveIngress(observe(true, false), true)
				case gapUnobserve:
					if remove != nil {
						remove()
						remove = nil
					}
				}
				if held := spool.heldFrames(); held > maxFrames {
					t.Fatalf("%s: spool holds %d frames, bound %d", label, held, maxFrames)
				}
			}

			// A generation that commits observes ingress, and whatsmeow
			// redelivers every message reported not handled.
			bridge.ObserveIngress(observe(false, false), true)
			for _, id := range captured {
				if unhandled[id] && !bridge.handleClientEvent(nil, 0, spoolTestEvent("message", id)) {
					t.Fatalf("%s: redelivered message %s not handled by a committing generation", label, id)
				}
			}
			if held := spool.heldFrames(); held != 0 {
				t.Fatalf("%s: spool still holds %d frames: %v", label, held, heldKeys(t, spool))
			}
			for _, id := range captured {
				keys := committed[id]
				if len(keys) == 0 {
					t.Fatalf("%s: message %s was never committed", label, id)
				}
				for _, key := range keys {
					if key != keys[0] {
						t.Fatalf("%s: message %s committed under keys %q and %q", label, id, keys[0], key)
					}
				}
				if stored, err := store.GetMessageByID("whatsapp:" + id); err != nil || stored == nil {
					t.Fatalf("%s: legacy store lacks message %s (%v)", label, id, err)
				}
			}
			checked++
		}
	}
	if len(sequences) != wantSequences || checked != 2*len(sequences) {
		t.Fatalf("checked %d interleavings of %d sequences, want 2 x %d", checked, len(sequences), wantSequences)
	}
}

// frameMessageID reads the message id from a captured message frame.
func frameMessageID(t *testing.T, frame IngressFrame) string {
	t.Helper()
	var envelope whatsappMessageIngressEnvelope
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatalf("decode frame %s: %v", frame.DedupeKey, err)
	}
	return envelope.MessageID
}
