package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	legacydb "github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

const (
	fixtureBaseMS    int64 = 1_700_000_000_000
	fixtureSignalACI       = "11111111-2222-3333-4444-555555555555"
)

type migrationFixture struct {
	sourcePath     string
	whatsAppRef    string
	scheduledBytes []byte
}

func TestTransformLegacyFixtureToMergedV2(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fixture := buildMigrationFixture(t, root)
	stagedStore := filepath.Join(root, "store.sqlite3.tmp")
	stagedBlobs := filepath.Join(root, "blobs.tmp")
	targetStore := filepath.Join(root, "canonical", "store.sqlite3")
	sourceHashBefore := testFileSHA256(t, fixture.sourcePath)
	sourceInfoBefore, err := os.Stat(fixture.sourcePath)
	mustNoError(t, err)

	report, err := Transform(context.Background(), Options{
		SourcePath:      fixture.sourcePath,
		TempStorePath:   stagedStore,
		TempBlobPath:    stagedBlobs,
		TargetPath:      filepath.Dir(targetStore),
		TargetStorePath: targetStore,
		Check:           true,
	})
	mustNoError(t, err)

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	mustNoError(t, err)
	t.Logf("fixture integrity report:\n%s", reportJSON)

	assertFixtureReport(t, report, sourceHashBefore)
	assertSourceUnchanged(t, fixture.sourcePath, sourceHashBefore, sourceInfoBefore)

	target := openReadOnlyTestDB(t, stagedStore)
	assertMergedMessageSchema(t, target)
	assertFixtureAccounts(t, target)
	assertFixtureMessages(t, target)
	assertFixtureAttachments(t, target, fixture.whatsAppRef)
	assertSignalIdentitiesRemainSeparate(t, target)
	assertFixtureSchedules(t, target, stagedBlobs, fixture.scheduledBytes)
	assertFixtureReadCursors(t, target)
}

func TestTransformReadsHotWALWithoutMutatingSourceFamily(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fixture := buildMigrationFixture(t, root)
	writer, err := sql.Open("sqlite", fixture.sourcePath)
	mustNoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close hot-WAL writer: %v", err)
		}
	})
	_, err = writer.Exec(`PRAGMA journal_mode=WAL`)
	mustNoError(t, err)
	_, err = writer.Exec(`PRAGMA wal_autocheckpoint=0`)
	mustNoError(t, err)
	_, err = writer.Exec(`
		INSERT INTO tabs (tab_id, name, position, created_at)
		VALUES ('hot-wal-tab', 'Hot WAL tab', 99, ?)
	`, fixtureBaseMS+40_000)
	mustNoError(t, err)

	before := snapshotSourceFamilyForTest(t, fixture.sourcePath)
	if !before["wal"].exists || len(before["wal"].contents) == 0 {
		t.Fatal("fixture did not retain a non-empty hot WAL")
	}
	if !before["shm"].exists {
		t.Fatal("fixture did not retain a hot shared-memory sidecar")
	}

	stagedStore := filepath.Join(root, "hot-wal-store.sqlite3.tmp")
	report, err := Transform(context.Background(), Options{
		SourcePath:      fixture.sourcePath,
		TempStorePath:   stagedStore,
		TempBlobPath:    filepath.Join(root, "hot-wal-blobs.tmp"),
		TargetPath:      filepath.Join(root, "hot-wal-target"),
		TargetStorePath: filepath.Join(root, "hot-wal-target", "store.sqlite3"),
		Check:           true,
	})
	if err != nil {
		t.Fatalf("Transform hot WAL: %v\nsource evidence: %+v\nvalidation: %+v", err, report.Source.Files, report.Validation)
	}
	if !report.OK || !report.Validation.SourceUnchanged || !report.Source.Unchanged {
		t.Fatalf("hot-WAL migration did not pass immutable-source validation: %+v", report.Validation)
	}
	if report.Dropped.Tabs != 2 {
		t.Fatalf("tabs visible through WAL = %d, want 2 (checkpointed + hot-WAL rows)", report.Dropped.Tabs)
	}
	if len(report.Source.Files) != 3 {
		t.Fatalf("source family evidence has %d files, want database/WAL/SHM", len(report.Source.Files))
	}
	for _, file := range report.Source.Files {
		if !file.Unchanged {
			t.Errorf("source evidence reports mutation for %s: %+v", file.Role, file)
		}
	}
	after := snapshotSourceFamilyForTest(t, fixture.sourcePath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("source database family changed during migration\nbefore: %#v\nafter:  %#v", before, after)
	}
	if _, err := os.Lstat(stagedStore + legacySnapshotSuffix); !os.IsNotExist(err) {
		t.Fatalf("migration left private source snapshot behind: %v", err)
	}
}

