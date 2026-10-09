package v2wire

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// migratedFixtureConversation is a legacy conversation present when the
// fixture store was migrated, so the v2 store holds it under a derived ID.
type migratedFixtureConversation struct {
	legacyID  string
	platform  string
	accountID string
}

var migratedFixtureConversations = []migratedFixtureConversation{
	{legacyID: "google-migrated", platform: "sms", accountID: googleAccountID},
	{legacyID: "whatsapp:15550002222@s.whatsapp.net", platform: "whatsapp", accountID: whatsappAccountID},
	{legacyID: "signal:+15550003333", platform: "signal", accountID: signalAccountID},
}

// postCutoverFixtureConversations exist only in the legacy store; the migrated
// v2 store has never seen them.
var postCutoverFixtureConversations = []migratedFixtureConversation{
	{legacyID: "google-after-cutover", platform: "sms", accountID: googleAccountID},
	{legacyID: "whatsapp:15559990000@s.whatsapp.net", platform: "whatsapp", accountID: whatsappAccountID},
	{legacyID: "signal:+15559990000", platform: "signal", accountID: signalAccountID},
}

// openMigratedTestStores runs the real migration over a small legacy store, so
// the v2 store has the shape the live install has: per-account accounts with
// the migration's bridge keys, and one current local installation device per
// account under a derived ID (not localDeviceID).
func openMigratedTestStores(t *testing.T) (*db.Store, *sqlite.Store) {
	t.Helper()
	root := t.TempDir()
	legacyPath := filepath.Join(root, "messages.db")
	legacy, err := db.New(legacyPath)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	const baseMS int64 = 1_700_000_000_000
	for index, conversation := range migratedFixtureConversations {
		if err := legacy.UpsertConversation(&db.Conversation{
			ConversationID: conversation.legacyID,
			Name:           "Migrated " + conversation.platform,
			Participants:   `[]`,
			LastMessageTS:  baseMS + int64(index),
			SourcePlatform: conversation.platform,
		}); err != nil {
			t.Fatalf("legacy UpsertConversation(%q): %v", conversation.legacyID, err)
		}
	}
	for _, message := range []*db.Message{
		{MessageID: "google-message-1", ConversationID: "google-migrated", SenderNumber: "+15550001111", Body: "google", TimestampMS: baseMS, SourcePlatform: "sms"},
		{MessageID: "whatsapp:wa-1", SourceID: "wa-1", ConversationID: "whatsapp:15550002222@s.whatsapp.net", SenderNumber: "15550002222@s.whatsapp.net", Body: "whatsapp", TimestampMS: baseMS + 1, SourcePlatform: "whatsapp"},
		{MessageID: "signal:1700000000002", SourceID: "1700000000002", ConversationID: "signal:+15550003333", SenderNumber: "+15550003333", Body: "signal", TimestampMS: baseMS + 2, SourcePlatform: "signal"},
	} {
		if err := legacy.UpsertMessage(message); err != nil {
			t.Fatalf("legacy UpsertMessage(%q): %v", message.MessageID, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	staged := filepath.Join(root, "staged.sqlite3")
	report, err := migration.Transform(context.Background(), migration.Options{
		SourcePath:      legacyPath,
		TempStorePath:   staged,
		TempBlobPath:    filepath.Join(root, "blobs"),
		TargetPath:      filepath.Join(root, "target"),
		TargetStorePath: filepath.Join(root, "target", "store.sqlite3"),
		Check:           true,
	})
	if err != nil {
		t.Fatalf("migration.Transform(): %v (report=%+v)", err, report)
	}
	v2, err := sqlite.Open(staged)
	if err != nil {
		t.Fatalf("sqlite.Open(migrated): %v", err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	legacy, err = db.New(legacyPath)
	if err != nil {
		t.Fatalf("reopen legacy store: %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	for _, conversation := range postCutoverFixtureConversations {
		seedLegacyConversation(t, legacy, conversation.legacyID, conversation.platform, false)
	}

	// Pin the shape the fix depends on, so a migration change cannot make
	// these tests pass vacuously.
	for _, accountID := range []string{googleAccountID, whatsappAccountID, signalAccountID} {
		device, err := v2.GetLocalInstallationDevice(context.Background(), accountID)
		if err != nil {
			t.Fatalf("migrated GetLocalInstallationDevice(%q): %v", accountID, err)
		}
		if want := v2keys.DeriveID("device", accountID, accountID+"\x1flocal"); device.DeviceID != want || !device.IsCurrent {
			t.Fatalf("migrated local device for %q = %+v, want current device %q", accountID, device, want)
		}
	}
	return legacy, v2
}

func TestMirrorOnMigratedStoreReusesMigratedLocalDevice(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	ctx := context.Background()
	for _, conversation := range postCutoverFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			accountBefore := mustGetAccount(t, v2, conversation.accountID)
			devicesBefore := mustListDevices(t, v2, conversation.accountID)
			migratedDevice, err := v2.GetLocalInstallationDevice(ctx, conversation.accountID)
			if err != nil {
				t.Fatalf("GetLocalInstallationDevice(): %v", err)
			}

			accountID, conversationID, err := MirrorConversation(legacy, v2, conversation.legacyID)
			if err != nil {
				t.Fatalf("MirrorConversation(): %v", err)
			}
			if accountID != conversation.accountID || conversationID != conversation.legacyID {
				t.Fatalf("MirrorConversation() = (%q, %q), want (%q, %q)", accountID, conversationID, conversation.accountID, conversation.legacyID)
			}
			for _, readAtMS := range []int64{1_910_000_000_000, 1_910_000_000_500} {
				if err := MirrorReadCursor(ctx, legacy, v2, conversation.legacyID, readAtMS); err != nil {
					t.Fatalf("MirrorReadCursor(%d): %v", readAtMS, err)
				}
				cursor, err := v2.GetReadCursor(migratedDevice.DeviceID, conversation.legacyID)
				if err != nil {
					t.Fatalf("GetReadCursor(migrated device): %v", err)
				}
				if cursor.AccountID != conversation.accountID || cursor.LastReadAtMS != readAtMS {
					t.Fatalf("cursor = %+v, want account %q read at %d", cursor, conversation.accountID, readAtMS)
				}
			}

			if _, err := v2.GetDevice(localDeviceID(conversation.accountID)); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("GetDevice(%q) error = %v, want ErrNotFound: the mirror minted a second local device", localDeviceID(conversation.accountID), err)
			}
			if got := mustListDevices(t, v2, conversation.accountID); !reflect.DeepEqual(got, devicesBefore) {
				t.Fatalf("devices = %+v, want unchanged %+v", got, devicesBefore)
			}
			if got := mustGetAccount(t, v2, conversation.accountID); !reflect.DeepEqual(got, accountBefore) {
				t.Fatalf("account = %+v, want unchanged %+v", got, accountBefore)
			}
		})
	}
}

// migratedOwner returns the v2 row the migration keyed a legacy thread under,
// and fails if the fixture no longer exercises a derived key.
func migratedOwner(t *testing.T, v2 *sqlite.Store, conversation migratedFixtureConversation) sqlite.Conversation {
	t.Helper()
	remoteID := v2keys.NormalizeRemoteConversationID(conversation.platform, conversation.legacyID)
	owner, err := v2.GetConversationByRemote(conversation.accountID, remoteID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", conversation.legacyID, err)
	}
	if owner.ConversationID == conversation.legacyID {
		t.Fatalf("migrated conversation kept legacy ID %q; fixture no longer exercises a derived key", owner.ConversationID)
	}
	if owner.RemoteConversationID != conversation.legacyID {
		t.Fatalf("migrated remote id = %q, want the legacy id %q", owner.RemoteConversationID, conversation.legacyID)
	}
	return owner
}

func TestMirrorOnMigratedStoreAdoptsMigratedConversationWithoutRewriting(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	ctx := context.Background()
	for _, conversation := range migratedFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			owner := migratedOwner(t, v2, conversation)
			// Give the v2 row state the legacy mirror would overwrite if it
			// upserted its own view of the thread.
			archivedAt := owner.LastMessageAtMS + 1
			owner.Title = "v2-owned title"
			owner.Kind = sqlite.ConversationKindGroup
			owner.NotificationMode = sqlite.NotificationModeMuted
			owner.IsFavorite = true
			owner.ArchivedAtMS = &archivedAt
			owner.LastMessageAtMS += 10_000
			owner.MetadataJSON = `{"owned_by":"v2"}`
			owner.UpdatedAtMS = owner.LastMessageAtMS
			if err := v2.UpsertConversation(owner); err != nil {
				t.Fatalf("UpsertConversation(owner): %v", err)
			}
			accountBefore := mustGetAccount(t, v2, conversation.accountID)
			devicesBefore := mustListDevices(t, v2, conversation.accountID)
			device, err := v2.GetLocalInstallationDevice(ctx, conversation.accountID)
			if err != nil {
				t.Fatalf("GetLocalInstallationDevice(): %v", err)
			}

			for range 2 {
				accountID, conversationID, err := MirrorConversation(legacy, v2, conversation.legacyID)
				if err != nil {
					t.Fatalf("MirrorConversation(): %v", err)
				}
				if accountID != conversation.accountID || conversationID != owner.ConversationID {
					t.Fatalf("MirrorConversation() = (%q, %q), want the migrated row (%q, %q)",
						accountID, conversationID, conversation.accountID, owner.ConversationID)
				}
			}
			for _, readAtMS := range []int64{1_910_000_000_000, 1_910_000_000_500} {
				if err := MirrorReadCursor(ctx, legacy, v2, conversation.legacyID, readAtMS); err != nil {
					t.Fatalf("MirrorReadCursor(%d): %v", readAtMS, err)
				}
				cursor, err := v2.GetReadCursor(device.DeviceID, owner.ConversationID)
				if err != nil {
					t.Fatalf("GetReadCursor(migrated device, migrated conversation): %v", err)
				}
				if cursor.AccountID != conversation.accountID || cursor.LastReadAtMS != readAtMS {
					t.Fatalf("cursor = %+v, want account %q read at %d", cursor, conversation.accountID, readAtMS)
				}
			}

			got, err := v2.GetConversation(owner.ConversationID)
			if err != nil {
				t.Fatalf("GetConversation(owner): %v", err)
			}
			if !reflect.DeepEqual(got, owner) {
				t.Fatalf("v2 conversation = %+v, want untouched %+v", got, owner)
			}
			if _, err := v2.GetConversation(conversation.legacyID); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("GetConversation(legacy id) error = %v, want ErrNotFound: the mirror added a second row for the thread", err)
			}
			if got := mustGetAccount(t, v2, conversation.accountID); !reflect.DeepEqual(got, accountBefore) {
				t.Fatalf("account = %+v, want unchanged %+v", got, accountBefore)
			}
			if got := mustListDevices(t, v2, conversation.accountID); !reflect.DeepEqual(got, devicesBefore) {
				t.Fatalf("devices = %+v, want unchanged %+v", got, devicesBefore)
			}
		})
	}
}

