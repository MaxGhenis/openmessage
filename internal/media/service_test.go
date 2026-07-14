package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

var mediaTestTime = time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

func TestResolveDownloadsBlobFirstAndReturnsSafeDescriptor(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-happy", []sqlite.MessageAttachment{{
		Ordinal:   0,
		RemoteID:  "remote-media",
		RemoteRef: []byte("opaque-ref"),
		Filename:  "../../photo.png",
		MIME:      "image/png",
	}})
	payload := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 64)...)
	downloader := &scriptedDownloader{steps: []downloadStep{byteStreamStep(payload, "image/png")}}
	registry := availableDownloadRegistry(downloader)
	service := newMediaTestService(t, env, registry)

	descriptor, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1",
		MessageID: "message-happy",
		Ordinal:   0,
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if descriptor.Ref.Hash == "" || descriptor.Ref.Size != int64(len(payload)) ||
		descriptor.Ref.MIME != "image/png" || descriptor.Size != int64(len(payload)) ||
		descriptor.MIME != "image/png" || descriptor.Filename != "photo.png" ||
		descriptor.Disposition != DispositionInline {
		t.Fatalf("Resolve() descriptor = %+v", descriptor)
	}
	requests := downloader.snapshotRequests()
	if len(requests) != 1 || requests[0].accountID != "account-1" ||
		requests[0].ref.RemoteID != "remote-media" ||
		!bytes.Equal(requests[0].ref.Opaque, []byte("opaque-ref")) ||
		requests[0].ref.Filename != "../../photo.png" || requests[0].ref.MIME != "image/png" {
		t.Fatalf("DownloadMedia requests = %+v", requests)
	}
	row := getMediaAttachment(t, env, "message-happy", 0)
	if row.State != "downloaded" || row.BlobHash == nil || *row.BlobHash != descriptor.Ref.Hash ||
		row.SizeBytes == nil || *row.SizeBytes != int64(len(payload)) {
		t.Fatalf("downloaded attachment = %+v", row)
	}

	reader, err := service.Open(descriptor.Ref)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	opened, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(opened, payload) {
		t.Fatalf("Open() = %d bytes, read=%v close=%v", len(opened), readErr, closeErr)
	}
}

func TestResolveUsesStreamMIMEPersistedByDownload(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-stream-mime", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-stream-mime", Filename: "resolved.png",
		MIME: "application/octet-stream",
	}})
	payload := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x24}, 64)...)
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep(payload, " IMAGE/PNG ; charset=binary "),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	descriptor, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-stream-mime", Ordinal: 0,
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if descriptor.MIME != "image/png" || descriptor.Disposition != DispositionInline ||
		descriptor.Ref.MIME != "IMAGE/PNG ; charset=binary" {
		t.Fatalf("stream-resolved descriptor = %+v", descriptor)
	}
	row := getMediaAttachment(t, env, "message-stream-mime", 0)
	if row.MIME != "IMAGE/PNG ; charset=binary" {
		t.Fatalf("persisted stream MIME = %q", row.MIME)
	}
}

func TestResolveSingleFlightFetchesAndPublishesOnce(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-single-flight", []sqlite.MessageAttachment{{
		Ordinal:   0,
		RemoteID:  "remote-single-flight",
		RemoteRef: []byte("single-flight-ref"),
		Filename:  "shared.bin",
		MIME:      "application/octet-stream",
	}})
	payload := bytes.Repeat([]byte("single-flight"), 128)
	entered := make(chan struct{})
	release := make(chan struct{})
	downloader := &scriptedDownloader{steps: []downloadStep{{
		beforeReturn: func() {
			close(entered)
			<-release
		},
		stream: func() bridge.MediaStream {
			return byteMediaStream(payload, "application/octet-stream")
		},
	}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-single-flight", Ordinal: 0}

	const callers = 8
	start := make(chan struct{})
	results := make(chan resolveResult, callers)
	for range callers {
		go func() {
			<-start
			descriptor, err := service.Resolve(context.Background(), command)
			results <- resolveResult{descriptor: descriptor, err: err}
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("DownloadMedia was not entered")
	}
	// Give every released caller an opportunity to join the occupied slot before
	// allowing its only scripted transport result to complete.
	for range 100 {
		runtime.Gosched()
	}
	close(release)

	var first blob.BlobRef
	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("Resolve()[%d]: %v", i, result.err)
		}
		if i == 0 {
			first = result.descriptor.Ref
		} else if result.descriptor.Ref != first {
			t.Fatalf("Resolve()[%d] ref = %+v, want %+v", i, result.descriptor.Ref, first)
		}
	}
	if got := downloader.requestCount(); got != 1 {
		t.Fatalf("DownloadMedia calls = %d, want 1", got)
	}
	if files := blobFiles(t, env.blobRoot); len(files) != 1 {
		t.Fatalf("blob files = %v, want exactly one", files)
	}
}

