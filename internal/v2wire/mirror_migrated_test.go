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
