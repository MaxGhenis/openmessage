package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// markReadFixtureThread is a legacy conversation the fixture store held when
// it was migrated, so the v2 store keys it by derived hash and keeps the
// legacy id as its remote conversation id.
type markReadFixtureThread struct {
	legacyID  string
	platform  string
	accountID string
}

var markReadFixtureThreads = []markReadFixtureThread{
	{legacyID: "google-thread-1", platform: "sms", accountID: "google-primary"},
	{legacyID: "whatsapp:15550002222@s.whatsapp.net", platform: "whatsapp", accountID: "whatsapp-primary"},
	{legacyID: "signal:+15550003333", platform: "signal", accountID: "signal-primary"},
}

// TestMarkReadV2PrimaryWritesNativeCursorOnMigratedStore is the live
// v2-primary shape: the migration keyed each conversation and each account's
// local device by hash. The UI's v2 id and a stored legacy id both mark the
// same thread, on the migrated device, with no other v2 row touched.
func TestMarkReadV2PrimaryWritesNativeCursorOnMigratedStore(t *testing.T) {
	v2 := openMigratedMarkReadStore(t)
	handler := newMarkReadHandler(t, v2, zerolog.Nop())
	for _, thread := range markReadFixtureThreads {
		owner := mustMigratedThread(t, v2, thread)
		deviceID := mustMigratedLocalDevice(t, v2, thread.accountID)
		for name, key := range map[string]string{"v2 id": owner.ConversationID, "legacy alias": thread.legacyID} {
			t.Run(thread.legacyID+"/"+name, func(t *testing.T) {
				before := snapshotMarkReadRows(t, v2)
				previous := before.cursors[markReadCursorKey{deviceID, owner.ConversationID}]

				startMS := time.Now().UnixMilli()
				postMarkRead(t, handler, key)
				endMS := time.Now().UnixMilli()

				cursor, err := v2.GetReadCursor(deviceID, owner.ConversationID)
				if err != nil {
					t.Fatalf("GetReadCursor(migrated device %q): %v", deviceID, err)
				}
				if cursor.AccountID != thread.accountID || cursor.ConversationID != owner.ConversationID ||
					cursor.LastReadMessageID != nil || cursor.SourceUpdatedAtMS != nil ||
					cursor.LastReadAtMS < startMS || cursor.LastReadAtMS > endMS ||
					cursor.UpdatedAtMS != cursor.LastReadAtMS || cursor.LastReadAtMS < previous.LastReadAtMS {
					t.Fatalf("cursor = %+v, want %s read in [%d, %d] with no message, after %+v",
						cursor, owner.ConversationID, startMS, endMS, previous)
				}

				after := snapshotMarkReadRows(t, v2)
				delete(before.cursors, markReadCursorKey{deviceID, owner.ConversationID})
				delete(after.cursors, markReadCursorKey{deviceID, owner.ConversationID})
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("v2 rows besides the cursor changed:\nbefore %+v\nafter  %+v", before, after)
				}
				if _, err := v2.GetDevice("local-primary:" + thread.accountID); !errors.Is(err, sqlite.ErrNotFound) {
					t.Fatalf("GetDevice(local-primary) error = %v, want ErrNotFound", err)
				}
				if _, err := v2.GetConversation(thread.legacyID); !errors.Is(err, sqlite.ErrNotFound) {
					t.Fatalf("GetConversation(legacy id) error = %v, want ErrNotFound: mark-read keyed a row by the legacy id", err)
				}
			})
		}
	}
}

// TestMarkReadV2PrimaryUnknownConversationLogsAndWritesNothing covers a
// legacy id for a thread the v2 store lacks: the response stays 200, the
// failure is logged, and nothing is written (the legacy mirror would have
// created the thread under its legacy id).
func TestMarkReadV2PrimaryUnknownConversationLogsAndWritesNothing(t *testing.T) {
	v2 := openMigratedMarkReadStore(t)
	var logs bytes.Buffer
	handler := newMarkReadHandler(t, v2, zerolog.New(&logs))
	before := snapshotMarkReadRows(t, v2)

	postMarkRead(t, handler, "google-after-cutover")

	if after := snapshotMarkReadRows(t, v2); !reflect.DeepEqual(after, before) {
		t.Fatalf("v2 rows changed:\nbefore %+v\nafter  %+v", before, after)
	}
	assertMarkReadWarning(t, logs.String(), "google-after-cutover")
}

