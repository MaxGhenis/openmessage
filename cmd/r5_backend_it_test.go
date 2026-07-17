package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/migration"
	omtools "github.com/maxghenis/openmessage/internal/tools"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/v2wire"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

const (
	r5GoogleAccountID   = "google-primary"
	r5WhatsAppAccountID = "whatsapp-primary"
	r5SignalAccountID   = "signal-primary"
	r5GChatAccountID    = "gchat-archive"
	r5IMessageAccountID = "imessage-archive"

	r5GoogleConversation   = "sms-r5-fixture"
	r5WhatsAppLID          = "whatsapp:r5opaque@lid"
	r5WhatsAppConversation = "whatsapp:14155550100@s.whatsapp.net"
	r5SignalACI            = "11111111-2222-3333-4444-555555555555"
	r5SignalConversation   = "signal:" + r5SignalACI
	r5SignalGroup          = "signal-group:r5-group-token"
	r5GChatConversation    = "gchat:r5-thread"
	r5IMessageConversation = "imessage:r5-chat"

	r5SharedSearchToken = "r5-shared-parity-token"
)

type r5FixtureMessage struct {
	LegacyID    string
	Body        string
	RemoteID    string
	FromMe      bool
	SenderName  string
	SenderValue string
}

type r5FixtureConversation struct {
	LegacyID  string
	RemoteID  string
	AccountID string
	Platform  string
	Name      string
	Messages  []r5FixtureMessage
}

func (conversation r5FixtureConversation) V2ID() string {
	return v2keys.DeriveID("conversation", conversation.AccountID, conversation.RemoteID)
}

type r5LegacyFixture struct {
	DataDir           string
	StorePath         string
	BaseMS            int64
	TotalMessages     int64
	ConversationCount int
	PlatformCounts    map[string]int64
	Conversations     []r5FixtureConversation
	ScheduledMedia    []byte
}