func TestDeterministicID(t *testing.T) {
	t.Parallel()

	const want = "5b66e34164c6cc052fd1dd40eb5975be"
	got := v2keys.DeriveID(
		"message",
		"google-primary",
		"sms-conversation\x1fgoogle-server-1",
	)
	if got != want {
		t.Fatalf("deriveID() = %q, want %q", got, want)
	}
	if again := v2keys.DeriveID("message", "google-primary", "sms-conversation\x1fgoogle-server-1"); again != got {
		t.Fatalf("deriveID() was not repeatable: first %q, second %q", got, again)
	}
	if len(got) != 32 || strings.ToLower(got) != got {
		t.Fatalf("deriveID() = %q, want 32 lowercase hex characters", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("deriveID() = %q, want lowercase hex: %v", got, err)
	}
	if got == v2keys.DeriveID("message", "signal-primary", "sms-conversation\x1fgoogle-server-1") {
		t.Fatal("deriveID() did not include account_id")
	}
	if got == v2keys.DeriveID("conversation", "google-primary", "sms-conversation\x1fgoogle-server-1") {
		t.Fatal("deriveID() did not include entity")
	}
}

func TestDeriveRemoteMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		message  legacyMessage
		expected string
	}{
		{name: "source id wins", message: legacyMessage{ID: "whatsapp:ignored", SourceID: "remote-from-source"}, expected: "remote-from-source"},
		{name: "google unprefixed", message: legacyMessage{ID: "google-server-id"}, expected: "google-server-id"},
		{name: "whatsapp prefix", message: legacyMessage{ID: "whatsapp:stanza-id"}, expected: "stanza-id"},
		{name: "signal timestamp", message: legacyMessage{ID: "signal:1700000000123"}, expected: "1700000000123"},
		{name: "signal fabricated local", message: legacyMessage{ID: "signal:local:deadbeef"}, expected: "local:deadbeef"},
		{name: "gchat prefix", message: legacyMessage{ID: "gchat:message-id"}, expected: "message-id"},
		{name: "imessage prefix", message: legacyMessage{ID: "imessage:message-id"}, expected: "message-id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deriveRemoteMessageID(test.message); got != test.expected {
				t.Fatalf("deriveRemoteMessageID(%q, %q) = %q, want %q", test.message.ID, test.message.SourceID, got, test.expected)
			}
		})
	}
}

