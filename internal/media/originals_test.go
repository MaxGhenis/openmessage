package media

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

var (
	thumbnailBytes = []byte("\xff\xd8\xff thumbnail 128px")
	originalBytes  = []byte("\xff\xd8\xff original 4032px, much longer than the thumbnail")
)

type recordingFullMediaRequester struct {
	mu    sync.Mutex
	refs  []bridge.MediaRef
	err   error
	block chan struct{}
}

func (r *recordingFullMediaRequester) RequestFullMedia(ctx context.Context, _ string, ref bridge.MediaRef) error {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.err
}

func (r *recordingFullMediaRequester) requests() []bridge.MediaRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bridge.MediaRef(nil), r.refs...)
}

func seedThumbnail(t *testing.T, env *mediaTestEnvironment, messageID string) {
	t.Helper()
	seedMediaMessage(t, env, messageID, []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "thumb-1", RemoteRef: []byte(`{"v":1,"media_id":"thumb-1","preview":true}`),
		RemotePreview: true, Filename: "IMG.jpeg", MIME: "image/jpeg",
	}})
}

// nameOriginal applies the update in which the transport names the original
// of messageID's image.
func nameOriginal(t *testing.T, env *mediaTestEnvironment, messageID, update string) {
	t.Helper()
	reprojectMediaMessage(t, env, messageID, update, []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "full-1", RemoteRef: []byte(`{"v":1,"media_id":"full-1"}`),
		Filename: "IMG.jpeg", MIME: "image/jpeg",
	}})
}

func resolveImage(t *testing.T, service *Service, messageID string) Descriptor {
	t.Helper()
	descriptor, err := service.Resolve(context.Background(), DownloadCommand{AccountID: "account-1", MessageID: messageID})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", messageID, err)
	}
	return descriptor
}

// A thumbnail is served as soon as it downloads, and the transport is asked
// for its original once per originalRetryAfter, however often it is served.
func TestResolveAsksForTheOriginalOfAThumbnailOncePerHold(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	requester := &recordingFullMediaRequester{}
	registry := availableDownloadRegistry(&scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
	}})
	registry.fullMedia = requester
	service := newMediaTestService(t, env, registry)
	t.Cleanup(service.Close)

	for range 3 {
		if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(thumbnailBytes) {
			t.Fatalf("Resolve() served %s, want the thumbnail", got.Ref.Hash)
		}
		service.background.Wait()
	}
	row := getMediaAttachment(t, env, "message-thumb", 0)
	if !row.BlobPreview || !row.RemotePreview || !row.AwaitsOriginal() {
		t.Fatalf("row after the thumbnail downloaded = %+v, want a reduced copy awaiting its original", row)
	}
	if got := requester.requests(); len(got) != 1 || got[0].RemoteID != "thumb-1" || string(got[0].Opaque) != string(row.RemoteRef) {
		t.Fatalf("full-size requests = %+v, want one for thumb-1 with its stored reference", got)
	}

	env.clock.advance(originalRetryAfter - time.Second)
	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	if got := len(requester.requests()); got != 1 {
		t.Fatalf("full-size requests just inside the hold = %d, want 1", got)
	}
	env.clock.advance(time.Second)
	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	if got := len(requester.requests()); got != 2 {
		t.Fatalf("full-size requests once the hold ends = %d, want 2", got)
	}
}

// A failed or refused request is not recorded and changes nothing; the hold
// it placed still paces the next one.
func TestResolveRequestFailureChangesNothing(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	requester := &recordingFullMediaRequester{err: errors.New("phone said no")}
	registry := availableDownloadRegistry(&scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
	}})
	registry.fullMedia = requester
	service := newMediaTestService(t, env, registry)
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	before := getMediaAttachment(t, env, "message-thumb", 0)
	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	after := getMediaAttachment(t, env, "message-thumb", 0)
	if len(requester.requests()) != 1 || after.LastError != nil || *after.BlobHash != *before.BlobHash || !after.AwaitsOriginal() {
		t.Fatalf("after a refused request: %d requests, row %+v; want one request and the row unchanged", len(requester.requests()), after)
	}
}

// A transport that cannot ask for originals gets no request, and the
// thumbnail is still served.
func TestResolveWithoutAFullMediaRequesterServesTheThumbnail(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	registry := availableDownloadRegistry(&scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
	}})
	service := newMediaTestService(t, env, registry)
	t.Cleanup(service.Close)

	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(thumbnailBytes) {
		t.Fatalf("Resolve() served %s, want the thumbnail", got.Ref.Hash)
	}
	service.background.Wait()
	if got := registry.acquireCalls(); got != 2 {
		t.Fatalf("leases = %d, want 2 (the download and the request that found no requester)", got)
	}
}

