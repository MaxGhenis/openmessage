package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// The rewrites in the 2026-10-09 plan audit change how SQLite reaches rows,
// never which rows: each property below runs the new statement and the
// statement it replaced (kept here verbatim as the reference) over random
// stores and requires identical answers. The stores are small and dense on
// purpose: timestamps collide, phone numbers repeat across accounts, senders
// are NULL, threads are archived and rosters overlap.

// oldMessagesBeforeCursorQuery and oldMessagesAfterCursorQuery are the OR-form
// keyset cursors the row-value constants replaced.
const (
	oldMessagesBeforeCursorQuery = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ?
		  AND (occurred_at_ms < ? OR (occurred_at_ms = ? AND message_id < ?))
		ORDER BY occurred_at_ms DESC, message_id DESC
		LIMIT ?
	`
	oldMessagesAfterCursorQuery = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ?
		  AND (occurred_at_ms > ? OR (occurred_at_ms = ? AND message_id > ?))
		ORDER BY occurred_at_ms ASC, message_id ASC
		LIMIT ?
	`
	oldConversationPeerIdentitiesQuery = `
		SELECT i.identity_id, i.account_id, i.kind, i.canonical_value, i.raw_value,
		       i.display_name, i.is_self, i.metadata_json, i.created_at_ms, i.updated_at_ms
		FROM conversation_participants p
		JOIN identities i
		  ON i.account_id = p.account_id AND i.identity_id = p.identity_id
		WHERE p.account_id = ? AND p.conversation_id = ?
		  AND p.is_active = 1 AND i.is_self = 0
		ORDER BY i.identity_id
	`
	oldDirectConversationBySolePeerQuery = `
		SELECT ` + conversationColumns + `
		FROM conversations c
		WHERE c.account_id = ? AND c.kind = 'direct'
		  AND EXISTS (
		      SELECT 1 FROM conversation_participants p
		      WHERE p.account_id = c.account_id AND p.conversation_id = c.conversation_id
		        AND p.identity_id = ? AND p.is_active = 1
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM conversation_participants p2
		      JOIN identities i2
		        ON i2.account_id = p2.account_id AND i2.identity_id = p2.identity_id
		      WHERE p2.account_id = c.account_id AND p2.conversation_id = c.conversation_id
		        AND p2.is_active = 1 AND i2.is_self = 0 AND p2.identity_id <> ?
		  )
		ORDER BY c.last_message_at_ms DESC, c.conversation_id
		LIMIT 1
	`
	oldGroupConversationsQuery = `
		SELECT ` + conversationColumns + `
		FROM conversations c
		WHERE c.account_id = ? AND c.kind = 'group'
		ORDER BY c.last_message_at_ms DESC, c.conversation_id
	`
	oldMessageHasReadCursorQuery = `SELECT COUNT(*) FROM read_cursors WHERE last_read_message_id = ?`
)

// oldSearchMessagesStatement is SearchMessages' builder before the audit: the
// sender filter joined identities and compared i.canonical_value.
func oldSearchMessagesStatement(query string, filter SearchQuery) (string, []any) {
	conditions := []string{"m.body LIKE '%' || ? || '%'"}
	args := []any{query}
	if filter.AccountID != "" {
		conditions = append(conditions, "m.account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.ConversationID != "" {
		conditions = append(conditions, "m.conversation_id = ?")
		args = append(args, filter.ConversationID)
	}
	if filter.SenderCanonicalValue != "" {
		conditions = append(conditions, "i.canonical_value = ?")
		args = append(args, filter.SenderCanonicalValue)
	}
	if filter.SinceMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms >= ?")
		args = append(args, filter.SinceMS)
	}
	if filter.UntilMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms <= ?")
		args = append(args, filter.UntilMS)
	}
	args = append(args, filter.Limit)
	return `
		SELECT ` + prefixedMessageColumns("m") + `
		FROM messages AS m
		LEFT JOIN identities AS i
		  ON i.account_id = m.account_id AND i.identity_id = m.sender_identity_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT ?
	`, args
}

// readPlanStore is a random store plus the probe values a property draws from.
type readPlanStore struct {
	Seed int64
}

func (readPlanStore) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(readPlanStore{Seed: r.Int63()})
}

