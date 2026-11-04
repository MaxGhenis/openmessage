package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"golang.org/x/sync/singleflight"
)

const failureRecordTimeout = 5 * time.Second

// NotReadyGrace is how long after an attachment's row is created its
// transport may go on naming no media (bridge.ErrMediaNotReady) before a
// download answers ErrGone instead of ErrNotReady. Google names an image
// about a second after the message; an attachment still unnamed after this
// long was never uploaded, or the update that named it was lost, and readers
// should stop waiting for it. Nothing is recorded either way: if a reference
// does arrive, the next download uses it.
const NotReadyGrace = 15 * time.Minute

const (
	// defaultOriginalWait is how long Resolve waits for an attachment's
	// original to replace its downloaded reduced copy before serving the
	// copy. The download carries on, and the attachment's media version
	// changes when it lands, so a reader asks again.
	defaultOriginalWait = 8 * time.Second
	// originalTimeout bounds one download of an original.
	originalTimeout = time.Minute
	// originalRetryAfter is how long a failed download of an original, or a
	// request for one, holds off the next attempt for the same attachment.
	originalRetryAfter = 30 * time.Minute
	// originalRequestTimeout bounds one request for an original, including
	// its wait for one of originalRequestSlots.
	originalRequestTimeout = 30 * time.Second
	// originalRequestSlots bounds requests for originals in flight, so a
	// thread full of old thumbnails does not ask the phone for all at once.
	originalRequestSlots = 4
)

// Clock is the same application clock contract used by MessageService.
type Clock = messaging.Clock

// Service resolves inbound attachment references and materializes their bytes
// in the application-owned BlobStore.
type Service struct {
	attachments *sqlite.MessageAttachmentRepository
	bridges     bridge.Registry
	blobs       *blob.BlobStore
	clock       Clock
	maxBytes    int64
	group       singleflight.Group

	// originals holds off work on reduced copies (a thumbnail standing in for
	// an image), by hold and attachment: when a download of the original last
	// failed, and when the original was last requested from the transport.
	// downloads holds the downloads of originals in flight. background tracks
	// the goroutines doing both, which can outlive the Resolve that started
	// them; backgroundCtx is theirs, and Close cancels it.
	originalsMu    sync.Mutex
	originals      map[string]time.Time
	downloads      map[string]*originalDownload
	closed         bool
	background     sync.WaitGroup
	backgroundCtx  context.Context
	stopBackground context.CancelFunc
	requestsSlots  chan struct{}
	originalWait   time.Duration
}

// DownloadCommand identifies one message-owned attachment. It deliberately
// carries neither a transport reference nor a filesystem path.
type DownloadCommand struct {
	AccountID string
	MessageID string
	Ordinal   int64
}

// Descriptor is the safe, path-free serving view of a materialized attachment.
type Descriptor struct {
	Ref         blob.BlobRef
	MIME        string
	Filename    string
	Size        int64
	Disposition Disposition
}

var (
	ErrUnsupported = errors.New("media: account bridge cannot download media")
	ErrUnavailable = errors.New("media: download capability unavailable")
	ErrNotFound    = errors.New("media: attachment not found")
	ErrTooLarge    = errors.New("media: attachment exceeds size limit")
	// ErrGone reports an attachment whose transport no longer has its bytes
	// (bridge.ErrMediaGone). The row stays pending: Import can still fill
	// it, and a later download succeeds if the transport's copy comes back.
	ErrGone = errors.New("media: attachment is no longer available from its transport")
	// ErrNotReady reports an attachment whose message arrived before its
	// transport said where the bytes are (bridge.ErrMediaNotReady). The row
	// stays pending with nothing recorded: the message's next update fills in
	// the reference, and a later download can then succeed. After
	// NotReadyGrace the same answer is ErrGone, still unrecorded.
	ErrNotReady = errors.New("media: attachment is not available from its transport yet")
	// ErrImportMismatch rejects imported bytes that contradict what the
	// attachment row or the caller says they must be.
	ErrImportMismatch = errors.New("media: imported content does not match the attachment")
	// ErrImportConflict rejects an import into an attachment that already
	// holds different bytes.
	ErrImportConflict = errors.New("media: attachment already holds different content")
)