func TestResolveDeduplicatesIdenticalAttachmentBytes(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-dedup", []sqlite.MessageAttachment{
		{Ordinal: 0, RemoteID: "remote-a", Filename: "a.bin", MIME: "application/octet-stream"},
		{Ordinal: 1, RemoteID: "remote-b", Filename: "b.bin", MIME: "application/octet-stream"},
	})
	payload := bytes.Repeat([]byte("same bytes"), 64)
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep(payload, "application/octet-stream"),
		byteStreamStep(payload, "application/octet-stream"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	first, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-dedup", Ordinal: 0,
	})
	if err != nil {
		t.Fatalf("Resolve(first): %v", err)
	}
	second, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-dedup", Ordinal: 1,
	})
	if err != nil {
		t.Fatalf("Resolve(second): %v", err)
	}
	if first.Ref.Hash != second.Ref.Hash {
		t.Fatalf("deduplicated hashes = %q, %q", first.Ref.Hash, second.Ref.Hash)
	}
	if got := downloader.requestCount(); got != 2 {
		t.Fatalf("DownloadMedia calls = %d, want 2 for different attachments", got)
	}
	if files := blobFiles(t, env.blobRoot); len(files) != 1 {
		t.Fatalf("deduplicated blob files = %v, want one", files)
	}
}

func TestResolveTooLargeLeavesAttachmentPendingAndNoBlob(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-too-large", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-large", Filename: "large.bin", MIME: "application/octet-stream",
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep([]byte("12345"), "application/octet-stream"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	service.maxBytes = 4

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-too-large", Ordinal: 0,
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Resolve() error = %v, want ErrTooLarge", err)
	}
	assertPendingWithoutBlob(t, env, "message-too-large", 0)
	if files := blobFiles(t, env.blobRoot); len(files) != 0 {
		t.Fatalf("oversize download left blob files: %v", files)
	}
	if got := attachmentLastError(t, env.dbPath, "message-too-large", 0); !got.Valid {
		t.Fatal("oversize download did not record last_error")
	}
}

func TestResolvePartialStreamFailureLeavesAttachmentPendingAndNoBlob(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-partial", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-partial", Filename: "partial.bin", MIME: "application/octet-stream",
	}})
	sourceErr := errors.New("source stream failed")
	downloader := &scriptedDownloader{steps: []downloadStep{{stream: func() bridge.MediaStream {
		return bridge.MediaStream{
			ReadCloser: &partialErrorReadCloser{data: []byte("partial bytes"), err: sourceErr},
			Size:       1024,
			MIME:       "application/octet-stream",
		}
	}}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-partial", Ordinal: 0,
	})
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Resolve() error = %v, want source error", err)
	}
	assertPendingWithoutBlob(t, env, "message-partial", 0)
	if files := blobFiles(t, env.blobRoot); len(files) != 0 {
		t.Fatalf("partial download left blob files: %v", files)
	}
	if got := attachmentLastError(t, env.dbPath, "message-partial", 0); !got.Valid ||
		!strings.Contains(got.String, sourceErr.Error()) {
		t.Fatalf("partial download last_error = %+v", got)
	}
}

