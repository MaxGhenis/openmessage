package v2wire

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// TestMarkReadV2OnMigratedStoreWritesDerivedDeviceCursor is the v2-primary
// shape of the live install: the migration keyed every conversation and each
// account's local device by hash. Marking a thread read by its v2 id or by
// the legacy id it had before cutover writes the same cursor, on the derived
// device, and touches nothing else.
func TestMarkReadV2OnMigratedStoreWritesDerivedDeviceCursor(t *testing.T) {
	_, v2 := openMigratedTestStores(t)
	ctx := context.Background()
	for _, conversation := range migratedFixtureConversations {
		t.Run(conversation.legacyID, func(t *testing.T) {
			owner := mustMigratedConversation(t, v2, conversation)
			deviceID := v2keys.LocalInstallationDeviceID(conversation.accountID)
			migratedCursor, err := v2.GetReadCursor(deviceID, owner.ConversationID)
			if err != nil {
				t.Fatalf("migrated GetReadCursor(): %v", err)
			}
			if migratedCursor.LastReadMessageID == nil {
				t.Fatalf("migrated cursor = %+v; fixture no longer starts from a positioned cursor", migratedCursor)
			}
			before := snapshotV2(t, v2)

			readAtMS := int64(1_910_000_000_000)
			for _, key := range []string{owner.ConversationID, conversation.legacyID, " " + conversation.legacyID + "\t"} {
				readAtMS += 1_000
				if err := MarkReadV2(ctx, v2, key, readAtMS); err != nil {
					t.Fatalf("MarkReadV2(%q): %v", key, err)
				}
				cursor, err := v2.GetReadCursor(deviceID, owner.ConversationID)
				if err != nil {
					t.Fatalf("GetReadCursor(derived device) after %q: %v", key, err)
				}
				want := sqlite.ReadCursor{
					AccountID: conversation.accountID, DeviceID: deviceID, ConversationID: owner.ConversationID,
					LastReadAtMS: readAtMS, UpdatedAtMS: readAtMS,
				}
				if !reflect.DeepEqual(cursor, want) {
					t.Fatalf("cursor after %q = %+v, want %+v", key, cursor, want)
				}
			}

			after := snapshotV2(t, v2)
			delete(after.cursors, cursorKey{deviceID, owner.ConversationID})
			delete(before.cursors, cursorKey{deviceID, owner.ConversationID})
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("v2 rows besides the cursor changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if _, err := v2.GetDevice(localDeviceID(conversation.accountID)); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("GetDevice(%q) error = %v, want ErrNotFound", localDeviceID(conversation.accountID), err)
			}
		})
	}
}