// buildR5LegacyFixture constructs the R5 cutover corpus exclusively through
// the retained legacy writers, then closes the store so backup/migrate see a
// stopped and checkpointed SQLite family.
func buildR5LegacyFixture(t *testing.T) r5LegacyFixture {
	t.Helper()

	dataDir := t.TempDir()
	storePath := filepath.Join(dataDir, legacyDatabaseName)
	store, err := db.New(storePath)
	if err != nil {
		t.Fatalf("create R5 legacy fixture: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()

	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixMilli()
	conversations := []*db.Conversation{
		{
			ConversationID: r5GoogleConversation,
			Name:           "R5 Google Friend",
			Participants:   `[{"name":"R5 Google Friend","number":"+15550100001"},{"name":"Me","number":"+15550009999","is_me":true}]`,
			LastMessageTS:  base + 3_000,
			UnreadCount:    2,
			SourcePlatform: "sms",
		},
		{
			ConversationID: r5WhatsAppLID,
			Name:           "R5 WhatsApp LID alias",
			Participants:   `[{"name":"R5 WhatsApp Friend","number":"14155550200@s.whatsapp.net"}]`,
			LastMessageTS:  base + 4_000,
			SourcePlatform: "whatsapp",
		},
		{
			ConversationID: r5WhatsAppConversation,
			Name:           "R5 WhatsApp Friend",
			Participants:   `[{"name":"R5 WhatsApp Friend","number":"14155550200@s.whatsapp.net"},{"name":"Me","number":"14155550999@s.whatsapp.net","is_me":true}]`,
			LastMessageTS:  base + 5_000,
			SourcePlatform: "whatsapp",
		},
		{
			ConversationID: r5SignalConversation,
			Name:           "R5 Signal ACI Friend",
			Participants:   `[{"name":"R5 Signal ACI Friend","number":"` + r5SignalACI + `"}]`,
			LastMessageTS:  base + 7_000,
			SourcePlatform: "signal",
		},
		{
			ConversationID: r5SignalGroup,
			Name:           "R5 Signal Group",
			IsGroup:        true,
			Participants:   `[{"name":"R5 Signal Group Friend","number":"+15550300003"}]`,
			LastMessageTS:  base + 8_000,
			SourcePlatform: "signal",
		},
		{
			ConversationID: r5GChatConversation,
			Name:           "R5 GChat Archive",
			Participants:   `[{"name":"R5 GChat Friend","email":"friend-r5@example.com"}]`,
			LastMessageTS:  base + 10_000,
			SourcePlatform: "gchat",
		},
		{
			ConversationID: r5IMessageConversation,
			Name:           "R5 iMessage Archive",
			Participants:   `[{"name":"R5 iMessage Friend","number":"+15550400004"}]`,
			LastMessageTS:  base + 12_000,
			SourcePlatform: "imessage",
		},
	}
	for _, conversation := range conversations {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed conversation %q: %v", conversation.ConversationID, err)
		}
	}

	whatsAppMediaRef := whatsappmedia.EncodeLocalMediaRef("Media/r5-fixture.webp")
	if whatsAppMediaRef == "" {
		t.Fatal("encode WhatsApp fixture media reference: empty result")
	}
	legacyMessages := []*db.Message{
		{
			MessageID: "google-r5-empty-source", ConversationID: r5GoogleConversation,
			SenderName: "R5 Google Friend", SenderNumber: "+15550100001",
			Body: r5SharedSearchToken + " google incoming media", TimestampMS: base + 1_000,
			Status: "RCS_DELIVERED", MediaID: "google-r5-media", MimeType: "image/jpeg",
			DecryptionKey: "deadbeef", Reactions: `[{"emoji":"👍","count":1}]`,
			SourcePlatform: "sms",
		},
		{
			MessageID: "google-r5-explicit-source", ConversationID: r5GoogleConversation,
			SenderName: "R5 Google Friend", SenderNumber: "+15550100001",
			Body: "google second incoming byte-exact", TimestampMS: base + 2_000,
			Status: "RCS_DELIVERED", SourcePlatform: "sms", SourceID: "google-remote-r5-explicit",
		},
		{
			MessageID: "google-r5-outgoing", ConversationID: r5GoogleConversation,
			SenderName: "Me", SenderNumber: "+15550009999", Body: "google outgoing byte-exact",
			TimestampMS: base + 3_000, Status: "sent", IsFromMe: true, SourcePlatform: "sms",
		},
		{
			MessageID: "whatsapp:wa-r5-lid", ConversationID: r5WhatsAppLID,
			SenderName: "R5 WhatsApp Friend", SenderNumber: "14155550200@s.whatsapp.net",
			Body: r5SharedSearchToken + " whatsapp incoming media", TimestampMS: base + 4_000,
			Status: "delivered", SourcePlatform: "whatsapp",
			// Source IDs win verbatim under R2, including a retained platform prefix.
			SourceID: "whatsapp:wa-r5-explicit-source", MediaID: whatsAppMediaRef, MimeType: "image/webp",
		},
		{
			MessageID: "whatsapp:wa-r5-outgoing", ConversationID: r5WhatsAppConversation,
			SenderName: "Me", Body: "whatsapp outgoing byte-exact", TimestampMS: base + 5_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "whatsapp",
		},
		{
			MessageID: "signal:1700000001000", ConversationID: r5SignalConversation,
			SenderName: "R5 Signal ACI Friend", SenderNumber: r5SignalACI,
			Body: r5SharedSearchToken + " signal incoming", TimestampMS: base + 6_000,
			Status: "delivered", SourcePlatform: "signal",
		},
		{
			MessageID: "signal:local:abc123r5", ConversationID: r5SignalConversation,
			SenderName: "Me", Body: "signal fabricated outgoing byte-exact", TimestampMS: base + 7_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "signal",
		},
		{
			MessageID: "signal:1700000002000", ConversationID: r5SignalGroup,
			SenderName: "R5 Signal Group Friend", SenderNumber: "+15550300003",
			Body: "signal group incoming byte-exact", TimestampMS: base + 8_000,
			Status: "delivered", SourcePlatform: "signal",
		},
		{
			MessageID: "gchat:r5-message-1", ConversationID: r5GChatConversation,
			SenderName: "R5 GChat Friend", SenderNumber: "friend-r5@example.com",
			Body: r5SharedSearchToken + " gchat incoming", TimestampMS: base + 9_000,
			Status: "delivered", SourcePlatform: "gchat",
		},
		{
			MessageID: "gchat:r5-message-2", ConversationID: r5GChatConversation,
			SenderName: "Me", Body: "gchat outgoing byte-exact", TimestampMS: base + 10_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "gchat",
		},
		{
			MessageID: "imessage:r5-message-1", ConversationID: r5IMessageConversation,
			SenderName: "R5 iMessage Friend", SenderNumber: "+15550400004",
			Body: r5SharedSearchToken + " imessage incoming", TimestampMS: base + 11_000,
			Status: "delivered", SourcePlatform: "imessage",
		},
		{
			MessageID: "imessage:r5-message-2", ConversationID: r5IMessageConversation,
			SenderName: "Me", Body: "imessage outgoing byte-exact", TimestampMS: base + 12_000,
			Status: "sent", IsFromMe: true, SourcePlatform: "imessage",
		},
	}
	for _, message := range legacyMessages {
		if err := store.UpsertMessage(message); err != nil {
			t.Fatalf("seed message %q: %v", message.MessageID, err)
		}
	}
	if err := store.MergeConversationIDs(r5WhatsAppLID, r5WhatsAppConversation); err != nil {
		t.Fatalf("merge WhatsApp @lid conversation: %v", err)
	}

	if err := store.UpsertUnifiedContact(&db.UnifiedContact{
		UnifiedID:   "r5-cross-platform-person",
		DisplayName: "R5 Unified Friend",
		Identifiers: `[{"platform":"sms","value":"+15550100001"},{"platform":"signal","value":"` + r5SignalACI + `"}]`,
	}); err != nil {
		t.Fatalf("seed unified contact: %v", err)
	}

	scheduledMedia := []byte("r5 scheduled media bytes\x00\x01")
	scheduleBase := time.Now().UTC().Add(24 * time.Hour).UnixMilli()
	scheduled := []*db.ScheduledMessage{
		{
			ID: "r5-pending-text", ConversationID: r5GoogleConversation,
			Body: "r5 pending scheduled text", SendAt: scheduleBase, Status: db.ScheduleStatusPending,
			CreatedAt: base + 20_000,
		},
		{
			ID: "r5-pending-media", ConversationID: r5WhatsAppConversation,
			Body: "r5 pending scheduled media caption", ReplyToID: "whatsapp:wa-r5-lid",
			SendAt: scheduleBase + 1_000, Status: db.ScheduleStatusPending, CreatedAt: base + 21_000,
			MediaData: scheduledMedia, MediaFilename: "r5-scheduled.bin", MediaMime: "application/octet-stream",
		},
		{
			ID: "r5-sending", ConversationID: r5SignalConversation,
			Body: "r5 ambiguous sending", SendAt: scheduleBase + 2_000, Status: db.ScheduleStatusSending,
			CreatedAt: base + 22_000,
		},
		{
			ID: "r5-sent", ConversationID: r5GoogleConversation,
			Body: "r5 already sent", SendAt: scheduleBase + 3_000, Status: db.ScheduleStatusSent,
			CreatedAt: base + 23_000, SentMessageID: "google-r5-outgoing",
		},
		{
			ID: "r5-canceled", ConversationID: r5GChatConversation,
			Body: "r5 canceled", SendAt: scheduleBase + 4_000, Status: db.ScheduleStatusCanceled,
			CreatedAt: base + 24_000,
		},
	}
	for _, message := range scheduled {
		if err := store.CreateScheduledMessage(message); err != nil {
			t.Fatalf("seed scheduled message %q: %v", message.ID, err)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close/checkpoint R5 legacy fixture: %v", err)
	}
	closed = true

	fixture := r5LegacyFixture{
		DataDir:           dataDir,
		StorePath:         storePath,
		BaseMS:            base,
		TotalMessages:     int64(len(legacyMessages)),
		ConversationCount: 6,
		PlatformCounts: map[string]int64{
			"sms": 3, "whatsapp": 2, "signal": 3, "gchat": 2, "imessage": 2,
		},
		ScheduledMedia: scheduledMedia,
		Conversations: []r5FixtureConversation{
			{
				LegacyID: r5GoogleConversation, RemoteID: r5GoogleConversation,
				AccountID: r5GoogleAccountID, Platform: "sms", Name: "R5 Google Friend",
				Messages: []r5FixtureMessage{
					{LegacyID: "google-r5-empty-source", RemoteID: "google-r5-empty-source", Body: r5SharedSearchToken + " google incoming media", SenderName: "R5 Unified Friend", SenderValue: "+15550100001"},
					{LegacyID: "google-r5-explicit-source", RemoteID: "google-remote-r5-explicit", Body: "google second incoming byte-exact", SenderName: "R5 Unified Friend", SenderValue: "+15550100001"},
					{LegacyID: "google-r5-outgoing", RemoteID: "google-r5-outgoing", Body: "google outgoing byte-exact", FromMe: true},
				},
			},
			{
				LegacyID: r5WhatsAppConversation, RemoteID: r5WhatsAppConversation,
				AccountID: r5WhatsAppAccountID, Platform: "whatsapp", Name: "R5 WhatsApp Friend",
				Messages: []r5FixtureMessage{
					{LegacyID: "whatsapp:wa-r5-lid", RemoteID: "whatsapp:wa-r5-explicit-source", Body: r5SharedSearchToken + " whatsapp incoming media", SenderName: "R5 WhatsApp Friend", SenderValue: "14155550200@s.whatsapp.net"},
					{LegacyID: "whatsapp:wa-r5-outgoing", RemoteID: "wa-r5-outgoing", Body: "whatsapp outgoing byte-exact", FromMe: true},
				},
			},
			{
				LegacyID: r5SignalConversation, RemoteID: r5SignalConversation,
				AccountID: r5SignalAccountID, Platform: "signal", Name: "R5 Signal ACI Friend",
				Messages: []r5FixtureMessage{
					{LegacyID: "signal:1700000001000", RemoteID: "1700000001000", Body: r5SharedSearchToken + " signal incoming", SenderName: "R5 Unified Friend", SenderValue: r5SignalACI},
					{LegacyID: "signal:local:abc123r5", RemoteID: "local:abc123r5", Body: "signal fabricated outgoing byte-exact", FromMe: true},
				},
			},
			{
				LegacyID: r5SignalGroup, RemoteID: r5SignalGroup,
				AccountID: r5SignalAccountID, Platform: "signal", Name: "R5 Signal Group",
				Messages: []r5FixtureMessage{
					{LegacyID: "signal:1700000002000", RemoteID: "1700000002000", Body: "signal group incoming byte-exact", SenderName: "R5 Signal Group Friend", SenderValue: "+15550300003"},
				},
			},
			{
				LegacyID: r5GChatConversation, RemoteID: r5GChatConversation,
				AccountID: r5GChatAccountID, Platform: "gchat", Name: "R5 GChat Archive",
				Messages: []r5FixtureMessage{
					{LegacyID: "gchat:r5-message-1", RemoteID: "r5-message-1", Body: r5SharedSearchToken + " gchat incoming", SenderName: "R5 GChat Friend", SenderValue: "friend-r5@example.com"},
					{LegacyID: "gchat:r5-message-2", RemoteID: "r5-message-2", Body: "gchat outgoing byte-exact", FromMe: true},
				},
			},
			{
				LegacyID: r5IMessageConversation, RemoteID: r5IMessageConversation,
				AccountID: r5IMessageAccountID, Platform: "imessage", Name: "R5 iMessage Archive",
				Messages: []r5FixtureMessage{
					{LegacyID: "imessage:r5-message-1", RemoteID: "r5-message-1", Body: r5SharedSearchToken + " imessage incoming", SenderName: "R5 iMessage Friend", SenderValue: "+15550400004"},
					{LegacyID: "imessage:r5-message-2", RemoteID: "r5-message-2", Body: "imessage outgoing byte-exact", FromMe: true},
				},
			},
		},
	}
	return fixture
}

func r5BackupDependencies(now time.Time) backupDependencies {
	deps := defaultBackupDependencies()
	deps.now = func() time.Time { return now }
	deps.version = "r5-backend-it"
	deps.commit = "r5-backend-it"
	deps.modified = false
	deps.probeURL = "http://127.0.0.1:7007/api/status"
	deps.httpClient = &http.Client{Transport: r5RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	})}
	deps.availableBytes = func(string) (uint64, error) { return math.MaxUint64, nil }
	return deps
}

