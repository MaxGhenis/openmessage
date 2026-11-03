package ingest

import (
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Third-round review of PR #200: the stale-binding check must not fail open
// on a participant it cannot key, and must not contradict a bound thread on
// roster entries that prove nothing (self, duplicates, entries with no
// number).

// A snapshot entry whose number cannot be keyed (a bare "+") is left off the
// roster, as an entry with no number is. Before, keying it failed the check,
// which skipped the snapshot as unusable without marking its ID contradicted,
// and the outgoing message was filed into the stale thread.
func TestHistoryStaleBindingCheckIgnoresAnUnkeyableEntry(t *testing.T) {
	unkeyable := &gmproto.Participant{FullName: "Mystery", ID: &gmproto.SmallInfo{Number: "+"}}

	t.Run("bound to another peer", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("hc-plus", "Ada", false, hwtAda))
		h.pump(t)
		ada := h.conversation(t, "hc-plus")

		bea := hwtConversation("hc-plus", "Bea", false, hwtBea)
		bea.Participants = append(bea.Participants, unkeyable)
		h.historyMessage(t, bea, hwtMessage{id: "hc-plus-out", conversation: "hc-plus", body: "hey Bea", at: hwtStart.Add(-time.Hour)}.proto())
		h.pump(t)

		if n := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, ada.ConversationID); n != 0 {
			t.Fatalf("Ada's thread holds %d messages, want none", n)
		}
		if counts := h.counts(); counts.HistorySkipped != 2 || counts.HistoryImported != 0 || counts.RemoteRebinds != 0 {
			t.Fatalf("counters = %+v, want the snapshot and the message skipped", counts)
		}
	})

	t.Run("unbound", func(t *testing.T) {
		h := newHWTHarness(t, false)
		bea := hwtConversation("hc-plus-new", "Bea", false, hwtBea)
		bea.Participants = append(bea.Participants, unkeyable)
		h.historyMessage(t, bea, hwtMessage{id: "hc-plus-new-1", conversation: "hc-plus-new", body: "hi", from: hwtBea, at: hwtStart.Add(-time.Hour)}.proto())
		h.pump(t)

		if counts := h.counts(); counts.HistoryConversations != 1 || counts.HistoryImported != 1 {
			t.Fatalf("counters = %+v, want Bea's thread minted and the message imported", counts)
		}
		if peers := h.peers(t, h.conversation(t, "hc-plus-new").ConversationID); len(peers) != 1 || peers[0] != hwtBea {
			t.Fatalf("minted roster = %v, want only Bea", peers)
		}
	})
}

// hcThreadColumns is every conversations column except last_message_at_ms,
// which history may move forward.
const hcThreadColumns = `SELECT conversation_id, account_id, remote_conversation_id, kind, title,
	remote_revision, notification_mode, is_favorite, archived_at_ms, metadata_json,
	created_at_ms, updated_at_ms FROM conversations WHERE conversation_id = ?`

// Roster entries that prove nothing about which thread a snapshot names never
// make it contradict its bound thread.
func TestHistoryStaleBindingCheckIgnoresEntriesThatProveNothing(t *testing.T) {
	tests := []struct {
		name     string
		bound    func() *gmproto.Conversation
		snapshot func() *gmproto.Conversation
		from     string
	}{
		{
			name:  "group snapshot with only self and entries without a number",
			bound: func() *gmproto.Conversation { return hwtConversation("hc-w7", "Family", true, hwtKarl, hwtShoshana) },
			snapshot: func() *gmproto.Conversation {
				snapshot := hwtConversation("hc-w7", "Family", true)
				snapshot.Participants = append(snapshot.Participants,
					&gmproto.Participant{FullName: "Unknown", ID: &gmproto.SmallInfo{}},
					&gmproto.Participant{FullName: "Also unknown"},
				)
				return snapshot
			},
			from: hwtKarl,
		},
		{
			name:  "self listed again without the self flag",
			bound: func() *gmproto.Conversation { return hwtConversation("hc-w4", "Ada", false, hwtAda) },
			snapshot: func() *gmproto.Conversation {
				snapshot := hwtConversation("hc-w4", "Ada", false, hwtAda)
				snapshot.Participants = append(snapshot.Participants,
					&gmproto.Participant{FullName: "Me", ID: &gmproto.SmallInfo{Number: hwtSelf}})
				return snapshot
			},
			from: hwtAda,
		},
		{
			name:     "the peer listed twice",
			bound:    func() *gmproto.Conversation { return hwtConversation("hc-w5", "Ada", false, hwtAda) },
			snapshot: func() *gmproto.Conversation { return hwtConversation("hc-w5", "Ada", false, hwtAda, hwtAda) },
			from:     hwtAda,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHWTHarness(t, false)
			bound := test.bound()
			h.liveConversation(t, bound)
			h.pump(t)
			thread := h.conversation(t, bound.ConversationID)
			before := h.rows(t, hcThreadColumns, thread.ConversationID)

			h.historyMessage(t, test.snapshot(), hwtMessage{
				id: bound.ConversationID + "-1", conversation: bound.ConversationID, body: "fetched", from: test.from,
				at: hwtStart.Add(-time.Hour),
			}.proto())
			h.pump(t)

			if counts := h.counts(); counts.HistoryImported != 1 || counts.HistorySkipped != 0 || counts.RemoteRebinds != 0 {
				t.Fatalf("counters = %+v, want the message imported and nothing skipped or rebound", counts)
			}
			if got := h.message(t, bound.ConversationID, bound.ConversationID+"-1"); got.ConversationID != thread.ConversationID {
				t.Fatalf("message filed in %q, want the bound thread %q", got.ConversationID, thread.ConversationID)
			}
			// Every column but the one-way recency bump.
			if after := h.rows(t, hcThreadColumns, thread.ConversationID); !sameRows(after, before) {
				t.Fatalf("bound thread changed beyond recency:\n before %v\n after  %v", before, after)
			}
		})
	}
}