// When the transport names the original of a downloaded thumbnail, the next
// read downloads it in the thumbnail's place and serves it, and later reads
// make no transport call.
func TestResolveReplacesAThumbnailWithItsOriginal(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
		byteStreamStep(originalBytes, "image/jpeg"),
	}}
	requester := &recordingFullMediaRequester{}
	registry := availableDownloadRegistry(downloader)
	registry.fullMedia = requester
	service := newMediaTestService(t, env, registry)
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	nameOriginal(t, env, "message-thumb", "named")
	named := getMediaAttachment(t, env, "message-thumb", 0)
	if named.State != "downloaded" || named.RemoteID != "full-1" || !named.NeedsOriginal() || *named.BlobHash != sha256Hex(thumbnailBytes) {
		t.Fatalf("row after the original was named = %+v, want the thumbnail kept and the original's reference", named)
	}

	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(originalBytes) || got.Size != int64(len(originalBytes)) {
		t.Fatalf("Resolve() after naming served %s (%d bytes), want the original", got.Ref.Hash, got.Size)
	}
	upgraded := getMediaAttachment(t, env, "message-thumb", 0)
	if upgraded.BlobPreview || upgraded.NeedsOriginal() || upgraded.AwaitsOriginal() || *upgraded.BlobHash != sha256Hex(originalBytes) {
		t.Fatalf("row after the upgrade = %+v, want the original and nothing more to do", upgraded)
	}
	requests := downloader.snapshotRequests()
	if len(requests) != 2 || requests[1].ref.RemoteID != "full-1" {
		t.Fatalf("downloads = %+v, want the thumbnail then full-1", requests)
	}

	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	if downloader.requestCount() != 2 || len(requester.requests()) != 1 {
		t.Fatalf("after the upgrade: %d downloads, %d requests; want no more of either", downloader.requestCount(), len(requester.requests()))
	}
}

