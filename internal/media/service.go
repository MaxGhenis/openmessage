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
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"golang.org/x/sync/singleflight"
)

const failureRecordTimeout = 5 * time.Second

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
	return &Service{
		attachments: attachments,
		bridges:     bridges,
		blobs:       blobs,
		clock:       clock,
		maxBytes:    messaging.DefaultMaxMediaBytes,
	}, nil
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
	if row.State == "downloaded" {
		return s.descriptorForRow(row)
	}

	key := row.MessageID + "\x00" + strconv.FormatInt(row.Ordinal, 10)
	_, err, _ = s.group.Do(key, func() (any, error) {
		return s.download(ctx, row)
	})
	if err != nil {
		return Descriptor{}, err
	}

	// Re-read after the slot rather than trusting its returned BlobRef. A
	// concurrent process may have won MarkDownloaded; the durable row is the
	// authoritative winner in that race and also contains the resolved MIME.
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
	return s.descriptorForRow(row)
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

	// Capabilities returns the zero set for both an unsupported adapter and an
	// unregistered account. Snapshot preserves the routing table's distinction:
	// unregistered is unavailable, while registered-but-unsupported is terminal.
	if _, registered := s.bridges.Snapshot(row.AccountID); !registered {
		return blob.BlobRef{}, ErrUnavailable
	}
	if !s.bridges.Capabilities(row.AccountID).MediaDownload {
		return blob.BlobRef{}, ErrUnsupported
	}

	lease, err := s.bridges.Acquire(ctx, row.AccountID, bridge.CapabilityMediaDownload)
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrAccountNotRegistered):
			return blob.BlobRef{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		case errors.Is(err, bridge.ErrCapabilityUnavailable):
			if _, registered := s.bridges.Snapshot(row.AccountID); !registered {
				return blob.BlobRef{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
			}
			if !s.bridges.Capabilities(row.AccountID).MediaDownload {
				return blob.BlobRef{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
			}
			return blob.BlobRef{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		default:
			return blob.BlobRef{}, err
		}
	}
	if lease == nil || lease.Download == nil {
		if lease != nil {
			lease.Close()
		}
		return blob.BlobRef{}, ErrUnavailable
	}
	defer lease.Close()

	stream, err := lease.Download.DownloadMedia(ctx, row.AccountID, bridge.MediaRef{
		RemoteID: row.RemoteID,
		Opaque:   row.RemoteRef,
		Filename: row.Filename,
		MIME:     row.MIME,
	})
	if err != nil {
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
	); err != nil {
		return blob.BlobRef{}, fmt.Errorf("download media: mark downloaded: %w", err)
	}
	return ref, nil
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
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureRecordTimeout)
	defer cancel()
	if err := s.attachments.MarkFailed(
		mutationCtx,
		row.MessageID,
		row.Ordinal,
		failure.Error(),
	); err != nil {
		return errors.Join(failure, fmt.Errorf("record media download failure: %w", err))
	}
	return failure
}

// recordGone reports a transport that no longer has the attachment. When the
// projection recorded the transport's receive-time account of why it never
// fetched the bytes, that account is the useful diagnosis: it is kept in the
// row and appended to the returned error instead of being overwritten.
func (s *Service) recordGone(
	ctx context.Context,
	row sqlite.MessageAttachment,
	transportErr error,
) error {
	failure := fmt.Errorf("%w: %w", ErrGone, transportErr)
	if row.LastError != nil && strings.HasPrefix(*row.LastError, sqlite.ReceiveAttachmentErrorPrefix) {
		return fmt.Errorf("%w (when the message arrived: %s)", failure,
			strings.TrimPrefix(*row.LastError, sqlite.ReceiveAttachmentErrorPrefix))
	}
	return s.recordFailure(ctx, row, failure)
}

// Import stores content as one attachment's bytes and marks it downloaded,
// for an attachment its transport cannot deliver (ErrGone). Only a pending
// attachment accepts new bytes. Importing the bytes an attachment already
// holds is an idempotent success that reports imported=false; different
// bytes fail with ErrImportConflict. The content must match command.SHA256
// when that is set, and the size the transport declared when it declared
// one. The stored MIME type and filename stay the transport's.
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
	digest := sha256.Sum256(data)
	gotSHA := hex.EncodeToString(digest[:])
	if wantSHA != "" && gotSHA != wantSHA {
		return Descriptor{}, false, fmt.Errorf("%w: content SHA-256 is %s, expected %s", ErrImportMismatch, gotSHA, wantSHA)
	}
	if row.State == "downloaded" {
		if row.BlobHash != nil && *row.BlobHash == gotSHA {
			descriptor, err := s.descriptorForRow(row)
			return descriptor, false, err
		}
		return Descriptor{}, false, ErrImportConflict
	}
	if row.SizeBytes != nil && *row.SizeBytes > 0 && int64(len(data)) != *row.SizeBytes {
		return Descriptor{}, false, fmt.Errorf("%w: content is %d bytes, the transport declared %d",
			ErrImportMismatch, len(data), *row.SizeBytes)
	}

	mime := firstNonEmpty(row.MIME, "application/octet-stream")
	ref, err := s.blobs.Put(ctx, bytes.NewReader(data), mime, s.maxBytes)
	if err != nil {
		return Descriptor{}, false, fmt.Errorf("import media: %w", err)
	}
	if err := s.attachments.MarkDownloaded(ctx, row.MessageID, row.Ordinal, ref.Hash, ref.Size, mime); err != nil {
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