func TestMarkReadV2PrimaryWithoutV2StoreStays200(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: "google-thread-1", SourcePlatform: "sms", UnreadCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	handler := APIHandlerWithOptions(legacy, nil, zerolog.New(&logs), nil, APIOptions{
		V2Primary: true,
		V2:        &V2Options{},
	})

	postMarkRead(t, handler, "google-thread-1")

	assertMarkReadWarning(t, logs.String(), "google-thread-1")
	conversation, err := legacy.GetConversation("google-thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.UnreadCount != 0 {
		t.Fatalf("legacy unread count = %d, want 0", conversation.UnreadCount)
	}
}

// TestMarkReadV2PrimaryCursorMonotoneProperty drives the handler with random
// sequences of v2 ids and legacy aliases over a migrated store, after giving
// random threads a cursor from another writer (an ingested receipt or the
// migration) that is positioned on a message and dated in the past or the
// future. After every request:
//   - a cursor dated after the request is unchanged (the write is monotone in
//     read time, so a mark-read never moves a cursor back);
//   - otherwise the cursor is {LastReadMessageID: nil, LastReadAtMS:
//     UpdatedAtMS: the request's time}, never earlier than before;
//   - no other cursor, device, account, or conversation changes.
func TestMarkReadV2PrimaryCursorMonotoneProperty(t *testing.T) {
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		v2 := openMigratedMarkReadStore(t)
		handler := newMarkReadHandler(t, v2, zerolog.Nop())
		messages, err := sqlite.NewMessageRepository(v2, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		owners := make([]sqlite.Conversation, len(markReadFixtureThreads))
		devices := make([]string, len(markReadFixtureThreads))
		nowMS := time.Now().UnixMilli()
		for index, thread := range markReadFixtureThreads {
			owners[index] = mustMigratedThread(t, v2, thread)
			devices[index] = mustMigratedLocalDevice(t, v2, thread.accountID)
			if random.Intn(3) == 0 {
				continue
			}
			latest, err := messages.ListMessagesByConversation(context.Background(), owners[index].ConversationID, 0, "", 1)
			if err != nil || len(latest) != 1 {
				t.Fatalf("latest message for %q = %+v, %v", owners[index].ConversationID, latest, err)
			}
			// Up to a day either side of now: past cursors lose to the next
			// mark-read, future ones win against every request in the run.
			at := nowMS + int64(random.Intn(2*86_400_000)) - 86_400_000
			if err := v2.UpsertReadCursor(sqlite.ReadCursor{
				AccountID: thread.accountID, DeviceID: devices[index], ConversationID: owners[index].ConversationID,
				LastReadMessageID: &latest[0].MessageID, LastReadAtMS: at, UpdatedAtMS: at,
			}); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}
		}

		for step := range 1 + random.Intn(8) {
			index := random.Intn(len(markReadFixtureThreads))
			key := owners[index].ConversationID
			if random.Intn(2) == 0 {
				key = markReadFixtureThreads[index].legacyID
			}
			before := snapshotMarkReadRows(t, v2)
			cursorKey := markReadCursorKey{devices[index], owners[index].ConversationID}
			previous, hadCursor := before.cursors[cursorKey]

			startMS := time.Now().UnixMilli()
			postMarkRead(t, handler, key)
			endMS := time.Now().UnixMilli()

			after := snapshotMarkReadRows(t, v2)
			cursor := after.cursors[cursorKey]
			// The request reads the clock somewhere in [startMS, endMS]. A
			// cursor dated after endMS must survive it, one dated at or before
			// startMS must give way, and one dated inside the window may do
			// either.
			kept := hadCursor && reflect.DeepEqual(cursor, previous) && previous.LastReadAtMS > startMS
			replaced := cursor.LastReadMessageID == nil && cursor.SourceUpdatedAtMS == nil &&
				cursor.LastReadAtMS >= startMS && cursor.LastReadAtMS <= endMS &&
				cursor.UpdatedAtMS == cursor.LastReadAtMS &&
				(!hadCursor || (cursor.LastReadAtMS >= previous.LastReadAtMS && previous.LastReadAtMS <= endMS))
			if !kept && !replaced {
				t.Errorf("seed %d step %d: cursor %+v after %+v; want it kept if dated after the request, else read in [%d, %d] with no message",
					seed, step, cursor, previous, startMS, endMS)
				return false
			}
			delete(before.cursors, cursorKey)
			delete(after.cursors, cursorKey)
			if !reflect.DeepEqual(after, before) {
				t.Errorf("seed %d step %d: other v2 rows changed:\nbefore %+v\nafter  %+v", seed, step, before, after)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
}

// openMigratedMarkReadStore runs the real migration over a small legacy store,
// as internal/v2wire's openMigratedTestStores does, so the v2 store has the
// live install's shape: per-account accounts and a current local
// installation device per account under a derived ID, conversations keyed by
// derived hash, and a positioned read cursor per conversation.
func openMigratedMarkReadStore(t *testing.T) *sqlite.Store {
	t.Helper()
	root := t.TempDir()
	legacyPath := filepath.Join(root, "messages.db")
	legacy, err := db.New(legacyPath)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	const baseMS int64 = 1_700_000_000_000
	for index, thread := range markReadFixtureThreads {
		if err := legacy.UpsertConversation(&db.Conversation{
			ConversationID: thread.legacyID,
			Name:           "Migrated " + thread.platform,
			Participants:   `[]`,
			LastMessageTS:  baseMS + 10 + int64(index),
			SourcePlatform: thread.platform,
			UnreadCount:    1,
		}); err != nil {
			t.Fatalf("legacy UpsertConversation(%q): %v", thread.legacyID, err)
		}
	}
	for _, message := range []*db.Message{
		{MessageID: "google-message-1", ConversationID: "google-thread-1", SenderNumber: "+15550001111", Body: "google 1", TimestampMS: baseMS, SourcePlatform: "sms"},
		{MessageID: "google-message-2", ConversationID: "google-thread-1", SenderNumber: "+15550001111", Body: "google 2", TimestampMS: baseMS + 10, SourcePlatform: "sms"},
		{MessageID: "whatsapp:wa-1", SourceID: "wa-1", ConversationID: "whatsapp:15550002222@s.whatsapp.net", SenderNumber: "15550002222@s.whatsapp.net", Body: "whatsapp 1", TimestampMS: baseMS + 1, SourcePlatform: "whatsapp"},
		{MessageID: "whatsapp:wa-2", SourceID: "wa-2", ConversationID: "whatsapp:15550002222@s.whatsapp.net", SenderNumber: "15550002222@s.whatsapp.net", Body: "whatsapp 2", TimestampMS: baseMS + 11, SourcePlatform: "whatsapp"},
		{MessageID: "signal:1700000000002", SourceID: "1700000000002", ConversationID: "signal:+15550003333", SenderNumber: "+15550003333", Body: "signal 1", TimestampMS: baseMS + 2, SourcePlatform: "signal"},
		{MessageID: "signal:1700000000012", SourceID: "1700000000012", ConversationID: "signal:+15550003333", SenderNumber: "+15550003333", Body: "signal 2", TimestampMS: baseMS + 12, SourcePlatform: "signal"},
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

	// Pin the shape the tests depend on, so a migration change cannot make
	// them pass vacuously: a derived device, and a migrated cursor that names
	// a message (unread_count 1 leaves the first of two messages read).
	for _, thread := range markReadFixtureThreads {
		owner := mustMigratedThread(t, v2, thread)
		deviceID := mustMigratedLocalDevice(t, v2, thread.accountID)
		cursor, err := v2.GetReadCursor(deviceID, owner.ConversationID)
		if err != nil || cursor.LastReadMessageID == nil {
			t.Fatalf("migrated cursor for %q = %+v, %v; want one that names a message", thread.legacyID, cursor, err)
		}
	}
	return v2
}

func mustMigratedThread(t *testing.T, v2 *sqlite.Store, thread markReadFixtureThread) sqlite.Conversation {
	t.Helper()
	remoteID := v2keys.NormalizeRemoteConversationID(thread.platform, thread.legacyID)
	owner, err := v2.GetConversationByRemote(thread.accountID, remoteID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q, %q): %v", thread.accountID, remoteID, err)
	}
	if owner.ConversationID == thread.legacyID {
		t.Fatalf("migrated conversation kept legacy ID %q; fixture no longer exercises a derived key", owner.ConversationID)
	}
	return owner
}

// mustMigratedLocalDevice returns the account's local installation device and
// checks it carries the migration's derived ID, not "local-primary:<account>".
func mustMigratedLocalDevice(t *testing.T, v2 *sqlite.Store, accountID string) string {
	t.Helper()
	device, err := v2.GetLocalInstallationDevice(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetLocalInstallationDevice(%q): %v", accountID, err)
	}
	if want := v2keys.DeriveID("device", accountID, accountID+"\x1flocal"); device.DeviceID != want || !device.IsCurrent {
		t.Fatalf("migrated local device for %q = %+v, want current device %q", accountID, device, want)
	}
	return device.DeviceID
}

func newMarkReadHandler(t *testing.T, v2 *sqlite.Store, logger zerolog.Logger) http.Handler {
	t.Helper()
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	return APIHandlerWithOptions(legacy, nil, logger, nil, APIOptions{
		V2Primary: true,
		V2:        &V2Options{V2Store: v2},
	})
}

func postMarkRead(t *testing.T, handler http.Handler, conversationID string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"conversation_id": conversationID})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/mark-read", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("mark-read %q = %d %s, want 200 ok", conversationID, response.StatusCode, raw)
	}
}

