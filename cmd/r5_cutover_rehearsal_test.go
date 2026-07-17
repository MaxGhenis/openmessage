package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/cutover"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

var r5CutoverNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func TestCutoverRehearsal(t *testing.T) {
	ctx := context.Background()
	fixture := buildR5LegacyFixture(t)
	googleConversation := fixture.Conversations[0]

	// R8 preflight: allow the shadow dispatcher to settle a confirmed send and
	// prove its projector made that send visible in the legacy cutover source.
	legacy, err := db.New(fixture.StorePath)
	if err != nil {
		t.Fatalf("open legacy fixture for projector drain: %v", err)
	}
	legacyClosed := false
	t.Cleanup(func() {
		if !legacyClosed {
			_ = legacy.Close()
		}
	})
	oldStack, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("create pre-cutover shadow v2 stack: %v", err)
	}
	google := scripted.New(r5GoogleAccountID, bridge.PlatformGoogle)
	if err := oldStack.RegisterAdapter(google); err != nil {
		_ = oldStack.Store.Close()
		t.Fatalf("register pre-cutover scripted Google adapter: %v", err)
	}
	google.EnqueueTextResult(bridge.SendResult{
		RemoteMessageID: "r5-preflight-confirmed-remote",
		AcceptedAt:      time.Now(),
		EchoExpected:    true,
	})
	confirmed, err := v2wire.SubmitText(ctx, v2wire.Deps{
		Legacy: legacy, V2: oldStack.Store, Service: oldStack.Service, Registry: oldStack.Registry,
	}, v2wire.TextInput{
		ConversationID: googleConversation.LegacyID,
		Body:           "r5 projector drain sentinel",
		IdempotencyKey: "r5-projector-drain",
	})
	if err != nil {
		_ = oldStack.Store.Close()
		t.Fatalf("submit pre-cutover v2 send: %v", err)
	}
	shadowCtx, cancelShadow := context.WithCancel(context.Background())
	stopShadow := oldStack.Start(shadowCtx, legacy, nil, false)
	shadowStopped := false
	t.Cleanup(func() {
		if !shadowStopped {
			stopShadow()
		}
		cancelShadow()
	})
	waitR5(t, "pre-cutover send confirmation", func() bool {
		delivery, getErr := oldStack.Service.Get(ctx, confirmed.OutboxID)
		return getErr == nil && delivery.State == messaging.OutboxConfirmed
	})
	waitR5(t, "pre-cutover projector drain into legacy", func() bool {
		messages, readErr := legacy.GetMessagesByConversation(googleConversation.LegacyID, 100)
		if readErr != nil {
			return false
		}
		for _, message := range messages {
			if message.Body == "r5 projector drain sentinel" && message.IsFromMe {
				return true
			}
		}
		return false
	})
	oldOutbox, err := sqlite.NewOutboxRepository(oldStack.Store, time.Now)
	if err != nil {
		t.Fatalf("open pre-cutover outbox for drain watermark: %v", err)
	}
	confirmedRow, err := oldOutbox.FindByID(ctx, confirmed.OutboxID)
	if err != nil {
		t.Fatalf("read pre-cutover confirmed row: %v", err)
	}
	beyondDrain, err := oldOutbox.ListConfirmedSince(ctx, confirmedRow.UpdatedAtMS, 10)
	if err != nil || len(beyondDrain) != 0 {
		t.Fatalf("confirmed rows beyond projector drain watermark = %+v, %v", beyondDrain, err)
	}

	// This future v2-native intent is absent from legacy and is exactly the
	// non-re-derivable work that store-aside + CarryPendingOutbox must retain.
	futureScheduledMS := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Millisecond).UnixMilli()
	pending, err := v2wire.SubmitTextV2(ctx, v2wire.NativeDeps{
		V2: oldStack.Store, Service: oldStack.Service, Registry: oldStack.Registry,
	}, v2wire.TextInput{
		ConversationID: googleConversation.LegacyID,
		Body:           "r5 carry future scheduled sentinel",
		IdempotencyKey: "r5-cutover-carry-future",
		NotBefore:      time.UnixMilli(futureScheduledMS),
	})
	if err != nil {
		t.Fatalf("submit future pre-cutover intent: %v", err)
	}
	if delivery, err := oldStack.Service.Get(ctx, pending.OutboxID); err != nil || delivery.State != messaging.OutboxQueued {
		t.Fatalf("future pre-cutover delivery = %+v, %v, want queued", delivery, err)
	}

	stopShadow()
	shadowStopped = true
	cancelShadow()
	if err := legacy.Close(); err != nil {
		t.Fatalf("close quiesced legacy source: %v", err)
	}
	legacyClosed = true
	legacyHashAtQuiesce := fileSHA256(t, fixture.StorePath)

	// Backup and a real --check are the go/no-go gates. The check uses a
	// separate target because canonical v2 state is intentionally still present
	// until the next store-aside step.
	backup := runR5Backup(t, fixture.DataDir, r5CutoverNow)
	manifestData, err := os.ReadFile(backup.ManifestPath)
	if err != nil {
		t.Fatalf("read cutover backup manifest: %v", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode cutover backup manifest: %v", err)
	}
	if manifest.Source.QuickCheck != "ok" {
		t.Fatalf("cutover backup quick_check = %q, want ok", manifest.Source.QuickCheck)
	}
	checkTarget := filepath.Join(fixture.DataDir, "v2-cutover-check")
	checkReport := runR5Migrate(t, fixture.DataDir, checkTarget, true, r5CutoverNow.Add(time.Minute))
	if !checkReport.OK || !checkReport.Validation.Passed || checkReport.Target.Published {
		t.Fatalf("cutover migrate --check gate = ok=%v passed=%v published=%v",
			checkReport.OK, checkReport.Validation.Passed, checkReport.Target.Published)
	}

	asideDir := moveR5CanonicalV2Aside(t, fixture.DataDir, r5CutoverNow)
	v2Dir := filepath.Join(fixture.DataDir, "v2")
	publishReport := runR5Migrate(t, fixture.DataDir, v2Dir, false, r5CutoverNow.Add(2*time.Minute))
	if !publishReport.OK || !publishReport.Validation.Passed || !publishReport.Target.Published {
		t.Fatalf("cutover migration publish = ok=%v passed=%v published=%v",
			publishReport.OK, publishReport.Validation.Passed, publishReport.Target.Published)
	}
	if got := fileSHA256(t, fixture.StorePath); got != legacyHashAtQuiesce {
		t.Fatalf("legacy source changed through backup/migration: got %s, want %s", got, legacyHashAtQuiesce)
	}

	oldStore, err := sqlite.Open(filepath.Join(asideDir, v2StoreName))
	if err != nil {
		t.Fatalf("open retained pre-cutover store: %v", err)
	}
	oldBlobs, err := blob.New(filepath.Join(asideDir, v2BlobName))
	if err != nil {
		_ = oldStore.Close()
		t.Fatalf("open retained pre-cutover blobs: %v", err)
	}
	freshStore, err := sqlite.Open(filepath.Join(v2Dir, v2StoreName))
	if err != nil {
		_ = oldStore.Close()
		t.Fatalf("open freshly migrated store for carry: %v", err)
	}
	freshBlobs, err := blob.New(filepath.Join(v2Dir, v2BlobName))
	if err != nil {
		_ = freshStore.Close()
		_ = oldStore.Close()
		t.Fatalf("open freshly migrated blobs for carry: %v", err)
	}
	oldEndpoint, err := cutover.NewEndpoint(oldStore, oldBlobs, time.Now)
	if err != nil {
		t.Fatalf("create old cutover endpoint: %v", err)
	}
	freshEndpoint, err := cutover.NewEndpoint(freshStore, freshBlobs, time.Now)
	if err != nil {
		t.Fatalf("create fresh cutover endpoint: %v", err)
	}
	carryReport, err := cutover.CarryPendingOutbox(ctx, oldEndpoint, freshEndpoint)
	if err != nil {
		t.Fatalf("carry pending pre-cutover outbox: %v", err)
	}
	if len(carryReport.Carried) != 1 || len(carryReport.Reported) != 0 || len(carryReport.Uncarryable) != 0 ||
		carryReport.Carried[0].IdempotencyKey != "r5-cutover-carry-future" {
		t.Fatalf("cutover carry report = %+v", carryReport)
	}
	freshCarryable, err := freshEndpoint.ListCarryable(ctx)
	if err != nil {
		t.Fatalf("list freshly carried outbox: %v", err)
	}
	carriedFound := false
	for _, item := range freshCarryable {
		if item.IdempotencyKey == "r5-cutover-carry-future" {
			carriedFound = item.ScheduledForMS == futureScheduledMS &&
				item.RemoteConversationID == googleConversation.RemoteID &&
				item.Body == "r5 carry future scheduled sentinel"
		}
	}
	if !carriedFound {
		t.Fatalf("fresh outbox did not preserve carried schedule/natural key: %+v", freshCarryable)
	}
	secondCarry, err := cutover.CarryPendingOutbox(ctx, oldEndpoint, freshEndpoint)
	if err != nil || len(secondCarry.Carried) != 0 {
		t.Fatalf("second cutover carry = %+v, %v, want idempotent no-op", secondCarry, err)
	}
	if err := freshStore.Close(); err != nil {
		t.Fatalf("close fresh carry store: %v", err)
	}
	if err := oldStore.Close(); err != nil {
		t.Fatalf("close retained carry source: %v", err)
	}

	// Flip the explicit selector, boot v2-primary, smoke the migrated corpus,
	// then reconstruct the stack over the same store to pin restart recovery.
	t.Setenv("OPENMESSAGES_DATA_DIR", fixture.DataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")
	mode, err := resolveV2RuntimeMode(false, fixture.DataDir)
	if err != nil || !mode.Primary || !mode.Send || !mode.Ingest {
		t.Fatalf("resolve cutover selector = %+v, %v", mode, err)
	}

	firstBoot, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("first v2-primary boot: %v", err)
	}
	firstBootCtx, cancelFirstBoot := context.WithCancel(context.Background())
	stopFirstBoot := firstBoot.Start(firstBootCtx, nil, nil, true)
	firstBootStopped := false
	t.Cleanup(func() {
		if !firstBootStopped {
			stopFirstBoot()
		}
		cancelFirstBoot()
	})
	firstReads := v2read.New(firstBoot.Store)
	assertR5CutoverSmokeReads(t, firstReads, googleConversation)
	if delivery, err := firstBoot.Service.Get(ctx, pending.OutboxID); err != nil || delivery.State != messaging.OutboxQueued {
		t.Fatalf("carried delivery on first boot = %+v, %v", delivery, err)
	}
	stopFirstBoot()
	firstBootStopped = true
	cancelFirstBoot()

	secondBoot, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("restart v2-primary stack: %v", err)
	}
	restartGoogle := scripted.New(r5GoogleAccountID, bridge.PlatformGoogle)
	if err := secondBoot.RegisterAdapter(restartGoogle); err != nil {
		_ = secondBoot.Store.Close()
		t.Fatalf("register post-restart Google adapter: %v", err)
	}
	if delivery, err := secondBoot.Service.Get(ctx, pending.OutboxID); err != nil || delivery.State != messaging.OutboxQueued {
		_ = secondBoot.Store.Close()
		t.Fatalf("carried delivery after restart = %+v, %v", delivery, err)
	}
	restartGoogle.EnqueueTextResult(bridge.SendResult{
		RemoteMessageID: "r5-cutover-smoke-remote", AcceptedAt: time.Now(), EchoExpected: true,
	})
	restartCtx, cancelRestart := context.WithCancel(context.Background())
	stopRestart := secondBoot.Start(restartCtx, nil, nil, true)
	restartStopped := false
	t.Cleanup(func() {
		if !restartStopped {
			stopRestart()
		}
		cancelRestart()
	})
	smokeSubmission, err := v2wire.SubmitTextV2(ctx, v2wire.NativeDeps{
		V2: secondBoot.Store, Service: secondBoot.Service, Registry: secondBoot.Registry,
	}, v2wire.TextInput{
		ConversationID: googleConversation.V2ID(),
		Body:           "r5 post-cutover smoke send",
		IdempotencyKey: "r5-post-cutover-smoke",
	})
	if err != nil {
		t.Fatalf("submit post-cutover smoke send: %v", err)
	}
	waitR5(t, "post-cutover smoke confirmation", func() bool {
		delivery, getErr := secondBoot.Service.Get(ctx, smokeSubmission.OutboxID)
		return getErr == nil && delivery.State == messaging.OutboxConfirmed &&
			delivery.RemoteMessageID == "r5-cutover-smoke-remote"
	})
	secondReads := v2read.New(secondBoot.Store)
	waitR5(t, "post-cutover smoke visibility", func() bool {
		messages, readErr := secondReads.SearchMessagesFiltered(
			"r5 post-cutover smoke send", db.SearchFilter{Limit: 10},
		)
		return readErr == nil && len(messages) == 1 && messages[0].IsFromMe &&
			messages[0].SourceID == "r5-cutover-smoke-remote"
	})
	stopRestart()
	restartStopped = true
	cancelRestart()

	if got := fileSHA256(t, fixture.StorePath); got != legacyHashAtQuiesce {
		t.Fatalf("legacy rollback source changed after v2 boot: got %s, want %s", got, legacyHashAtQuiesce)
	}
	for _, retained := range []string{v2StoreName, v2BlobName} {
		if _, err := os.Stat(filepath.Join(asideDir, retained)); err != nil {
			t.Fatalf("retained pre-cutover artifact %q missing: %v", retained, err)
		}
	}
}

