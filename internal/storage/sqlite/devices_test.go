package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
)

func TestUpsertDeviceRejectsMovingDeviceToAnotherAccount(t *testing.T) {
	store := openRepositoryTestStore(t)
	for _, accountID := range []string{"account-a", "account-b"} {
		mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(repositoryTestAccount(accountID)))
	}
	seedMessageConversation(t, store, "conversation-a", "account-a")

	withCursor := localTestDevice("device-with-cursor", "account-a", true)
	withoutChildren := localTestDevice("device-without-children", "account-a", false)
	for _, device := range []Device{withCursor, withoutChildren} {
		mustRepositoryWrite(t, "UpsertDevice", store.UpsertDevice(device))
	}
	cursor := ReadCursor{
		AccountID:      "account-a",
		DeviceID:       withCursor.DeviceID,
		ConversationID: "conversation-a",
		LastReadAtMS:   repositoryTestTimeMS,
		UpdatedAtMS:    repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "UpsertReadCursor", store.UpsertReadCursor(cursor))

	// The device without children is the case the old upsert allowed: with no
	// read cursor to trip the NO ACTION foreign key, account_id was silently
	// rewritten and the device moved accounts.
	for _, original := range []Device{withCursor, withoutChildren} {
		moved := original
		moved.AccountID = "account-b"
		moved.IsCurrent = false
		moved.DisplayName = "moved"
		moved.UpdatedAtMS = original.UpdatedAtMS + 1
		err := store.UpsertDevice(moved)
		if !errors.Is(err, ErrCrossAccountDevice) || !errors.Is(err, ErrConstraintViolation) {
			t.Fatalf("UpsertDevice(%q to account-b) error = %v, want ErrCrossAccountDevice and ErrConstraintViolation", original.DeviceID, err)
		}
		got, err := store.GetDevice(original.DeviceID)
		mustRepositoryRead(t, "GetDevice", err)
		assertRepositoryEqual(t, "device after rejected move", got, original)
	}
	gotCursor, err := store.GetReadCursor(withCursor.DeviceID, "conversation-a")
	mustRepositoryRead(t, "GetReadCursor", err)
	assertRepositoryEqual(t, "read cursor after rejected move", gotCursor, cursor)
}

func TestUpsertDeviceUpdatesSameAccountDeviceWithReadCursors(t *testing.T) {
	store := openRepositoryTestStore(t)
	mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(repositoryTestAccount("account-a")))
	seedMessageConversation(t, store, "conversation-a", "account-a")
	device := localTestDevice("device-a", "account-a", true)
	mustRepositoryWrite(t, "UpsertDevice", store.UpsertDevice(device))
	cursor := ReadCursor{
		AccountID:      "account-a",
		DeviceID:       "device-a",
		ConversationID: "conversation-a",
		LastReadAtMS:   repositoryTestTimeMS,
		UpdatedAtMS:    repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "UpsertReadCursor", store.UpsertReadCursor(cursor))

	updated := device
	updated.DisplayName = "renamed"
	updated.State = DeviceStateRevoked
	updated.IsCurrent = false
	updated.CreatedAtMS = device.CreatedAtMS + 50
	updated.UpdatedAtMS = device.UpdatedAtMS + 100
	mustRepositoryWrite(t, "UpsertDevice(update)", store.UpsertDevice(updated))

	updated.CreatedAtMS = device.CreatedAtMS
	got, err := store.GetDevice("device-a")
	mustRepositoryRead(t, "GetDevice", err)
	assertRepositoryEqual(t, "updated device", got, updated)
	gotCursor, err := store.GetReadCursor("device-a", "conversation-a")
	mustRepositoryRead(t, "GetReadCursor", err)
	assertRepositoryEqual(t, "read cursor after device update", gotCursor, cursor)
}