// A transport returning (MediaStream{}, nil) — nil error with a nil embedded
// ReadCloser — must be a recorded transient failure, not a panic at
// stream.Close/blob.Put (the MediaStream value wrapping a nil interface is
// itself non-nil, so blob.Put's nil-reader guard cannot catch it).
func TestResolveNilStreamWithoutErrorIsTransientNotPanic(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-nilstream", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-nilstream", Filename: "x.bin", MIME: "application/octet-stream",
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{{stream: func() bridge.MediaStream {
		return bridge.MediaStream{}
	}}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-nilstream", Ordinal: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "transport returned no stream") {
		t.Fatalf("Resolve() error = %v, want transport-returned-no-stream failure", err)
	}
	assertPendingWithoutBlob(t, env, "message-nilstream", 0)
	if got := attachmentLastError(t, env.dbPath, "message-nilstream", 0); !got.Valid ||
		!strings.Contains(got.String, "transport returned no stream") {
		t.Fatalf("nil-stream download last_error = %+v", got)
	}
}

func TestResolveForcesSpoofedActiveContentToDownload(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-spoof", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-spoof", Filename: "../unsafe.html", MIME: "image/png",
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep([]byte("<html><script>alert(1)</script></html>"), ""),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	descriptor, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-spoof", Ordinal: 0,
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if descriptor.Disposition != DispositionForcedDownload ||
		descriptor.MIME != "application/octet-stream" || descriptor.Filename != "unsafe.html" {
		t.Fatalf("spoofed descriptor = %+v", descriptor)
	}
}

func TestResolveCapabilityRouting(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*fakeRegistry)
		want        error
		wantAcquire int
	}{
		{
			name: "unsupported",
			configure: func(registry *fakeRegistry) {
				registry.capabilities = bridge.CapabilitySet{}
			},
			want: ErrUnsupported,
		},
		{
			name: "declared but unwired",
			configure: func(registry *fakeRegistry) {
				registry.capabilities.MediaDownload = true
				registry.acquireErr = bridge.ErrCapabilityUnavailable
			},
			want:        ErrUnavailable,
			wantAcquire: 1,
		},
		{
			name: "lease has nil downloader",
			configure: func(registry *fakeRegistry) {
				registry.capabilities.MediaDownload = true
				registry.nilDownload = true
			},
			want:        ErrUnavailable,
			wantAcquire: 1,
		},
		{
			name: "account not registered",
			configure: func(registry *fakeRegistry) {
				registry.registered = false
			},
			want: ErrUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newMediaTestEnvironment(t)
			seedMediaMessage(t, env, "message-routing", []sqlite.MessageAttachment{{
				Ordinal: 0, RemoteID: "remote-routing", Filename: "routing.bin", MIME: "application/octet-stream",
			}})
			downloader := &scriptedDownloader{}
			registry := &fakeRegistry{registered: true, downloader: downloader}
			tt.configure(registry)
			service := newMediaTestService(t, env, registry)

			_, err := service.Resolve(context.Background(), DownloadCommand{
				AccountID: "account-1", MessageID: "message-routing", Ordinal: 0,
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Resolve() error = %v, want %v", err, tt.want)
			}
			if got := registry.acquireCalls(); got != tt.wantAcquire {
				t.Fatalf("Acquire calls = %d, want %d", got, tt.wantAcquire)
			}
			if got := downloader.requestCount(); got != 0 {
				t.Fatalf("DownloadMedia calls = %d, want 0", got)
			}
			assertPendingWithoutBlob(t, env, "message-routing", 0)
		})
	}
}

func TestResolveTerminalTransportErrorRecordsFailure(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-terminal", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-terminal", Filename: "terminal.bin", MIME: "application/octet-stream",
	}})
	terminal := bridge.OpError{
		Class:     bridge.FailureUnpaired,
		Operation: "download_media",
		Cause:     errors.New("pairing was removed"),
	}
	downloader := &scriptedDownloader{steps: []downloadStep{{err: terminal}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-terminal", Ordinal: 0,
	})
	var got bridge.OpError
	if !errors.As(err, &got) || got.Class != bridge.FailureUnpaired {
		t.Fatalf("Resolve() error = %v, want unpaired OpError", err)
	}
	assertPendingWithoutBlob(t, env, "message-terminal", 0)
	lastError := attachmentLastError(t, env.dbPath, "message-terminal", 0)
	if !lastError.Valid || !strings.Contains(lastError.String, terminal.Error()) {
		t.Fatalf("terminal last_error = %+v", lastError)
	}
}