func TestMigrateRefusesExistingV2Store(t *testing.T) {
	fixture := buildR5LegacyFixture(t)

	t.Run("published store", func(t *testing.T) {
		targetDir := filepath.Join(t.TempDir(), "v2")
		report := runR5Migrate(t, fixture.DataDir, targetDir, false, r5CutoverNow)
		if !report.Target.Published {
			t.Fatal("first migration did not publish the canonical v2 store")
		}
		storePath := filepath.Join(targetDir, v2StoreName)
		before, err := os.ReadFile(storePath)
		if err != nil {
			t.Fatalf("read first published store: %v", err)
		}

		failure, commandErr := runR5MigrateExpectFailure(
			t, fixture.DataDir, targetDir, false, r5CutoverNow.Add(time.Minute),
		)
		assertR5CanonicalStateRefusal(t, failure, commandErr, storePath)
		after, err := os.ReadFile(storePath)
		if err != nil {
			t.Fatalf("read refused target store: %v", err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("second migration changed the existing canonical v2 store")
		}
	})

	for _, name := range []string{v2StoreName + "-wal", v2StoreName + "-shm", v2BlobName} {
		name := name
		t.Run(name, func(t *testing.T) {
			targetDir := filepath.Join(t.TempDir(), "v2")
			if err := os.MkdirAll(targetDir, 0o700); err != nil {
				t.Fatalf("create target: %v", err)
			}
			canonicalPath := filepath.Join(targetDir, name)
			sentinelPath := canonicalPath
			if name == v2BlobName {
				if err := os.Mkdir(canonicalPath, 0o700); err != nil {
					t.Fatalf("create canonical blobs sentinel: %v", err)
				}
				sentinelPath = filepath.Join(canonicalPath, "retain-me")
			}
			if err := os.WriteFile(sentinelPath, []byte("canonical-state-must-survive"), 0o600); err != nil {
				t.Fatalf("write canonical state sentinel: %v", err)
			}

			failure, commandErr := runR5MigrateExpectFailure(
				t, fixture.DataDir, targetDir, false, r5CutoverNow.Add(2*time.Minute),
			)
			assertR5CanonicalStateRefusal(t, failure, commandErr, canonicalPath)
			got, err := os.ReadFile(sentinelPath)
			if err != nil {
				t.Fatalf("read retained canonical state sentinel: %v", err)
			}
			if string(got) != "canonical-state-must-survive" {
				t.Fatalf("canonical state sentinel = %q, want unchanged", got)
			}
		})
	}
}

func TestMigrateCheckNoGoOnRemoteIDCollision(t *testing.T) {
	fixture := buildR5LegacyFixture(t)
	conversation := fixture.Conversations[0]
	original := conversation.Messages[0]
	legacy, err := db.New(fixture.StorePath)
	if err != nil {
		t.Fatalf("reopen legacy fixture for collision: %v", err)
	}
	if err := legacy.UpsertMessage(&db.Message{
		MessageID:      "r5-deliberate-remote-id-collision",
		ConversationID: conversation.LegacyID,
		SenderName:     "Collision Sender",
		SenderNumber:   "+15559990000",
		Body:           "deliberately colliding migration row",
		TimestampMS:    fixture.BaseMS + 13_000,
		Status:         "delivered",
		SourcePlatform: conversation.Platform,
		SourceID:       original.RemoteID,
	}); err != nil {
		_ = legacy.Close()
		t.Fatalf("seed deliberate remote-ID collision: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close colliding legacy fixture: %v", err)
	}

	targetDir := filepath.Join(fixture.DataDir, "v2-no-go")
	var stdout, stderr bytes.Buffer
	commandErr := runMigrateCommand(
		context.Background(),
		[]string{"--check", "--from", fixture.DataDir, "--to", targetDir},
		r5MigrateDependencies(r5CutoverNow),
		&stdout,
		&stderr,
	)
	if commandErr == nil {
		t.Fatalf("migrate --check accepted a remote-ID collision\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if got := ExitCode(commandErr); got != migrateTransformExitCode {
		t.Fatalf("migrate --check no-go exit code = %d, want %d: %v", got, migrateTransformExitCode, commandErr)
	}
	report := decodeR5MigrationReport(t, stdout.Bytes())
	if report.OK || !report.Check || report.Validation.Passed || report.Target.Published {
		t.Fatalf("migrate --check no-go report = ok=%v check=%v passed=%v published=%v",
			report.OK, report.Check, report.Validation.Passed, report.Target.Published)
	}
	if len(report.MessageCollisions) != 1 {
		t.Fatalf("migrate --check collisions = %+v, want exactly one", report.MessageCollisions)
	}
	collision := report.MessageCollisions[0]
	if collision.AccountID != conversation.AccountID ||
		collision.ConversationID != conversation.LegacyID ||
		collision.RemoteMessageID != original.RemoteID ||
		len(collision.LegacyMessageIDs) != 2 {
		t.Fatalf("migrate --check collision evidence = %+v", collision)
	}
	if _, err := os.Stat(filepath.Join(targetDir, v2StoreName)); !os.IsNotExist(err) {
		t.Fatalf("no-go check published a canonical store: %v", err)
	}
	for _, staged := range []string{v2TempStoreName, v2TempBlobName} {
		if _, err := os.Stat(filepath.Join(targetDir, staged)); !os.IsNotExist(err) {
			t.Fatalf("no-go check retained staged state %q: %v", staged, err)
		}
	}
}

func runR5MigrateExpectFailure(
	t *testing.T,
	sourceDir string,
	targetDir string,
	check bool,
	now time.Time,
) (migrateFailureOutput, error) {
	t.Helper()
	args := []string{"--from", sourceDir, "--to", targetDir}
	if check {
		args = append([]string{"--check"}, args...)
	}
	var stdout, stderr bytes.Buffer
	err := runMigrateCommand(
		context.Background(), args, r5MigrateDependencies(now), &stdout, &stderr,
	)
	if err == nil {
		t.Fatalf("runMigrateCommand unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	var failure migrateFailureOutput
	if decodeErr := json.Unmarshal(stdout.Bytes(), &failure); decodeErr != nil {
		t.Fatalf(
			"decode migrate failure output: %v\nstdout:\n%s\nstderr:\n%s",
			decodeErr,
			stdout.String(),
			stderr.String(),
		)
	}
	return failure, err
}

func assertR5CanonicalStateRefusal(
	t *testing.T,
	failure migrateFailureOutput,
	commandErr error,
	canonicalPath string,
) {
	t.Helper()
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(canonicalPath))
	if err != nil {
		t.Fatalf("canonicalize refusal path: %v", err)
	}
	canonicalPath = filepath.Join(canonicalParent, filepath.Base(canonicalPath))
	if got := ExitCode(commandErr); got != migrateSourceExitCode {
		t.Fatalf("migrate refusal exit code = %d, want %d: %v", got, migrateSourceExitCode, commandErr)
	}
	if failure.OK || failure.ExitCode != migrateSourceExitCode {
		t.Fatalf("migrate refusal JSON = %+v", failure)
	}
	for _, required := range []string{
		"v2 target already contains canonical state at " + canonicalPath,
		"existing stores and blobs are never overwritten",
	} {
		if !strings.Contains(commandErr.Error(), required) || !strings.Contains(failure.Error, required) {
			t.Fatalf("migrate refusal did not contain %q: err=%v JSON=%+v", required, commandErr, failure)
		}
	}
}

func decodeR5MigrationReport(t *testing.T, stdout []byte) migration.Report {
	t.Helper()
	var report migration.Report
	if err := json.Unmarshal(stdout, &report); err != nil {
		t.Fatalf("decode migration report: %v\nstdout:\n%s", err, stdout)
	}
	return report
}

func moveR5CanonicalV2Aside(t *testing.T, dataDir string, now time.Time) string {
	t.Helper()
	v2Dir := filepath.Join(dataDir, "v2")
	asideDir := filepath.Join(v2Dir, "pre-cutover-"+now.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(asideDir, 0o700); err != nil {
		t.Fatalf("create retained pre-cutover directory: %v", err)
	}
	moved := make(map[string]bool)
	for _, name := range []string{v2StoreName, v2StoreName + "-wal", v2StoreName + "-shm", v2BlobName} {
		source := filepath.Join(v2Dir, name)
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatalf("inspect canonical v2 artifact %q: %v", name, err)
		}
		if err := os.Rename(source, filepath.Join(asideDir, name)); err != nil {
			t.Fatalf("retain canonical v2 artifact %q: %v", name, err)
		}
		moved[name] = true
	}
	if !moved[v2StoreName] || !moved[v2BlobName] {
		t.Fatalf("store-aside moved artifacts = %+v, want store and blobs", moved)
	}
	return asideDir
}

func assertR5CutoverSmokeReads(
	t *testing.T,
	reads *v2read.Source,
	googleConversation r5FixtureConversation,
) {
	t.Helper()
	conversations, err := reads.ListConversations(50)
	if err != nil || len(conversations) == 0 {
		t.Fatalf("cutover smoke list conversations = %d, %v", len(conversations), err)
	}
	messages, err := reads.GetMessagesByConversation(googleConversation.V2ID(), 100)
	if err != nil {
		t.Fatalf("cutover smoke read Google conversation: %v", err)
	}
	wantBody := googleConversation.Messages[0].Body
	readFound := false
	projectedFound := false
	for _, message := range messages {
		readFound = readFound || message.Body == wantBody
		projectedFound = projectedFound || message.Body == "r5 projector drain sentinel"
	}
	if !readFound || !projectedFound {
		t.Fatalf("cutover smoke thread omitted migrated fixture/projected send: %+v", messages)
	}
	search, err := reads.SearchMessagesFiltered(r5SharedSearchToken, db.SearchFilter{Limit: 50})
	if err != nil || len(search) < 5 {
		t.Fatalf("cutover smoke search = %d, %v, want all platforms", len(search), err)
	}
}