func r5MigrateDependencies(now time.Time) migrateDependencies {
	deps := defaultMigrateDependencies()
	deps.now = func() time.Time { return now }
	deps.version = "r5-backend-it"
	deps.commit = "r5-backend-it"
	deps.probeURL = "http://127.0.0.1:7007/api/status"
	deps.httpClient = &http.Client{Transport: r5RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	})}
	deps.availableBytes = func(string) (uint64, error) { return math.MaxUint64, nil }
	deps.transform = migration.Transform
	return deps
}

type r5RoundTripperFunc func(*http.Request) (*http.Response, error)

func (function r5RoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func runR5Backup(t *testing.T, dataDir string, now time.Time) backupCommandOutput {
	t.Helper()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	destination := filepath.Join(dataDir, "migration-backups", now.UTC().Format("20060102T150405Z"))
	var stdout, stderr bytes.Buffer
	if err := runBackupCommand(
		context.Background(),
		[]string{"--to", destination, "--json"},
		r5BackupDependencies(now),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("run real backup: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	var output backupCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode backup command output: %v\n%s", err, stdout.String())
	}
	if !output.OK || output.ExitCode != 0 || output.ManifestPath == "" || output.BackupPath == "" {
		t.Fatalf("backup command output = %+v", output)
	}
	return output
}

func runR5Migrate(
	t *testing.T,
	sourceDir string,
	targetDir string,
	check bool,
	now time.Time,
) migration.Report {
	t.Helper()
	args := []string{"--from", sourceDir, "--to", targetDir, "--json"}
	if check {
		args = append([]string{"--check"}, args...)
	}
	var stdout, stderr bytes.Buffer
	if err := runMigrateCommand(
		context.Background(), args, r5MigrateDependencies(now), &stdout, &stderr,
	); err != nil {
		t.Fatalf("run real migrate (check=%v): %v\nstderr:\n%s\nstdout:\n%s", check, err, stderr.String(), stdout.String())
	}
	var report migration.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode migration integrity report: %v\n%s", err, stdout.String())
	}
	return report
}