func TestResolveTransientTransportErrorRetries(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-transient", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-transient", Filename: "transient.txt", MIME: "text/plain",
	}})
	transient := bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "download_media",
		Cause:     errors.New("temporary transport failure"),
	}
	downloader := &scriptedDownloader{steps: []downloadStep{
		{err: transient},
		byteStreamStep([]byte("retry succeeded"), "text/plain"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-transient", Ordinal: 0}

	if _, err := service.Resolve(context.Background(), command); err == nil {
		t.Fatal("Resolve(first) succeeded, want transient error")
	}
	assertPendingWithoutBlob(t, env, "message-transient", 0)
	descriptor, err := service.Resolve(context.Background(), command)
	if err != nil {
		t.Fatalf("Resolve(retry): %v", err)
	}
	if descriptor.Ref.Hash == "" || downloader.requestCount() != 2 {
		t.Fatalf("retry descriptor/calls = %+v / %d", descriptor, downloader.requestCount())
	}
	if got := attachmentLastError(t, env.dbPath, "message-transient", 0); got.Valid {
		t.Fatalf("successful retry retained last_error = %q", got.String)
	}
}

func TestResolveCanceledTransportStillRecordsFailure(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-canceled", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-canceled", Filename: "canceled.bin", MIME: "application/octet-stream",
	}})
	ctx, cancel := context.WithCancel(context.Background())
	transportErr := errors.New("transport canceled the operation")
	downloader := &scriptedDownloader{steps: []downloadStep{{
		beforeReturn: cancel,
		err:          transportErr,
	}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	_, err := service.Resolve(ctx, DownloadCommand{
		AccountID: "account-1", MessageID: "message-canceled", Ordinal: 0,
	})
	if !errors.Is(err, transportErr) {
		t.Fatalf("Resolve() error = %v, want transport error", err)
	}
	assertPendingWithoutBlob(t, env, "message-canceled", 0)
	lastError := attachmentLastError(t, env.dbPath, "message-canceled", 0)
	if !lastError.Valid || !strings.Contains(lastError.String, transportErr.Error()) {
		t.Fatalf("canceled transport last_error = %+v", lastError)
	}
}

func TestResolveAccountFenceDoesNotLeakOrFetch(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-fenced", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-fenced", Filename: "fenced.bin", MIME: "application/octet-stream",
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep([]byte("must not fetch"), "application/octet-stream"),
	}}
	registry := availableDownloadRegistry(downloader)
	service := newMediaTestService(t, env, registry)

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "different-account", MessageID: "message-fenced", Ordinal: 0,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrNotFound", err)
	}
	if registry.acquireCalls() != 0 || downloader.requestCount() != 0 {
		t.Fatalf("account fence transport calls = acquire %d, download %d",
			registry.acquireCalls(), downloader.requestCount())
	}
}

func TestResolveAlreadyDownloadedMakesNoTransportCall(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-cached", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-cached", Filename: "cached.txt", MIME: "text/plain",
	}})
	payload := []byte("already cached")
	ref, err := env.blobs.Put(context.Background(), bytes.NewReader(payload), "text/plain", int64(len(payload)))
	if err != nil {
		t.Fatalf("Put(): %v", err)
	}
	if err := env.attachments.MarkDownloaded(
		context.Background(), "message-cached", 0, ref.Hash, ref.Size, ref.MIME,
	); err != nil {
		t.Fatalf("MarkDownloaded(): %v", err)
	}
	downloader := &scriptedDownloader{}
	registry := availableDownloadRegistry(downloader)
	service := newMediaTestService(t, env, registry)

	descriptor, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-cached", Ordinal: 0,
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if descriptor.Ref.Hash != ref.Hash || descriptor.Ref.Size != ref.Size {
		t.Fatalf("cached descriptor = %+v, want ref %+v", descriptor, ref)
	}
	if registry.acquireCalls() != 0 || downloader.requestCount() != 0 {
		t.Fatalf("cached transport calls = acquire %d, download %d",
			registry.acquireCalls(), downloader.requestCount())
	}
}

