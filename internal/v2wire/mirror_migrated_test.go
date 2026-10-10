package v2wire

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// migratedFixtureConversation is a legacy conversation present when the
// fixture store was migrated, so the v2 store holds it under a derived ID.
// platform is the legacy stored platform; accountID is the account the v2 row
// lives under (for a migrated thread, the account the migration chose).
type migratedFixtureConversation struct {
	legacyID  string
	platform  string
	accountID string
}

var migratedFixtureConversations = []migratedFixtureConversation{
	{legacyID: "google-migrated", platform: "sms", accountID: googleAccountID},
	{legacyID: "whatsapp:15550002222@s.whatsapp.net", platform: "whatsapp", accountID: whatsappAccountID},
	{legacyID: "signal:+15550003333", platform: "signal", accountID: signalAccountID},
	// The migration normalizes this to remote ID "signal:+16505550100"; the
	// mirror must meet that row rather than mint one keyed by the spaced form.
	{legacyID: "signal:  +16505550100", platform: "signal", accountID: signalAccountID},
	// Stored as sms, so the migration files it under Google by stored platform,
	// while the mirror routes it to Signal by its prefix.
	{legacyID: "signal:  +16505550123", platform: "sms", accountID: googleAccountID},
}

// postCutoverFixtureConversations exist only in the legacy store; the migrated
// v2 store has never seen them.
var postCutoverFixtureConversations = []migratedFixtureConversation{
	{legacyID: "google-after-cutover", platform: "sms", accountID: googleAccountID},
	{legacyID: "whatsapp:15559990000@s.whatsapp.net", platform: "whatsapp", accountID: whatsappAccountID},
	{legacyID: "signal:+15559990000", platform: "signal", accountID: signalAccountID},
	{legacyID: "signal:  +16505559999", platform: "signal", accountID: signalAccountID},
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
		{MessageID: "signal:1700000000003", SourceID: "1700000000003", ConversationID: "signal:  +16505550100", SenderNumber: "+16505550100", Body: "spaced signal", TimestampMS: baseMS + 3, SourcePlatform: "signal"},
		{MessageID: "google-message-misrouted", ConversationID: "signal:  +16505550123", SenderNumber: "+16505550123", Body: "stored as sms", TimestampMS: baseMS + 4, SourcePlatform: "sms"},
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
			mirrored, err := v2.GetConversation(conversation.legacyID)
			if err != nil {
				t.Fatalf("GetConversation(): %v", err)
			}
			if want := v2keys.NormalizeRemoteConversationID(conversation.platform, conversation.legacyID); mirrored.RemoteConversationID != want {
				t.Fatalf("mirrored remote ID = %q, want the migration's normalized %q", mirrored.RemoteConversationID, want)
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

func TestMirrorOnMigratedStoreRefusesMigratedConversationWithoutWriting(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	ctx := context.Background()
	for _, conversation := range migratedFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			remoteID := v2keys.NormalizeRemoteConversationID(conversation.platform, conversation.legacyID)
			owner, err := v2.GetConversationByRemote(conversation.accountID, remoteID)
			if err != nil {
				t.Fatalf("GetConversationByRemote(): %v", err)
			}
			if owner.ConversationID == conversation.legacyID {
				t.Fatalf("migrated conversation kept legacy ID %q; fixture no longer exercises a derived key", owner.ConversationID)
			}
			// Give the v2 row state the legacy mirror would overwrite.
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
			cursorBefore, cursorErrBefore := v2.GetReadCursor(device.DeviceID, owner.ConversationID)

			_, _, mirrorErr := MirrorConversation(legacy, v2, conversation.legacyID)
			cursorErr := MirrorReadCursor(ctx, legacy, v2, conversation.legacyID, 1_910_000_000_000)
			for name, err := range map[string]error{"MirrorConversation": mirrorErr, "MirrorReadCursor": cursorErr} {
				if !errors.Is(err, sqlite.ErrConversationIdentityConflict) || !strings.Contains(err.Error(), owner.ConversationID) {
					t.Fatalf("%s error = %v, want ErrConversationIdentityConflict naming %q", name, err, owner.ConversationID)
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
				t.Fatalf("GetConversation(legacy id) error = %v, want ErrNotFound", err)
			}
			if got := mustGetAccount(t, v2, conversation.accountID); !reflect.DeepEqual(got, accountBefore) {
				t.Fatalf("account = %+v, want unchanged %+v", got, accountBefore)
			}
			if got := mustListDevices(t, v2, conversation.accountID); !reflect.DeepEqual(got, devicesBefore) {
				t.Fatalf("devices = %+v, want unchanged %+v", got, devicesBefore)
			}
			cursorAfter, cursorErrAfter := v2.GetReadCursor(device.DeviceID, owner.ConversationID)
			if !reflect.DeepEqual(cursorAfter, cursorBefore) || (cursorErrAfter == nil) != (cursorErrBefore == nil) {
				t.Fatalf("read cursor = (%+v, %v), want unchanged (%+v, %v)", cursorAfter, cursorErrAfter, cursorBefore, cursorErrBefore)
			}
		})
	}
}