// TestUpsertDeviceCompilesNoForeignKeyChildWork pins the 2026-10-09 plan
// audit finding: with account_id in the DO UPDATE SET, SQLite compiled a scan
// of the device's read_cursors children (and, with it, an outbox_read_receipts
// scan) into every upsert, because account_id is part of the parent key
// read_cursors(account_id, device_id) references. EXPLAIN QUERY PLAN does not
// show FK work, so this reads the bytecode.
func TestUpsertDeviceCompilesNoForeignKeyChildWork(t *testing.T) {
	store := openRepositoryTestStore(t)
	children := foreignKeyChildBtrees(t, store, "devices")
	for _, table := range []string{"read_cursors", "outbox_read_receipts"} {
		if !childTablesContain(children, table) {
			t.Fatalf("FK children of devices = %v, want %s among them", children, table)
		}
	}

	for _, op := range explainOpcodes(t, store, upsertDeviceSQL,
		"device", "account", nil, DeviceKindLocalInstallation, "", DeviceStateActive, true, nil, 1, 1,
	) {
		switch op.opcode {
		case "FkCounter", "FkIfZero":
			t.Errorf("upsertDeviceSQL compiles %s at %d; the upsert should do no FK child bookkeeping", op.opcode, op.addr)
		case "OpenRead", "OpenWrite", "ReopenIdx":
			if name, ok := children[op.p2]; ok && op.p3 == 0 {
				t.Errorf("upsertDeviceSQL opens FK child b-tree %s at %d", name, op.addr)
			}
		}
	}
}

func TestEnsureLocalInstallationDevice(t *testing.T) {
	const accountID = "account-a"
	tests := []struct {
		name     string
		existing []Device
		want     string
		minted   bool
	}{
		{
			name:   "mints when the account has no device",
			want:   "local-primary:account-a",
			minted: true,
		},
		{
			name: "mints when the account has only remote endpoints",
			existing: []Device{
				remoteTestDevice("remote-a", accountID),
			},
			want:   "local-primary:account-a",
			minted: true,
		},
		{
			name: "reuses the migrated current device under a derived id",
			existing: []Device{
				localTestDevice("9bcc1343derivedhash", accountID, true),
				remoteTestDevice("remote-a", accountID),
			},
			want: "9bcc1343derivedhash",
		},
		{
			name: "reuses a non-current local device rather than adding a current one",
			existing: []Device{
				localTestDevice("older-local", accountID, false),
			},
			want: "older-local",
		},
		{
			name: "prefers the current local device",
			existing: []Device{
				localTestDevice("aaa-non-current", accountID, false),
				localTestDevice("zzz-current", accountID, true),
			},
			want: "zzz-current",
		},
		{
			name: "ignores another account's local device",
			existing: []Device{
				localTestDevice("local-b", "account-b", true),
			},
			want:   "local-primary:account-a",
			minted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRepositoryTestStore(t)
			for _, id := range []string{accountID, "account-b"} {
				mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(repositoryTestAccount(id)))
			}
			for _, device := range test.existing {
				mustRepositoryWrite(t, "seed UpsertDevice", store.UpsertDevice(device))
			}
			before := allTestDevices(t, store)
			candidate := localTestDevice("local-primary:account-a", accountID, true)

			for attempt := range 2 {
				got, err := store.EnsureLocalInstallationDevice(context.Background(), candidate)
				if err != nil {
					t.Fatalf("EnsureLocalInstallationDevice() attempt %d: %v", attempt, err)
				}
				if got.DeviceID != test.want || got.AccountID != accountID || got.Kind != DeviceKindLocalInstallation {
					t.Fatalf("EnsureLocalInstallationDevice() attempt %d = %+v, want device %q", attempt, got, test.want)
				}
				resolved, err := store.GetLocalInstallationDevice(context.Background(), accountID)
				mustRepositoryRead(t, "GetLocalInstallationDevice", err)
				assertRepositoryEqual(t, "ensured device vs resolved", got, resolved)
			}

			want := before
			if test.minted {
				want = append(append([]Device{}, before...), candidate)
			}
			assertSameDevices(t, allTestDevices(t, store), want)
		})
	}
}

func TestEnsureLocalInstallationDeviceRejectsInvalidCandidates(t *testing.T) {
	store := openRepositoryTestStore(t)
	for _, id := range []string{"account-a", "account-b"} {
		mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(repositoryTestAccount(id)))
	}
	mustRepositoryWrite(t, "seed UpsertDevice", store.UpsertDevice(localTestDevice("taken", "account-b", true)))
	before := allTestDevices(t, store)

	remote := remoteTestDevice("remote-candidate", "account-a")
	if _, err := store.EnsureLocalInstallationDevice(context.Background(), remote); err == nil ||
		!strings.Contains(err.Error(), "is not") {
		t.Fatalf("EnsureLocalInstallationDevice(remote kind) error = %v, want kind rejection", err)
	}
	// account-a has no local device, so the insert runs and collides with
	// account-b's device ID; it must fail rather than adopt that row.
	collision := localTestDevice("taken", "account-a", true)
	if _, err := store.EnsureLocalInstallationDevice(context.Background(), collision); !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("EnsureLocalInstallationDevice(colliding id) error = %v, want ErrConstraintViolation", err)
	}
	assertSameDevices(t, allTestDevices(t, store), before)
}