func TestR5BackendIntegrationMigratedData(t *testing.T) {
	fixture := buildR5LegacyFixture(t)
	now := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)

	t.Run("real backup", func(t *testing.T) {
		output := runR5Backup(t, fixture.DataDir, now)
		if output.Counts == nil || output.Counts.Conversations != int64(fixture.ConversationCount) ||
			output.Counts.Messages != fixture.TotalMessages || output.Counts.UnifiedContacts != 1 ||
			output.Counts.ScheduledMessages != 5 {
			t.Fatalf("backup counts = %+v", output.Counts)
		}
		manifestBytes, err := os.ReadFile(output.ManifestPath)
		if err != nil {
			t.Fatalf("read backup manifest: %v", err)
		}
		var manifest backupManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatalf("decode backup manifest: %v", err)
		}
		if manifest.Source.QuickCheck != "ok" {
			t.Fatalf("backup source quick_check = %q, want ok", manifest.Source.QuickCheck)
		}
		backupDB, err := db.New(filepath.Join(output.BackupPath, legacyDatabaseName))
		if err != nil {
			t.Fatalf("open VACUUM INTO backup: %v", err)
		}
		messages, err := backupDB.MessageCount("")
		closeErr := backupDB.Close()
		if err != nil || closeErr != nil || int64(messages) != fixture.TotalMessages {
			t.Fatalf("backup message count/close = %d, %v / %v", messages, err, closeErr)
		}
	})

	targetDir := filepath.Join(fixture.DataDir, "v2")
	checkReport := runR5Migrate(t, fixture.DataDir, targetDir, true, now)
	assertR5IntegrityReport(t, checkReport, fixture, true)
	if _, err := os.Stat(filepath.Join(targetDir, v2StoreName)); !os.IsNotExist(err) {
		t.Fatalf("migrate --check published canonical store: %v", err)
	}

	publishReport := runR5Migrate(t, fixture.DataDir, targetDir, false, now)
	assertR5IntegrityReport(t, publishReport, fixture, false)
	if _, err := os.Stat(filepath.Join(targetDir, v2StoreName)); err != nil {
		t.Fatalf("published v2 store missing: %v", err)
	}

	t.Setenv("OPENMESSAGES_DATA_DIR", fixture.DataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	// v2StackDeps intentionally remains production-adapter-typed in R5-1. Open
	// the real migrated stack first, then use its public registration seam for
	// the hermetic adapters.
	stack, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("open migrated v2 stack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Store.Close() })

	google := scripted.New(r5GoogleAccountID, bridge.PlatformGoogle)
	whatsApp := scripted.New(r5WhatsAppAccountID, bridge.PlatformWhatsApp)
	signal := scripted.New(r5SignalAccountID, bridge.PlatformSignal)
	for _, adapter := range []bridge.Adapter{google, whatsApp, signal} {
		if err := stack.RegisterAdapter(adapter); err != nil {
			t.Fatalf("register scripted %s adapter: %v", adapter.Platform(), err)
		}
	}

	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create inert legacy store for v2-primary surfaces: %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	stopStack := stack.Start(ctx, legacy, nil, true)
	t.Cleanup(func() {
		stopStack()
		cancel()
	})

	reads := v2read.New(stack.Store)
	assertR5ReadParity(t, reads, fixture)

	shared, err := reads.SearchMessagesFiltered(r5SharedSearchToken, db.SearchFilter{Limit: 50})
	if err != nil {
		t.Fatalf("search migrated v2 data: %v", err)
	}
	if len(shared) != 5 {
		t.Fatalf("shared-token search count = %d, want one result on each platform", len(shared))
	}
	platforms := map[string]bool{}
	for _, message := range shared {
		platforms[message.SourcePlatform] = true
	}
	for _, platform := range []string{"sms", "whatsapp", "signal", "gchat", "imessage"} {
		if !platforms[platform] {
			t.Errorf("shared-token search omitted platform %q", platform)
		}
	}

	google.EnqueueTextResult(bridge.SendResult{
		RemoteMessageID: "gm-r5-permanent", AcceptedAt: time.Now(), EchoExpected: true,
	})
	googleConversation := fixture.Conversations[0]
	submission, err := v2wire.SubmitTextV2(context.Background(), v2wire.NativeDeps{
		V2: stack.Store, Service: stack.Service, Registry: stack.Registry,
	}, v2wire.TextInput{
		ConversationID: googleConversation.V2ID(),
		Body:           "r5 scripted confirmed send",
		IdempotencyKey: "r5-backend-it-send",
	})
	if err != nil {
		t.Fatalf("submit v2-native scripted send: %v", err)
	}
	waitR5(t, "scripted send confirmation", func() bool {
		delivery, getErr := stack.Service.Get(context.Background(), submission.OutboxID)
		return getErr == nil && delivery.State == messaging.OutboxConfirmed &&
			delivery.RemoteMessageID == "gm-r5-permanent"
	})
	waitR5(t, "confirmed send visible through v2 read source", func() bool {
		messages, readErr := reads.GetMessagesByConversation(googleConversation.V2ID(), 100)
		if readErr != nil {
			return false
		}
		for _, message := range messages {
			if message.Body == "r5 scripted confirmed send" && message.SourceID == "gm-r5-permanent" && message.IsFromMe {
				return true
			}
		}
		return false
	})
	if requests := google.TextRequests(); len(requests) != 1 ||
		requests[0].Conversation.RemoteID != r5GoogleConversation ||
		requests[0].Body != "r5 scripted confirmed send" {
		t.Fatalf("scripted Google text requests = %+v", requests)
	}

	ingestBodies := map[string]string{
		"sms":      "r5 google ingest projected",
		"whatsapp": "r5 whatsapp ingest projected",
		"signal":   "r5 signal ingest projected",
	}
	for _, record := range r5IngressRecords(t, fixture.BaseMS, ingestBodies) {
		if err := stack.Sink.AppendIngress(context.Background(), record); err != nil {
			t.Fatalf("append %s ingress frame: %v", record.Codec, err)
		}
	}
	for platform, body := range ingestBodies {
		platform, body := platform, body
		t.Run("ingest "+platform, func(t *testing.T) {
			waitR5(t, platform+" ingress projection", func() bool {
				messages, searchErr := reads.SearchMessagesFiltered(body, db.SearchFilter{Limit: 10})
				return searchErr == nil && len(messages) == 1 && messages[0].Body == body &&
					messages[0].SourcePlatform == platform && !messages[0].IsFromMe
			})
		})
	}

	webHandler := web.APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, web.APIOptions{
		Reads:     reads,
		V2Primary: true,
		V2: &web.V2Options{
			Service: stack.Service, Media: stack.Media, V2Store: stack.Store,
			Blobs: stack.Blobs, Registry: stack.Registry,
		},
	})
	downloadedMedia := []byte("r5 scripted migrated attachment bytes\x00\x01")
	google.EnqueueDownload(downloadedMedia, "r5-google.jpg", "image/jpeg")
	assertR5HTTPSurfaces(t, webHandler, reads, fixture, downloadedMedia, google)
	assertR5MCPReadSurfaces(t, legacy, reads, fixture)

	var cliBanner bytes.Buffer
	cliSession, err := openCommandReadSource(zerolog.Nop(), &cliBanner)
	if err != nil {
		t.Fatalf("open v2-primary CLI read source: %v", err)
	}
	defer cliSession.Close()
	if cliBanner.String() != "reading v2 store\n" {
		t.Fatalf("v2 CLI banner = %q", cliBanner.String())
	}
	cliResults, err := cliSession.Reads.SearchMessagesFiltered(r5SharedSearchToken, db.SearchFilter{Limit: 50})
	if err != nil || len(cliResults) != len(shared) {
		t.Fatalf("CLI v2 read results = %d, %v; want %d", len(cliResults), err, len(shared))
	}
}

