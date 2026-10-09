package v2read

import (
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite/sqlitetest"
)

// The read mapping's statement count must not grow with what it maps: a
// conversation list costs the same handful of statements for 5 rows or 300,
// and a thread page the same for 5 messages or 300. The per-row reference is
// measured alongside to show the cost each fix removes.

type budgetStore struct {
	ids     []string
	source  *Source
	ref     referenceSource
	counter *sqlitetest.Counter
}

func buildBudgetStore(t *testing.T, accounts, conversations, participants, messagesPer int) *budgetStore {
	t.Helper()
	built := sqlitetest.BuildUniformStore(t, sqlitetest.Uniform{
		Accounts: accounts, Conversations: conversations, Participants: participants, MessagesPer: messagesPer,
	})
	source := New(built.Store)
	return &budgetStore{ids: built.ConversationIDs, source: source, ref: referenceSource{s: source}, counter: built.Counter}
}

func (b *budgetStore) count(t *testing.T, read func() error) int {
	t.Helper()
	b.counter.Reset()
	if err := read(); err != nil {
		t.Fatalf("read: %v", err)
	}
	return b.counter.Count()
}

func TestConversationListStatementsDoNotGrowWithRowsOrParticipants(t *testing.T) {
	const accounts = 2
	b := buildBudgetStore(t, accounts, 160, 6, 1)
	counts := map[int]int{}
	for _, limit := range []int{5, 300} {
		counts[limit] = b.count(t, func() error { _, err := b.source.ListConversations(limit); return err })
		reference := b.count(t, func() error { _, err := b.ref.ListConversations(limit); return err })
		t.Logf("ListConversations(%d): %d statements (per-row reference %d)", limit, counts[limit], reference)
		if reference <= counts[limit] {
			t.Fatalf("reference issued %d statements, batched %d: the counter is not measuring", reference, counts[limit])
		}
	}
	if counts[5] != counts[300] {
		t.Fatalf("ListConversations statements grew with the limit: %v\n%s", counts, strings.Join(b.counter.Statements(), "\n---\n"))
	}
	// ListAccounts + the recency read (one statement per account until the
	// bounded single statement lands) + ListAccounts for the account index +
	// one roster read.
	if max := 3 + accounts; counts[300] > max {
		t.Fatalf("ListConversations(300) = %d statements, want <= %d", counts[300], max)
	}

	previews := map[int]int{}
	for _, n := range []int{5, 300} {
		ids := b.ids[:n]
		previews[n] = b.count(t, func() error { _, err := b.source.LatestConversationPreviews(ids); return err })
		reference := b.count(t, func() error { _, err := b.ref.LatestConversationPreviews(ids); return err })
		t.Logf("LatestConversationPreviews(%d ids): %d statements (per-row reference %d)", n, previews[n], reference)
	}
	if previews[5] != 2 || previews[300] != 2 {
		t.Fatalf("LatestConversationPreviews statements = %v, want 2 (latest messages, attachments)", previews)
	}

	byID := b.count(t, func() error { _, err := b.source.GetConversationsByID(b.ids[:300]); return err })
	if byID > 3 {
		t.Fatalf("GetConversationsByID(300) = %d statements, want <= 3", byID)
	}
	platform := b.count(t, func() error { _, err := b.source.ListPlatformConversations("sms", 300); return err })
	if max := 3; platform > max {
		t.Fatalf("ListPlatformConversations(sms, 300) = %d statements, want <= %d", platform, max)
	}
}

func TestMessagePageStatementsDoNotGrowWithPageSize(t *testing.T) {
	b := buildBudgetStore(t, 1, 3, 4, 320)
	conversationID := b.ids[0]
	counts := map[int]int{}
	for _, limit := range []int{6, 300} {
		counts[limit] = b.count(t, func() error {
			_, err := b.source.GetMessagesByConversation(conversationID, limit)
			return err
		})
		reference := b.count(t, func() error {
			_, err := b.ref.GetMessagesByConversation(conversationID, limit)
			return err
		})
		t.Logf("GetMessagesByConversation(%d): %d statements (per-row reference %d)", limit, counts[limit], reference)
	}
	if counts[6] != counts[300] {
		t.Fatalf("thread page statements grew with the page: %v", counts)
	}
	// resolve + list + ListAccounts + reactions + senders + attachments +
	// one send-state read for the page's one account.
	if counts[300] > 7 {
		t.Fatalf("GetMessagesByConversation(300) = %d statements, want <= 7:\n%s", counts[300], strings.Join(b.counter.Statements(), "\n---\n"))
	}

	latest := b.count(t, func() error { _, err := b.source.LatestMessagesByConversation(b.ids); return err })
	if latest > 8 {
		t.Fatalf("LatestMessagesByConversation(%d) = %d statements, want <= 8", len(b.ids), latest)
	}
}
