package v2wire

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// A send names its conversation by v2 ID only. SubmitTextV2 and SubmitMediaV2
// refuse a conversation's remote ID (a Google Messages thread number, a
// WhatsApp JID, "signal:+1…") before they queue anything: Google thread IDs
// are device-local, and a phone change hands an old thread number to another
// person's thread (issue #177), so resolving one could text the wrong person.
// Reads still resolve remote IDs (issue #155); showing the wrong thread is
// recoverable, sending to it is not.
func TestSubmitV2RefusesARemoteConversationID(t *testing.T) {
	tests := map[string]func(context.Context, NativeDeps) error{
		"text": func(ctx context.Context, deps NativeDeps) error {
			_, err := SubmitTextV2(ctx, deps, TextInput{
				ConversationID: "remote-v2-conversation",
				Body:           "must not queue",
				IdempotencyKey: "remote-id-text",
			})
			return err
		},
		"media": func(ctx context.Context, deps NativeDeps) error {
			_, err := SubmitMediaV2(ctx, deps, MediaInput{
				ConversationID: "remote-v2-conversation",
				Content:        bytes.NewReader([]byte("must not queue")),
				Filename:       "photo.jpg",
				MIME:           "image/jpeg",
				IdempotencyKey: "remote-id-media",
			})
			return err
		},
	}
	for name, submit := range tests {
		t.Run(name, func(t *testing.T) {
			v2 := openV2TestStore(t)
			seedNativeConversation(t, v2, "native-account", "v2-conversation")
			registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
				"native-account": {TextSend: true, MediaSend: true},
			}}
			service := newSubmitTestService(t, v2, registry, nil)

			err := submit(context.Background(), NativeDeps{V2: v2, Service: service, Registry: registry})
			if !errors.Is(err, ErrUnknownConversation) || !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("submit by remote ID error = %v, want ErrUnknownConversation wrapping sqlite.ErrNotFound", err)
			}
			pending, listErr := service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
			if listErr != nil || len(pending) != 0 {
				t.Fatalf("refused send left outbox rows: %+v, %v", pending, listErr)
			}
		})
	}
}

// Invariant: a send reaches exactly the v2 conversation whose ID it names.
// SubmitTextV2 refuses any other key with ErrUnknownConversation and queues
// nothing. The test draws v2 IDs and remote IDs from one pool and accounts
// from two, so a key is often one conversation's remote ID and another's v2
// ID (the v2 ID must win), or a remote ID under two accounts at once.
func TestSubmitTextV2SendsOnlyToTheConversationItsIDNames(t *testing.T) {
	pool := []string{"2957", "3100", "c0", "c1", "c2"}
	accounts := []string{"account-a", "account-b"}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		v2 := sqliteStoreForProperty(t)
		defer v2.Close()
		now := time.Now().UnixMilli()
		caps := map[string]bridge.CapabilitySet{}
		for _, account := range accounts {
			caps[account] = bridge.CapabilitySet{TextSend: true}
			if err := v2.UpsertAccount(sqlite.Account{
				AccountID: account, BridgeKey: "google_messages", DisplayName: account,
				Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
				CreatedAtMS: now, UpdatedAtMS: now,
			}); err != nil {
				t.Fatalf("UpsertAccount(%s): %v", account, err)
			}
		}
		owner := map[string]string{}
		usedRemote := map[string]bool{}
		for _, id := range pool {
			if random.Intn(2) == 0 {
				continue
			}
			account := accounts[random.Intn(len(accounts))]
			remote := pool[random.Intn(len(pool))]
			if usedRemote[account+"\x1f"+remote] {
				continue
			}
			usedRemote[account+"\x1f"+remote] = true
			if err := v2.UpsertConversation(sqlite.Conversation{
				ConversationID: id, AccountID: account, RemoteConversationID: remote,
				Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll,
				MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
			}); err != nil {
				t.Fatalf("UpsertConversation(%s): %v", id, err)
			}
			owner[id] = account
		}
		registry := submitTestRegistry{caps: caps}
		service := newSubmitTestService(t, v2, registry, nil)
		key := append(append([]string(nil), pool...), "unknown")[random.Intn(len(pool)+1)]

		submission, err := SubmitTextV2(context.Background(), NativeDeps{V2: v2, Service: service, Registry: registry}, TextInput{
			ConversationID: key, Body: "property", IdempotencyKey: "property-key",
		})
		pending, listErr := service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
		if listErr != nil {
			t.Fatalf("ListPending(): %v", listErr)
		}
		account, isV2ID := owner[key]
		if !isV2ID {
			if !errors.Is(err, ErrUnknownConversation) || !errors.Is(err, sqlite.ErrNotFound) || len(pending) != 0 {
				t.Logf("seed %d: key %q names no v2 conversation (owners %v) but err=%v, pending=%+v", seed, key, owner, err, pending)
				return false
			}
			return true
		}
		if err != nil || len(pending) != 1 ||
			pending[0].OutboxID != submission.OutboxID ||
			pending[0].ConversationID != key ||
			pending[0].AccountID != account {
			t.Logf("seed %d: key %q (account %s) err=%v pending=%+v", seed, key, account, err, pending)
			return false
		}
		return true
	}
	cases := 120
	if raceDetectorEnabled {
		cases = 30
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
}