// TestMirrorConversationAdoptsRowIngestCreatesDuringTheMirror covers v2 ingest
// keying a thread between the mirror's lookup and its upsert: the guarded
// upsert writes nothing, and the mirror returns the ingested row.
func TestMirrorConversationAdoptsRowIngestCreatesDuringTheMirror(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	conversation := postCutoverFixtureConversations[2] // Signal
	ingested := sqlite.Conversation{
		ConversationID:       v2keys.DeriveID("conversation", conversation.accountID, conversation.legacyID),
		AccountID:            conversation.accountID,
		RemoteConversationID: conversation.legacyID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "ingested first",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          1_905_000_000_000,
		UpdatedAtMS:          1_905_000_000_000,
	}
	calls := 0
	beforeOwnedConversationUpsert = func() {
		calls++
		if err := v2.UpsertConversation(ingested); err != nil {
			t.Errorf("ingest UpsertConversation(): %v", err)
		}
	}
	t.Cleanup(func() { beforeOwnedConversationUpsert = func() {} })

	accountID, conversationID, err := MirrorConversation(legacy, v2, conversation.legacyID)
	if err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	if calls != 1 || accountID != conversation.accountID || conversationID != ingested.ConversationID {
		t.Fatalf("MirrorConversation() = (%q, %q) after %d hook calls, want the ingested row %q",
			accountID, conversationID, calls, ingested.ConversationID)
	}
	if got, err := v2.GetConversation(ingested.ConversationID); err != nil || !reflect.DeepEqual(got, ingested) {
		t.Fatalf("ingested row = %+v, %v; want untouched %+v", got, err, ingested)
	}
	if _, err := v2.GetConversation(conversation.legacyID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetConversation(legacy id) error = %v, want ErrNotFound", err)
	}
}