func TestSubmitTextOnMigratedStoreUsesMirrorWithoutDeviceCollision(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true},
	}}
	deps := Deps{Legacy: legacy, V2: v2, Service: newSubmitTestService(t, v2, registry, nil), Registry: registry}

	submission, err := SubmitText(context.Background(), deps, TextInput{
		ConversationID: "google-after-cutover",
		Body:           "sent after cutover",
		IdempotencyKey: "migrated-store-send",
	})
	if err != nil {
		t.Fatalf("SubmitText(post-cutover conversation): %v", err)
	}
	if submission.LocalMessageID == "" {
		t.Fatalf("submission = %+v, want a queued local message", submission)
	}

	_, err = SubmitText(context.Background(), deps, TextInput{
		ConversationID: "google-migrated",
		Body:           "sent to a migrated thread",
		IdempotencyKey: "migrated-thread-send",
	})
	if !errors.Is(err, sqlite.ErrConversationIdentityConflict) {
		t.Fatalf("SubmitText(migrated conversation) error = %v, want ErrConversationIdentityConflict", err)
	}
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

// TestMirrorNeverChangesLegacyIDReads is a differential check against v2 reads:
// a legacy ID must resolve to the same history before and after the mirror
// runs on it. A mirror row created under the legacy ID beside a migrated row
// would win v2read's direct-ID lookup and hide the migrated messages.
func TestMirrorNeverChangesLegacyIDReads(t *testing.T) {
	legacy, v2 := openMigratedTestStores(t)
	reader := v2read.New(v2)
	read := func(legacyID string) []string {
		t.Helper()
		messages, err := reader.GetMessagesByConversation(legacyID, 50)
		if err != nil {
			t.Fatalf("v2read GetMessagesByConversation(%q): %v", legacyID, err)
		}
		ids := make([]string, 0, len(messages))
		for _, message := range messages {
			ids = append(ids, message.MessageID)
		}
		return ids
	}
	for _, conversation := range migratedFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			before := read(conversation.legacyID)
			if len(before) == 0 {
				t.Fatalf("legacy ID %q reads no migrated history; the fixture no longer exercises shadowing", conversation.legacyID)
			}
			_, _, mirrorErr := MirrorConversation(legacy, v2, conversation.legacyID)
			cursorErr := MirrorReadCursor(context.Background(), legacy, v2, conversation.legacyID, 1_910_000_000_000)
			for name, err := range map[string]error{"MirrorConversation": mirrorErr, "MirrorReadCursor": cursorErr} {
				if !errors.Is(err, sqlite.ErrConversationIdentityConflict) {
					t.Fatalf("%s error = %v, want ErrConversationIdentityConflict", name, err)
				}
			}
			if after := read(conversation.legacyID); !reflect.DeepEqual(after, before) {
				t.Fatalf("legacy-ID read = %v after mirroring, want unchanged %v", after, before)
			}
		})
	}
}