type seededReadStore struct {
	store         *Store
	accounts      []string
	conversations []Conversation
	identities    []Identity
	messages      []Message
	phones        []string
	terms         []string
	outboxLocal   []string // local_message_id values that have outbox rows
}

var readPlanBodies = []string{"hello", "the plan", "zebra crossing", "", "lunch?", "THE END", "hello again"}

// seedReadStore fills a fresh store from seed. All writes go through SQL so the
// shapes the repositories would refuse (none are needed here) cannot sneak in,
// and every foreign key holds.
func seedReadStore(t *testing.T, seed int64) seededReadStore {
	t.Helper()
	return seedReadStoreInto(t, openRepositoryTestStore(t), seed)
}

// seedReadStoreInto writes seedReadStore's rows into an open store.
func seedReadStoreInto(t *testing.T, store *Store, seed int64) seededReadStore {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	db := store.db
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v\n%s %v", err, query, args)
		}
	}
	out := seededReadStore{store: store, terms: []string{"", "hello", "the", "zebra", "absent-term", "?"}}
	nAccounts := 1 + r.Intn(3)
	for a := 0; a < nAccounts; a++ {
		id := fmt.Sprintf("account-%d", a)
		out.accounts = append(out.accounts, id)
		exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, ?, 1, 1)`,
			id, []string{"google_messages", "signal_cli", "whatsmeow"}[a])
		exec(`INSERT INTO devices (device_id, account_id, kind, is_current, created_at_ms, updated_at_ms) VALUES (?, ?, 'local_installation', 1, 1, 1)`,
			"device-"+id, id)
	}
	// Phone numbers deliberately repeat across accounts and kinds.
	out.phones = []string{"+15550000001", "+15550000002", "+15550000003", "+15550000004"}
	identityNo := 0
	for _, account := range out.accounts {
		self := Identity{IdentityID: fmt.Sprintf("identity-%d", identityNo), AccountID: account, Kind: "phone", CanonicalValue: "self-" + account, IsSelf: true}
		identityNo++
		out.identities = append(out.identities, self)
		for k := 0; k < 2+r.Intn(5); k++ {
			kind := []string{"phone", "e164", "uuid"}[r.Intn(3)]
			value := out.phones[r.Intn(len(out.phones))]
			if kind == "uuid" {
				value = fmt.Sprintf("uuid-%d", r.Intn(6))
			}
			identity := Identity{IdentityID: fmt.Sprintf("identity-%d", identityNo), AccountID: account, Kind: IdentityKind(kind), CanonicalValue: value}
			identityNo++
			// (account, kind, canonical) is unique; skip a collision.
			if slices.ContainsFunc(out.identities, func(i Identity) bool {
				return i.AccountID == account && i.Kind == identity.Kind && i.CanonicalValue == value
			}) {
				continue
			}
			out.identities = append(out.identities, identity)
		}
	}
	for _, identity := range out.identities {
		exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, 1, 1)`,
			identity.IdentityID, identity.AccountID, identity.Kind, identity.CanonicalValue, identity.CanonicalValue, identity.IsSelf)
	}
	accountIdentities := func(account string) []Identity {
		var ids []Identity
		for _, identity := range out.identities {
			if identity.AccountID == account {
				ids = append(ids, identity)
			}
		}
		return ids
	}
	nConversations := 1 + r.Intn(10)
	for c := 0; c < nConversations; c++ {
		account := out.accounts[r.Intn(len(out.accounts))]
		conversation := Conversation{
			ConversationID:   fmt.Sprintf("conversation-%02d", c),
			AccountID:        account,
			Kind:             []ConversationKind{ConversationKindDirect, ConversationKindGroup}[r.Intn(2)],
			LastMessageAtMS:  int64(r.Intn(6)) * 10, // ties on purpose
			NotificationMode: NotificationModeAll,
		}
		var archived any
		if r.Intn(4) == 0 {
			archivedAt := int64(r.Intn(3))
			archived = archivedAt
			conversation.ArchivedAtMS = &archivedAt
		}
		out.conversations = append(out.conversations, conversation)
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, archived_at_ms, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, 1, 1)`,
			conversation.ConversationID, account, "remote-"+conversation.ConversationID, conversation.Kind, archived, conversation.LastMessageAtMS)
		for _, identity := range accountIdentities(account) {
			if r.Intn(2) == 0 {
				continue
			}
			exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, is_active) VALUES (?, ?, ?, ?)`,
				account, conversation.ConversationID, identity.IdentityID, r.Intn(4) != 0)
		}
	}
	nMessages := r.Intn(60)
	for m := 0; m < nMessages; m++ {
		conversation := out.conversations[r.Intn(len(out.conversations))]
		var sender *string
		if candidates := accountIdentities(conversation.AccountID); r.Intn(4) != 0 {
			id := candidates[r.Intn(len(candidates))].IdentityID
			sender = &id
		}
		message := Message{
			// IDs are not in time order, so ties are broken by ID, not insertion.
			MessageID:        fmt.Sprintf("message-%02d", r.Intn(1000)),
			ConversationID:   conversation.ConversationID,
			AccountID:        conversation.AccountID,
			RemoteMessageID:  fmt.Sprintf("remote-%d", m),
			SenderIdentityID: sender,
			Direction:        []MessageDirection{MessageDirectionIncoming, MessageDirectionOutgoing}[r.Intn(2)],
			Body:             readPlanBodies[r.Intn(len(readPlanBodies))],
			OccurredAtMS:     1 + int64(r.Intn(8)), // dense: many ties
		}
		if slices.ContainsFunc(out.messages, func(other Message) bool { return other.MessageID == message.MessageID }) {
			continue
		}
		out.messages = append(out.messages, message)
		exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`,
			message.MessageID, message.ConversationID, message.AccountID, message.RemoteMessageID, message.SenderIdentityID, message.Direction, message.Body, message.OccurredAtMS)
	}
	// Read cursors point at some messages; outbox rows name some as local IDs.
	for _, message := range out.messages {
		if r.Intn(5) == 0 {
			exec(`INSERT OR IGNORE INTO read_cursors (account_id, device_id, conversation_id, last_read_message_id, last_read_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, 1, 1)`,
				message.AccountID, "device-"+message.AccountID, message.ConversationID, message.MessageID)
		}
		for k := 0; k < r.Intn(3); k++ {
			outboxID := fmt.Sprintf("outbox-%s-%d", message.MessageID, k)
			exec(`INSERT INTO outbox (outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation, state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 'text', ?, 'h', 'send', ?, ?, ?, 1, ?, ?)`,
				outboxID, message.AccountID, message.ConversationID, outboxID, []string{"confirmed", "rejected", "canceled"}[r.Intn(3)], message.MessageID, outboxID, int64(1+r.Intn(3)), int64(10))
			out.outboxLocal = append(out.outboxLocal, message.MessageID)
		}
	}
	return out
}

func queryMessageIDs(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, query)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.MessageID)
	}
	return ids
}

func queryConversationIDs(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, query)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

var readPlanQuickConfig = &quick.Config{MaxCount: 30}

// Invariant: the row-value keyset cursor selects exactly the rows of the
// OR-form cursor, in the same order, for every cursor and limit; and paging a
// conversation with it visits every message once, newest first.
func TestKeysetCursorMatchesORFormProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x5eed))
		ids := []string{"", "message-00", "message-500", "message-999", "zzz"}
		for _, message := range s.messages {
			ids = append(ids, message.MessageID)
		}
		for range 40 {
			conversation := s.conversations[r.Intn(len(s.conversations))].ConversationID
			ms := int64(r.Intn(11)) - 1
			id := ids[r.Intn(len(ids))]
			limit := 1 + r.Intn(8)
			for _, pair := range [][2]string{
				{messagesBeforeCursorQuery, oldMessagesBeforeCursorQuery},
				{messagesAfterCursorQuery, oldMessagesAfterCursorQuery},
			} {
				got := queryMessageIDs(t, s.store.db, pair[0], conversation, ms, id, limit)
				want := queryMessageIDs(t, s.store.db, pair[1], conversation, ms, ms, id, limit)
				if !slices.Equal(got, want) {
					t.Errorf("seed %d: cursor (%d, %q) limit %d in %s = %v, OR form = %v", c.Seed, ms, id, limit, conversation, got, want)
					return false
				}
			}
		}
		repository := mustMessageRepository(t, s.store, 100)
		for _, conversation := range s.conversations {
			var want []Message
			for _, message := range s.messages {
				if message.ConversationID == conversation.ConversationID {
					want = append(want, message)
				}
			}
			sort.Slice(want, func(i, j int) bool {
				if want[i].OccurredAtMS != want[j].OccurredAtMS {
					return want[i].OccurredAtMS > want[j].OccurredAtMS
				}
				return want[i].MessageID > want[j].MessageID
			})
			var walked []string
			var beforeMS int64
			var beforeID string
			for page := 0; page <= len(want)+1; page++ {
				rows, err := repository.ListMessagesByConversation(context.Background(), conversation.ConversationID, beforeMS, beforeID, 3)
				if err != nil {
					t.Fatalf("ListMessagesByConversation: %v", err)
				}
				for _, row := range rows {
					walked = append(walked, row.MessageID)
				}
				if len(rows) < 3 {
					break
				}
				beforeMS, beforeID = rows[len(rows)-1].OccurredAtMS, rows[len(rows)-1].MessageID
			}
			wantIDs := make([]string, 0, len(want))
			for _, message := range want {
				wantIDs = append(wantIDs, message.MessageID)
			}
			if !slices.Equal(walked, wantIDs) {
				t.Errorf("seed %d: walking %s in pages of 3 = %v, want %v", c.Seed, conversation.ConversationID, walked, wantIDs)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, readPlanQuickConfig); err != nil {
		t.Fatal(err)
	}
}

// Invariant: for every filter combination, the audited search statement
// returns the same messages, in the same order, as the statement it replaced.
func TestSearchStatementMatchesPreviousStatementProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x5ea7c4))
		pick := func(values []string) string { return values[r.Intn(len(values))] }
		for range 60 {
			filter := SearchQuery{Limit: 1 + r.Intn(12)}
			if r.Intn(4) == 0 {
				filter.AccountID = pick(append(slices.Clone(s.accounts), "account-none"))
			}
			if r.Intn(3) == 0 {
				filter.ConversationID = s.conversations[r.Intn(len(s.conversations))].ConversationID
			}
			if r.Intn(2) == 0 {
				filter.SenderCanonicalValue = pick(append(slices.Clone(s.phones), "uuid-1", "self-account-0", "+19999999999"))
			}
			if r.Intn(3) == 0 {
				filter.SinceMS = int64(r.Intn(9))
			}
			if r.Intn(3) == 0 {
				filter.UntilMS = int64(r.Intn(9))
			}
			term := pick(s.terms)
			newSQL, newArgs := searchMessagesStatement(term, filter)
			oldSQL, oldArgs := oldSearchMessagesStatement(term, filter)
			got := queryMessageIDs(t, s.store.db, newSQL, newArgs...)
			want := queryMessageIDs(t, s.store.db, oldSQL, oldArgs...)
			if !slices.Equal(got, want) {
				t.Errorf("seed %d: search %q %+v = %v, previous statement = %v", c.Seed, term, filter, got, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, readPlanQuickConfig); err != nil {
		t.Fatal(err)
	}
}

// Invariant: the rebind lookups return what the replaced statements and the
// replaced group scan returned, for every account, identity and peer set.
func TestRebindLookupsMatchPreviousQueriesProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x2eb1d))
		db := s.store.db
		identityIDs := []string{"identity-missing"}
		for _, identity := range s.identities {
			identityIDs = append(identityIDs, identity.IdentityID)
		}
		for _, account := range s.accounts {
			for _, conversation := range s.conversations {
				got, err := s.store.ListConversationPeerIdentities(account, conversation.ConversationID)
				if err != nil {
					t.Fatalf("ListConversationPeerIdentities: %v", err)
				}
				rows, err := db.Query(oldConversationPeerIdentitiesQuery, account, conversation.ConversationID)
				if err != nil {
					t.Fatalf("old peers: %v", err)
				}
				var want []string
				for rows.Next() {
					var identity Identity
					if err := rows.Scan(&identity.IdentityID, &identity.AccountID, &identity.Kind, &identity.CanonicalValue,
						&identity.RawValue, &identity.DisplayName, &identity.IsSelf, &identity.MetadataJSON,
						&identity.CreatedAtMS, &identity.UpdatedAtMS); err != nil {
						t.Fatalf("scan old peers: %v", err)
					}
					want = append(want, identity.IdentityID)
				}
				rows.Close()
				gotIDs := []string{}
				for _, identity := range got {
					gotIDs = append(gotIDs, identity.IdentityID)
				}
				if !slices.Equal(gotIDs, append([]string{}, want...)) {
					t.Errorf("seed %d: peers of %s/%s = %v, previous = %v", c.Seed, account, conversation.ConversationID, gotIDs, want)
					return false
				}
			}
			for _, identityID := range identityIDs {
				got, gotErr := s.store.FindDirectConversationBySolePeer(account, identityID)
				want := queryConversationIDs(t, db, oldDirectConversationBySolePeerQuery, account, identityID, identityID)
				switch {
				case len(want) == 0 && !errorsIsNotFound(gotErr):
					t.Errorf("seed %d: sole peer %s/%s = %v, %v; previous found none", c.Seed, account, identityID, got.ConversationID, gotErr)
					return false
				case len(want) == 1 && (gotErr != nil || got.ConversationID != want[0]):
					t.Errorf("seed %d: sole peer %s/%s = %q, %v; previous = %q", c.Seed, account, identityID, got.ConversationID, gotErr, want[0])
					return false
				}
			}
			// Peer sets: every active roster in the store, plus random sets.
			sets := [][]string{}
			for _, conversation := range s.conversations {
				peers, _ := s.store.ListConversationPeerIdentities(account, conversation.ConversationID)
				var set []string
				for _, peer := range peers {
					set = append(set, peer.IdentityID)
				}
				if len(set) > 0 {
					r.Shuffle(len(set), func(i, j int) { set[i], set[j] = set[j], set[i] })
					sets = append(sets, set)
				}
			}
			for range 6 {
				var set []string
				for range 1 + r.Intn(3) {
					set = append(set, identityIDs[r.Intn(len(identityIDs))])
				}
				sets = append(sets, set)
			}
			for _, set := range sets {
				got, gotErr := s.store.FindGroupConversationByPeerSet(account, set)
				want, wantFound := previousGroupByPeerSet(t, db, account, set)
				if wantFound != (gotErr == nil) || (wantFound && got.ConversationID != want) {
					t.Errorf("seed %d: group with peers %v in %s = %q, %v; previous = %q, found %v", c.Seed, set, account, got.ConversationID, gotErr, want, wantFound)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, readPlanQuickConfig); err != nil {
		t.Fatal(err)
	}
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// previousGroupByPeerSet is FindGroupConversationByPeerSet as it was: every
// group of the account newest first, each roster compared with the wanted set.
func previousGroupByPeerSet(t *testing.T, db *sql.DB, account string, identityIDs []string) (string, bool) {
	t.Helper()
	want := map[string]struct{}{}
	for _, id := range identityIDs {
		want[id] = struct{}{}
	}
	for _, group := range queryConversationIDs(t, db, oldGroupConversationsQuery, account) {
		rows, err := db.Query(oldConversationPeerIdentitiesQuery, account, group)
		if err != nil {
			t.Fatalf("old roster: %v", err)
		}
		var peers []string
		for rows.Next() {
			var identityID, skip string
			var isSelf bool
			var created, updated int64
			if err := rows.Scan(&identityID, &skip, &skip, &skip, &skip, &skip, &isSelf, &skip, &created, &updated); err != nil {
				t.Fatalf("scan old roster: %v", err)
			}
			peers = append(peers, identityID)
		}
		rows.Close()
		if len(peers) != len(want) {
			continue
		}
		match := true
		for _, peer := range peers {
			if _, ok := want[peer]; !ok {
				match = false
				break
			}
		}
		if match {
			return group, true
		}
	}
	return "", false
}

// Invariant: the one-statement recency list equals sorting every conversation
// of every account (archived ones included) and keeping the first limit.
func TestConversationsByRecencyMatchesFullSortProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		all := slices.Clone(s.conversations)
		sort.Slice(all, func(i, j int) bool {
			if all[i].LastMessageAtMS != all[j].LastMessageAtMS {
				return all[i].LastMessageAtMS > all[j].LastMessageAtMS
			}
			return all[i].ConversationID < all[j].ConversationID
		})
		for limit := 0; limit <= len(all)+2; limit++ {
			got, err := s.store.ListConversationsByRecencyAllAccounts(limit)
			if err != nil {
				t.Fatalf("ListConversationsByRecencyAllAccounts(%d): %v", limit, err)
			}
			want := all[:min(limit, len(all))]
			if len(got) != len(want) {
				t.Errorf("seed %d: limit %d returned %d rows, want %d", c.Seed, limit, len(got), len(want))
				return false
			}
			for i := range got {
				if got[i].ConversationID != want[i].ConversationID {
					t.Errorf("seed %d: limit %d row %d = %s, want %s", c.Seed, limit, i, got[i].ConversationID, want[i].ConversationID)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, readPlanQuickConfig); err != nil {
		t.Fatal(err)
	}
}

// Invariant: the per-account aggregates equal the same aggregates computed
// over the seeded rows: latest is the max occurred_at_ms (0 when none), latest
// incoming is the max over incoming rows, and the counts are row counts.
// MessageHasReadCursor agrees with counting cursors directly, and the outbox
// state lookup returns the newest row by (created_at_ms, outbox_id).
func TestAccountAggregatesAndLookupsMatchReferenceProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		for _, account := range append(slices.Clone(s.accounts), "account-none") {
			var wantLatest, wantIncoming, wantMessages, wantConversations int64
			for _, message := range s.messages {
				if message.AccountID != account {
					continue
				}
				wantMessages++
				wantLatest = max(wantLatest, message.OccurredAtMS)
				if message.Direction == MessageDirectionIncoming {
					wantIncoming = max(wantIncoming, message.OccurredAtMS)
				}
			}
			for _, conversation := range s.conversations {
				if conversation.AccountID == account {
					wantConversations++
				}
			}
			latest, incoming, err := s.store.LatestMessageTimes(account)
			if err != nil || latest != wantLatest || incoming != wantIncoming {
				t.Errorf("seed %d: LatestMessageTimes(%s) = %d, %d, %v; want %d, %d", c.Seed, account, latest, incoming, err, wantLatest, wantIncoming)
				return false
			}
			messages, err := s.store.CountMessages(account)
			if err != nil || messages != wantMessages {
				t.Errorf("seed %d: CountMessages(%s) = %d, %v; want %d", c.Seed, account, messages, err, wantMessages)
				return false
			}
			conversations, err := s.store.CountConversations(account)
			if err != nil || conversations != wantConversations {
				t.Errorf("seed %d: CountConversations(%s) = %d, %v; want %d", c.Seed, account, conversations, err, wantConversations)
				return false
			}
		}
		for _, id := range append([]string{"message-missing"}, messageIDs(s.messages)...) {
			got, err := s.store.MessageHasReadCursor(id)
			var count int64
			if scanErr := s.store.db.QueryRow(oldMessageHasReadCursorQuery, id).Scan(&count); scanErr != nil {
				t.Fatalf("old cursor count: %v", scanErr)
			}
			if err != nil || got != (count > 0) {
				t.Errorf("seed %d: MessageHasReadCursor(%s) = %v, %v; cursor count %d", c.Seed, id, got, err, count)
				return false
			}
		}
		outbox, err := NewOutboxRepository(s.store, func() time.Time { return time.UnixMilli(100) })
		if err != nil {
			t.Fatalf("NewOutboxRepository: %v", err)
		}
		for _, message := range s.messages {
			got, ok, err := outbox.LatestStateForLocalMessage(context.Background(), message.AccountID, message.MessageID)
			var want string
			wantErr := s.store.db.QueryRow(`
				SELECT state FROM outbox NOT INDEXED
				WHERE account_id = ? AND local_message_id = ?
				ORDER BY created_at_ms DESC, outbox_id DESC LIMIT 1
			`, message.AccountID, message.MessageID).Scan(&want)
			if err != nil || ok != (wantErr == nil) || (ok && string(got) != want) {
				t.Errorf("seed %d: LatestStateForLocalMessage(%s) = %q, %v, %v; want %q (%v)", c.Seed, message.MessageID, got, ok, err, want, wantErr)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, readPlanQuickConfig); err != nil {
		t.Fatal(err)
	}
}

func messageIDs(messages []Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.MessageID)
	}
	return ids
}