func buildMigrationFixture(t *testing.T, root string) migrationFixture {
	t.Helper()

	sourcePath := filepath.Join(root, "legacy.sqlite3")
	store, err := legacydb.New(sourcePath)
	mustNoError(t, err)
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()

	conversations := []*legacydb.Conversation{
		{
			ConversationID: "sms-conversation", Name: "Unified Friend",
			Participants:  `[{"name":"Unified Friend","number":"+1 (555) 010-0001"},{"name":"Me","number":"+15550009999","is_me":true}]`,
			LastMessageTS: fixtureBaseMS + 2_000, UnreadCount: 1,
			SourcePlatform: "sms", DisplayProtocol: "RCS", IsFavorite: true,
			NotificationMode: "muted",
		},
		{
			ConversationID: "whatsapp:fixture-group@g.us", Name: "Fixture WhatsApp group", IsGroup: true,
			Participants:  `[{"name":"Collision Name","number":"15550100002@s.whatsapp.net"},{"name":"Me","number":"15550009999@s.whatsapp.net","is_me":true}]`,
			LastMessageTS: fixtureBaseMS + 4_000, SourcePlatform: "whatsapp",
			NotificationMode: "mentions",
		},
		{
			ConversationID: "signal:+16505550100", Name: "Collision Name",
			Participants:  `[{"name":"Collision Name","number":"+16505550100"}]`,
			LastMessageTS: fixtureBaseMS + 6_000, SourcePlatform: "signal",
		},
		{
			ConversationID: "signal:" + fixtureSignalACI, Name: "Collision Name",
			Participants:  `[{"name":"Collision Name","number":"` + fixtureSignalACI + `"}]`,
			LastMessageTS: fixtureBaseMS + 7_000, UnreadCount: 1, SourcePlatform: "signal",
		},
		{
			ConversationID: "gchat:thread-alpha", Name: "Unified Friend",
			Participants:  `[{"name":"Unified Friend","email":"friend@example.com"}]`,
			LastMessageTS: fixtureBaseMS + 9_000, SourcePlatform: "gchat",
		},
		{
			ConversationID: "imessage:chat-alpha", Name: "Collision Name",
			Participants:  `[{"name":"Collision Name","number":"+15550100003"}]`,
			LastMessageTS: fixtureBaseMS + 11_000, SourcePlatform: "imessage",
		},
	}
	for _, conversation := range conversations {
		mustNoError(t, store.UpsertConversation(conversation))
	}

	tab, err := store.CreateTab("Fixture custom tab")
	mustNoError(t, err)
	mustNoError(t, store.SetConversationTab("gchat:thread-alpha", legacydb.TabArchive))
	mustNoError(t, store.SetConversationTab("imessage:chat-alpha", tab.TabID))

	whatsAppRef := whatsappmedia.EncodeLocalMediaRef("Media/fixture.webp")
	if whatsAppRef == "" {
		t.Fatal("EncodeLocalMediaRef returned an empty fixture reference")
	}
	messages := []*legacydb.Message{
		{
			MessageID: "google-server-1", ConversationID: "sms-conversation",
			SenderName: "Unified Friend", SenderNumber: "+15550100001",
			Body: "google incoming media", TimestampMS: fixtureBaseMS + 1_000,
			Status: "RCS_DELIVERED", MentionsMe: true, SourcePlatform: "sms",
			MediaID: "google-media-1", MimeType: "image/jpeg", DecryptionKey: "deadbeef",
			Reactions: `[{"emoji":"👍","count":1,"actors":["+15550100001"]}]`,
		},
		{
			MessageID: "google-server-2", ConversationID: "sms-conversation",
			SenderName: "Me", SenderNumber: "+15550009999", Body: "google outgoing reply",
			TimestampMS: fixtureBaseMS + 2_000, Status: "sent", IsFromMe: true,
			ReplyToID: "google-server-1", SourcePlatform: "sms",
		},
		{
			MessageID: "whatsapp:wa-stanza-1", ConversationID: "whatsapp:fixture-group@g.us",
			SenderName: "Collision Name", SenderNumber: "15550100002@s.whatsapp.net",
			Body: "whatsapp incoming media", TimestampMS: fixtureBaseMS + 3_000,
			Status: "delivered", SourcePlatform: "whatsapp", SourceID: "wa-source-1",
			MediaID: whatsAppRef, MimeType: "image/webp",
		},
		{
			MessageID: "whatsapp:wa-stanza-2", ConversationID: "whatsapp:fixture-group@g.us",
			SenderName: "Me", Body: "whatsapp outgoing reply", TimestampMS: fixtureBaseMS + 4_000,
			Status: "sent", IsFromMe: true, ReplyToID: "whatsapp:wa-stanza-1",
			SourcePlatform: "whatsapp",
		},
		{
			MessageID: "signal:1700000001000", ConversationID: "signal:+16505550100",
			SenderName: "Collision Name", SenderNumber: "+16505550100",
			Body: "signal incoming media", TimestampMS: fixtureBaseMS + 5_000,
			Status: "delivered", SourcePlatform: "signal", MediaID: "signalatt:att-42",
			MimeType: "video/mp4",
		},
		{
			MessageID: "signal:local:abc123", ConversationID: "signal:+16505550100",
			SenderName: "Me", Body: "signal fabricated outgoing", TimestampMS: fixtureBaseMS + 6_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "signal",
		},
		{
			MessageID: "signal:1700000002000", ConversationID: "signal:" + fixtureSignalACI,
			SenderName: "Collision Name", SenderNumber: fixtureSignalACI,
			Body: "signal ACI incoming", TimestampMS: fixtureBaseMS + 7_000,
			Status: "delivered", SourcePlatform: "signal",
		},
		{
			MessageID: "gchat:message-1", ConversationID: "gchat:thread-alpha",
			SenderName: "Unified Friend", SenderNumber: "friend@example.com",
			Body: "gchat incoming", TimestampMS: fixtureBaseMS + 8_000,
			Status: "delivered", SourcePlatform: "gchat",
		},
		{
			MessageID: "gchat:message-2", ConversationID: "gchat:thread-alpha",
			SenderName: "Me", Body: "gchat outgoing", TimestampMS: fixtureBaseMS + 9_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "gchat",
		},
		{
			MessageID: "imessage:message-1", ConversationID: "imessage:chat-alpha",
			SenderName: "Collision Name", SenderNumber: "+15550100003",
			Body: "imessage incoming", TimestampMS: fixtureBaseMS + 10_000,
			Status: "delivered", SourcePlatform: "imessage",
		},
		{
			MessageID: "imessage:message-2", ConversationID: "imessage:chat-alpha",
			SenderName: "Me", Body: "imessage outgoing", TimestampMS: fixtureBaseMS + 11_000,
			Status: "sent", IsFromMe: true, ReplyToID: "imessage:message-1",
			SourcePlatform: "imessage",
		},
	}
	for _, message := range messages {
		mustNoError(t, store.UpsertMessage(message))
	}
	transcriptModel := "fixture-transcriber"
	mustNoError(t, store.SetMessageTranscript("google-server-1", "fixture transcript", &transcriptModel))

	mustNoError(t, store.UpsertContact(&legacydb.Contact{
		ContactID: "contact-unified", Name: "Unified Friend", Number: "+15550100001",
	}))
	mustNoError(t, store.UpsertUnifiedContact(&legacydb.UnifiedContact{
		UnifiedID: "unified-friend", DisplayName: "Unified Friend",
		Identifiers: `[{"platform":"sms","value":"+15550100001"},{"platform":"gchat","value":"friend@example.com"}]`,
	}))
	mustNoError(t, store.UpsertContactAvatar(legacydb.ContactAvatarCandidate{
		SourcePlatform: "sms", ContactID: "contact-unified", PhoneNumber: "+15550100001",
		DisplayName: "Unified Friend",
	}, []byte("avatar bytes"), "image/png", "avatar-hash", fixtureBaseMS))
	mustNoError(t, store.UpsertDraft(&legacydb.Draft{
		DraftID: "draft-fixture", ConversationID: "sms-conversation",
		Body: "legacy draft", CreatedAt: fixtureBaseMS + 20_000,
	}))
	claimed, _, err := store.ClaimOutgoingSendKey("legacy-send-key", "sms-conversation")
	mustNoError(t, err)
	if !claimed {
		t.Fatal("fixture outgoing send key was not claimed")
	}
	mustNoError(t, store.SetContactTags(
		legacydb.PersonKey("Unified Friend"), "Unified Friend", []string{"friend", "fixture"},
	))

	scheduledBytes := []byte("scheduled media fixture bytes")
	scheduled := []*legacydb.ScheduledMessage{
		{
			ID: "scheduled-pending-text", ConversationID: "sms-conversation",
			Body: "pending scheduled text", ReplyToID: "google-server-1",
			SendAt: fixtureBaseMS + 100_000, Status: legacydb.ScheduleStatusPending,
			CreatedAt: fixtureBaseMS + 30_000,
		},
		{
			ID: "scheduled-pending-media", ConversationID: "whatsapp:fixture-group@g.us",
			Body: "pending scheduled caption", ReplyToID: "whatsapp:wa-stanza-1",
			SendAt: fixtureBaseMS + 200_000, Status: legacydb.ScheduleStatusPending,
			CreatedAt: fixtureBaseMS + 31_000, MediaData: scheduledBytes,
			MediaFilename: "scheduled.bin", MediaMime: "application/octet-stream",
		},
		{
			ID: "scheduled-sending", ConversationID: "signal:+16505550100",
			Body: "ambiguous sending", SendAt: fixtureBaseMS + 300_000,
			Status: legacydb.ScheduleStatusSending, CreatedAt: fixtureBaseMS + 32_000,
		},
		{
			ID: "scheduled-sent", ConversationID: "sms-conversation", Body: "already sent",
			SendAt: fixtureBaseMS + 400_000, Status: legacydb.ScheduleStatusSent,
			CreatedAt: fixtureBaseMS + 33_000, SentMessageID: "google-server-2",
		},
		{
			ID: "scheduled-canceled", ConversationID: "gchat:thread-alpha", Body: "canceled",
			SendAt: fixtureBaseMS + 500_000, Status: legacydb.ScheduleStatusCanceled,
			CreatedAt: fixtureBaseMS + 34_000,
		},
		{
			ID: "scheduled-failed", ConversationID: "imessage:chat-alpha", Body: "failed",
			SendAt: fixtureBaseMS + 600_000, Status: legacydb.ScheduleStatusFailed,
			Attempts: 5, LastError: "fixture permanent failure", CreatedAt: fixtureBaseMS + 35_000,
		},
	}
	for _, message := range scheduled {
		mustNoError(t, store.CreateScheduledMessage(message))
	}

	mustNoError(t, store.Close())
	closed = true
	return migrationFixture{
		sourcePath: sourcePath, whatsAppRef: whatsAppRef, scheduledBytes: scheduledBytes,
	}
}