// ImportCommand fills one pending attachment with bytes obtained outside its
// transport, such as a copy saved from another client of the same account.
type ImportCommand struct {
	AccountID string
	MessageID string
	Ordinal   int64
	Content   io.Reader
	// SHA256 is the lowercase hex digest the content must have, or empty.
	SHA256 string
	// Replace lets the import swap the bytes of an attachment that is
	// already downloaded, to correct an earlier import of the wrong file.
	Replace bool
}

// NewService constructs the media download service over application-owned
// storage and transport capabilities. The caller retains ownership of every
// dependency.
func NewService(
	store *sqlite.Store,
	bridges bridge.Registry,
	blobs *blob.BlobStore,
	clock Clock,
) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("create media service: store is nil")
	}
	if bridges == nil {
		return nil, fmt.Errorf("create media service: bridge registry is nil")
	}
	if blobs == nil {
		return nil, fmt.Errorf("create media service: blob store is nil")
	}
	if clock == nil {
		return nil, fmt.Errorf("create media service: clock is nil")
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		return nil, fmt.Errorf("create media service attachments: %w", err)
	}
	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	return &Service{
		attachments:    attachments,
		bridges:        bridges,
		blobs:          blobs,
		clock:          clock,
		maxBytes:       messaging.DefaultMaxMediaBytes,
		originals:      make(map[string]time.Time),
		downloads:      make(map[string]*originalDownload),
		backgroundCtx:  backgroundCtx,
		stopBackground: stopBackground,
		requestsSlots:  make(chan struct{}, originalRequestSlots),
		originalWait:   defaultOriginalWait,
	}, nil
}

// Close cancels the work toward originals that Resolve started in the
// background and waits for it to end, so the caller can close the store
// after. Resolve still serves what is stored afterwards; it starts no more
// such work.
func (s *Service) Close() {
	s.originalsMu.Lock()
	s.closed = true
	s.originalsMu.Unlock()
	s.stopBackground()
	s.background.Wait()
}

// Resolve returns a safe descriptor for one message attachment, downloading it
// on demand when the durable row is still pending.
func (s *Service) Resolve(ctx context.Context, command DownloadCommand) (Descriptor, error) {
	if ctx == nil {
		return Descriptor{}, fmt.Errorf("resolve media: context is nil")
	}
	if strings.TrimSpace(command.AccountID) == "" {
		return Descriptor{}, fmt.Errorf("resolve media: account ID is empty")
	}
	if strings.TrimSpace(command.MessageID) == "" {
		return Descriptor{}, fmt.Errorf("resolve media: message ID is empty")
	}
	if command.Ordinal < 0 {
		return Descriptor{}, fmt.Errorf("resolve media: attachment ordinal is negative")
	}

	row, err := s.attachments.GetForDownload(ctx, command.MessageID, command.Ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		return Descriptor{}, ErrNotFound
	}
	if err != nil {
		return Descriptor{}, fmt.Errorf("resolve media: read attachment: %w", err)
	}
	if row.AccountID != command.AccountID {
		return Descriptor{}, ErrNotFound
	}
	if row.State != "downloaded" {
		if err := s.unavailableError(row); err != nil {
			return Descriptor{}, err
		}
		_, err, _ = s.group.Do(attachmentKey(row), func() (any, error) {
			return s.download(ctx, row)
		})
		if err != nil {
			return Descriptor{}, err
		}

		// Re-read after the slot rather than trusting its returned BlobRef. A
		// concurrent process may have won MarkDownloaded; the durable row is
		// the authoritative winner in that race and also contains the resolved
		// MIME.
		row, err = s.attachments.GetForDownload(ctx, command.MessageID, command.Ordinal)
		if errors.Is(err, sql.ErrNoRows) {
			return Descriptor{}, ErrNotFound
		}
		if err != nil {
			return Descriptor{}, fmt.Errorf("resolve media: reload attachment: %w", err)
		}
		if row.AccountID != command.AccountID {
			return Descriptor{}, ErrNotFound
		}
	}
	return s.descriptorForRow(s.improve(ctx, row))
}

