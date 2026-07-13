package messaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestQueuedMediaSurvivesRestartAndMapsRequest(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	content := []byte("media payload after restart")

	beforeRestart := newScriptedRegistry("media-before-restart", &scriptedTextSender{})
	beforeRestart.setMediaSender(&scriptedMediaSender{})
	first := newDispatchTestService(t, store, beforeRestart, blobs, clock)
	parent := mustSendText(t, first, SendTextCommand{
		CommonCommand: testCommonCommand("media-reply-parent"),
		Body:          "reply parent",
	})
	parentRow := mustOutboxItem(t, first, parent.OutboxID)
	if _, err := first.Cancel(context.Background(), parent.OutboxID); err != nil {
		t.Fatalf("Cancel(parent) error = %v", err)
	}
	submission := mustSendDispatchMedia(t, first, SendMediaCommand{
		CommonCommand:    testCommonCommand("media-restart"),
		Content:          bytes.NewReader(content),
		Filename:         "restart.bin",
		MIME:             "application/x-restart-test",
		Caption:          "surviving caption",
		ReplyToMessageID: parent.LocalMessageID,
	})

	sender := &scriptedMediaSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-media-after-restart"},
	}}}
	afterRestart := newScriptedRegistry("media-after-restart", &scriptedTextSender{})
	afterRestart.setMediaSender(sender)
	afterRestart.setAvailable(true)
	restarted := newDispatchTestService(t, store, afterRestart, blobs, clock)

	if before := mustDelivery(t, restarted, submission.OutboxID); before.State != OutboxQueued ||
		before.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("restarted delivery = %+v, want original queued IDs", before)
	}
	if processed, err := restarted.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(restarted media) = %d, %v; want 1, nil", processed, err)
	}
	if after := mustDelivery(t, restarted, submission.OutboxID); after.State != OutboxConfirmed ||
		after.RemoteMessageID != "remote-media-after-restart" {
		t.Fatalf("delivery after restart = %+v", after)
	}

	requests := sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("media requests = %+v, want one", requests)
	}
	request := requests[0]
	gotContent, err := io.ReadAll(request.Reader)
	if err != nil {
		t.Fatalf("read mapped media request: %v", err)
	}
	outboxRow := mustOutboxItem(t, restarted, submission.OutboxID)
	if request.AccountID != "account-1" ||
		request.Conversation.RemoteID != "remote-conversation" ||
		!bytes.Equal(gotContent, content) ||
		request.Size != int64(len(content)) ||
		request.Filename != "restart.bin" ||
		request.MIME != "application/x-restart-test" ||
		request.Caption != "surviving caption" ||
		request.ReplyTo == nil || request.ReplyTo.RemoteID != parentRow.TransportRequestID ||
		request.RequestID != outboxRow.TransportRequestID {
		t.Fatalf("mapped media request = %+v", request)
	}
}

func TestDeclaredButUnwiredMediaReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-media-available"},
	}}}
	registry := newScriptedRegistry("media-unavailable", &scriptedTextSender{})
	registry.setMediaSender(sender)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-unavailable"),
		Content:       bytes.NewReader([]byte("queued media")),
		Filename:      "queued.bin",
		MIME:          "application/octet-stream",
	})
	registry.setMediaSender(nil)
	registry.setMediaSendDeclared(true)
	registry.setAvailable(true)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unwired media) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
		t.Fatalf("unwired media row = %+v, want queued/attempt=0/no next-attempt", row)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("unwired media send count = %d, want 0", got)
	}
}

func TestUnavailableMediaAccountReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-media-available"},
	}}}
	registry := newScriptedRegistry("media-account-unavailable", &scriptedTextSender{})
	registry.setMediaSender(sender)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-account-unavailable"),
		Content:       bytes.NewReader([]byte("queued while account unavailable")),
		Filename:      "queued.bin",
		MIME:          "application/octet-stream",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unavailable media account) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
		t.Fatalf("unavailable media account row = %+v, want queued/attempt=0/no next-attempt", row)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("unavailable media account send count = %d, want 0", got)
	}
}

func TestMediaLeaseWithoutSenderReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	baseRegistry := newScriptedRegistry("media-nil-lease", &scriptedTextSender{})
	baseRegistry.setMediaSendDeclared(true)
	baseRegistry.setAvailable(true)
	registry := nilMediaLeaseRegistry{scriptedRegistry: baseRegistry}
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-nil-lease"),
		Content:       bytes.NewReader([]byte("nil media lease")),
		Filename:      "nil-lease.bin",
		MIME:          "application/octet-stream",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(nil media lease) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
		t.Fatalf("nil media lease row = %+v, want queued/attempt=0/no next-attempt", row)
	}
}

func TestUndeclaredMediaCapabilityIsRejected(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{}
	registry := newScriptedRegistry("media-undeclared", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-undeclared"),
		Content:       bytes.NewReader([]byte("unsupported later")),
		Filename:      "unsupported.bin",
		MIME:          "application/octet-stream",
	})
	registry.setMediaSender(nil)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(undeclared media) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureUnsupported) ||
		delivery.ErrorCode != "media_send_unsupported" {
		t.Fatalf("undeclared media delivery = %+v, want rejected/unsupported", delivery)
	}
	if row := mustOutboxItem(t, service, submission.OutboxID); row.AttemptCount != 0 {
		t.Fatalf("undeclared media attempt count = %d, want 0", row.AttemptCount)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("undeclared media send count = %d, want 0", got)
	}
}