func assertFixtureReport(t *testing.T, report Report, sourceHash string) {
	t.Helper()

	if report.Format != ReportFormat || !report.OK || !report.Check {
		t.Fatalf("report header = format %d, ok %v, check %v", report.Format, report.OK, report.Check)
	}
	if report.Source.QuickCheck != "ok" || report.Source.SHA256Before != sourceHash ||
		report.Source.SHA256After != sourceHash || !report.Source.Unchanged {
		t.Fatalf("source report did not prove immutability: %+v", report.Source)
	}
	if len(report.Source.Files) != 3 {
		t.Fatalf("source report files = %d, want database/WAL/SHM evidence", len(report.Source.Files))
	}
	for _, file := range report.Source.Files {
		if !file.Unchanged || file.ExistsBefore != file.ExistsAfter {
			t.Errorf("source file evidence did not reconcile: %+v", file)
		}
	}
	if report.Target.SchemaVersion != 10 || len(report.Target.MigrationChecksums) != 10 {
		t.Fatalf("target schema = version %d with %d checksums, want version 10 with 10 checksums", report.Target.SchemaVersion, len(report.Target.MigrationChecksums))
	}
	wantTargetCounts := map[string]int64{
		"accounts": 5, "devices": 5, "people": 1, "person_identities": 2,
		"conversations": 6, "inbox": 0, "messages": 14,
		"message_attachments": 3, "outbox": 2, "outbox_attachments": 1,
		"read_cursors": 6, "reactions": 0, "reaction_snapshot_fences": 0,
	}
	for table, want := range wantTargetCounts {
		if got := report.Target.Counts[table]; got != want {
			t.Errorf("target count %s = %d, want %d", table, got, want)
		}
	}
	if report.Identities.Created != report.Target.Counts["identities"] ||
		report.Identities.PeopleCreated != 1 || report.Identities.LinksByProvenance["explicit"] != 2 ||
		report.Identities.UnunifiedIdentities != report.Identities.Created-2 {
		t.Errorf("identity report = %+v", report.Identities)
	}

	messages := report.TableCounts["messages"]
	if messages.Legacy != 11 || messages.V2 != 11 || messages.Reconciled != 11 || messages.ReconciliationRatio != 1 {
		t.Errorf("message reconciliation = %+v, want 11/11 and ratio 1", messages)
	}
	if len(report.MessageCollisions) != 0 {
		t.Errorf("message collisions = %+v, want none", report.MessageCollisions)
	}
	wantPlatformMessages := map[string]int64{
		"sms": 2, "whatsapp": 2, "signal": 3, "gchat": 2, "imessage": 2,
	}
	for platform, want := range wantPlatformMessages {
		got := report.PlatformCounts[platform]
		if got.Legacy != want || got.V2 != want || got.ReconciliationRatio != 1 {
			t.Errorf("platform reconciliation %s = %+v, want %d/%d and ratio 1", platform, got, want, want)
		}
	}

	if report.Schedule.LegacyTotal != 6 || report.Schedule.ByStatus["pending"] != 2 ||
		report.Schedule.PendingToOutbox != 2 || report.Schedule.PendingMedia != 1 ||
		report.Schedule.AmbiguousSending != 1 || report.Schedule.OptimisticOnlyRows != 1 ||
		report.Schedule.SkippedSent != 1 || report.Schedule.SkippedCanceled != 1 ||
		report.Schedule.SkippedFailed != 1 {
		t.Errorf("schedule report = %+v", report.Schedule)
	}
	if report.ReadState.LossyWarning != "legacy read state was lossy" ||
		report.ReadState.ConversationsWithUnreadCount != 2 || report.ReadState.CursorsCreated != 6 {
		t.Errorf("read state report = %+v", report.ReadState)
	}
	wantDropped := DroppedDimensions{
		ReactionsBearingMessages: 1, TranscriptBearingMessages: 1,
		ContactAvatars: 1, Drafts: 1, ContactMetaCRM: 1,
		OutgoingSendKeys: 1, Tabs: 1,
	}
	if !reflect.DeepEqual(report.Dropped, wantDropped) {
		t.Errorf("dropped dimensions = %+v, want %+v", report.Dropped, wantDropped)
	}
	if report.SignalLocalRows != 1 {
		t.Errorf("signal local rows = %d, want 1", report.SignalLocalRows)
	}
	if report.Media.LegacyAttachments != 3 || report.Media.PendingAttachments != 3 ||
		report.Media.WhatsAppUnresolvable != 0 || report.Media.SignalLocalAttachments != 0 ||
		report.Media.UnsupportedArchiveAttachments != 0 || report.Media.ScheduledBlobsVerified != 1 {
		t.Errorf("media report = %+v", report.Media)
	}
	if len(report.SampledHashes) != 11 {
		t.Errorf("sampled hashes = %d, want all 11 fixture messages", len(report.SampledHashes))
	}
	for _, sample := range report.SampledHashes {
		if !sample.Matched || sample.ExpectedSHA256 != sample.ActualSHA256 {
			t.Errorf("sample hash mismatch: %+v", sample)
		}
	}
	if report.Validation.QuickCheck != "ok" || len(report.Validation.ForeignKeyViolations) != 0 ||
		orphanTotal(report.Validation.Orphans) != 0 || !report.Validation.CountsMatched ||
		!report.Validation.SampledHashesMatched || !report.Validation.BlobReferencesValid ||
		!report.Validation.SourceUnchanged || !report.Validation.Passed {
		t.Errorf("validation report = %+v", report.Validation)
	}
	for _, required := range []string{
		"legacy read state was lossy", "reactions", "transcripts", "contact avatars",
		"drafts", "IMPORTANT: 1 contact_meta CRM", "outgoing_send_keys", "custom tabs",
		"ambiguous in-flight scheduled sends",
	} {
		if !containsWarning(report.Warnings, required) {
			t.Errorf("warnings did not contain %q: %v", required, report.Warnings)
		}
	}
}