// improve works toward the original of an attachment whose downloaded bytes
// are a reduced copy. When the reference names the original it downloads the
// original in the copy's place, waiting up to originalWait (8 s) for it; otherwise
// it asks the transport to name the original. It returns the row to serve and
// never fails: the reduced copy still shows the attachment.
func (s *Service) improve(ctx context.Context, row sqlite.MessageAttachment) sqlite.MessageAttachment {
	switch {
	case row.NeedsOriginal():
		download := s.startOriginalDownload(row)
		if download == nil {
			return row
		}
		timer := s.clock.NewTimer(s.originalWait)
		defer timer.Stop()
		select {
		case <-download.done:
			if download.err == nil {
				return download.row
			}
		case <-timer.C():
		case <-ctx.Done():
		}
	case row.AwaitsOriginal():
		s.startOriginalRequest(row)
	}
	return row
}

// Holds on work toward an attachment's original. A failed download of the
// original holds off the next for originalRetryAfter, and so does each
// request for one, which the transport answers, if at all, by naming it.
const (
	originalDownloadHold = "download\x00"
	originalRequestHold  = "request\x00"
)

// originalDownload is one download of an attachment's original in flight;
// done closes when row and err are set.
type originalDownload struct {
	done chan struct{}
	row  sqlite.MessageAttachment
	err  error
}

// startOriginalDownload starts downloading row's original, or joins the
// download already in flight, and returns nil while a failed one holds off
// the next or after Close. The download runs detached from any request.
func (s *Service) startOriginalDownload(row sqlite.MessageAttachment) *originalDownload {
	key := attachmentKey(row)
	s.originalsMu.Lock()
	defer s.originalsMu.Unlock()
	if download, ok := s.downloads[key]; ok {
		return download
	}
	if s.closed || s.heldLocked(originalDownloadHold+key) {
		return nil
	}
	download := &originalDownload{done: make(chan struct{})}
	s.downloads[key] = download
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		upgraded, err := s.downloadOriginal(row)
		s.originalsMu.Lock()
		delete(s.downloads, key)
		if err != nil {
			s.holdLocked(originalDownloadHold + key)
		}
		s.originalsMu.Unlock()
		download.row, download.err = upgraded, err
		close(download.done)
	}()
	return download
}

// startOriginalRequest asks row's transport for its original in the
// background, unless a request for it was made within originalRetryAfter or
// the service is closed. The request holds off the next whatever its outcome.
func (s *Service) startOriginalRequest(row sqlite.MessageAttachment) {
	key := originalRequestHold + attachmentKey(row)
	s.originalsMu.Lock()
	defer s.originalsMu.Unlock()
	if s.closed || s.heldLocked(key) {
		return
	}
	s.holdLocked(key)
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		s.requestOriginal(row)
	}()
}

func (s *Service) heldLocked(key string) bool {
	since, ok := s.originals[key]
	return ok && s.clock.Now().Sub(since) < originalRetryAfter
}

func (s *Service) holdLocked(key string) {
	now := s.clock.Now()
	if len(s.originals) >= 4096 {
		for other, since := range s.originals {
			if now.Sub(since) >= originalRetryAfter {
				delete(s.originals, other)
			}
		}
	}
	s.originals[key] = now
}

// downloadOriginal downloads the original row's reference names and puts it
// in place of the reduced copy row holds. It runs detached from the request
// that started it, so a reader that stops waiting does not waste it.
func (s *Service) downloadOriginal(row sqlite.MessageAttachment) (sqlite.MessageAttachment, error) {
	if row.BlobHash == nil {
		return row, errors.New("download original media: attachment holds no bytes")
	}
	ctx, cancel := context.WithTimeout(s.backgroundCtx, originalTimeout)
	defer cancel()
	lease, err := s.acquireDownload(ctx, row.AccountID)
	if err != nil {
		return row, err
	}
	defer lease.Close()
	stream, err := lease.Download.DownloadMedia(ctx, row.AccountID, bridge.MediaRef{
		RemoteID: row.RemoteID,
		Opaque:   row.RemoteRef,
		Filename: row.Filename,
		MIME:     row.MIME,
	})
	if err != nil {
		return row, fmt.Errorf("download original media: %w", err)
	}
	if stream.ReadCloser == nil {
		return row, errors.New("download original media: transport returned no stream")
	}
	defer stream.Close()
	mime := firstNonEmpty(stream.MIME, row.MIME, "application/octet-stream")
	ref, err := s.blobs.Put(ctx, stream, mime, s.maxBytes)
	if err != nil {
		return row, fmt.Errorf("download original media: store: %w", err)
	}
	if _, err := s.attachments.ReplaceDownloaded(
		ctx, row.MessageID, row.Ordinal, *row.BlobHash, ref.Hash, ref.Size, mime, false,
	); err != nil {
		return row, fmt.Errorf("download original media: replace: %w", err)
	}
	// The durable row decides: an import may have replaced the bytes first.
	current, err := s.attachments.GetForDownload(ctx, row.MessageID, row.Ordinal)
	if err != nil {
		return row, fmt.Errorf("download original media: reload attachment: %w", err)
	}
	return current, nil
}