func assertMarkReadWarning(t *testing.T, logs, conversationID string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["level"] == "warn" && entry["message"] == "Failed to write v2 read cursor" &&
			entry["conv_id"] == conversationID && entry["error"] != nil {
			return
		}
	}
	t.Fatalf("logs = %q, want a warning for %q", logs, conversationID)
}

type markReadCursorKey struct {
	deviceID       string
	conversationID string
}

// markReadRows is every v2 row a mark-read could write: accounts, each
// account's devices, conversations, and every device's cursor on every
// conversation. A cursor's foreign keys need an existing device and
// conversation, so a write the snapshot misses would show as a new one.
type markReadRows struct {
	accounts      []sqlite.Account
	devices       map[string][]sqlite.Device
	conversations []sqlite.Conversation
	cursors       map[markReadCursorKey]sqlite.ReadCursor
}

func snapshotMarkReadRows(t *testing.T, v2 *sqlite.Store) markReadRows {
	t.Helper()
	accounts, err := v2.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	conversations, err := v2.ListConversationsByRecencyAllAccounts(10_000)
	if err != nil {
		t.Fatalf("ListConversationsByRecencyAllAccounts(): %v", err)
	}
	rows := markReadRows{
		accounts:      accounts,
		devices:       map[string][]sqlite.Device{},
		conversations: conversations,
		cursors:       map[markReadCursorKey]sqlite.ReadCursor{},
	}
	for _, account := range accounts {
		devices, err := v2.ListDevices(account.AccountID)
		if err != nil {
			t.Fatalf("ListDevices(%q): %v", account.AccountID, err)
		}
		rows.devices[account.AccountID] = devices
		for _, device := range devices {
			for _, conversation := range conversations {
				cursor, err := v2.GetReadCursor(device.DeviceID, conversation.ConversationID)
				if errors.Is(err, sqlite.ErrNotFound) {
					continue
				}
				if err != nil {
					t.Fatalf("GetReadCursor(): %v", err)
				}
				rows.cursors[markReadCursorKey{device.DeviceID, conversation.ConversationID}] = cursor
			}
		}
	}
	return rows
}