// TestConversationPlacementMatchesMigration is a differential check: the
// placement rule the mirror asks the migration for must name the row the real
// migration wrote, for every fixture thread, including the one whose stored
// platform disagrees with its ID prefix.
func TestConversationPlacementMatchesMigration(t *testing.T) {
	_, v2 := openMigratedTestStores(t)
	for _, conversation := range migratedFixtureConversations {
		accountID, remoteID, ok := migration.ConversationPlacement(conversation.platform, conversation.legacyID)
		if !ok || accountID != conversation.accountID {
			t.Fatalf("ConversationPlacement(%q, %q) = (%q, %q, %v), want account %q",
				conversation.platform, conversation.legacyID, accountID, remoteID, ok, conversation.accountID)
		}
		row, err := v2.GetConversationByRemote(accountID, remoteID)
		if err != nil {
			t.Fatalf("migrated row for %q at (%q, %q): %v", conversation.legacyID, accountID, remoteID, err)
		}
		if want := v2keys.DeriveID("conversation", accountID, conversation.legacyID); row.ConversationID != want {
			t.Fatalf("migrated row for %q has ID %q, want %q", conversation.legacyID, row.ConversationID, want)
		}
	}
	if _, _, ok := migration.ConversationPlacement("telegram", "telegram:1"); ok {
		t.Fatal("ConversationPlacement(telegram) ok = true, want false for a platform the migration rejects")
	}
}

// TestMirrorConversationIgnoresUnrelatedThreadInAnotherAccount pins that only
// the routed key and the migration's placement are checked. Google thread IDs
// are opaque, so a second account can hold an unrelated thread under the same
// remote ID; that must not block the first account's sends or mark-read.
func TestMirrorConversationIgnoresUnrelatedThreadInAnotherAccount(t *testing.T) {
	for _, order := range []string{"mirror first", "other account first"} {
		t.Run(order, func(t *testing.T) {
			legacy := openLegacyTestStore(t)
			v2 := openV2TestStore(t)
			const legacyID = "2873"
			seedLegacyConversation(t, legacy, legacyID, "sms", false)
			registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{googleAccountID: {TextSend: true}}}
			deps := Deps{Legacy: legacy, V2: v2, Service: newSubmitTestService(t, v2, registry, nil), Registry: registry}
			seedOther := func() sqlite.Conversation {
				t.Helper()
				if _, err := v2.EnsureAccount(sqlite.Account{
					AccountID: "google-secondary", BridgeKey: "google_messages", DisplayName: "Second phone",
					Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
					CreatedAtMS: 1_700_000_000_000, UpdatedAtMS: 1_700_000_000_000,
				}); err != nil {
					t.Fatalf("EnsureAccount(google-secondary): %v", err)
				}
				other := sqlite.Conversation{
					ConversationID: "secondary-thread", AccountID: "google-secondary", RemoteConversationID: legacyID,
					Kind: sqlite.ConversationKindDirect, Title: "unrelated", NotificationMode: sqlite.NotificationModeAll,
					MetadataJSON: "{}", CreatedAtMS: 1_700_000_000_000, UpdatedAtMS: 1_700_000_000_000,
				}
				if err := v2.UpsertConversation(other); err != nil {
					t.Fatalf("UpsertConversation(secondary-thread): %v", err)
				}
				return other
			}

			var other sqlite.Conversation
			if order == "other account first" {
				other = seedOther()
			}
			if _, _, err := MirrorConversation(legacy, v2, legacyID); err != nil {
				t.Fatalf("MirrorConversation(first): %v", err)
			}
			if order == "mirror first" {
				other = seedOther()
			}

			if _, _, err := MirrorConversation(legacy, v2, legacyID); err != nil {
				t.Fatalf("MirrorConversation(refresh): %v", err)
			}
			if err := MirrorReadCursor(context.Background(), legacy, v2, legacyID, 1_910_000_000_000); err != nil {
				t.Fatalf("MirrorReadCursor(): %v", err)
			}
			if _, err := SubmitText(context.Background(), deps, TextInput{
				ConversationID: legacyID, Body: "hi", IdempotencyKey: "unrelated-thread-send",
			}); err != nil {
				t.Fatalf("SubmitText(): %v", err)
			}
			mirrored, err := v2.GetConversation(legacyID)
			if err != nil || mirrored.AccountID != googleAccountID || mirrored.RemoteConversationID != legacyID {
				t.Fatalf("mirrored row = %+v, %v; want the google-primary thread", mirrored, err)
			}
			if got, err := v2.GetConversation(other.ConversationID); err != nil || !reflect.DeepEqual(got, other) {
				t.Fatalf("other account's thread = %+v, %v; want unchanged %+v", got, err, other)
			}
		})
	}
}