// requestOriginal asks row's transport to name the original of the reduced
// copy row holds. The transport answers, if at all, with an update of the
// message that the projection applies. Failures are not recorded; the hold
// placed for this request paces the next one.
func (s *Service) requestOriginal(row sqlite.MessageAttachment) {
	ctx, cancel := context.WithTimeout(s.backgroundCtx, originalRequestTimeout)
	defer cancel()
	select {
	case s.requestsSlots <- struct{}{}:
		defer func() { <-s.requestsSlots }()
	case <-ctx.Done():
		return
	}
	lease, err := s.acquireDownload(ctx, row.AccountID)
	if err != nil {
		return
	}
	defer lease.Close()
	if lease.FullMedia == nil {
		return
	}
	_ = lease.FullMedia.RequestFullMedia(ctx, row.AccountID, bridge.MediaRef{
		RemoteID: row.RemoteID,
		Opaque:   row.RemoteRef,
		Filename: row.Filename,
		MIME:     row.MIME,
	})
}

func attachmentKey(row sqlite.MessageAttachment) string {
	return row.MessageID + "\x00" + strconv.FormatInt(row.Ordinal, 10)
}

// Open delegates to the BlobStore's hash-validated, private-root reader.
func (s *Service) Open(ref blob.BlobRef) (io.ReadCloser, error) {
	return s.blobs.Open(ref)
}

func (s *Service) download(
	ctx context.Context,
	requested sqlite.MessageAttachment,
) (blob.BlobRef, error) {
	row, err := s.attachments.GetForDownload(ctx, requested.MessageID, requested.Ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		return blob.BlobRef{}, ErrNotFound
	}
	if err != nil {
		return blob.BlobRef{}, fmt.Errorf("download media: reload attachment: %w", err)
	}
	if row.AccountID != requested.AccountID {
		return blob.BlobRef{}, ErrNotFound
	}
	if row.State == "downloaded" {
		return blobRefForAttachment(row)
	}
	if err := s.unavailableError(row); err != nil {
		return blob.BlobRef{}, err
	}

	lease, err := s.acquireDownload(ctx, row.AccountID)
	if err != nil {
		return blob.BlobRef{}, err
	}
	defer lease.Close()

	stream, err := lease.Download.DownloadMedia(ctx, row.AccountID, bridge.MediaRef{
		RemoteID: row.RemoteID,
		Opaque:   row.RemoteRef,
		Filename: row.Filename,
		MIME:     row.MIME,
	})
	if err != nil {
		if errors.Is(err, bridge.ErrMediaNotReady) {
			if waited := s.clock.Now().Sub(time.UnixMilli(row.CreatedAtMS)); waited >= NotReadyGrace {
				return blob.BlobRef{}, fmt.Errorf(
					"%w: %w (still, %s after the attachment arrived)",
					ErrGone, err, waited.Round(time.Minute),
				)
			}
			return blob.BlobRef{}, fmt.Errorf("%w: %w", ErrNotReady, err)
		}
		if errors.Is(err, bridge.ErrMediaGone) {
			return blob.BlobRef{}, s.recordGone(ctx, row, err)
		}
		return blob.BlobRef{}, s.recordFailure(ctx, row, classifyTransportError(err))
	}
	if stream.ReadCloser == nil {
		// A MediaStream value wrapping a nil interface is itself non-nil, so
		// blob.Put's nil-reader guard cannot catch it and Close would panic.
		return blob.BlobRef{}, s.recordFailure(ctx, row, errors.New(
			"download media: transport returned no stream",
		))
	}
	defer stream.Close()

	mime := firstNonEmpty(stream.MIME, row.MIME, "application/octet-stream")
	ref, err := s.blobs.Put(ctx, stream, mime, s.maxBytes)
	if err != nil {
		var tooLarge *blob.ErrTooLarge
		if errors.As(err, &tooLarge) {
			failure := fmt.Errorf("%w: %v", ErrTooLarge, tooLarge)
			return blob.BlobRef{}, s.recordFailure(ctx, row, failure)
		}
		return blob.BlobRef{}, s.recordFailure(ctx, row, err)
	}
	if err := s.attachments.MarkDownloaded(
		ctx,
		row.MessageID,
		row.Ordinal,
		ref.Hash,
		ref.Size,
		mime,
		row.RemotePreview,
	); err != nil {
		return blob.BlobRef{}, fmt.Errorf("download media: mark downloaded: %w", err)
	}
	return ref, nil
}