func sqliteStoreForProperty(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	return store
}

// The property above draws these collisions only now and then, and none of
// its fewer draws under -race, so they run here every time. A key that is
// one conversation's v2 ID and another's remote ID sends to the conversation
// the v2 ID names. SubmitTextV2 refuses a remote ID that two accounts share
// and that no conversation has as its v2 ID, and queues nothing.
func TestSubmitTextV2SendsToTheV2IDWhenIDsCollide(t *testing.T) {
	type thread struct{ id, account, remote string }
	tests := map[string]struct {
		threads []thread
		key     string
		want    string // the conversation the send reaches; empty when refused
	}{
		"a v2 ID that is also another thread's remote ID": {
			threads: []thread{{"c0", "account-a", "2957"}, {"2957", "account-b", "c0"}},
			key:     "2957", want: "2957",
		},
		"the same key as a v2 ID and a remote ID under one account": {
			threads: []thread{{"c0", "account-a", "c1"}, {"c1", "account-a", "3100"}},
			key:     "c1", want: "c1",
		},
		"a remote ID that two accounts share": {
			threads: []thread{{"c0", "account-a", "3100"}, {"c1", "account-b", "3100"}},
			key:     "3100",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			v2 := sqliteStoreForProperty(t)
			defer v2.Close()
			now := time.Now().UnixMilli()
			caps := map[string]bridge.CapabilitySet{}
			for _, account := range []string{"account-a", "account-b"} {
				caps[account] = bridge.CapabilitySet{TextSend: true}
				if err := v2.UpsertAccount(sqlite.Account{
					AccountID: account, BridgeKey: "google_messages", DisplayName: account,
					Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
					CreatedAtMS: now, UpdatedAtMS: now,
				}); err != nil {
					t.Fatalf("UpsertAccount(%s): %v", account, err)
				}
			}
			accounts := map[string]string{}
			for _, thread := range test.threads {
				if err := v2.UpsertConversation(sqlite.Conversation{
					ConversationID: thread.id, AccountID: thread.account, RemoteConversationID: thread.remote,
					Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll,
					MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
				}); err != nil {
					t.Fatalf("UpsertConversation(%s): %v", thread.id, err)
				}
				accounts[thread.id] = thread.account
			}
			registry := submitTestRegistry{caps: caps}
			service := newSubmitTestService(t, v2, registry, nil)
			submission, err := SubmitTextV2(context.Background(), NativeDeps{V2: v2, Service: service, Registry: registry}, TextInput{
				ConversationID: test.key, Body: "collision", IdempotencyKey: "collision-key",
			})
			pending, listErr := service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
			if listErr != nil {
				t.Fatalf("ListPending(): %v", listErr)
			}
			if test.want == "" {
				if !errors.Is(err, ErrUnknownConversation) || !errors.Is(err, sqlite.ErrNotFound) || len(pending) != 0 {
					t.Fatalf("send to %q: err=%v, pending=%+v; want ErrUnknownConversation and nothing queued", test.key, err, pending)
				}
				return
			}
			if err != nil || len(pending) != 1 || pending[0].OutboxID != submission.OutboxID ||
				pending[0].ConversationID != test.want || pending[0].AccountID != accounts[test.want] {
				t.Fatalf("send to %q: err=%v, pending=%+v; want one queued send to %s under %s", test.key, err, pending, test.want, accounts[test.want])
			}
		})
	}
}