func assertR5IntegrityReport(
	t *testing.T,
	report migration.Report,
	fixture r5LegacyFixture,
	check bool,
) {
	t.Helper()
	if report.Format != migration.ReportFormat || !report.OK || report.Check != check ||
		!report.Validation.Passed || report.Target.Published == check {
		t.Fatalf("migration report header = format=%d ok=%v check=%v validation=%v published=%v",
			report.Format, report.OK, report.Check, report.Validation.Passed, report.Target.Published)
	}
	messages := report.TableCounts["messages"]
	if messages.Legacy != fixture.TotalMessages || messages.V2 != fixture.TotalMessages ||
		messages.Reconciled != fixture.TotalMessages || messages.ReconciliationRatio != 1 {
		t.Errorf("message reconciliation = %+v, want %d/%d ratio 1", messages, fixture.TotalMessages, fixture.TotalMessages)
	}
	if len(report.MessageCollisions) != 0 {
		t.Errorf("message collisions = %+v, want none", report.MessageCollisions)
	}
	wantAccounts := map[string]string{
		"sms": r5GoogleAccountID, "whatsapp": r5WhatsAppAccountID,
		"signal": r5SignalAccountID, "gchat": r5GChatAccountID, "imessage": r5IMessageAccountID,
	}
	for platform, want := range fixture.PlatformCounts {
		got := report.PlatformCounts[platform]
		if got.AccountID != wantAccounts[platform] || got.Legacy != want || got.V2 != want ||
			got.ReconciliationRatio != 1 {
			t.Errorf("platform reconciliation %s = %+v, want account %q and %d/%d ratio 1",
				platform, got, wantAccounts[platform], want, want)
		}
	}
	if report.Identities.Created != 8 || report.Identities.PeopleCreated != 1 ||
		report.Identities.LinksByProvenance["explicit"] != 2 ||
		report.Identities.UnunifiedIdentities != 6 {
		t.Errorf("identity report = %+v", report.Identities)
	}
	// The current transformer defines PendingToOutbox as every pending row,
	// including the one media row; the exact fixture total is therefore two.
	if report.Schedule.LegacyTotal != 5 || report.Schedule.PendingToOutbox != 2 ||
		report.Schedule.PendingMedia != 1 || report.Schedule.AmbiguousSending != 1 ||
		report.Schedule.SkippedSent != 1 || report.Schedule.SkippedCanceled != 1 ||
		report.Schedule.SkippedFailed != 0 || report.Schedule.OptimisticOnlyRows != 1 {
		t.Errorf("schedule report = %+v", report.Schedule)
	}
	if report.Dropped.ReactionsBearingMessages != 1 {
		t.Errorf("reactions-bearing messages = %d, want 1", report.Dropped.ReactionsBearingMessages)
	}
	if report.ReadState.ConversationsWithUnreadCount != 1 || report.ReadState.LossyWarning == "" {
		t.Errorf("read-state report = %+v", report.ReadState)
	}
	if report.SignalLocalRows != 1 {
		t.Errorf("signal_local_rows = %d, want 1", report.SignalLocalRows)
	}
	if report.Target.Counts["accounts"] != 5 ||
		report.Target.Counts["conversations"] != int64(fixture.ConversationCount) ||
		report.Target.Counts["messages"] != fixture.TotalMessages+3 ||
		report.Target.Counts["outbox"] != 2 || report.Target.Counts["outbox_attachments"] != 1 {
		t.Errorf("target counts = %+v", report.Target.Counts)
	}
	if report.Target.Counts["identities"] != 8 || report.Target.Counts["conversation_participants"] != 8 ||
		report.Target.Counts["message_attachments"] != 2 {
		t.Errorf("identity/participant/attachment target counts = %+v", report.Target.Counts)
	}
	if report.Media.LegacyAttachments != 2 || report.Media.PendingAttachments != 2 ||
		report.Media.WhatsAppUnresolvable != 0 || report.Media.ScheduledBlobsVerified != 1 {
		t.Errorf("media report = %+v", report.Media)
	}
	if report.Validation.QuickCheck != "ok" || len(report.Validation.ForeignKeyViolations) != 0 ||
		!report.Validation.CountsMatched || !report.Validation.SampledHashesMatched ||
		!report.Validation.BlobReferencesValid || !report.Validation.SourceUnchanged {
		t.Errorf("validation report = %+v", report.Validation)
	}
	if len(report.SampledHashes) != int(fixture.TotalMessages) {
		t.Errorf("sampled hashes = %d, want all %d fixture messages", len(report.SampledHashes), fixture.TotalMessages)
	}
	for _, sample := range report.SampledHashes {
		if !sample.Matched || sample.ExpectedSHA256 != sample.ActualSHA256 {
			t.Errorf("sampled hash mismatch: %+v", sample)
		}
	}
}