// acquireDownload leases accountID's media download capability. The caller
// closes the lease.
func (s *Service) acquireDownload(ctx context.Context, accountID string) (*bridge.DispatchLease, error) {
	// Capabilities returns the zero set for both an unsupported adapter and an
	// unregistered account. Snapshot preserves the routing table's distinction:
	// unregistered is unavailable, while registered-but-unsupported is terminal.
	if _, registered := s.bridges.Snapshot(accountID); !registered {
		return nil, ErrUnavailable
	}
	if !s.bridges.Capabilities(accountID).MediaDownload {
		return nil, ErrUnsupported
	}

	lease, err := s.bridges.Acquire(ctx, accountID, bridge.CapabilityMediaDownload)
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrAccountNotRegistered):
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		case errors.Is(err, bridge.ErrCapabilityUnavailable):
			if _, registered := s.bridges.Snapshot(accountID); !registered {
				return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
			}
			if !s.bridges.Capabilities(accountID).MediaDownload {
				return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
			}
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		default:
			return nil, err
		}
	}
	if lease == nil || lease.Download == nil {
		if lease != nil {
			lease.Close()
		}
		return nil, ErrUnavailable
	}
	return lease, nil
}

func (s *Service) descriptorForRow(row sqlite.MessageAttachment) (Descriptor, error) {
	ref, err := blobRefForAttachment(row)
	if err != nil {
		return Descriptor{}, err
	}
	reader, err := s.blobs.Open(ref)
	if err != nil {
		return Descriptor{}, fmt.Errorf("describe media: open blob: %w", err)
	}
	sniff, readErr := io.ReadAll(io.LimitReader(reader, 512))
	closeErr := reader.Close()
	if readErr != nil {
		return Descriptor{}, fmt.Errorf("describe media: sniff blob: %w", readErr)
	}
	if closeErr != nil {
		return Descriptor{}, fmt.Errorf("describe media: close blob: %w", closeErr)
	}

	resolvedMIME := normalizeMIME(firstNonEmpty(row.MIME, ref.MIME, "application/octet-stream"))
	if resolvedMIME == "" {
		resolvedMIME = "application/octet-stream"
	}
	disposition := ServingDisposition(resolvedMIME, sniff)
	if disposition == DispositionForcedDownload {
		resolvedMIME = "application/octet-stream"
	}
	return Descriptor{
		Ref:         ref,
		MIME:        resolvedMIME,
		Filename:    sanitizeMediaFilename(row.Filename),
		Size:        ref.Size,
		Disposition: disposition,
	}, nil
}

func (s *Service) recordFailure(
	ctx context.Context,
	row sqlite.MessageAttachment,
	failure error,
) error {
	if row.LastError != nil && strings.HasPrefix(*row.LastError, sqlite.ReceiveAttachmentErrorPrefix) {
		// A transient failure (the transport offline, say) says nothing new
		// about this attachment; keep the transport's receive-time report.
		return failure
	}
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureRecordTimeout)
	defer cancel()
	text := failure.Error()
	if row.LastError != nil && strings.HasPrefix(*row.LastError, sqlite.UnavailableAttachmentErrorPrefix) {
		// Nor does it lift an unavailable mark. Rewriting the mark restarts
		// its window, so a transport that keeps failing is asked at most
		// once per window rather than on every request.
		text = *row.LastError
	}
	if err := s.attachments.MarkFailed(
		mutationCtx,
		row.MessageID,
		row.Ordinal,
		text,
	); err != nil {
		return errors.Join(failure, fmt.Errorf("record media download failure: %w", err))
	}
	return failure
}