func assertSourceUnchanged(t *testing.T, path, beforeHash string, before os.FileInfo) {
	t.Helper()

	if got := testFileSHA256(t, path); got != beforeHash {
		t.Fatalf("source hash changed: got %s, want %s", got, beforeHash)
	}
	after, err := os.Stat(path)
	mustNoError(t, err)
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("source metadata changed: before size/mod/mode %d/%v/%v, after %d/%v/%v", before.Size(), before.ModTime(), before.Mode(), after.Size(), after.ModTime(), after.Mode())
	}
}

func assertMergedMessageSchema(t *testing.T, database *sql.DB) {
	t.Helper()

	rows, err := database.Query(`PRAGMA table_info(messages)`)
	mustNoError(t, err)
	defer rows.Close()
	type column struct {
		name    string
		notNull bool
	}
	columns := map[string]column{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		mustNoError(t, rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey))
		columns[name] = column{name: name, notNull: notNull != 0}
	}
	mustNoError(t, rows.Err())
	want := []string{
		"message_id", "conversation_id", "account_id", "remote_message_id",
		"sender_identity_id", "direction", "body", "reply_to_remote_id", "state",
		"occurred_at_ms", "created_at_ms", "updated_at_ms",
	}
	for _, name := range want {
		if _, ok := columns[name]; !ok {
			t.Errorf("merged messages schema is missing %q", name)
		}
	}
	if !columns["remote_message_id"].notNull {
		t.Error("merged messages.remote_message_id is nullable")
	}
	for _, forbidden := range []string{"status", "kind", "client_request_id", "content_json", "mentions_self", "source_inbox_id", "reply_to_message_id"} {
		if _, ok := columns[forbidden]; ok {
			t.Errorf("messages unexpectedly contains aspirational column %q", forbidden)
		}
	}
}

func assertFixtureAccounts(t *testing.T, database *sql.DB) {
	t.Helper()

	want := map[string][2]string{
		"google-primary":   {"google_messages", "live"},
		"whatsapp-primary": {"whatsmeow", "live"},
		"signal-primary":   {"signal_cli", "live"},
		"gchat-archive":    {"gchat", "archive"},
		"imessage-archive": {"imessage", "archive"},
	}
	rows, err := database.Query(`SELECT account_id, bridge_key, mode FROM accounts ORDER BY account_id`)
	mustNoError(t, err)
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var accountID, bridgeKey, mode string
		mustNoError(t, rows.Scan(&accountID, &bridgeKey, &mode))
		got[accountID] = [2]string{bridgeKey, mode}
	}
	mustNoError(t, rows.Err())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("accounts = %#v, want %#v", got, want)
	}
	if got := mustQueryInt64(t, database, `SELECT COUNT(*) FROM people`); got != 1 {
		t.Errorf("people count = %d, want only the one unified_contact", got)
	}
}

func assertFixtureMessages(t *testing.T, database *sql.DB) {
	t.Helper()

	type expectedMessage struct {
		legacyConversation string
		accountID          string
		remoteID           string
		body               string
		direction          string
		occurredAt         int64
		replyTo            string
		senderIdentityID   string
	}
	wants := []expectedMessage{
		{
			legacyConversation: "sms-conversation", accountID: "google-primary",
			remoteID: "google-server-1", body: "google incoming media", direction: "incoming",
			occurredAt:       fixtureBaseMS + 1_000,
			senderIdentityID: v2keys.DeriveID("identity", "google-primary", "e164\x1f+15550100001"),
		},
		{
			legacyConversation: "sms-conversation", accountID: "google-primary",
			remoteID: "google-server-2", body: "google outgoing reply", direction: "outgoing",
			occurredAt: fixtureBaseMS + 2_000, replyTo: "google-server-1",
		},
		{
			legacyConversation: "whatsapp:fixture-group@g.us", accountID: "whatsapp-primary",
			remoteID: "wa-source-1", body: "whatsapp incoming media", direction: "incoming",
			occurredAt:       fixtureBaseMS + 3_000,
			senderIdentityID: v2keys.DeriveID("identity", "whatsapp-primary", "jid\x1f15550100002@s.whatsapp.net"),
		},
		{
			legacyConversation: "whatsapp:fixture-group@g.us", accountID: "whatsapp-primary",
			remoteID: "wa-stanza-2", body: "whatsapp outgoing reply", direction: "outgoing",
			occurredAt: fixtureBaseMS + 4_000, replyTo: "wa-source-1",
		},
		{
			legacyConversation: "signal:+16505550100", accountID: "signal-primary",
			remoteID: "1700000001000", body: "signal incoming media", direction: "incoming",
			occurredAt:       fixtureBaseMS + 5_000,
			senderIdentityID: v2keys.DeriveID("identity", "signal-primary", "e164\x1f+16505550100"),
		},
		{
			legacyConversation: "signal:+16505550100", accountID: "signal-primary",
			remoteID: "local:abc123", body: "signal fabricated outgoing", direction: "outgoing",
			occurredAt: fixtureBaseMS + 6_000,
		},
	}
	for _, want := range wants {
		messageID := v2keys.DeriveID(
			"message", want.accountID, want.legacyConversation+"\x1f"+want.remoteID,
		)
		conversationID := v2keys.DeriveID("conversation", want.accountID, want.legacyConversation)
		var gotConversationID, gotAccountID, remoteID, body, direction, state string
		var replyTo, sender sql.NullString
		var occurredAt, createdAt, updatedAt int64
		err := database.QueryRow(`
			SELECT conversation_id, account_id, remote_message_id, sender_identity_id,
			       direction, body, reply_to_remote_id, state, occurred_at_ms,
			       created_at_ms, updated_at_ms
			FROM messages WHERE message_id = ?
		`, messageID).Scan(
			&gotConversationID, &gotAccountID, &remoteID, &sender, &direction, &body,
			&replyTo, &state, &occurredAt, &createdAt, &updatedAt,
		)
		mustNoError(t, err)
		if gotConversationID != conversationID || gotAccountID != want.accountID ||
			remoteID != want.remoteID || body != want.body || direction != want.direction ||
			state != "active" || occurredAt != want.occurredAt || createdAt != want.occurredAt ||
			updatedAt != want.occurredAt {
			t.Errorf("message %s = conv/account/remote/body/direction/state/times %q/%q/%q/%q/%q/%q/%d/%d/%d", messageID, gotConversationID, gotAccountID, remoteID, body, direction, state, occurredAt, createdAt, updatedAt)
		}
		if replyTo.Valid != (want.replyTo != "") || replyTo.String != want.replyTo {
			t.Errorf("message %s reply_to_remote_id = %+v, want %q", messageID, replyTo, want.replyTo)
		}
		if sender.Valid != (want.senderIdentityID != "") || sender.String != want.senderIdentityID {
			t.Errorf("message %s sender_identity_id = %+v, want %q", messageID, sender, want.senderIdentityID)
		}
	}
	if got := mustQueryInt64(t, database, `SELECT COUNT(*) FROM inbox`); got != 0 {
		t.Errorf("inbox rows = %d, want ImportMessage to leave historical inbox empty", got)
	}
}