func TestSubmitTextOnMigratedStoreSendsToMigratedAndNewThreads(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true},
	}}
	deps := Deps{Legacy: legacy, V2: v2, Service: newSubmitTestService(t, v2, registry, nil), Registry: registry}
	owner := migratedOwner(t, v2, migratedFixtureConversations[0])

	for _, test := range []struct {
		legacyID           string
		wantConversationID string
	}{
		{legacyID: "google-after-cutover", wantConversationID: "google-after-cutover"},
		{legacyID: "google-migrated", wantConversationID: owner.ConversationID},
	} {
		submission, err := SubmitText(context.Background(), deps, TextInput{
			ConversationID: test.legacyID,
			Body:           "sent to " + test.legacyID,
			IdempotencyKey: "send-" + test.legacyID,
		})
		if err != nil {
			t.Fatalf("SubmitText(%q): %v", test.legacyID, err)
		}
		message := mustGetV2Message(t, v2, submission.LocalMessageID)
		if message.ConversationID != test.wantConversationID || message.AccountID != googleAccountID {
			t.Fatalf("queued message for %q = %+v, want conversation %q", test.legacyID, message, test.wantConversationID)
		}
		// The dispatcher addresses the transport through this column.
		conversation, err := v2.GetConversation(message.ConversationID)
		if err != nil || conversation.RemoteConversationID != test.legacyID {
			t.Fatalf("queued conversation = %+v, %v; want remote id %q", conversation, err, test.legacyID)
		}
	}
}