// receiveCauseMarker opens the receive-time cause an unavailable mark keeps.
const receiveCauseMarker = " (when the message arrived: "

// recordGone marks an attachment whose transport has no copy of it
// unavailable (sqlite.UnavailableAttachmentErrorPrefix), so requests within
// sqlite.UnavailableAttachmentRecheckAfter answer ErrGone without asking the
// transport. The reason keeps the transport's receive-time account of the
// failure, from the projection's note or the mark it replaces, because that
// is the useful diagnosis.
func (s *Service) recordGone(
	ctx context.Context,
	row sqlite.MessageAttachment,
	transportErr error,
) error {
	receiveCause := ""
	if row.LastError != nil {
		if cause, ok := strings.CutPrefix(*row.LastError, sqlite.ReceiveAttachmentErrorPrefix); ok {
			receiveCause = receiveCauseMarker + strings.TrimSpace(cause) + ")"
		} else if strings.HasPrefix(*row.LastError, sqlite.UnavailableAttachmentErrorPrefix) {
			if at := strings.Index(*row.LastError, receiveCauseMarker); at >= 0 {
				receiveCause = (*row.LastError)[at:]
			}
		}
	}
	failure := fmt.Errorf("%w: %w%s", ErrGone, transportErr, receiveCause)
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureRecordTimeout)
	defer cancel()
	if err := s.attachments.MarkFailed(
		mutationCtx,
		row.MessageID,
		row.Ordinal,
		sqlite.UnavailableAttachmentErrorPrefix+transportErr.Error()+receiveCause,
	); err != nil {
		return errors.Join(failure, fmt.Errorf("record media download failure: %w", err))
	}
	return failure
}

// unavailableError answers for an attachment whose unavailable mark still
// stands; an older mark lets the transport be asked again.
func (s *Service) unavailableError(row sqlite.MessageAttachment) error {
	reason, ok := row.UnavailableReasonAt(s.clock.Now())
	if !ok {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrGone, reason)
}