func assertFixtureAttachments(t *testing.T, database *sql.DB, whatsAppRef string) {
	t.Helper()

	tests := []struct {
		name               string
		legacyConversation string
		accountID          string
		remoteMessageID    string
		remoteID           string
		mime               string
		opaque             string
		validate           func([]byte) error
	}{
		{
			name: "google", legacyConversation: "sms-conversation", accountID: "google-primary",
			remoteMessageID: "google-server-1", remoteID: "google-media-1", mime: "image/jpeg",
			opaque:   `{"v":1,"media_id":"google-media-1","decryption_key":"deadbeef"}`,
			validate: googleadapter.ValidateDownloadOpaque,
		},
		{
			name: "whatsapp", legacyConversation: "whatsapp:fixture-group@g.us", accountID: "whatsapp-primary",
			remoteMessageID: "wa-source-1", remoteID: whatsAppRef, mime: "image/webp",
			opaque:   `{"v":1,"kind":"local","local_ref":"` + whatsAppRef + `"}`,
			validate: whatsappadapter.ValidateDownloadOpaque,
		},
		{
			name: "signal", legacyConversation: "signal:+16505550100", accountID: "signal-primary",
			remoteMessageID: "1700000001000", remoteID: "signalatt:att-42", mime: "video/mp4",
			opaque:   `{"v":1,"kind":"remote","att_id":"att-42","conversation_id":"signal:+16505550100"}`,
			validate: signaladapter.ValidateDownloadOpaque,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messageID := v2keys.DeriveID(
				"message", test.accountID,
				test.legacyConversation+"\x1f"+test.remoteMessageID,
			)
			var ordinal int64
			var remoteID, mime, state string
			var remoteRef []byte
			var blobHash sql.NullString
			err := database.QueryRow(`
				SELECT ordinal, remote_id, remote_ref, mime, state, blob_hash
				FROM message_attachments WHERE message_id = ?
			`, messageID).Scan(&ordinal, &remoteID, &remoteRef, &mime, &state, &blobHash)
			mustNoError(t, err)
			if ordinal != 0 || remoteID != test.remoteID || string(remoteRef) != test.opaque ||
				mime != test.mime || state != "pending" || blobHash.Valid {
				t.Fatalf("attachment = ordinal/remote/ref/mime/state/blob %d/%q/%q/%q/%q/%+v", ordinal, remoteID, remoteRef, mime, state, blobHash)
			}
			if !json.Valid(remoteRef) {
				t.Fatalf("attachment opaque is not valid JSON: %q", remoteRef)
			}
			// The remote_ref this migration packs must round-trip through the
			// REAL adapter download codec (string equality alone would miss
			// struct-tag drift between the packer and the Wave-4 unpacker).
			if err := test.validate(remoteRef); err != nil {
				t.Fatalf("packed remote_ref does not decode with the %s download codec: %v", test.name, err)
			}
		})
	}
}

func assertSignalIdentitiesRemainSeparate(t *testing.T, database *sql.DB) {
	t.Helper()

	rows, err := database.Query(`
		SELECT identity_id, kind, canonical_value
		FROM identities
		WHERE account_id = 'signal-primary'
		  AND canonical_value IN ('+16505550100', ?)
		ORDER BY canonical_value
	`, fixtureSignalACI)
	mustNoError(t, err)
	defer rows.Close()
	type identity struct {
		id, kind, canonical string
	}
	var identities []identity
	for rows.Next() {
		var got identity
		mustNoError(t, rows.Scan(&got.id, &got.kind, &got.canonical))
		identities = append(identities, got)
	}
	mustNoError(t, rows.Err())
	if len(identities) != 2 {
		t.Fatalf("signal phone/ACI identities = %+v, want two separate rows", identities)
	}
	kinds := []string{identities[0].kind, identities[1].kind}
	sort.Strings(kinds)
	if !reflect.DeepEqual(kinds, []string{"e164", "signal_aci"}) || identities[0].id == identities[1].id {
		t.Fatalf("signal phone/ACI identities = %+v, want distinct e164 and signal_aci rows", identities)
	}
	for _, identity := range identities {
		if got := mustQueryInt64(t, database, `SELECT COUNT(*) FROM person_identities WHERE identity_id = ?`, identity.id); got != 0 {
			t.Errorf("signal identity %s was suspiciously unified (%d links)", identity.canonical, got)
		}
	}
}