// TestMirrorReplyTargetOnMigratedStoreReusesMigratedMessages is a differential
// check against the real migration: the remote ID the mirror derives for a
// reply target is the one migration.Transform wrote, so the mirror returns the
// migrated message and adds none.
func TestMirrorReplyTargetOnMigratedStoreReusesMigratedMessages(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true}, whatsappAccountID: {TextSend: true}, signalAccountID: {TextSend: true},
	}}
	deps := Deps{Legacy: legacy, V2: v2, Service: newSubmitTestService(t, v2, registry, nil), Registry: registry}
	for index, conversation := range migratedFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			owner := migratedOwner(t, v2, conversation)
			targets, err := legacy.GetMessagesByConversation(conversation.legacyID, 10)
			if err != nil || len(targets) != 1 {
				t.Fatalf("legacy fixture messages = %+v, %v; want one", targets, err)
			}
			target := targets[0]
			migratedMessages := listV2Messages(t, v2, owner.ConversationID)
			if len(migratedMessages) != 1 {
				t.Fatalf("migrated messages = %+v, want one", migratedMessages)
			}
			migrated := migratedMessages[0]
			ids, err := replyTargetRemoteIDs(conversation.accountID, target)
			if err != nil {
				t.Fatalf("replyTargetRemoteIDs(%q): %v", target.MessageID, err)
			}
			if ids.remote != migrated.RemoteMessageID {
				t.Fatalf("mirror remote id %q, migration wrote %q", ids.remote, migrated.RemoteMessageID)
			}

			for range 2 {
				got, err := MirrorReplyTarget(legacy, v2, target.MessageID)
				if err != nil {
					t.Fatalf("MirrorReplyTarget(%q): %v", target.MessageID, err)
				}
				if got != migrated.MessageID {
					t.Fatalf("MirrorReplyTarget(%q) = %q, want the migrated message %q", target.MessageID, got, migrated.MessageID)
				}
			}
			submission, err := SubmitText(context.Background(), deps, TextInput{
				ConversationID: conversation.legacyID,
				Body:           "reply",
				ReplyToID:      target.MessageID,
				IdempotencyKey: fmt.Sprintf("reply-%d", index),
			})
			if err != nil {
				t.Fatalf("SubmitText(reply): %v", err)
			}
			queued := mustGetV2Message(t, v2, submission.LocalMessageID)
			if queued.ReplyToRemoteID == nil || *queued.ReplyToRemoteID != migrated.RemoteMessageID {
				t.Fatalf("queued reply = %+v, want reply to remote %q", queued, migrated.RemoteMessageID)
			}
			// The migrated target plus the queued reply, and nothing else.
			if count := countV2Messages(t, v2, owner.ConversationID); count != 2 {
				t.Fatalf("v2 messages in the migrated thread = %d, want 2", count)
			}
		})
	}
}