func TestNewServiceRejectsMissingDependencies(t *testing.T) {
	env := newMediaTestEnvironment(t)
	registry := availableDownloadRegistry(&scriptedDownloader{})
	tests := []struct {
		name     string
		store    *sqlite.Store
		registry bridge.Registry
		blobs    *blob.BlobStore
		clock    Clock
	}{
		{name: "store", registry: registry, blobs: env.blobs, clock: env.clock},
		{name: "registry", store: env.store, blobs: env.blobs, clock: env.clock},
		{name: "blobs", store: env.store, registry: registry, clock: env.clock},
		{name: "clock", store: env.store, registry: registry, blobs: env.blobs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewService(tt.store, tt.registry, tt.blobs, tt.clock); err == nil {
				t.Fatal("NewService() succeeded with missing dependency")
			}
		})
	}
}

type mediaTestEnvironment struct {
	store       *sqlite.Store
	attachments *sqlite.MessageAttachmentRepository
	blobs       *blob.BlobStore
	clock       *mediaManualClock
	dbPath      string
	blobRoot    string
}

func newMediaTestEnvironment(t *testing.T) *mediaTestEnvironment {
	t.Helper()
	clock := &mediaManualClock{now: mediaTestTime}
	dbPath := filepath.Join(t.TempDir(), "media.sqlite")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "account-1", BridgeKey: "test", DisplayName: "Test account",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: `{}`,
		CreatedAtMS: clock.Now().UnixMilli(), UpdatedAtMS: clock.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: "conversation-1", AccountID: "account-1",
		RemoteConversationID: "remote-conversation", Kind: sqlite.ConversationKindDirect,
		Title: "Test conversation", NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: `{}`, CreatedAtMS: clock.Now().UnixMilli(), UpdatedAtMS: clock.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	blobs, err := blob.New(blobRoot)
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	return &mediaTestEnvironment{
		store: store, attachments: attachments, blobs: blobs, clock: clock,
		dbPath: dbPath, blobRoot: blobRoot,
	}
}

func seedMediaMessage(
	t *testing.T,
	env *mediaTestEnvironment,
	messageID string,
	attachments []sqlite.MessageAttachment,
) {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(env.store, env.clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	inboxID := "inbox-" + messageID
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID: inboxID, AccountID: "account-1", Generation: 1,
		DedupeKey: "dedupe-" + messageID, Codec: "test", CodecVersion: 1,
		Payload: []byte("payload"),
	}); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	for index := range attachments {
		attachments[index].MessageID = messageID
	}
	if err := repository.ProjectMessage(context.Background(), sqlite.MessageProjection{
		InboxID: inboxID,
		Message: sqlite.Message{
			MessageID: messageID, ConversationID: "conversation-1", AccountID: "account-1",
			RemoteMessageID: "remote-" + messageID, Direction: sqlite.MessageDirectionIncoming,
			Body: "media", State: sqlite.MessageStateActive, OccurredAtMS: env.clock.Now().UnixMilli(),
		},
		Attachments: attachments,
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}
}

func newMediaTestService(
	t *testing.T,
	env *mediaTestEnvironment,
	registry bridge.Registry,
) *Service {
	t.Helper()
	service, err := NewService(env.store, registry, env.blobs, env.clock)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	return service
}

func getMediaAttachment(
	t *testing.T,
	env *mediaTestEnvironment,
	messageID string,
	ordinal int64,
) sqlite.MessageAttachment {
	t.Helper()
	row, err := env.attachments.GetForDownload(context.Background(), messageID, ordinal)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	return row
}

func assertPendingWithoutBlob(
	t *testing.T,
	env *mediaTestEnvironment,
	messageID string,
	ordinal int64,
) {
	t.Helper()
	row := getMediaAttachment(t, env, messageID, ordinal)
	if row.State != "pending" || row.BlobHash != nil {
		t.Fatalf("attachment after failed download = %+v, want pending without blob", row)
	}
}