func assertFixtureSchedules(t *testing.T, database *sql.DB, blobRoot string, scheduledBytes []byte) {
	t.Helper()

	type expectedOutbox struct {
		id, accountID, legacyConversation, kind string
		scheduledFor                            int64
	}
	wants := []expectedOutbox{
		{"scheduled-pending-text", "google-primary", "sms-conversation", "text", fixtureBaseMS + 100_000},
		{"scheduled-pending-media", "whatsapp-primary", "whatsapp:fixture-group@g.us", "media", fixtureBaseMS + 200_000},
	}
	for _, want := range wants {
		outboxID := v2keys.DeriveID("outbox", want.accountID, want.id)
		requestID := v2keys.DeriveID("transport_request", want.accountID, want.id)
		messageID := v2keys.DeriveID(
			"message", want.accountID, want.legacyConversation+"\x1f"+requestID,
		)
		conversationID := v2keys.DeriveID("conversation", want.accountID, want.legacyConversation)
		var gotOutboxID, accountID, gotConversationID, kind, idempotencyKey, state string
		var localMessageID, transportRequestID string
		var scheduledFor, attempts int64
		err := database.QueryRow(`
			SELECT outbox_id, account_id, conversation_id, kind, idempotency_key,
			       state, local_message_id, transport_request_id, scheduled_for_ms,
			       attempt_count
			FROM outbox WHERE idempotency_key = ?
		`, want.id).Scan(
			&gotOutboxID, &accountID, &gotConversationID, &kind, &idempotencyKey,
			&state, &localMessageID, &transportRequestID, &scheduledFor, &attempts,
		)
		mustNoError(t, err)
		if gotOutboxID != outboxID || accountID != want.accountID ||
			gotConversationID != conversationID || kind != want.kind || idempotencyKey != want.id ||
			state != "queued" || localMessageID != messageID || transportRequestID != requestID ||
			scheduledFor != want.scheduledFor || attempts != 0 {
			t.Errorf("outbox %s = id/account/conv/kind/key/state/message/request/schedule/attempts %q/%q/%q/%q/%q/%q/%q/%q/%d/%d", want.id, gotOutboxID, accountID, gotConversationID, kind, idempotencyKey, state, localMessageID, transportRequestID, scheduledFor, attempts)
		}
		var optimisticRemote, direction string
		mustNoError(t, database.QueryRow(`SELECT remote_message_id, direction FROM messages WHERE message_id = ?`, messageID).Scan(&optimisticRemote, &direction))
		if optimisticRemote != requestID || direction != "outgoing" {
			t.Errorf("optimistic message %s = remote/direction %q/%q, want %q/outgoing", messageID, optimisticRemote, direction, requestID)
		}
	}

	for _, skipped := range []string{"scheduled-sending", "scheduled-sent", "scheduled-canceled", "scheduled-failed"} {
		if got := mustQueryInt64(t, database, `SELECT COUNT(*) FROM outbox WHERE idempotency_key = ?`, skipped); got != 0 {
			t.Errorf("outbox contains %d rows for skipped schedule %q", got, skipped)
		}
	}
	sendingRequestID := v2keys.DeriveID("transport_request", "signal-primary", "scheduled-sending")
	sendingMessageID := v2keys.DeriveID(
		"message", "signal-primary", "signal:+16505550100\x1f"+sendingRequestID,
	)
	var sendingBody, sendingDirection string
	mustNoError(t, database.QueryRow(`SELECT body, direction FROM messages WHERE message_id = ?`, sendingMessageID).Scan(&sendingBody, &sendingDirection))
	if sendingBody != "ambiguous sending" || sendingDirection != "outgoing" {
		t.Errorf("ambiguous sending optimistic row = %q/%q", sendingBody, sendingDirection)
	}

	mediaOutboxID := v2keys.DeriveID("outbox", "whatsapp-primary", "scheduled-pending-media")
	var hash, mime, filename string
	var size int64
	mustNoError(t, database.QueryRow(`
		SELECT blob_hash, size_bytes, mime, filename
		FROM outbox_attachments WHERE outbox_id = ? AND ordinal = 0
	`, mediaOutboxID).Scan(&hash, &size, &mime, &filename))
	wantHashBytes := sha256.Sum256(scheduledBytes)
	wantHash := hex.EncodeToString(wantHashBytes[:])
	if hash != wantHash || size != int64(len(scheduledBytes)) ||
		mime != "application/octet-stream" || filename != "scheduled.bin" {
		t.Errorf("scheduled attachment = hash/size/mime/name %q/%d/%q/%q", hash, size, mime, filename)
	}
	blobStore, err := blob.New(blobRoot)
	mustNoError(t, err)
	reader, err := blobStore.Open(blob.BlobRef{Hash: hash, Size: size, MIME: mime})
	mustNoError(t, err)
	contents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	mustNoError(t, readErr)
	mustNoError(t, closeErr)
	if string(contents) != string(scheduledBytes) {
		t.Errorf("scheduled blob = %q, want %q", contents, scheduledBytes)
	}
}