// TestProjectorWritesAdoptedConversationSendsIntoTheLegacyThread covers the
// legacy visibility projector on an adopted conversation. The legacy upsert
// rewrites conversation_id on a message-id conflict, so projecting under the
// v2 hash would also pull an already-ingested echo out of its thread.
func TestProjectorWritesAdoptedConversationSendsIntoTheLegacyThread(t *testing.T) {
	for _, preinsertEcho := range []bool{false, true} {
		t.Run(fmt.Sprintf("echo first %v", preinsertEcho), func(t *testing.T) {
			legacy, v2 := openMigratedTestStores(t)
			conversation := migratedFixtureConversations[2] // Signal
			owner := migratedOwner(t, v2, conversation)
			if _, conversationID, err := MirrorConversation(legacy, v2, conversation.legacyID); err != nil || conversationID != owner.ConversationID {
				t.Fatalf("MirrorConversation() = %q, %v; want the migrated row %q", conversationID, err, owner.ConversationID)
			}
			clock := &projectorTestClock{now: time.UnixMilli(1_910_000_000_000)}
			outbox := newProjectorTestOutbox(t, v2, clock)
			confirmedAtMS := enqueueConfirmedProjectorMessage(t, v2, outbox, clock, projectorMessageSeed{
				OutboxID: "adopted-send", AccountID: signalAccountID, ConversationID: owner.ConversationID,
				Kind: sqlite.OutboxKindText, LocalMessageID: "adopted-local", RequestID: "adopted-request",
				RemoteID: "1910000000000", Body: "sent into a migrated thread", ReplyToRemoteID: "1700000000002",
			})
			want := &db.Message{
				MessageID: "signal:1910000000000", ConversationID: conversation.legacyID,
				Body: "sent into a migrated thread", TimestampMS: confirmedAtMS, Status: "sent",
				IsFromMe: true, ReplyToID: "signal:1700000000002", SourcePlatform: "signal", SourceID: "1910000000000",
			}
			if preinsertEcho {
				echo := *want
				echo.Status = "echo-arrived-first"
				if err := legacy.UpsertMessage(&echo); err != nil {
					t.Fatalf("UpsertMessage(echo): %v", err)
				}
			}

			events := &projectorTestEvents{}
			projector := Projector{V2Store: v2, Outbox: outbox, Legacy: legacy, Events: events, Logger: zerolog.Nop(), Now: clock.Now}
			cursor := newProjectorCursor(confirmedAtMS - 1)
			if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
				t.Fatalf("projectConfirmedSince(): %v", err)
			}
			got, err := legacy.GetMessageByID(want.MessageID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("projected legacy row = %+v, %v\nwant %+v", got, err, want)
			}
			if stray, err := legacy.GetMessagesByConversation(owner.ConversationID, 10); err != nil || len(stray) != 0 {
				t.Fatalf("legacy messages under the v2 hash = %+v, %v; want none", stray, err)
			}
			if len(events.conversationIDs) != 1 || events.conversationIDs[0] != conversation.legacyID {
				t.Fatalf("published message events = %v, want the legacy thread %q", events.conversationIDs, conversation.legacyID)
			}
		})
	}
}