func attachmentLastError(t *testing.T, dbPath, messageID string, ordinal int64) sql.NullString {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	defer db.Close()
	var lastError sql.NullString
	if err := db.QueryRowContext(context.Background(), `
		SELECT last_error
		FROM message_attachments
		WHERE message_id = ? AND ordinal = ?
	`, messageID, ordinal).Scan(&lastError); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	return lastError
}

func blobFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir(blob root): %v", err)
	}
	return files
}

type resolveResult struct {
	descriptor Descriptor
	err        error
}

type mediaManualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mediaManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mediaManualClock) NewTimer(delay time.Duration) messaging.Timer {
	return mediaSystemTimer{Timer: time.NewTimer(delay)}
}

type mediaSystemTimer struct{ *time.Timer }

func (t mediaSystemTimer) C() <-chan time.Time { return t.Timer.C }

type fakeRegistry struct {
	mu           sync.Mutex
	registered   bool
	capabilities bridge.CapabilitySet
	downloader   bridge.MediaDownloader
	acquireErr   error
	nilDownload  bool
	acquires     int
}

func availableDownloadRegistry(downloader bridge.MediaDownloader) *fakeRegistry {
	return &fakeRegistry{
		registered:   true,
		capabilities: bridge.CapabilitySet{MediaDownload: true},
		downloader:   downloader,
	}
}

func (r *fakeRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != "account-1" || !r.registered {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{AccountID: accountID, Platform: "test", Generation: 1}, true
}

func (r *fakeRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	r.mu.Lock()
	r.acquires++
	err := r.acquireErr
	downloader := r.downloader
	nilDownload := r.nilDownload
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("acquire media download: %w", err)
	}
	if capability != bridge.CapabilityMediaDownload {
		return nil, bridge.ErrCapabilityUnavailable
	}
	lease := &bridge.DispatchLease{
		AccountID: accountID, Platform: "test", Generation: 1, Ctx: ctx,
	}
	if !nilDownload {
		lease.Download = downloader
	}
	return lease, nil
}

func (r *fakeRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != "account-1" {
		return bridge.CapabilitySet{}
	}
	return r.capabilities
}

func (r *fakeRegistry) acquireCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acquires
}

type downloadStep struct {
	stream       func() bridge.MediaStream
	err          error
	beforeReturn func()
}

func byteStreamStep(data []byte, mime string) downloadStep {
	content := append([]byte(nil), data...)
	return downloadStep{stream: func() bridge.MediaStream {
		return byteMediaStream(content, mime)
	}}
}

func byteMediaStream(data []byte, mime string) bridge.MediaStream {
	return bridge.MediaStream{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		Size:       int64(len(data)),
		MIME:       mime,
	}
}

type downloadRequest struct {
	accountID string
	ref       bridge.MediaRef
}

type scriptedDownloader struct {
	mu       sync.Mutex
	steps    []downloadStep
	requests []downloadRequest
}

func (d *scriptedDownloader) DownloadMedia(
	_ context.Context,
	accountID string,
	ref bridge.MediaRef,
) (bridge.MediaStream, error) {
	d.mu.Lock()
	ref.Opaque = append([]byte(nil), ref.Opaque...)
	d.requests = append(d.requests, downloadRequest{accountID: accountID, ref: ref})
	if len(d.steps) == 0 {
		d.mu.Unlock()
		return bridge.MediaStream{}, errors.New("unexpected DownloadMedia call")
	}
	step := d.steps[0]
	d.steps = d.steps[1:]
	d.mu.Unlock()
	if step.beforeReturn != nil {
		step.beforeReturn()
	}
	if step.err != nil {
		return bridge.MediaStream{}, step.err
	}
	return step.stream(), nil
}

func (d *scriptedDownloader) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

func (d *scriptedDownloader) snapshotRequests() []downloadRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]downloadRequest(nil), d.requests...)
}

type partialErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *partialErrorReadCloser) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(buffer, r.data), nil
	}
	return 0, r.err
}

func (r *partialErrorReadCloser) Close() error { return nil }