// TestMarkReadV2UnknownConversationWritesNothing covers ids the v2 store does
// not know, including legacy ids of threads that only reached the legacy
// store after cutover. The legacy mirror would create those threads; the
// native path refuses without writing.
func TestMarkReadV2UnknownConversationWritesNothing(t *testing.T) {
	_, v2 := openMigratedTestStores(t)
	ctx := context.Background()
	before := snapshotV2(t, v2)
	keys := []string{"", "   ", "not-a-conversation", v2keys.DeriveID("conversation", googleAccountID, "nowhere")}
	for _, conversation := range postCutoverFixtureConversations {
		keys = append(keys, conversation.legacyID)
	}
	for _, key := range keys {
		if err := MarkReadV2(ctx, v2, key, 1_910_000_000_000); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("MarkReadV2(%q) error = %v, want ErrNotFound", key, err)
		}
	}
	if after := snapshotV2(t, v2); !reflect.DeepEqual(after, before) {
		t.Fatalf("v2 rows changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestMarkReadV2RejectsInvalidInputWithoutWriting(t *testing.T) {
	_, v2 := openMigratedTestStores(t)
	owner := mustMigratedConversation(t, v2, migratedFixtureConversations[0])
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := snapshotV2(t, v2)
	for name, call := range map[string]func() error{
		"nil context":        func() error { return MarkReadV2(nil, v2, owner.ConversationID, 1_910_000_000_000) },
		"canceled context":   func() error { return MarkReadV2(canceled, v2, owner.ConversationID, 1_910_000_000_000) },
		"nil store":          func() error { return MarkReadV2(context.Background(), nil, owner.ConversationID, 1_910_000_000_000) },
		"zero timestamp":     func() error { return MarkReadV2(context.Background(), v2, owner.ConversationID, 0) },
		"negative timestamp": func() error { return MarkReadV2(context.Background(), v2, owner.ConversationID, -1) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: MarkReadV2() error = nil, want an error", name)
		}
	}
	if after := snapshotV2(t, v2); !reflect.DeepEqual(after, before) {
		t.Fatalf("v2 rows changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestMarkReadV2CreatesDerivedLocalDeviceOnlyWhenAccountHasNone covers an
// account the live bootstrap created after cutover, which has no local
// installation device, next to one whose only local device is non-current and
// keyed by neither the derived nor the mirror ID.
func TestMarkReadV2CreatesDerivedLocalDeviceOnlyWhenAccountHasNone(t *testing.T) {
	v2 := openV2TestStore(t)
	ctx := context.Background()
	seedMarkReadAccount(t, v2, googleAccountID)
	seedMarkReadAccount(t, v2, signalAccountID)
	google := seedMarkReadConversation(t, v2, googleAccountID, "google-thread")
	signal := seedMarkReadConversation(t, v2, signalAccountID, "signal:+15550004444")
	const otherDeviceID = "pre-existing-local"
	if err := v2.UpsertDevice(sqlite.Device{
		DeviceID: otherDeviceID, AccountID: signalAccountID, Kind: sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage", State: sqlite.DeviceStateActive, IsCurrent: false,
		CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	}); err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}
	signalDevicesBefore := mustListDevices(t, v2, signalAccountID)

	for _, readAtMS := range []int64{1_910_000_000_000, 1_910_000_000_500} {
		if err := MarkReadV2(ctx, v2, "google-thread", readAtMS); err != nil {
			t.Fatalf("MarkReadV2(google): %v", err)
		}
		if err := MarkReadV2(ctx, v2, "signal:+15550004444", readAtMS); err != nil {
			t.Fatalf("MarkReadV2(signal): %v", err)
		}
	}

	devices := mustListDevices(t, v2, googleAccountID)
	derived := v2keys.LocalInstallationDeviceID(googleAccountID)
	if len(devices) != 1 || devices[0].DeviceID != derived || !devices[0].IsCurrent ||
		devices[0].Kind != sqlite.DeviceKindLocalInstallation || devices[0].CreatedAtMS != 1_910_000_000_000 {
		t.Fatalf("google devices = %+v, want one current local device %q created at the first read", devices, derived)
	}
	if got := mustListDevices(t, v2, signalAccountID); !reflect.DeepEqual(got, signalDevicesBefore) {
		t.Fatalf("signal devices = %+v, want unchanged %+v", got, signalDevicesBefore)
	}
	for deviceID, conversationID := range map[string]string{derived: google.ConversationID, otherDeviceID: signal.ConversationID} {
		cursor, err := v2.GetReadCursor(deviceID, conversationID)
		if err != nil || cursor.LastReadAtMS != 1_910_000_000_500 || cursor.LastReadMessageID != nil {
			t.Fatalf("cursor on %q = %+v, %v; want read at 1910000000500 with no message", deviceID, cursor, err)
		}
	}
}

// TestMarkReadV2CursorProperties runs random mark-read sequences over random
// account, device, and cursor shapes. Invariants, checked after every call:
//   - the touched cursor's read time is the max of its previous read time and
//     every mark-read time so far (monotone: an older call is a no-op), and a
//     call that does not lose that race leaves exactly
//     {LastReadMessageID: nil, LastReadAtMS: at, UpdatedAtMS: at};
//   - the v2 id and the legacy alias of a thread write the same cursor;
//   - no other cursor, conversation, or account changes, and the only device
//     ever added is LocalInstallationDeviceID for an account that had no local
//     installation device;
//   - the final state does not depend on the order of the calls.
func TestMarkReadV2CursorProperties(t *testing.T) {
	type thread struct {
		accountID string
		remoteID  string
	}
	threads := []thread{
		{googleAccountID, "google-thread-a"},
		{googleAccountID, "google-thread-b"},
		{whatsappAccountID, "15550001111@s.whatsapp.net"},
		{signalAccountID, "signal:+15550002222"},
		{signalAccountID, "signal-group:QUJD="},
	}
	type call struct {
		thread   int
		useAlias bool
		atMS     int64
	}
	const (
		deviceNone = iota
		deviceDerivedCurrent
		deviceOtherNonCurrent
		deviceShapeCount
	)
	// build seeds the same store for a seed every time it is called, so a
	// replay starts from an identical state.
	build := func(seed int64) (*sqlite.Store, map[string]int, []sqlite.Conversation) {
		random := rand.New(rand.NewSource(seed))
		v2 := openV2TestStore(t)
		shapes := map[string]int{}
		for _, accountID := range []string{googleAccountID, whatsappAccountID, signalAccountID} {
			seedMarkReadAccount(t, v2, accountID)
			shapes[accountID] = random.Intn(deviceShapeCount)
			switch shapes[accountID] {
			case deviceDerivedCurrent:
				seedMarkReadDevice(t, v2, accountID, v2keys.LocalInstallationDeviceID(accountID), true)
			case deviceOtherNonCurrent:
				seedMarkReadDevice(t, v2, accountID, "other-local:"+accountID, false)
			}
		}
		conversations := make([]sqlite.Conversation, len(threads))
		for index, thread := range threads {
			conversations[index] = seedMarkReadConversation(t, v2, thread.accountID, thread.remoteID)
			if shapes[thread.accountID] == deviceNone || random.Intn(2) == 0 {
				continue
			}
			// A positioned cursor like the migration's or an ingested
			// receipt's, at a time that may beat later mark-reads.
			messageID := fmt.Sprintf("message-%d-%d", seed, index)
			projectV2TestMessage(t, v2, sqlite.Message{
				MessageID: messageID, ConversationID: conversations[index].ConversationID,
				AccountID: thread.accountID, RemoteMessageID: "remote-" + messageID,
				Direction: sqlite.MessageDirectionOutgoing, Body: "seeded", State: sqlite.MessageStateActive,
				OccurredAtMS: 1_900_000_000_000,
			})
			device, err := v2.GetLocalInstallationDevice(context.Background(), thread.accountID)
			if err != nil {
				t.Fatalf("GetLocalInstallationDevice(): %v", err)
			}
			at := 1_910_000_000_000 + int64(random.Intn(2_000))
			if err := v2.UpsertReadCursor(sqlite.ReadCursor{
				AccountID: thread.accountID, DeviceID: device.DeviceID,
				ConversationID: conversations[index].ConversationID, LastReadMessageID: &messageID,
				LastReadAtMS: at, UpdatedAtMS: at,
			}); err != nil {
				t.Fatalf("UpsertReadCursor(seed): %v", err)
			}
		}
		return v2, shapes, conversations
	}
	apply := func(v2 *sqlite.Store, conversations []sqlite.Conversation, c call) error {
		key := conversations[c.thread].ConversationID
		if c.useAlias {
			key = threads[c.thread].remoteID
		}
		return MarkReadV2(context.Background(), v2, key, c.atMS)
	}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		v2, shapes, conversations := build(seed)
		initial := snapshotV2(t, v2)
		calls := make([]call, 1+random.Intn(12))
		for index := range calls {
			// A narrow time window makes older, equal, and newer calls all
			// common, against each other and against the seeded cursors.
			calls[index] = call{
				thread:   random.Intn(len(threads)),
				useAlias: random.Intn(2) == 0,
				atMS:     1_910_000_000_000 + int64(random.Intn(2_000)),
			}
		}

		model := initial
		model.devices = map[string][]sqlite.Device{}
		for accountID, devices := range initial.devices {
			model.devices[accountID] = slices.Clone(devices)
		}
		model.cursors = cloneCursors(initial.cursors)
		for step, c := range calls {
			if err := apply(v2, conversations, c); err != nil {
				t.Errorf("seed %d step %d: MarkReadV2(%+v): %v", seed, step, c, err)
				return false
			}
			thread := threads[c.thread]
			device, err := v2.GetLocalInstallationDevice(context.Background(), thread.accountID)
			if err != nil {
				t.Errorf("seed %d: GetLocalInstallationDevice(%q): %v", seed, thread.accountID, err)
				return false
			}
			if shapes[thread.accountID] == deviceNone {
				if device.DeviceID != v2keys.LocalInstallationDeviceID(thread.accountID) {
					t.Errorf("seed %d: created device %q, want derived id", seed, device.DeviceID)
					return false
				}
				if !containsDevice(model.devices[thread.accountID], device.DeviceID) {
					model.devices[thread.accountID] = append(model.devices[thread.accountID], device)
				}
			}
			key := cursorKey{device.DeviceID, conversations[c.thread].ConversationID}
			if previous, ok := model.cursors[key]; !ok || c.atMS >= previous.LastReadAtMS {
				model.cursors[key] = sqlite.ReadCursor{
					AccountID: thread.accountID, DeviceID: device.DeviceID,
					ConversationID: key.conversationID, LastReadAtMS: c.atMS, UpdatedAtMS: c.atMS,
				}
			}
			got := snapshotV2(t, v2)
			if !reflect.DeepEqual(got, model) {
				t.Errorf("seed %d step %d (%+v):\ngot  %+v\nwant %+v", seed, step, c, got, model)
				return false
			}
		}

		// Order independence: the same calls in reverse from the same start
		// end at the same cursors. (A created device's timestamps record
		// the first call, so only its ID is order independent.)
		replay, _, replayConversations := build(seed)
		for index := len(calls) - 1; index >= 0; index-- {
			if err := apply(replay, replayConversations, calls[index]); err != nil {
				t.Errorf("seed %d replay: %v", seed, err)
				return false
			}
		}
		reversed, forward := snapshotV2(t, replay), snapshotV2(t, v2)
		if !reflect.DeepEqual(reversed.cursors, forward.cursors) {
			t.Errorf("seed %d: reversed calls ended at\n%+v\nforward calls at\n%+v", seed, reversed.cursors, forward.cursors)
			return false
		}
		for accountID, devices := range forward.devices {
			if !reflect.DeepEqual(deviceIDs(reversed.devices[accountID]), deviceIDs(devices)) {
				t.Errorf("seed %d: reversed devices %+v, forward %+v", seed, reversed.devices[accountID], devices)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 60, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
}

type cursorKey struct {
	deviceID       string
	conversationID string
}

// v2Snapshot is every row a mark-read could write: accounts, each account's
// devices, conversations, and the cursors of every device on every
// conversation. A cursor's foreign keys need an existing device and
// conversation, so a write the snapshot misses would show as a new one of
// those.
type v2Snapshot struct {
	accounts      []sqlite.Account
	devices       map[string][]sqlite.Device
	conversations []sqlite.Conversation
	cursors       map[cursorKey]sqlite.ReadCursor
}

func snapshotV2(t *testing.T, v2 *sqlite.Store) v2Snapshot {
	t.Helper()
	accounts, err := v2.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	conversations, err := v2.ListConversationsByRecencyAllAccounts(10_000)
	if err != nil {
		t.Fatalf("ListConversationsByRecencyAllAccounts(): %v", err)
	}
	snapshot := v2Snapshot{
		accounts:      accounts,
		devices:       map[string][]sqlite.Device{},
		conversations: conversations,
		cursors:       map[cursorKey]sqlite.ReadCursor{},
	}
	for _, account := range accounts {
		devices := mustListDevices(t, v2, account.AccountID)
		snapshot.devices[account.AccountID] = devices
		for _, device := range devices {
			for _, conversation := range conversations {
				cursor, err := v2.GetReadCursor(device.DeviceID, conversation.ConversationID)
				if errors.Is(err, sqlite.ErrNotFound) {
					continue
				}
				if err != nil {
					t.Fatalf("GetReadCursor(): %v", err)
				}
				snapshot.cursors[cursorKey{device.DeviceID, conversation.ConversationID}] = cursor
			}
		}
	}
	return snapshot
}

func deviceIDs(devices []sqlite.Device) []string {
	ids := make([]string, 0, len(devices))
	for _, device := range devices {
		ids = append(ids, device.DeviceID)
	}
	return ids
}

func cloneCursors(cursors map[cursorKey]sqlite.ReadCursor) map[cursorKey]sqlite.ReadCursor {
	clone := make(map[cursorKey]sqlite.ReadCursor, len(cursors))
	for key, cursor := range cursors {
		clone[key] = cursor
	}
	return clone
}

func mustMigratedConversation(t *testing.T, v2 *sqlite.Store, conversation migratedFixtureConversation) sqlite.Conversation {
	t.Helper()
	remoteID := v2keys.NormalizeRemoteConversationID(conversation.platform, conversation.legacyID)
	owner, err := v2.GetConversationByRemote(conversation.accountID, remoteID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", remoteID, err)
	}
	if owner.ConversationID == conversation.legacyID {
		t.Fatalf("migrated conversation kept legacy ID %q; fixture no longer exercises a derived key", owner.ConversationID)
	}
	return owner
}

func seedMarkReadAccount(t *testing.T, v2 *sqlite.Store, accountID string) {
	t.Helper()
	bridgeKey, displayName := accountBootstrap(accountID)
	if err := v2.UpsertAccount(sqlite.Account{
		AccountID: accountID, BridgeKey: bridgeKey, DisplayName: displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	}); err != nil {
		t.Fatalf("UpsertAccount(%q): %v", accountID, err)
	}
}

func seedMarkReadDevice(t *testing.T, v2 *sqlite.Store, accountID, deviceID string, current bool) {
	t.Helper()
	if err := v2.UpsertDevice(sqlite.Device{
		DeviceID: deviceID, AccountID: accountID, Kind: sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage", State: sqlite.DeviceStateActive, IsCurrent: current,
		CreatedAtMS: 1_800_000_000_000, UpdatedAtMS: 1_800_000_000_000,
	}); err != nil {
		t.Fatalf("UpsertDevice(%q): %v", deviceID, err)
	}
}

// seedMarkReadConversation stores a conversation the way live v2 ingest keys it:
// a derived ID, with the legacy-form remote ID as its natural key.
func seedMarkReadConversation(t *testing.T, v2 *sqlite.Store, accountID, remoteID string) sqlite.Conversation {
	t.Helper()
	conversation := sqlite.Conversation{
		ConversationID:       v2keys.DeriveID("conversation", accountID, remoteID),
		AccountID:            accountID,
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		LastMessageAtMS:      1_900_000_000_000,
		CreatedAtMS:          1_800_000_000_000,
		UpdatedAtMS:          1_800_000_000_000,
	}
	if err := v2.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", remoteID, err)
	}
	stored, err := v2.GetConversation(conversation.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation(%q): %v", conversation.ConversationID, err)
	}
	return stored
}