// TestMirrorOnMigratedStoreProperties runs random mirror call sequences on a
// migrated store. Before the sequence, v2 ingest may already have keyed some
// post-cutover threads by derived hash, as it does when it runs beside a
// legacy-primary daemon, with or without the reply target under its v2 remote
// ID. After every call it checks that:
//   - each thread resolves to the one v2 row holding its natural key, whose
//     remote ID is the legacy ID, and the mirror keys a row by the legacy ID
//     only for a thread v2 had no row for;
//   - every row that existed before is byte-identical, as are accounts and
//     devices;
//   - a reply target resolves to the same v2 message every time, and v2 never
//     holds two copies of one legacy message;
//   - a read cursor lands on the migrated local device and the resolved row.
func TestMirrorOnMigratedStoreProperties(t *testing.T) {
	type thread struct {
		migratedFixtureConversation
		target *db.Message
	}
	postCutoverTargets := map[string]*db.Message{
		"google-after-cutover":                {MessageID: "google-after-1", Body: "google after", TimestampMS: 1_905_000_000_001},
		"whatsapp:15559990000@s.whatsapp.net": {MessageID: "whatsapp:wa-after-1", SourceID: "wa-after-1", Body: "whatsapp after", TimestampMS: 1_905_000_000_002},
		"signal:+15559990000":                 {MessageID: "signal:1905000000003", SourceID: "1905000000003", IsFromMe: true, Body: "signal after", TimestampMS: 1_905_000_000_003},
	}
	// v2RemoteID is how the migration and the v2 decoders key each target,
	// written out independently of the mirror's own derivation.
	v2RemoteID := func(target *db.Message) string {
		if target.SourceID != "" {
			return target.SourceID
		}
		return target.MessageID
	}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		legacy, v2 := openMigratedTestStores(t)
		ctx := context.Background()
		var threads []thread
		for _, conversation := range migratedFixtureConversations {
			targets, err := legacy.GetMessagesByConversation(conversation.legacyID, 10)
			if err != nil || len(targets) != 1 {
				t.Errorf("legacy fixture messages for %q = %+v, %v", conversation.legacyID, targets, err)
				return false
			}
			threads = append(threads, thread{conversation, targets[0]})
		}
		for _, conversation := range postCutoverFixtureConversations {
			target := *postCutoverTargets[conversation.legacyID]
			target.ConversationID = conversation.legacyID
			target.SourcePlatform = conversation.platform
			if err := legacy.UpsertMessage(&target); err != nil {
				t.Errorf("legacy UpsertMessage(%q): %v", target.MessageID, err)
				return false
			}
			threads = append(threads, thread{conversation, &target})
			if random.Intn(2) == 0 {
				continue
			}
			ingestedID := v2keys.DeriveID("conversation", conversation.accountID, conversation.legacyID)
			if err := v2.UpsertConversation(sqlite.Conversation{
				ConversationID: ingestedID, AccountID: conversation.accountID,
				RemoteConversationID: conversation.legacyID, Kind: sqlite.ConversationKindDirect,
				Title: "ingested", NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
				CreatedAtMS: 1_905_000_000_000, UpdatedAtMS: 1_905_000_000_000,
			}); err != nil {
				t.Errorf("seed ingested conversation %q: %v", conversation.legacyID, err)
				return false
			}
			if random.Intn(2) == 0 {
				direction := sqlite.MessageDirectionIncoming
				if target.IsFromMe {
					direction = sqlite.MessageDirectionOutgoing
				}
				projectV2TestMessage(t, v2, sqlite.Message{
					MessageID: "ingested:" + target.MessageID, ConversationID: ingestedID,
					AccountID: conversation.accountID, RemoteMessageID: v2RemoteID(&target),
					Direction: direction, Body: target.Body, State: sqlite.MessageStateActive,
					OccurredAtMS: target.TimestampMS,
				})
			}
		}

		rowsBefore := map[string]sqlite.Conversation{}
		accountsBefore := map[string]sqlite.Account{}
		devicesBefore := map[string][]sqlite.Device{}
		for _, thread := range threads {
			if row, err := v2.GetConversationByRemote(thread.accountID, thread.legacyID); err == nil {
				rowsBefore[thread.legacyID] = row
			} else if !errors.Is(err, sqlite.ErrNotFound) {
				t.Errorf("GetConversationByRemote(%q): %v", thread.legacyID, err)
				return false
			}
			accountsBefore[thread.accountID] = mustGetAccount(t, v2, thread.accountID)
			devicesBefore[thread.accountID] = mustListDevices(t, v2, thread.accountID)
		}

		resolvedTargets := map[string]string{}
		readAtMS := int64(1_910_000_000_000)
		for step := range 1 + random.Intn(10) {
			thread := threads[random.Intn(len(threads))]
			switch random.Intn(3) {
			case 0:
				accountID, conversationID, err := MirrorConversation(legacy, v2, thread.legacyID)
				if err != nil || accountID != thread.accountID {
					t.Errorf("seed %d step %d: MirrorConversation(%q) = %q, %v", seed, step, thread.legacyID, accountID, err)
					return false
				}
				if owner, err := v2.GetConversationByRemote(thread.accountID, thread.legacyID); err != nil || owner.ConversationID != conversationID {
					t.Errorf("seed %d step %d: MirrorConversation(%q) = %q, natural key owner %+v, %v", seed, step, thread.legacyID, conversationID, owner, err)
					return false
				}
			case 1:
				readAtMS += int64(1 + random.Intn(1000))
				if err := MirrorReadCursor(ctx, legacy, v2, thread.legacyID, readAtMS); err != nil {
					t.Errorf("seed %d step %d: MirrorReadCursor(%q): %v", seed, step, thread.legacyID, err)
					return false
				}
				device, err := v2.GetLocalInstallationDevice(ctx, thread.accountID)
				if err != nil {
					t.Errorf("seed %d: GetLocalInstallationDevice(%q): %v", seed, thread.accountID, err)
					return false
				}
				owner, err := v2.GetConversationByRemote(thread.accountID, thread.legacyID)
				if err != nil {
					t.Errorf("seed %d: GetConversationByRemote(%q): %v", seed, thread.legacyID, err)
					return false
				}
				if cursor, err := v2.GetReadCursor(device.DeviceID, owner.ConversationID); err != nil || cursor.LastReadAtMS != readAtMS {
					t.Errorf("seed %d step %d: cursor for %q on (%q, %q) = %+v, %v; want read at %d",
						seed, step, thread.legacyID, device.DeviceID, owner.ConversationID, cursor, err, readAtMS)
					return false
				}
			case 2:
				messageID, err := MirrorReplyTarget(legacy, v2, thread.target.MessageID)
				if err != nil {
					t.Errorf("seed %d step %d: MirrorReplyTarget(%q): %v", seed, step, thread.target.MessageID, err)
					return false
				}
				if earlier, ok := resolvedTargets[thread.target.MessageID]; ok && earlier != messageID {
					t.Errorf("seed %d step %d: MirrorReplyTarget(%q) = %q, earlier %q", seed, step, thread.target.MessageID, messageID, earlier)
					return false
				}
				resolvedTargets[thread.target.MessageID] = messageID
			}

			for _, checked := range threads {
				owner, err := v2.GetConversationByRemote(checked.accountID, checked.legacyID)
				before, existed := rowsBefore[checked.legacyID]
				switch {
				case existed && (err != nil || !reflect.DeepEqual(owner, before)):
					t.Errorf("seed %d step %d: row for %q = %+v, %v; want untouched %+v", seed, step, checked.legacyID, owner, err, before)
					return false
				case !existed && err == nil && owner.ConversationID != checked.legacyID:
					t.Errorf("seed %d step %d: new row for %q keyed %q, want the legacy id", seed, step, checked.legacyID, owner.ConversationID)
					return false
				case !existed && err != nil && !errors.Is(err, sqlite.ErrNotFound):
					t.Errorf("seed %d step %d: GetConversationByRemote(%q): %v", seed, step, checked.legacyID, err)
					return false
				}
				if err != nil {
					continue
				}
				copies := 0
				for _, message := range listV2Messages(t, v2, owner.ConversationID) {
					if message.OccurredAtMS == checked.target.TimestampMS && message.Body == checked.target.Body {
						copies++
					}
				}
				if copies > 1 {
					t.Errorf("seed %d step %d: v2 holds %d copies of %q", seed, step, copies, checked.target.MessageID)
					return false
				}
				if !reflect.DeepEqual(mustGetAccount(t, v2, checked.accountID), accountsBefore[checked.accountID]) ||
					!reflect.DeepEqual(mustListDevices(t, v2, checked.accountID), devicesBefore[checked.accountID]) {
					t.Errorf("seed %d step %d: account or devices for %q changed", seed, step, checked.accountID)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 30, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

func mustGetV2Message(t *testing.T, store *sqlite.Store, messageID string) sqlite.Message {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	message, err := repository.GetMessage(context.Background(), messageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", messageID, err)
	}
	return message
}

// TestAccountBootstrapMatchesMigrationAccounts is a differential check: an
// account the mirror creates must read back exactly like the one the
// migration creates, since v2 reads derive platform from the bridge key.
func TestAccountBootstrapMatchesMigrationAccounts(t *testing.T) {
	_, v2 := openMigratedTestStores(t)
	for _, accountID := range []string{googleAccountID, whatsappAccountID, signalAccountID} {
		migrated := mustGetAccount(t, v2, accountID)
		bridgeKey, displayName := accountBootstrap(accountID)
		if bridgeKey != migrated.BridgeKey || displayName != migrated.DisplayName {
			t.Errorf("accountBootstrap(%q) = (%q, %q), migration wrote (%q, %q)",
				accountID, bridgeKey, displayName, migrated.BridgeKey, migrated.DisplayName)
		}
	}
}

func TestMirrorConversationKeepsExistingAccountMetadata(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat", "sms", false)
	bootstrapped := sqlite.Account{
		AccountID:   googleAccountID,
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     false,
		ConfigJSON:  `{"kept":true}`,
		CreatedAtMS: 1_800_000_000_000,
		UpdatedAtMS: 1_800_000_000_000,
	}
	if err := v2.UpsertAccount(bootstrapped); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if _, _, err := MirrorConversation(legacy, v2, "google-chat"); err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	if got := mustGetAccount(t, v2, googleAccountID); !reflect.DeepEqual(got, bootstrapped) {
		t.Fatalf("account = %+v, want unchanged %+v", got, bootstrapped)
	}
}

// TestMirrorReadCursorDeviceProperties runs random mirror sequences over random
// pre-existing account and device shapes and checks after every call that:
//   - each account's pre-existing devices are unchanged, and the only device
//     the mirror ever adds is localDeviceID, only for an account with no local
//     installation device;
//   - the cursor lands on the device GetLocalInstallationDevice resolves, the
//     one ingest advances receipts for;
//   - a pre-existing account row is never modified.
func TestMirrorReadCursorDeviceProperties(t *testing.T) {
	accounts := []string{googleAccountID, whatsappAccountID, signalAccountID}
	conversationFor := map[string]migratedFixtureConversation{}
	for _, conversation := range postCutoverFixtureConversations {
		conversationFor[conversation.accountID] = conversation
	}
	const (
		shapeNoAccount = iota
		shapeAccountOnly
		shapeMigratedDevice
		shapeNonCurrentLocal
		shapeCount
	)
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		legacy := openLegacyTestStore(t)
		v2 := openV2TestStore(t)
		ctx := context.Background()
		accountsBefore := map[string]sqlite.Account{}
		devicesBefore := map[string][]sqlite.Device{}
		hadLocal := map[string]bool{}
		for _, accountID := range accounts {
			conversation := conversationFor[accountID]
			seedLegacyConversation(t, legacy, conversation.legacyID, conversation.platform, false)
			shape := random.Intn(shapeCount)
			if shape != shapeNoAccount {
				bridgeKey, displayName := accountBootstrap(accountID)
				account := sqlite.Account{
					AccountID: accountID, BridgeKey: bridgeKey, DisplayName: displayName,
					Mode: sqlite.AccountModeLive, Enabled: random.Intn(2) == 0,
					ConfigJSON: fmt.Sprintf(`{"seed":%d}`, seed), CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
				}
				if err := v2.UpsertAccount(account); err != nil {
					t.Errorf("UpsertAccount(%q): %v", accountID, err)
					return false
				}
				accountsBefore[accountID] = account
			}
			if shape == shapeMigratedDevice || shape == shapeNonCurrentLocal {
				device := sqlite.Device{
					DeviceID: v2keys.DeriveID("device", accountID, accountID+"\x1flocal"), AccountID: accountID,
					Kind: sqlite.DeviceKindLocalInstallation, DisplayName: "OpenMessage", State: sqlite.DeviceStateActive,
					IsCurrent: shape == shapeMigratedDevice, CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
				}
				if err := v2.UpsertDevice(device); err != nil {
					t.Errorf("UpsertDevice(%q): %v", device.DeviceID, err)
					return false
				}
				hadLocal[accountID] = true
			}
			devicesBefore[accountID] = mustListDevices(t, v2, accountID)
		}

		readAtMS := int64(1_910_000_000_000)
		for step := range 1 + random.Intn(6) {
			accountID := accounts[random.Intn(len(accounts))]
			readAtMS += int64(1 + random.Intn(1000))
			if err := MirrorReadCursor(ctx, legacy, v2, conversationFor[accountID].legacyID, readAtMS); err != nil {
				t.Errorf("seed %d step %d: MirrorReadCursor(%q): %v", seed, step, accountID, err)
				return false
			}
			device, err := v2.GetLocalInstallationDevice(ctx, accountID)
			if err != nil {
				t.Errorf("seed %d: GetLocalInstallationDevice(%q): %v", seed, accountID, err)
				return false
			}
			cursor, err := v2.GetReadCursor(device.DeviceID, conversationFor[accountID].legacyID)
			if err != nil || cursor.LastReadAtMS != readAtMS {
				t.Errorf("seed %d: cursor on %q = %+v, %v; want read at %d", seed, device.DeviceID, cursor, err, readAtMS)
				return false
			}
			for _, checked := range accounts {
				got := mustListDevices(t, v2, checked)
				want := devicesBefore[checked]
				if added := len(got) - len(want); added != 0 {
					if hadLocal[checked] || added != 1 || !containsDevice(got, localDeviceID(checked)) {
						t.Errorf("seed %d: devices for %q = %+v, before %+v", seed, checked, got, want)
						return false
					}
					got = withoutDevice(got, localDeviceID(checked))
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("seed %d: pre-existing devices for %q changed: %+v, want %+v", seed, checked, got, want)
					return false
				}
				if before, ok := accountsBefore[checked]; ok {
					if after := mustGetAccount(t, v2, checked); !reflect.DeepEqual(after, before) {
						t.Errorf("seed %d: account %q = %+v, want unchanged %+v", seed, checked, after, before)
						return false
					}
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

func mustGetAccount(t *testing.T, store *sqlite.Store, accountID string) sqlite.Account {
	t.Helper()
	account, err := store.GetAccount(accountID)
	if err != nil {
		t.Fatalf("GetAccount(%q): %v", accountID, err)
	}
	return account
}

func mustListDevices(t *testing.T, store *sqlite.Store, accountID string) []sqlite.Device {
	t.Helper()
	devices, err := store.ListDevices(accountID)
	if err != nil {
		t.Fatalf("ListDevices(%q): %v", accountID, err)
	}
	return devices
}

func containsDevice(devices []sqlite.Device, deviceID string) bool {
	for _, device := range devices {
		if device.DeviceID == deviceID {
			return true
		}
	}
	return false
}

func withoutDevice(devices []sqlite.Device, deviceID string) []sqlite.Device {
	kept := make([]sqlite.Device, 0, len(devices))
	for _, device := range devices {
		if device.DeviceID != deviceID {
			kept = append(kept, device)
		}
	}
	return kept
}