// TestEnsureLocalInstallationDeviceProperties checks, over random device
// tables, that ensuring a local device:
//   - inserts exactly the candidate when the account has no local device and
//     the candidate ID is free, and otherwise inserts nothing;
//   - returns the device GetLocalInstallationDevice resolved before the call
//     whenever the account already had one;
//   - fails without writing when it must insert and the candidate ID is taken;
//   - never changes another row and is idempotent.
//
// The schema's devices_current_local_uq makes "at most one current local
// device per account" hold for any successful write; the generator respects
// it when seeding.
func TestEnsureLocalInstallationDeviceProperties(t *testing.T) {
	store := openRepositoryTestStore(t)
	ctx := context.Background()
	iteration := 0
	property := func(seed int64) bool {
		iteration++
		random := rand.New(rand.NewSource(seed))
		accountA := fmt.Sprintf("a-%d", iteration)
		accountB := fmt.Sprintf("b-%d", iteration)
		for _, id := range []string{accountA, accountB} {
			if err := store.UpsertAccount(repositoryTestAccount(id)); err != nil {
				t.Errorf("UpsertAccount(%q): %v", id, err)
				return false
			}
		}
		var seededIDs []string
		for _, accountID := range []string{accountA, accountB} {
			currentLocal := false
			for index := range random.Intn(4) {
				id := fmt.Sprintf("%s-device-%d", accountID, index)
				device := remoteTestDevice(id, accountID)
				if random.Intn(2) == 0 {
					isCurrent := !currentLocal && random.Intn(2) == 0
					currentLocal = currentLocal || isCurrent
					device = localTestDevice(id, accountID, isCurrent)
				}
				if err := store.UpsertDevice(device); err != nil {
					t.Errorf("seed UpsertDevice(%q): %v", id, err)
					return false
				}
				seededIDs = append(seededIDs, id)
			}
		}

		candidateID := "local-primary:" + accountA
		if len(seededIDs) > 0 && random.Intn(3) == 0 {
			candidateID = seededIDs[random.Intn(len(seededIDs))]
		}
		candidate := localTestDevice(candidateID, accountA, true)
		before := allTestDevices(t, store)
		existing, existingErr := store.GetLocalInstallationDevice(ctx, accountA)
		hadLocal := existingErr == nil
		if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
			t.Errorf("GetLocalInstallationDevice(before): %v", existingErr)
			return false
		}
		idTaken := false
		for _, device := range before {
			idTaken = idTaken || device.DeviceID == candidateID
		}

		got, err := store.EnsureLocalInstallationDevice(ctx, candidate)
		after := allTestDevices(t, store)
		switch {
		case hadLocal:
			if err != nil || !reflect.DeepEqual(got, existing) || !reflect.DeepEqual(after, before) {
				t.Errorf("seed %d: had local %+v; got %+v, err %v; rows changed %v", seed, existing, got, err, !reflect.DeepEqual(after, before))
				return false
			}
		case idTaken:
			if !errors.Is(err, ErrConstraintViolation) || !reflect.DeepEqual(after, before) {
				t.Errorf("seed %d: candidate id %q taken; err %v; rows changed %v", seed, candidateID, err, !reflect.DeepEqual(after, before))
				return false
			}
			return true
		default:
			want := append(append([]Device{}, before...), candidate)
			sortTestDevices(want)
			if err != nil || !reflect.DeepEqual(got, candidate) || !reflect.DeepEqual(after, want) {
				t.Errorf("seed %d: minted; got %+v, err %v; rows %+v, want %+v", seed, got, err, after, want)
				return false
			}
		}

		again, err := store.EnsureLocalInstallationDevice(ctx, candidate)
		if err != nil || !reflect.DeepEqual(again, got) || !reflect.DeepEqual(allTestDevices(t, store), after) {
			t.Errorf("seed %d: second ensure = %+v, err %v; want idempotent %+v", seed, again, err, got)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureAccountInsertsOnceAndPreservesExistingRow(t *testing.T) {
	store := openRepositoryTestStore(t)
	bootstrap := Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        AccountModeLive,
		Enabled:     false,
		ConfigJSON:  `{"kept":true}`,
		CreatedAtMS: repositoryTestTimeMS,
		UpdatedAtMS: repositoryTestTimeMS,
	}
	got, err := store.EnsureAccount(bootstrap)
	mustRepositoryWrite(t, "EnsureAccount(insert)", err)
	assertRepositoryEqual(t, "inserted account", got, bootstrap)

	clobber := bootstrap
	clobber.BridgeKey = "google"
	clobber.DisplayName = "google-primary"
	clobber.Enabled = true
	clobber.ConfigJSON = `{}`
	clobber.CreatedAtMS = repositoryTestTimeMS + 10
	clobber.UpdatedAtMS = repositoryTestTimeMS + 10
	got, err = store.EnsureAccount(clobber)
	mustRepositoryWrite(t, "EnsureAccount(existing)", err)
	assertRepositoryEqual(t, "existing account", got, bootstrap)
	stored, err := store.GetAccount("google-primary")
	mustRepositoryRead(t, "GetAccount", err)
	assertRepositoryEqual(t, "stored account", stored, bootstrap)

	invalid := repositoryTestAccount("account-invalid")
	invalid.Mode = AccountMode("invalid")
	if _, err := store.EnsureAccount(invalid); !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("EnsureAccount(invalid mode) error = %v, want ErrConstraintViolation", err)
	}
}