func assertR5ReadParity(t *testing.T, reads *v2read.Source, fixture r5LegacyFixture) {
	t.Helper()
	conversations, err := reads.ListConversations(50)
	if err != nil {
		t.Fatalf("list migrated conversations: %v", err)
	}
	if len(conversations) != fixture.ConversationCount {
		t.Fatalf("migrated conversation count = %d, want %d", len(conversations), fixture.ConversationCount)
	}
	for index := 1; index < len(conversations); index++ {
		previous, current := conversations[index-1], conversations[index]
		if previous.LastMessageTS < current.LastMessageTS ||
			(previous.LastMessageTS == current.LastMessageTS && previous.ConversationID > current.ConversationID) {
			t.Errorf("conversation recency order at %d: %+v before %+v", index, previous, current)
		}
	}
	whatsAppCount := 0
	for _, conversation := range conversations {
		if conversation.SourcePlatform == "whatsapp" {
			whatsAppCount++
		}
	}
	if whatsAppCount != 1 {
		t.Fatalf("migrated @lid-merged WhatsApp conversations = %d, want one", whatsAppCount)
	}

	for _, expectedConversation := range fixture.Conversations {
		gotConversation, err := reads.GetConversation(expectedConversation.V2ID())
		if err != nil {
			t.Fatalf("get migrated conversation %q: %v", expectedConversation.LegacyID, err)
		}
		if gotConversation.Name != expectedConversation.Name || gotConversation.SourcePlatform != expectedConversation.Platform {
			t.Errorf("conversation %q = %+v", expectedConversation.LegacyID, gotConversation)
		}
		gotMessages, err := reads.GetMessagesByConversation(expectedConversation.V2ID(), 100)
		if err != nil {
			t.Fatalf("read migrated conversation %q: %v", expectedConversation.LegacyID, err)
		}
		// Pending schedules add optimistic messages to the Google and WhatsApp
		// threads, and the ambiguous Signal sending row adds the report-only
		// optimistic message. Pin those known additions per thread so a missing
		// history row cannot hide behind the global reconciliation count.
		extraOptimistic := map[string]int{
			r5GoogleConversation:   1,
			r5WhatsAppConversation: 1,
			r5SignalConversation:   1,
		}[expectedConversation.LegacyID]
		if len(gotMessages) != len(expectedConversation.Messages)+extraOptimistic {
			t.Errorf("conversation %q message count = %d, want %d history + %d known optimistic",
				expectedConversation.LegacyID, len(gotMessages), len(expectedConversation.Messages), extraOptimistic)
		}
		byRemote := make(map[string]*db.Message, len(gotMessages))
		for _, message := range gotMessages {
			byRemote[message.SourceID] = message
		}
		for _, expected := range expectedConversation.Messages {
			got := byRemote[expected.RemoteID]
			if got == nil {
				t.Errorf("conversation %q missing remote message %q; got %+v", expectedConversation.LegacyID, expected.RemoteID, sortedR5Keys(byRemote))
				continue
			}
			if got.Body != expected.Body || got.IsFromMe != expected.FromMe ||
				got.SourcePlatform != expectedConversation.Platform || got.SourceID != expected.RemoteID {
				t.Errorf("message parity %q = body %q from_me=%v platform=%q source=%q; want %q/%v/%q/%q",
					expected.LegacyID, got.Body, got.IsFromMe, got.SourcePlatform, got.SourceID,
					expected.Body, expected.FromMe, expectedConversation.Platform, expected.RemoteID)
			}
			wantMessageID := v2keys.DeriveID(
				"message",
				expectedConversation.AccountID,
				expectedConversation.LegacyID+"\x1f"+expected.RemoteID,
			)
			if got.MessageID != wantMessageID {
				t.Errorf("message ID mapping %q = %q, want %q", expected.LegacyID, got.MessageID, wantMessageID)
			}
			if !expected.FromMe && (got.SenderNumber != expected.SenderValue || got.SenderName != expected.SenderName) {
				t.Errorf("message sender parity %q = %q/%q, want %q/%q",
					expected.LegacyID, got.SenderName, got.SenderNumber, expected.SenderName, expected.SenderValue)
			}
		}
	}
}