// TestMirrorConversationRefusesDisplacedBinding covers ingest re-keying a stale
// Google binding: the row under the legacy ID keeps the ID but its remote key
// becomes "displaced:…", and a fresh row may take the key. The mirror must not
// adopt the displaced key (dispatch would send to it) nor touch the fresh row.
func TestMirrorConversationRefusesDisplacedBinding(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	const legacyID = "2873"
	seedLegacyConversation(t, legacy, legacyID, "sms", false)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{googleAccountID: {TextSend: true}}}
	deps := Deps{Legacy: legacy, V2: v2, Service: newSubmitTestService(t, v2, registry, nil), Registry: registry}
	if _, _, err := MirrorConversation(legacy, v2, legacyID); err != nil {
		t.Fatalf("MirrorConversation(first): %v", err)
	}
	if displaced, err := v2.DisplaceConversationRemoteID(googleAccountID, legacyID, 1_800_000_000_000); err != nil || !displaced {
		t.Fatalf("DisplaceConversationRemoteID() = %v, %v; want displaced", displaced, err)
	}
	displacedRow, err := v2.GetConversation(legacyID)
	if err != nil || displacedRow.RemoteConversationID == legacyID {
		t.Fatalf("displaced row = %+v, %v; want a rewritten remote key", displacedRow, err)
	}
	submit := func(key string) error {
		_, err := SubmitText(context.Background(), deps, TextInput{ConversationID: legacyID, Body: "hi", IdempotencyKey: key})
		return err
	}

	// No row holds the key yet: the legacy-ID row is keyed elsewhere.
	if err := submit("displaced-unowned"); !errors.Is(err, sqlite.ErrConversationIdentityConflict) {
		t.Fatalf("SubmitText(displaced, unowned key) error = %v, want ErrConversationIdentityConflict", err)
	}

	fresh := sqlite.Conversation{
		ConversationID: v2keys.DeriveID("conversation", googleAccountID, legacyID), AccountID: googleAccountID,
		RemoteConversationID: legacyID, Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", CreatedAtMS: 1_800_000_000_001, UpdatedAtMS: 1_800_000_000_001,
	}
	if err := v2.UpsertConversation(fresh); err != nil {
		t.Fatalf("UpsertConversation(fresh binding): %v", err)
	}
	if err := submit("displaced-owned"); !errors.Is(err, sqlite.ErrConversationIdentityConflict) ||
		!strings.Contains(err.Error(), fresh.ConversationID) {
		t.Fatalf("SubmitText(displaced, re-bound key) error = %v, want ErrConversationIdentityConflict naming %q", err, fresh.ConversationID)
	}
	if got, err := v2.GetConversation(legacyID); err != nil || !reflect.DeepEqual(got, displacedRow) {
		t.Fatalf("displaced row = %+v, %v; want unchanged %+v", got, err, displacedRow)
	}
	if got, err := v2.GetConversation(fresh.ConversationID); err != nil || !reflect.DeepEqual(got, fresh) {
		t.Fatalf("fresh row = %+v, %v; want unchanged %+v", got, err, fresh)
	}
}

// TestMirrorConversationRefusesEarlierRawRowBesideNormalizedOwner covers a store
// where an earlier mirror keyed a spaced Signal thread by its raw ID and
// another row now holds the normalized key. Refreshing the raw row would keep
// it shadowing that row for legacy-ID reads.
func TestMirrorConversationRefusesEarlierRawRowBesideNormalizedOwner(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	const spaced = "signal:  +16505550100"
	seedLegacyConversation(t, legacy, spaced, "signal", false)
	bridgeKey, displayName := accountBootstrap(signalAccountID)
	if _, err := v2.EnsureAccount(sqlite.Account{
		AccountID: signalAccountID, BridgeKey: bridgeKey, DisplayName: displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: 1_700_000_000_000, UpdatedAtMS: 1_700_000_000_000,
	}); err != nil {
		t.Fatalf("EnsureAccount(): %v", err)
	}
	earlier := sqlite.Conversation{
		ConversationID: spaced, AccountID: signalAccountID, RemoteConversationID: spaced,
		Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", CreatedAtMS: 1_700_000_000_000, UpdatedAtMS: 1_700_000_000_000,
	}
	owner := earlier
	owner.ConversationID = v2keys.DeriveID("conversation", signalAccountID, "signal:+16505550100")
	owner.RemoteConversationID = "signal:+16505550100"
	for _, row := range []sqlite.Conversation{earlier, owner} {
		if err := v2.UpsertConversation(row); err != nil {
			t.Fatalf("UpsertConversation(%q): %v", row.ConversationID, err)
		}
	}

	_, _, err := MirrorConversation(legacy, v2, spaced)
	if !errors.Is(err, sqlite.ErrConversationIdentityConflict) || !strings.Contains(err.Error(), owner.ConversationID) {
		t.Fatalf("MirrorConversation() error = %v, want ErrConversationIdentityConflict naming %q", err, owner.ConversationID)
	}
	for _, want := range []sqlite.Conversation{earlier, owner} {
		if got, err := v2.GetConversation(want.ConversationID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("row %q = %+v, %v; want unchanged %+v", want.ConversationID, got, err, want)
		}
	}
}