// An original whose download outlasts originalWait is served the next time
// after it lands; the reader in between gets the thumbnail.
func TestResolveServesTheThumbnailWhileASlowOriginalDownloads(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	release := make(chan struct{})
	slow := byteStreamStep(originalBytes, "image/jpeg")
	slow.beforeReturn = func() { <-release }
	downloader := &scriptedDownloader{steps: []downloadStep{byteStreamStep(thumbnailBytes, "image/jpeg"), slow}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	service.originalWait = 10 * time.Millisecond
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	nameOriginal(t, env, "message-thumb", "named")
	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(thumbnailBytes) {
		t.Fatalf("Resolve() during a slow original served %s, want the thumbnail", got.Ref.Hash)
	}
	close(release)
	service.background.Wait()
	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(originalBytes) {
		t.Fatalf("Resolve() after the original landed served %s, want the original", got.Ref.Hash)
	}
	if got := downloader.requestCount(); got != 2 {
		t.Fatalf("downloads = %d, want 2", got)
	}
}

// Readers that arrive while an original downloads share that download.
func TestResolveConcurrentReadersShareOneOriginalDownload(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	release := make(chan struct{})
	started := make(chan struct{})
	slow := byteStreamStep(originalBytes, "image/jpeg")
	slow.beforeReturn = func() {
		close(started)
		<-release
	}
	downloader := &scriptedDownloader{steps: []downloadStep{byteStreamStep(thumbnailBytes, "image/jpeg"), slow}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	nameOriginal(t, env, "message-thumb", "named")
	const readers = 8
	results := make(chan resolveResult, readers)
	for range readers {
		go func() {
			descriptor, err := service.Resolve(context.Background(), DownloadCommand{AccountID: "account-1", MessageID: "message-thumb"})
			results <- resolveResult{descriptor: descriptor, err: err}
		}()
	}
	<-started
	close(release)
	for range readers {
		result := <-results
		if result.err != nil {
			t.Fatalf("Resolve(): %v", result.err)
		}
		if hash := result.descriptor.Ref.Hash; hash != sha256Hex(originalBytes) && hash != sha256Hex(thumbnailBytes) {
			t.Fatalf("Resolve() served %s, want the thumbnail or the original", hash)
		}
	}
	service.background.Wait()
	if got := downloader.requestCount(); got != 2 {
		t.Fatalf("downloads = %d, want the thumbnail and one original", got)
	}
}

// A failed original download leaves the thumbnail served and in place, and
// holds off the next attempt for originalRetryAfter.
func TestResolveFailedOriginalKeepsTheThumbnailAndWaitsToRetry(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	downloader := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
		{err: bridge.OpError{Class: bridge.FailureTransient, Operation: "download_media", Cause: errors.New("phone offline")}},
		byteStreamStep(originalBytes, "image/jpeg"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	nameOriginal(t, env, "message-thumb", "named")
	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(thumbnailBytes) {
		t.Fatalf("Resolve() after a failed original served %s, want the thumbnail", got.Ref.Hash)
	}
	service.background.Wait()
	if row := getMediaAttachment(t, env, "message-thumb", 0); !row.NeedsOriginal() || row.LastError != nil {
		t.Fatalf("row after a failed original = %+v, want the thumbnail, the original still wanted, nothing recorded", row)
	}

	env.clock.advance(originalRetryAfter - time.Second)
	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	if got := downloader.requestCount(); got != 2 {
		t.Fatalf("downloads inside the hold = %d, want 2", got)
	}
	env.clock.advance(time.Second)
	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(originalBytes) {
		t.Fatalf("Resolve() once the hold ends served %s, want the original", got.Ref.Hash)
	}
}

// Bytes imported over a thumbnail are taken as the original: nothing asks
// for or downloads another.
func TestImportOverAThumbnailEndsTheWaitForTheOriginal(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	requester := &recordingFullMediaRequester{}
	downloader := &scriptedDownloader{steps: []downloadStep{byteStreamStep(thumbnailBytes, "image/jpeg")}}
	registry := availableDownloadRegistry(downloader)
	registry.fullMedia = requester
	service := newMediaTestService(t, env, registry)
	t.Cleanup(service.Close)

	resolveImage(t, service, "message-thumb")
	service.background.Wait()
	nameOriginal(t, env, "message-thumb", "named")
	replace := importCommand("message-thumb", originalBytes, sha256Hex(originalBytes))
	replace.Replace = true
	if _, imported, err := service.Import(context.Background(), replace); err != nil || !imported {
		t.Fatalf("Import(replace) = imported=%v, %v", imported, err)
	}
	if got := resolveImage(t, service, "message-thumb"); got.Ref.Hash != sha256Hex(originalBytes) {
		t.Fatalf("Resolve() after the import served %s, want the imported bytes", got.Ref.Hash)
	}
	service.background.Wait()
	if downloader.requestCount() != 1 || len(requester.requests()) != 1 {
		t.Fatalf("after the import: %d downloads, %d requests; want no more of either", downloader.requestCount(), len(requester.requests()))
	}
}

// contextDownloader blocks each download until its context ends.
type contextDownloader struct {
	started chan struct{}
}

func (d *contextDownloader) DownloadMedia(ctx context.Context, _ string, _ bridge.MediaRef) (bridge.MediaStream, error) {
	close(d.started)
	<-ctx.Done()
	return bridge.MediaStream{}, ctx.Err()
}

// Close cancels an original downloading in the background and waits for it,
// and Resolve afterwards serves what is stored without starting more.
func TestCloseStopsWorkTowardOriginals(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedThumbnail(t, env, "message-thumb")
	seedThumbnail(t, env, "message-other")
	seedThumbnail(t, env, "message-late")
	thumbnails := &scriptedDownloader{steps: []downloadStep{
		byteStreamStep(thumbnailBytes, "image/jpeg"),
		byteStreamStep(thumbnailBytes, "image/jpeg"),
	}}
	registry := availableDownloadRegistry(thumbnails)
	requester := &recordingFullMediaRequester{}
	registry.fullMedia = requester
	service := newMediaTestService(t, env, registry)
	service.originalWait = time.Millisecond

	resolveImage(t, service, "message-thumb")
	resolveImage(t, service, "message-other")
	service.background.Wait()
	nameOriginal(t, env, "message-thumb", "named")
	nameOriginal(t, env, "message-other", "named")
	blocking := &contextDownloader{started: make(chan struct{})}
	registry.mu.Lock()
	registry.downloader = blocking
	registry.mu.Unlock()
	resolveImage(t, service, "message-thumb")
	<-blocking.started

	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return while an original was downloading")
	}
	if row := getMediaAttachment(t, env, "message-thumb", 0); !row.NeedsOriginal() {
		t.Fatalf("row after Close = %+v, want the thumbnail still in place", row)
	}

	// Nothing starts after Close: not the original of a thumbnail whose
	// download failed (message-thumb, also held off), nor of one never tried
	// (message-other), nor a request for the original of a thumbnail first
	// downloaded now (message-late).
	acquires := registry.acquireCalls()
	for _, messageID := range []string{"message-thumb", "message-other"} {
		if got := resolveImage(t, service, messageID); got.Ref.Hash != sha256Hex(thumbnailBytes) {
			t.Fatalf("Resolve(%s) after Close served %s, want the thumbnail", messageID, got.Ref.Hash)
		}
	}
	service.background.Wait()
	if got := registry.acquireCalls(); got != acquires {
		t.Fatalf("leases after Close = %d, want %d: no new work", got, acquires)
	}
	registry.mu.Lock()
	registry.downloader = &scriptedDownloader{steps: []downloadStep{byteStreamStep(thumbnailBytes, "image/jpeg")}}
	registry.mu.Unlock()
	requests := len(requester.requests())
	if got := resolveImage(t, service, "message-late"); got.Ref.Hash != sha256Hex(thumbnailBytes) {
		t.Fatalf("Resolve(message-late) after Close served %s, want its thumbnail", got.Ref.Hash)
	}
	service.background.Wait()
	if got := len(requester.requests()); got != requests {
		t.Fatalf("requests for originals after Close = %d, want %d", got, requests)
	}
}