func sortedR5Keys(values map[string]*db.Message) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func r5IngressRecords(t *testing.T, baseMS int64, bodies map[string]string) []bridge.RawIngressRecord {
	t.Helper()

	googleMessage := &gmproto.Message{
		MessageID:      "google-r5-ingest-remote",
		ConversationID: r5GoogleConversation,
		Timestamp:      (baseMS + 30_000) * 1_000,
		SenderParticipant: &gmproto.Participant{
			FullName: "R5 Google Ingest Sender",
			ID:       &gmproto.SmallInfo{Number: "+15550900001"},
		},
		MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: bodies["sms"]},
			},
		}},
	}
	googlePayload, googleProto, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{
		Message: googleMessage,
	})
	if err != nil {
		t.Fatalf("marshal Google ingress fixture: %v", err)
	}
	googleHash := sha256.Sum256(googleProto)

	whatsAppProto, err := proto.Marshal(&waE2E.Message{Conversation: proto.String(bodies["whatsapp"])})
	if err != nil {
		t.Fatalf("marshal WhatsApp ingress protobuf: %v", err)
	}
	whatsAppPayload, err := json.Marshal(map[string]any{
		"kind": "message", "chat_jid": "14155550100@s.whatsapp.net",
		"sender_jid": "14155550200@s.whatsapp.net", "message_id": "wa-r5-ingest-remote",
		"timestamp_ms": baseMS + 31_000, "is_from_me": false, "is_group": false,
		"push_name": "R5 WhatsApp Ingest Sender", "proto_b64": whatsAppProto,
	})
	if err != nil {
		t.Fatalf("marshal WhatsApp ingress envelope: %v", err)
	}

	signalTimestamp := baseMS + 32_000
	signalLine := []byte(fmt.Sprintf(
		`{"account":"+15550009999","envelope":{"sourceServiceId":%q,"sourceName":"R5 Signal Ingest Sender","timestamp":%d,"dataMessage":{"timestamp":%d,"message":%q}}}`,
		r5SignalACI, signalTimestamp, signalTimestamp, bodies["signal"],
	))
	signalRecord, ephemeral, err := ingest.BuildSignalIngress(
		r5SignalAccountID, 1, "+15550009999", signalLine, "", "", time.UnixMilli(signalTimestamp),
	)
	if err != nil || signalRecord == nil || ephemeral != nil {
		t.Fatalf("build Signal ingress fixture = record %v ephemeral %v error %v", signalRecord, ephemeral, err)
	}

	return []bridge.RawIngressRecord{
		{
			AccountID: r5GoogleAccountID, Generation: 1,
			DedupeKey: "msg:google-r5-ingest-remote:" + hex.EncodeToString(googleHash[:4]),
			Codec:     ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion,
			ReceivedAt: time.UnixMilli(baseMS + 30_000), Payload: googlePayload,
		},
		{
			AccountID: r5WhatsAppAccountID, Generation: 1, DedupeKey: "msg:wa-r5-ingest-remote:r5",
			Codec: ingest.WhatsAppCodec, CodecVersion: ingest.WhatsAppCodecVersion,
			ReceivedAt: time.UnixMilli(baseMS + 31_000), Payload: whatsAppPayload,
		},
		*signalRecord,
	}
}