func TestUpsertOwnedConversationRefusesAnotherConversationsNaturalKey(t *testing.T) {
	store := openRepositoryTestStore(t)
	mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(repositoryTestAccount("account-a")))
	archivedAt := repositoryTestTimeMS + 5
	owner := Conversation{
		ConversationID:       "derived-hash-id",
		AccountID:            "account-a",
		RemoteConversationID: "legacy-thread",
		Kind:                 ConversationKindGroup,
		Title:                "v2 title",
		NotificationMode:     NotificationModeMuted,
		IsFavorite:           true,
		ArchivedAtMS:         &archivedAt,
		LastMessageAtMS:      repositoryTestTimeMS + 900,
		MetadataJSON:         `{"v2":true}`,
		CreatedAtMS:          repositoryTestTimeMS,
		UpdatedAtMS:          repositoryTestTimeMS + 900,
	}
	mustRepositoryWrite(t, "UpsertConversation(owner)", store.UpsertConversation(owner))

	intruder := Conversation{
		ConversationID:       "legacy-thread",
		AccountID:            "account-a",
		RemoteConversationID: "legacy-thread",
		Kind:                 ConversationKindDirect,
		Title:                "legacy title",
		NotificationMode:     NotificationModeAll,
		LastMessageAtMS:      repositoryTestTimeMS + 1,
		MetadataJSON:         `{}`,
		CreatedAtMS:          repositoryTestTimeMS + 1000,
		UpdatedAtMS:          repositoryTestTimeMS + 1000,
	}
	err := store.UpsertOwnedConversation(intruder)
	if !errors.Is(err, ErrConversationIdentityConflict) || !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("UpsertOwnedConversation(intruder) error = %v, want ErrConversationIdentityConflict", err)
	}
	got, err := store.GetConversation("derived-hash-id")
	mustRepositoryRead(t, "GetConversation(owner)", err)
	assertRepositoryEqual(t, "owner after refused upsert", got, owner)
	assertRepositoryNotFound(t, "GetConversation(intruder)", func() error {
		_, err := store.GetConversation("legacy-thread")
		return err
	})

	// The owner itself still refreshes through its own ID, and a new natural
	// key inserts.
	refreshed := owner
	refreshed.Title = "refreshed"
	refreshed.UpdatedAtMS = owner.UpdatedAtMS + 1
	mustRepositoryWrite(t, "UpsertOwnedConversation(owner)", store.UpsertOwnedConversation(refreshed))
	got, err = store.GetConversation("derived-hash-id")
	mustRepositoryRead(t, "GetConversation(refreshed)", err)
	assertRepositoryEqual(t, "refreshed owner", got, refreshed)

	fresh := intruder
	fresh.ConversationID = "fresh-thread"
	fresh.RemoteConversationID = "fresh-thread"
	mustRepositoryWrite(t, "UpsertOwnedConversation(fresh)", store.UpsertOwnedConversation(fresh))
	got, err = store.GetConversation("fresh-thread")
	mustRepositoryRead(t, "GetConversation(fresh)", err)
	assertRepositoryEqual(t, "fresh conversation", got, fresh)

	// UpsertConversation keeps its natural-key merge for the migration.
	mustRepositoryWrite(t, "UpsertConversation(intruder)", store.UpsertConversation(intruder))
	got, err = store.GetConversation("derived-hash-id")
	mustRepositoryRead(t, "GetConversation(merged)", err)
	if got.Title != intruder.Title || got.CreatedAtMS != owner.CreatedAtMS {
		t.Fatalf("UpsertConversation merge = %+v, want intruder metadata on the owner row", got)
	}
}