// Import stores content as one attachment's bytes and marks it downloaded,
// for an attachment its transport cannot deliver (ErrGone). Only a pending
// attachment accepts new bytes, unless command.Replace is set. Importing the
// bytes an attachment already holds is an idempotent success that reports
// imported=false; different bytes fail with ErrImportConflict. The content
// must be non-empty, match command.SHA256 when that is set, and match the
// size the transport declared when it declared one. The stored MIME type and
// filename stay the transport's.
func (s *Service) Import(ctx context.Context, command ImportCommand) (Descriptor, bool, error) {
	if ctx == nil {
		return Descriptor{}, false, fmt.Errorf("import media: context is nil")
	}
	if strings.TrimSpace(command.AccountID) == "" {
		return Descriptor{}, false, fmt.Errorf("import media: account ID is empty")
	}
	if strings.TrimSpace(command.MessageID) == "" {
		return Descriptor{}, false, fmt.Errorf("import media: message ID is empty")
	}
	if command.Ordinal < 0 {
		return Descriptor{}, false, fmt.Errorf("import media: attachment ordinal is negative")
	}
	if command.Content == nil {
		return Descriptor{}, false, fmt.Errorf("import media: content is nil")
	}
	wantSHA := strings.ToLower(strings.TrimSpace(command.SHA256))

	row, err := s.attachments.GetForDownload(ctx, command.MessageID, command.Ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		return Descriptor{}, false, ErrNotFound
	}
	if err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: read attachment: %w", err)
	}
	if row.AccountID != command.AccountID {
		return Descriptor{}, false, ErrNotFound
	}

	// Validate the whole content before anything reaches the BlobStore, so a
	// rejected import leaves no blob behind.
	data, err := io.ReadAll(io.LimitReader(command.Content, s.maxBytes+1))
	if err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: read content: %w", err)
	}
	if int64(len(data)) > s.maxBytes {
		return Descriptor{}, false, fmt.Errorf("%w: content exceeds %d bytes", ErrTooLarge, s.maxBytes)
	}
	if len(data) == 0 {
		return Descriptor{}, false, fmt.Errorf("%w: content is empty", ErrImportMismatch)
	}
	digest := sha256.Sum256(data)
	gotSHA := hex.EncodeToString(digest[:])
	if wantSHA != "" && gotSHA != wantSHA {
		return Descriptor{}, false, fmt.Errorf("%w: content SHA-256 is %s, expected %s", ErrImportMismatch, gotSHA, wantSHA)
	}
	replacing := false
	if row.State == "downloaded" {
		if row.BlobHash != nil && *row.BlobHash == gotSHA {
			descriptor, err := s.descriptorForRow(row)
			return descriptor, false, err
		}
		if !command.Replace || row.BlobHash == nil {
			return Descriptor{}, false, ErrImportConflict
		}
		replacing = true
	}
	// A downloaded row's size_bytes is the stored blob's size, not a
	// declaration, so only a pending row's size constrains the import.
	if !replacing && row.SizeBytes != nil && *row.SizeBytes > 0 && int64(len(data)) != *row.SizeBytes {
		return Descriptor{}, false, fmt.Errorf("%w: content is %d bytes, the transport declared %d",
			ErrImportMismatch, len(data), *row.SizeBytes)
	}

	mime := firstNonEmpty(row.MIME, "application/octet-stream")
	ref, err := s.blobs.Put(ctx, bytes.NewReader(data), mime, s.maxBytes)
	if err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: %w", err)
	}
	if replacing {
		swapped, err := s.attachments.ReplaceDownloaded(ctx, row.MessageID, row.Ordinal, *row.BlobHash, ref.Hash, ref.Size, mime, false)
		if err != nil {
			return Descriptor{}, false, fmt.Errorf("import media: replace: %w", err)
		}
		if !swapped {
			// Lost a race; the reload below accepts a winner that stored
			// these same bytes.
			row, err = s.attachments.GetForDownload(ctx, command.MessageID, command.Ordinal)
			if err != nil {
				return Descriptor{}, false, fmt.Errorf("import media: reload attachment: %w", err)
			}
			if row.State != "downloaded" || row.BlobHash == nil || *row.BlobHash != ref.Hash {
				return Descriptor{}, false, ErrImportConflict
			}
			descriptor, err := s.descriptorForRow(row)
			return descriptor, false, err
		}
	} else if err := s.attachments.MarkDownloaded(ctx, row.MessageID, row.Ordinal, ref.Hash, ref.Size, mime, false); err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: mark downloaded: %w", err)
	}
	// A concurrent download may have won MarkDownloaded with other bytes; the
	// durable row decides.
	row, err = s.attachments.GetForDownload(ctx, command.MessageID, command.Ordinal)
	if err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: reload attachment: %w", err)
	}
	if row.State != "downloaded" || row.BlobHash == nil || *row.BlobHash != ref.Hash {
		return Descriptor{}, false, ErrImportConflict
	}
	descriptor, err := s.descriptorForRow(row)
	return descriptor, err == nil, err
}

func blobRefForAttachment(row sqlite.MessageAttachment) (blob.BlobRef, error) {
	if row.State != "downloaded" || row.BlobHash == nil {
		return blob.BlobRef{}, fmt.Errorf("describe media: attachment is not downloaded")
	}
	var size int64
	if row.SizeBytes != nil {
		size = *row.SizeBytes
	}
	return blob.BlobRef{Hash: *row.BlobHash, Size: size, MIME: row.MIME}, nil
}

func classifyTransportError(err error) error {
	failure, ok := asOpError(err)
	if ok && failure.Class == bridge.FailureUnsupported {
		return errors.Join(ErrUnsupported, err)
	}
	// The service returns all other adapter errors intact. Their OpError class
	// tells callers whether the result is terminal (unsupported, unpaired, or
	// expired credentials) or transient without inventing a durable failed state.
	return err
}

func asOpError(err error) (bridge.OpError, bool) {
	var pointer *bridge.OpError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	var value bridge.OpError
	if errors.As(err, &value) {
		return value, true
	}
	return bridge.OpError{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