// TestMirrorConversationKeepsEarlierRawRemoteID covers a store an earlier
// mirror wrote, which keyed rows by the raw legacy ID. Normalizing that key
// now would collide with the row's own primary key.
func TestMirrorConversationKeepsEarlierRawRemoteID(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	const spaced = "signal:  +16505550100"
	seedLegacyConversation(t, legacy, spaced, "signal", false)
	bridgeKey, displayName := accountBootstrap(signalAccountID)
	if _, err := v2.EnsureAccount(sqlite.Account{
		AccountID: signalAccountID, BridgeKey: bridgeKey, DisplayName: displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	}); err != nil {
		t.Fatalf("EnsureAccount(): %v", err)
	}
	if err := v2.UpsertConversation(sqlite.Conversation{
		ConversationID: spaced, AccountID: signalAccountID, RemoteConversationID: spaced,
		Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", CreatedAtMS: 1_700_000_000_000, UpdatedAtMS: 1_700_000_000_000,
	}); err != nil {
		t.Fatalf("UpsertConversation(earlier mirror row): %v", err)
	}

	if err := MirrorReadCursor(context.Background(), legacy, v2, spaced, 1_910_000_000_000); err != nil {
		t.Fatalf("MirrorReadCursor(): %v", err)
	}
	got, err := v2.GetConversation(spaced)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if got.RemoteConversationID != spaced || got.Title != "Conversation "+spaced {
		t.Fatalf("conversation = %+v, want the earlier row refreshed under its raw remote ID", got)
	}
	if _, err := v2.GetConversationByRemote(signalAccountID, "signal:+16505550100"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetConversationByRemote(normalized) error = %v, want ErrNotFound: a second row was minted", err)
	}
}

// TestMirrorConversationRefusalWritesNothing pins that the ownership check runs
// before the account and device bootstraps: an account whose local device is
// missing must not gain one from a call that is then refused.
func TestMirrorConversationRefusalWritesNothing(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-thread", "sms", false)
	bridgeKey, displayName := accountBootstrap(googleAccountID)
	account, err := v2.EnsureAccount(sqlite.Account{
		AccountID: googleAccountID, BridgeKey: bridgeKey, DisplayName: displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	})
	if err != nil {
		t.Fatalf("EnsureAccount(): %v", err)
	}
	owner := sqlite.Conversation{
		ConversationID: v2keys.DeriveID("conversation", googleAccountID, "google-thread"),
		AccountID:      googleAccountID, RemoteConversationID: "google-thread",
		Kind: sqlite.ConversationKindDirect, Title: "ingested", NotificationMode: sqlite.NotificationModeMuted,
		MetadataJSON: "{}", CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	}
	if err := v2.UpsertConversation(owner); err != nil {
		t.Fatalf("UpsertConversation(owner): %v", err)
	}

	_, _, err = MirrorConversation(legacy, v2, "google-thread")
	if !errors.Is(err, sqlite.ErrConversationIdentityConflict) || !strings.Contains(err.Error(), owner.ConversationID) {
		t.Fatalf("MirrorConversation() error = %v, want ErrConversationIdentityConflict naming %q", err, owner.ConversationID)
	}
	if devices := mustListDevices(t, v2, googleAccountID); len(devices) != 0 {
		t.Fatalf("devices = %+v, want none: the refused call bootstrapped a device", devices)
	}
	if got := mustGetAccount(t, v2, googleAccountID); !reflect.DeepEqual(got, account) {
		t.Fatalf("account = %+v, want unchanged %+v", got, account)
	}
	if got, err := v2.GetConversation(owner.ConversationID); err != nil || !reflect.DeepEqual(got, owner) {
		t.Fatalf("owner = %+v, %v; want unchanged %+v", got, err, owner)
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
