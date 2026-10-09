package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/storage/sqlite/sqlitetest"
)

type batchRepositories struct {
	store       *sqlite.Store
	messages    *sqlite.MessageRepository
	attachments *sqlite.MessageAttachmentRepository
	outbox      *sqlite.OutboxRepository
}

func newBatchRepositories(t *testing.T, store *sqlite.Store) batchRepositories {
	t.Helper()
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := sqlite.NewOutboxRepository(store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return batchRepositories{store: store, messages: messages, attachments: attachments, outbox: outbox}
}

// keySample draws a key list with repeats and keys that match no row.
func keySample(rng *rand.Rand, keys []string) []string {
	sample := []string{"missing", ""}
	for _, key := range keys {
		if rng.Intn(3) != 0 {
			sample = append(sample, key)
		}
		if rng.Intn(8) == 0 {
			sample = append(sample, key)
		}
	}
	rng.Shuffle(len(sample), func(i, j int) { sample[i], sample[j] = sample[j], sample[i] })
	return sample
}

// Each batched read returns, for every key, exactly what the single-row read
// it replaces returns for that key, and nothing for keys it would not find.
func TestBatchedReadsMatchSingleRowReadsProperty(t *testing.T) {
	ctx := context.Background()
	config := &quick.Config{MaxCount: 25, Rand: rand.New(rand.NewSource(20261009))}
	if testing.Short() {
		config.MaxCount = 5
	}
	property := func(seed int64) bool {
		rs := sqlitetest.BuildRandomStore(t, seed, sqlitetest.DenseShape)
		repos := newBatchRepositories(t, rs.Store)
		rng := rand.New(rand.NewSource(seed))
		ok := true
		fail := func(format string, args ...any) {
			t.Errorf("seed %d: "+format, append([]any{seed}, args...)...)
			ok = false
		}

		conversationKeys := keySample(rng, rs.ConversationIDs)
		conversations, err := rs.Store.ConversationsByID(conversationKeys)
		if err != nil {
			t.Fatal(err)
		}
		rosters, err := rs.Store.ListParticipantIdentities(conversationKeys)
		if err != nil {
			t.Fatal(err)
		}
		latest, err := repos.messages.LatestMessagesForConversations(ctx, conversationKeys)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, id := range conversationKeys {
			want, err := rs.Store.GetConversation(id)
			got, present := conversations[id]
			if errors.Is(err, sqlite.ErrNotFound) {
				if present {
					fail("ConversationsByID answered unknown %q", id)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if !present || !reflect.DeepEqual(got, want) {
				fail("ConversationsByID[%q] = %+v (present %v), want %+v", id, got, present, want)
			} else {
				found[id] = true
			}

			participants, err := rs.Store.ListParticipants(id)
			if err != nil {
				t.Fatal(err)
			}
			var wantRoster []sqlite.ParticipantIdentity
			for _, participant := range participants {
				identity, err := rs.Store.GetIdentity(participant.IdentityID)
				if err != nil {
					t.Fatal(err)
				}
				wantRoster = append(wantRoster, sqlite.ParticipantIdentity{
					ConversationID: id, IdentityID: participant.IdentityID,
					ParticipantDisplayName: participant.DisplayName, IdentityFound: true,
					CanonicalValue: identity.CanonicalValue, IdentityDisplayName: identity.DisplayName,
					IsSelf: identity.IsSelf,
				})
			}
			if !reflect.DeepEqual(rosters[id], wantRoster) {
				fail("ListParticipantIdentities[%q] = %+v, want %+v", id, rosters[id], wantRoster)
			}

			page, err := repos.messages.ListMessagesByConversation(ctx, id, 0, "", 1)
			if err != nil {
				t.Fatal(err)
			}
			gotLatest, present := latest[id]
			if len(page) == 0 {
				if present {
					fail("LatestMessagesForConversations answered empty %q", id)
				}
			} else if !present || !reflect.DeepEqual(gotLatest, page[0]) {
				fail("LatestMessagesForConversations[%q] = %+v, want %+v", id, gotLatest, page[0])
			}
		}
		if len(conversations) != len(found) {
			fail("ConversationsByID returned %d rows, %d distinct keys exist", len(conversations), len(found))
		}

		messageKeys := keySample(rng, rs.MessageIDs)
		for _, ordinal := range []int64{0, 1, 2} {
			attachments, err := repos.attachments.ListForDownload(ctx, messageKeys, ordinal)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range messageKeys {
				want, err := repos.attachments.GetForDownload(ctx, id, ordinal)
				got, ok := attachments[id]
				switch {
				case errors.Is(err, sql.ErrNoRows):
					if ok {
						fail("ListForDownload(%d) answered missing %q", ordinal, id)
					}
				case err != nil:
					t.Fatal(err)
				case !ok || !reflect.DeepEqual(got, want):
					fail("ListForDownload(%d)[%q] = %+v, want %+v", ordinal, id, got, want)
				}
			}
		}

		var identityIDs []string
		for _, message := range rs.Messages {
			if message.SenderIdentityID != nil {
				identityIDs = append(identityIDs, *message.SenderIdentityID)
			}
		}
		identityKeys := keySample(rng, identityIDs)
		identities, err := rs.Store.IdentitiesByID(identityKeys)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range identityKeys {
			want, err := rs.Store.GetIdentity(id)
			got, present := identities[id]
			if errors.Is(err, sqlite.ErrNotFound) {
				if present {
					fail("IdentitiesByID answered unknown %q", id)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if !present || !reflect.DeepEqual(got, want) {
				fail("IdentitiesByID[%q] = %+v, want %+v", id, got, want)
			}
		}

		for _, account := range rs.Accounts {
			states, err := repos.outbox.LatestStatesForLocalMessages(ctx, account.ID, messageKeys)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range messageKeys {
				want, wantOK, err := repos.outbox.LatestStateForLocalMessage(ctx, account.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				got, gotOK := states[id]
				if gotOK != wantOK || got != want {
					fail("LatestStatesForLocalMessages(%s)[%q] = %q/%v, want %q/%v", account.ID, id, got, gotOK, want, wantOK)
				}
			}
		}
		return ok
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// Every batched read seeks an index once per key: none of them scans a table
// that grows with history. (The outbox read seeks the account's outbox rows by
// an account-leading index; the VALUES list a latest-message read iterates is
// the requested keys, not a table.)
func TestBatchedReadPlansSeekPerKey(t *testing.T) {
	ctx := context.Background()
	rs := sqlitetest.BuildRandomStore(t, 3, sqlitetest.DenseShape)
	if err := rs.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, counter, err := sqlitetest.OpenCounting(rs.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repos := newBatchRepositories(t, store)
	keys := []string{"a", "b", "c"}
	reads := map[string]func() error{
		"ConversationsByID":         func() error { _, err := store.ConversationsByID(keys); return err },
		"IdentitiesByID":            func() error { _, err := store.IdentitiesByID(keys); return err },
		"ListParticipantIdentities": func() error { _, err := store.ListParticipantIdentities(keys); return err },
		"LatestMessagesForConversations": func() error {
			_, err := repos.messages.LatestMessagesForConversations(ctx, keys)
			return err
		},
		"ListForDownload": func() error { _, err := repos.attachments.ListForDownload(ctx, keys, 0); return err },
		"LatestStatesForLocalMessages": func() error {
			_, err := repos.outbox.LatestStatesForLocalMessages(ctx, "acct-0", keys)
			return err
		},
	}
	raw := sqlitetest.OpenRaw(t, rs.Path)
	for name, read := range reads {
		counter.Reset()
		if err := read(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		statements := counter.Statements()
		if len(statements) != 1 {
			t.Fatalf("%s issued %d statements for 3 keys, want 1", name, len(statements))
		}
		plan := explainQueryPlan(t, raw, statements[0])
		for _, line := range plan {
			// The only scans allowed are of the requested keys themselves.
			if strings.HasPrefix(line, "SCAN ") && line != "SCAN requested" && !strings.Contains(line, "CONSTANT ROW") {
				t.Errorf("%s plan scans: %q\n%s", name, line, strings.Join(plan, "\n"))
			}
		}
		t.Logf("%s:\n  %s", name, strings.Join(plan, "\n  "))
	}
}

func explainQueryPlan(t *testing.T, raw *sql.DB, statement string) []string {
	t.Helper()
	args := make([]any, strings.Count(statement, "?"))
	for i := range args {
		args[i] = fmt.Sprintf("key-%d", i)
	}
	rows, err := raw.Query("EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