func assertFixtureReadCursors(t *testing.T, database *sql.DB) {
	t.Helper()

	smsConversationID := v2keys.DeriveID("conversation", "google-primary", "sms-conversation")
	smsDeviceID := v2keys.DeriveID(
		"device", "google-primary", "google-primary\x1flocal",
	)
	smsLastReadMessageID := v2keys.DeriveID(
		"message", "google-primary", "sms-conversation\x1fgoogle-server-1",
	)
	var lastRead sql.NullString
	var lastReadAt int64
	mustNoError(t, database.QueryRow(`
		SELECT last_read_message_id, last_read_at_ms
		FROM read_cursors WHERE device_id = ? AND conversation_id = ?
	`, smsDeviceID, smsConversationID).Scan(&lastRead, &lastReadAt))
	if !lastRead.Valid || lastRead.String != smsLastReadMessageID || lastReadAt != fixtureBaseMS+1_000 {
		t.Errorf("SMS lossy read cursor = %+v at %d, want %s at %d", lastRead, lastReadAt, smsLastReadMessageID, fixtureBaseMS+1_000)
	}

	aciConversation := "signal:" + fixtureSignalACI
	aciConversationID := v2keys.DeriveID("conversation", "signal-primary", aciConversation)
	signalDeviceID := v2keys.DeriveID("device", "signal-primary", "signal-primary\x1flocal")
	mustNoError(t, database.QueryRow(`
		SELECT last_read_message_id, last_read_at_ms
		FROM read_cursors WHERE device_id = ? AND conversation_id = ?
	`, signalDeviceID, aciConversationID).Scan(&lastRead, &lastReadAt))
	if lastRead.Valid || lastReadAt != 0 {
		t.Errorf("fully unread one-message Signal cursor = %+v at %d, want NULL at 0", lastRead, lastReadAt)
	}
}

func openReadOnlyTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", readOnlySQLiteDSN(path))
	mustNoError(t, err)
	database.SetMaxOpenConns(1)
	mustNoError(t, database.Ping())
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close target database: %v", err)
		}
	})
	return database
}

func mustQueryInt64(t *testing.T, database *sql.DB, query string, args ...any) int64 {
	t.Helper()

	var value int64
	mustNoError(t, database.QueryRow(query, args...).Scan(&value))
	return value
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()

	file, err := os.Open(path)
	mustNoError(t, err)
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	mustNoError(t, copyErr)
	mustNoError(t, closeErr)
	return hex.EncodeToString(hash.Sum(nil))
}

type testSourceFileState struct {
	exists       bool
	contents     string
	size         int64
	mode         os.FileMode
	modifiedAtNS int64
}

func snapshotSourceFamilyForTest(t *testing.T, databasePath string) map[string]testSourceFileState {
	t.Helper()

	paths := map[string]string{
		"database": databasePath,
		"wal":      databasePath + "-wal",
		"shm":      databasePath + "-shm",
	}
	result := make(map[string]testSourceFileState, len(paths))
	for role, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			result[role] = testSourceFileState{}
			continue
		}
		mustNoError(t, err)
		contents, err := os.ReadFile(path)
		mustNoError(t, err)
		result[role] = testSourceFileState{
			exists: true, contents: string(contents), size: info.Size(),
			mode: info.Mode(), modifiedAtNS: info.ModTime().UnixNano(),
		}
	}
	return result
}

func containsWarning(warnings []string, substring string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, substring) {
			return true
		}
	}
	return false
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A single corrupt Signal attachment ref must NOT abort the whole migration
// (symmetric with the WhatsApp unresolvable path): the message migrates, the
// attachment is dropped to pending/empty-ref, and the report counts it. One
// unreadable ref in years of history cannot block the cutover go/no-go gate.
func TestTransformToleratesMalformedSignalMedia(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sourcePath := filepath.Join(root, "messages.db")
	store, err := legacydb.New(sourcePath)
	mustNoError(t, err)

	base := int64(1700000000000)
	mustNoError(t, store.UpsertConversation(&legacydb.Conversation{
		ConversationID: "signal:+16505550199", Name: "Signal Self", SourcePlatform: "signal",
		Participants: `[{"name":"Peer","number":"+16505550199"}]`,
	}))
	// A message carrying a corrupt signallocal: ref (invalid base64) and one
	// carrying an empty signalatt: ref — both previously aborted the transform.
	for i, mediaID := range []string{"signallocal:@@@not-base64@@@", "signalatt:"} {
		mustNoError(t, store.UpsertMessage(&legacydb.Message{
			MessageID:      "signal:170000009900" + string(rune('0'+i)),
			ConversationID: "signal:+16505550199",
			SenderName:     "Peer", SenderNumber: "+16505550199",
			Body: "signal media with broken ref", TimestampMS: base + int64(i)*1000,
			Status: "delivered", SourcePlatform: "signal", MediaID: mediaID, MimeType: "image/png",
		}))
	}
	mustNoError(t, store.Close())

	report, err := Transform(context.Background(), Options{
		SourcePath:      sourcePath,
		TempStorePath:   filepath.Join(root, "store.sqlite3.tmp"),
		TempBlobPath:    filepath.Join(root, "blobs.tmp"),
		TargetPath:      filepath.Join(root, "canonical"),
		TargetStorePath: filepath.Join(root, "canonical", "store.sqlite3"),
		Check:           true,
	})
	mustNoError(t, err) // must NOT abort
	if report.Media.SignalUnresolvable != 2 {
		t.Fatalf("signal_unresolvable = %d, want 2", report.Media.SignalUnresolvable)
	}
	// Both messages still migrated (ratio must stay 1.0 for the go/no-go gate).
	target := openReadOnlyTestDB(t, filepath.Join(root, "store.sqlite3.tmp"))
	if got := mustQueryInt64(t, target, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Fatalf("migrated messages = %d, want 2", got)
	}
	// Attachments were dropped (no pending row with a ref), not fabricated.
	if got := mustQueryInt64(t, target, `SELECT COUNT(*) FROM message_attachments WHERE remote_ref != x''`); got != 0 {
		t.Fatalf("attachment rows with a ref = %d, want 0 (unresolvable)", got)
	}
}