func TestMissingMediaBlobIsRejected(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{}
	registry := newScriptedRegistry("media-blob-missing", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-blob-missing"),
		Content:       bytes.NewReader([]byte("delete me")),
		Filename:      "missing.bin",
		MIME:          "application/octet-stream",
	})
	attachment, err := service.outbox.GetOutboxAttachment(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment() error = %v", err)
	}
	if err := blobs.Delete(blob.BlobRef{Hash: attachment.BlobHash}); err != nil {
		t.Fatalf("Delete(blob) error = %v", err)
	}

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(missing blob) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureMisconfigured) ||
		delivery.ErrorCode != "blob_missing" {
		t.Fatalf("missing blob delivery = %+v, want rejected/misconfigured/blob_missing", delivery)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("missing blob media send count = %d, want 0", got)
	}
}

func TestMediaSuccessWithoutRemoteMessageIDBecomesUncertain(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{steps: []sendStep{{}}}
	registry := newScriptedRegistry("media-missing-result", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-missing-result"),
		Content:       bytes.NewReader([]byte("missing result")),
		Filename:      "missing-result.bin",
		MIME:          "application/octet-stream",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(missing media result) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxUncertain || delivery.ErrorCode != "missing_remote_message_id" {
		t.Fatalf("missing media result delivery = %+v, want uncertain", delivery)
	}
}

func TestPostCallMediaTimeoutBecomesUncertainAndIsNotRedispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{steps: []sendStep{{err: bridge.OpError{
		Class:    bridge.FailureTransient,
		Dispatch: bridge.DispatchUncertain,
		Cause:    context.DeadlineExceeded,
	}}}}
	registry := newScriptedRegistry("media-timeout", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-timeout"),
		Content:       bytes.NewReader([]byte("ambiguous media")),
		Filename:      "timeout.bin",
		MIME:          "application/octet-stream",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(media timeout) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxUncertain || delivery.ErrorCode != mediaOperation || delivery.Warning == "" {
		t.Fatalf("media timeout delivery = %+v, want uncertain with %q code", delivery, mediaOperation)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after media timeout) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("media timeout send count = %d, want 1", got)
	}
}

func TestMediaRetryOpensFreshReader(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	content := []byte("read from offset zero every time")
	sender := &scriptedMediaSender{steps: []sendStep{
		{err: bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "prepare_media",
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("safe retry"),
		}},
		{result: bridge.SendResult{RemoteMessageID: "remote-media-retry"}},
	}}
	registry := newScriptedRegistry("media-reader-retry", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("media-reader-retry"),
		Content:       bytes.NewReader(content),
		Filename:      "retry.bin",
		MIME:          "application/octet-stream",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(first media attempt) = %d, %v; want 1, nil", processed, err)
	}
	if first := mustDelivery(t, service, submission.OutboxID); first.State != OutboxNotDispatched {
		t.Fatalf("first media delivery = %+v, want not_dispatched", first)
	}
	if _, err := service.RetryNotDispatched(context.Background(), submission.OutboxID); err != nil {
		t.Fatalf("RetryNotDispatched(media) error = %v", err)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(second media attempt) = %d, %v; want 1, nil", processed, err)
	}
	if final := mustDelivery(t, service, submission.OutboxID); final.State != OutboxConfirmed {
		t.Fatalf("final media delivery = %+v, want confirmed", final)
	}

	requests := sender.snapshotRequests()
	var contents [][]byte
	for index, request := range requests {
		content, err := io.ReadAll(request.Reader)
		if err != nil {
			t.Fatalf("read media retry request %d: %v", index, err)
		}
		contents = append(contents, content)
	}
	if len(requests) != 2 ||
		!bytes.Equal(contents[0], content) ||
		!bytes.Equal(contents[1], content) ||
		requests[0].RequestID == "" || requests[0].RequestID != requests[1].RequestID {
		t.Fatalf("media retry requests = %+v, want two full reads with stable request ID", requests)
	}
}

type nilMediaLeaseRegistry struct {
	*scriptedRegistry
}

func (r nilMediaLeaseRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if capability != bridge.CapabilityMediaSend {
		return r.scriptedRegistry.Acquire(ctx, accountID, capability)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &bridge.DispatchLease{
		AccountID:  accountID,
		Platform:   "media-nil-lease",
		Generation: 7,
		Ctx:        ctx,
	}, nil
}

func newDispatchTestBlobStore(t *testing.T) *blob.BlobStore {
	t.Helper()
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	return blobs
}

func newDispatchTestService(
	t *testing.T,
	store *sqlite.Store,
	registry bridge.Registry,
	blobs *blob.BlobStore,
	clock Clock,
) *MessageService {
	t.Helper()
	service, err := NewMessageService(store, registry, blobs, clock, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	return service
}

func mustSendDispatchMedia(
	t *testing.T,
	service *MessageService,
	command SendMediaCommand,
) Submission {
	t.Helper()
	submission, err := service.SendMedia(context.Background(), command)
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	return submission
}