func assertR5HTTPSurfaces(
	t *testing.T,
	handler http.Handler,
	reads *v2read.Source,
	fixture r5LegacyFixture,
	downloadedMedia []byte,
	google *scripted.Adapter,
) {
	t.Helper()
	request := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		return recorder
	}

	conversations := request("/api/conversations?limit=50")
	if conversations.Code != http.StatusOK {
		t.Fatalf("HTTP list conversations = %d: %s", conversations.Code, conversations.Body.String())
	}
	var conversationPayload []*db.Conversation
	if err := json.Unmarshal(conversations.Body.Bytes(), &conversationPayload); err != nil ||
		len(conversationPayload) != fixture.ConversationCount {
		t.Fatalf("HTTP conversation payload = %d, %v: %s", len(conversationPayload), err, conversations.Body.String())
	}

	googleFixture := fixture.Conversations[0]
	messages := request("/api/conversations/" + googleFixture.V2ID() + "/messages?limit=100")
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), googleFixture.Messages[0].Body) {
		t.Fatalf("HTTP conversation messages = %d: %s", messages.Code, messages.Body.String())
	}
	search := request("/api/search?q=" + r5SharedSearchToken + "&limit=50")
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), r5SharedSearchToken) {
		t.Fatalf("HTTP search = %d: %s", search.Code, search.Body.String())
	}

	mediaMessages, err := reads.SearchMessagesFiltered(r5SharedSearchToken+" google incoming media", db.SearchFilter{Limit: 10})
	if err != nil || len(mediaMessages) != 1 || mediaMessages[0].MediaID == "" {
		t.Fatalf("locate migrated Google media message = %+v, %v", mediaMessages, err)
	}
	media := request("/api/media/" + mediaMessages[0].MessageID)
	if media.Code != http.StatusOK || !bytes.Equal(media.Body.Bytes(), downloadedMedia) {
		t.Fatalf("HTTP migrated media = %d %q, want %q", media.Code, media.Body.Bytes(), downloadedMedia)
	}
	if calls := google.DownloadRequests(); len(calls) != 1 || calls[0].Ref.RemoteID != "google-r5-media" {
		t.Fatalf("scripted migrated media download calls = %+v", calls)
	}
	// The second read must be blob-local; the scripted queue is empty and a
	// second transport call would fail this assertion or the response itself.
	mediaFromBlob := request("/api/media/" + mediaMessages[0].MessageID)
	if mediaFromBlob.Code != http.StatusOK || !bytes.Equal(mediaFromBlob.Body.Bytes(), downloadedMedia) ||
		len(google.DownloadRequests()) != 1 {
		t.Fatalf("HTTP persisted v2 blob = %d %q, downloads=%d", mediaFromBlob.Code, mediaFromBlob.Body.Bytes(), len(google.DownloadRequests()))
	}

	// The scheduled media blob is migrated into outbox_attachments rather than
	// message_attachments; exercise its canonical v1 outbox download as well.
	outboxID := v2keys.DeriveID("outbox", r5WhatsAppAccountID, "r5-pending-media")
	scheduledMedia := request("/api/v1/outbox/" + outboxID + "/media")
	if scheduledMedia.Code != http.StatusOK || !bytes.Equal(scheduledMedia.Body.Bytes(), fixture.ScheduledMedia) {
		t.Fatalf("HTTP migrated scheduled outbox media = %d %q, want %q", scheduledMedia.Code, scheduledMedia.Body.Bytes(), fixture.ScheduledMedia)
	}
}

func assertR5MCPReadSurfaces(
	t *testing.T,
	legacy *db.Store,
	reads *v2read.Source,
	fixture r5LegacyFixture,
) {
	t.Helper()
	a := &app.App{Store: legacy, Logger: zerolog.Nop()}
	server := mcpserver.NewMCPServer("r5-backend-it", "test")
	omtools.RegisterWithOptions(server, a, omtools.Options{Reads: reads, V2Primary: true})

	checks := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "list_conversations", args: map[string]any{"limit": float64(50)}, want: fixture.Conversations[0].Name},
		{name: "get_messages", args: map[string]any{"limit": float64(100)}, want: "r5 google ingest projected"},
		{name: "search_messages", args: map[string]any{"query": r5SharedSearchToken, "limit": float64(50)}, want: r5SharedSearchToken},
		{name: "get_status", args: map[string]any{}, want: "Serving store (v2)"},
	}
	for _, check := range checks {
		registered := server.GetTool(check.name)
		if registered == nil {
			t.Fatalf("MCP tool %q was not registered", check.name)
		}
		request := mcp.CallToolRequest{}
		request.Params.Arguments = check.args
		result, err := registered.Handler(context.Background(), request)
		if err != nil {
			t.Fatalf("MCP %s: %v", check.name, err)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil || result.IsError || !strings.Contains(string(encoded), check.want) {
			t.Fatalf("MCP %s result = %s, marshal=%v error=%v; want %q", check.name, encoded, marshalErr, result.IsError, check.want)
		}
	}
}

func waitR5(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