func localTestDevice(deviceID, accountID string, isCurrent bool) Device {
	return Device{
		DeviceID:    deviceID,
		AccountID:   accountID,
		Kind:        DeviceKindLocalInstallation,
		DisplayName: "OpenMessage",
		State:       DeviceStateActive,
		IsCurrent:   isCurrent,
		CreatedAtMS: repositoryTestTimeMS,
		UpdatedAtMS: repositoryTestTimeMS,
	}
}

func remoteTestDevice(deviceID, accountID string) Device {
	remoteID := "remote-" + deviceID
	return Device{
		DeviceID:       deviceID,
		AccountID:      accountID,
		RemoteDeviceID: &remoteID,
		Kind:           DeviceKindRemoteEndpoint,
		State:          DeviceStateActive,
		CreatedAtMS:    repositoryTestTimeMS,
		UpdatedAtMS:    repositoryTestTimeMS,
	}
}

func allTestDevices(t *testing.T, store *Store) []Device {
	t.Helper()
	rows, err := store.db.Query("SELECT " + deviceColumns + " FROM devices ORDER BY device_id")
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	devices, err := collectRows(rows, scanDevice)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	return devices
}

func sortTestDevices(devices []Device) {
	sort.Slice(devices, func(i, j int) bool { return devices[i].DeviceID < devices[j].DeviceID })
}

func assertSameDevices(t *testing.T, got, want []Device) {
	t.Helper()
	want = append([]Device{}, want...)
	sortTestDevices(want)
	assertRepositoryEqual(t, "devices", got, want)
}

type explainedOp struct {
	addr   int64
	opcode string
	p2, p3 int64
}

func explainOpcodes(t *testing.T, store *Store, statement string, args ...any) []explainedOp {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN "+statement, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var ops []explainedOp
	for rows.Next() {
		var op explainedOp
		var p1, p5 int64
		var p4, comment any
		if err := rows.Scan(&op.addr, &op.opcode, &p1, &op.p2, &op.p3, &p4, &p5, &comment); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	if len(ops) == 0 {
		t.Fatal("EXPLAIN returned no bytecode")
	}
	return ops
}

// foreignKeyChildBtrees maps the root page of every table and index belonging
// to a table whose foreign keys reference parent to that b-tree's name.
func foreignKeyChildBtrees(t *testing.T, store *Store, parent string) map[int64]string {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT s.name, s.rootpage
		FROM sqlite_schema s
		WHERE s.rootpage > 0 AND s.tbl_name IN (
			SELECT m.name
			FROM sqlite_schema m, pragma_foreign_key_list(m.name) f
			WHERE m.type = 'table' AND f."table" = ?
		)
	`, parent)
	if err != nil {
		t.Fatalf("list FK children of %s: %v", parent, err)
	}
	defer rows.Close()
	btrees := map[int64]string{}
	for rows.Next() {
		var name string
		var rootpage int64
		if err := rows.Scan(&name, &rootpage); err != nil {
			t.Fatalf("scan FK child b-tree: %v", err)
		}
		btrees[rootpage] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list FK children of %s: %v", parent, err)
	}
	return btrees
}

func childTablesContain(btrees map[int64]string, table string) bool {
	for _, name := range btrees {
		if name == table {
			return true
		}
	}
	return false
}
