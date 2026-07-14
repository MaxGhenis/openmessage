package messaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

var messagingTestTime = time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)

func TestUnavailableTextReturnsToQueuedWithoutAttempt(t *testing.T) {
	for _, platform := range []bridge.Platform{"scripted-alpha", "future-platform"} {
		t.Run(string(platform), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{
				result: bridge.SendResult{RemoteMessageID: "remote-available"},
			}}}
			registry := newScriptedRegistry(platform, sender)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-unavailable"),
				Body:          "durable hello",
			})

			processed, err := service.DispatchDue(context.Background(), 8)
			if err != nil || processed != 1 {
				t.Fatalf("DispatchDue(unavailable) = %d, %v; want 1, nil", processed, err)
			}
			row := mustOutboxItem(t, service, submission.OutboxID)
			if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
				t.Fatalf("unavailable row = %+v, want queued/attempt=0/no next-attempt", row)
			}
			if got := sender.requestCount(); got != 0 {
				t.Fatalf("unavailable send count = %d, want 0", got)
			}

			registry.setAvailable(true)
			processed, err = service.DispatchDue(context.Background(), 8)
			if err != nil || processed != 1 {
				t.Fatalf("DispatchDue(available) = %d, %v; want 1, nil", processed, err)
			}
			if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed ||
				got.RemoteMessageID != "remote-available" {
				t.Fatalf("available delivery = %+v, want confirmed", got)
			}
		})
	}
}

func TestAvailableTextDispatchesOnceAndConfirms(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-1", AcceptedAt: clock.Now()},
	}}}
	registry := newScriptedRegistry("not-hard-coded", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-confirm"),
		Body:          "hello once",
	})

	processed, err := service.DispatchDue(context.Background(), 4)
	if err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	processed, err = service.DispatchDue(context.Background(), 4)
	if err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after confirm) = %d, %v; want 0, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want one", requests)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxConfirmed || row.AttemptCount != 1 ||
		requests[0].RequestID != row.TransportRequestID ||
		requests[0].Conversation.RemoteID != "remote-conversation" ||
		requests[0].Body != "hello once" {
		t.Fatalf("request/row = %+v / %+v", requests[0], row)
	}
}

func TestRetryableNotDispatchedReusesTransportRequestID(t *testing.T) {
	classes := []bridge.FailureClass{
		bridge.FailureTransient,
		bridge.FailureRateLimited,
		bridge.FailureCredentialsExpired,
	}
	for _, class := range classes {
		class := class
		t.Run(string(class), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			cause := error(errors.New("failed before remote dispatch"))
			if class == bridge.FailureTransient {
				// An explicit clean not-dispatched result remains safe even when
				// the adapter's preflight cause is a deadline.
				cause = context.DeadlineExceeded
			}
			sender := &scriptedTextSender{steps: []sendStep{
				{err: bridge.OpError{
					Class:     class,
					Operation: "prepare_send",
					Dispatch:  bridge.DispatchNotCalled,
					Cause:     cause,
				}},
				{result: bridge.SendResult{RemoteMessageID: "remote-retry"}},
			}}
			registry := newScriptedRegistry("retry-platform", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-retry-" + string(class)),
				Body:          "retry safely",
			})

			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(first) = %d, %v", processed, err)
			}
			first := mustDelivery(t, service, submission.OutboxID)
			if first.State != OutboxNotDispatched || first.ErrorClass != string(class) {
				t.Fatalf("first delivery = %+v, want %q not_dispatched", first, class)
			}
			row := mustOutboxItem(t, service, submission.OutboxID)
			if row.AttemptCount != 1 {
				t.Fatalf("attempt count = %d, want 1", row.AttemptCount)
			}

			retried, err := service.RetryNotDispatched(context.Background(), submission.OutboxID)
			if err != nil || retried.State != OutboxNotDispatched {
				t.Fatalf("RetryNotDispatched() = %+v, %v; want due not_dispatched", retried, err)
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(retry) = %d, %v", processed, err)
			}
			if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxConfirmed {
				t.Fatalf("retry state = %q, want confirmed", got)
			}
			requests := sender.snapshotRequests()
			if len(requests) != 2 || requests[0].RequestID == "" ||
				requests[0].RequestID != requests[1].RequestID {
				t.Fatalf("request IDs = %+v, want same stable ID", requests)
			}
		})
	}
}

func TestContextCanceledDuringTransportStillRecordsUncertain(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	sender := &scriptedTextSender{
		steps:  []sendStep{{err: context.Canceled}},
		onSend: cancel,
	}
	registry := newScriptedRegistry("cancel-during-send", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-cancel-during-send"),
		Body:          "record my ambiguous cancellation",
	})

	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(canceled send) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxUncertain || row.AttemptCount != 1 || row.LeaseToken != nil {
		t.Fatalf("canceled transport row = %+v, want uncertain finalized row", row)
	}
}

func TestDispatchDueRecoversLaterLeasesThatExpireWithinBatch(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	var advanceOnce sync.Once
	sender := &scriptedTextSender{
		steps: []sendStep{
			{result: bridge.SendResult{RemoteMessageID: "remote-slow-first"}},
			{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
		},
		onSend: func() {
			advanceOnce.Do(func() { clock.Advance(defaultLeaseTime + time.Second) })
		},
	}
	registry := newScriptedRegistry("batch-expiry", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-slow-first"),
		Body:          "first",
	})
	second := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-expired-second"),
		Body:          "second",
	})

	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(expiring batch) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, first.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("first state = %q, want confirmed", got)
	}
	secondRow := mustOutboxItem(t, service, second.OutboxID)
	if secondRow.State != sqlite.OutboxNotDispatched || secondRow.AttemptCount != 0 || secondRow.LeaseToken != nil {
		t.Fatalf("expired later lease = %+v, want recoverable not_dispatched", secondRow)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(recovered second) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, second.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("second state = %q, want confirmed", got)
	}
}

func TestPostCallTimeoutBecomesUncertainAndIsNotRedispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}}}}
	registry := newScriptedRegistry("timeout-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-timeout"),
		Body:          "ambiguous text",
	})

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(timeout) = %d, %v", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain || got.Warning == "" {
		t.Fatalf("timeout delivery = %+v, want uncertain warning", got)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after timeout) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("timeout send count = %d, want 1", got)
	}
}

func TestTerminalFailuresAreRejected(t *testing.T) {
	classes := []bridge.FailureClass{
		bridge.FailureUnpaired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired,
		bridge.FailureMisconfigured,
		bridge.FailureUnsupported,
	}
	for _, class := range classes {
		class := class
		t.Run(string(class), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{err: bridge.OpError{
				Class:     class,
				Operation: "send_text",
				Dispatch:  bridge.DispatchNotCalled,
				Cause:     errors.New("terminal failure"),
			}}}}
			registry := newScriptedRegistry("terminal-platform", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-terminal-" + string(class)),
				Body:          "cannot send",
			})

			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v", processed, err)
			}
			delivery := mustDelivery(t, service, submission.OutboxID)
			if delivery.State != OutboxRejected || delivery.ErrorClass != string(class) {
				t.Fatalf("delivery = %+v, want rejected %q", delivery, class)
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
				t.Fatalf("DispatchDue(after reject) = %d, %v", processed, err)
			}
		})
	}
}

func TestNewServiceOverSameStoreDispatchesQueuedText(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	unavailable := newScriptedRegistry("before-restart", &scriptedTextSender{})
	first := newMessagingTestService(t, store, unavailable, clock)
	submission := mustSendText(t, first, SendTextCommand{
		CommonCommand: testCommonCommand("key-restart"),
		Body:          "survive service restart",
	})

	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-after-restart"},
	}}}
	available := newScriptedRegistry("after-restart", sender)
	available.setAvailable(true)
	restarted := newMessagingTestService(t, store, available, clock)

	if before := mustDelivery(t, restarted, submission.OutboxID); before.State != OutboxQueued ||
		before.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("restarted delivery = %+v, want original queued IDs", before)
	}
	if processed, err := restarted.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(restarted) = %d, %v", processed, err)
	}
	if after := mustDelivery(t, restarted, submission.OutboxID); after.State != OutboxConfirmed ||
		after.RemoteMessageID != "remote-after-restart" {
		t.Fatalf("delivery after restart = %+v", after)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Body != "survive service restart" {
		t.Fatalf("request after restart = %+v", requests)
	}
}

func TestDuplicateSendTextReturnsSameSubmissionAndDispatchesOnce(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-deduplicated"},
	}}}
	registry := newScriptedRegistry("duplicate-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("same-key"),
		Body:          "same body",
	}

	first := mustSendText(t, service, command)
	second := mustSendText(t, service, command)
	if first != second {
		t.Fatalf("duplicate submissions differ: first=%+v second=%+v", first, second)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("duplicate send count = %d, want 1", got)
	}

	command.Body = "changed body"
	if _, err := service.SendText(context.Background(), command); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("SendText(mismatch) error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSendMediaWritesBlobAndDurableRows(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs, blobRoot := newMessagingTestBlobStore(t)
	registry := newScriptedRegistry("media-platform", nil)
	registry.setMediaSender(&scriptedMediaSender{})
	service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
	content := []byte("durable media content")

	submission := mustSendMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("key-media-happy"),
		Content:       bytes.NewReader(content),
		Filename:      "  photo.test  ",
		MIME:          "  application/x-openmessage-test  ",
		Caption:       "a durable caption",
	})

	item := mustOutboxItem(t, service, submission.OutboxID)
	if item.Kind != sqlite.OutboxKindMedia || item.Operation != mediaOperation ||
		item.LocalMessageID == nil || *item.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("media outbox item = %+v", item)
	}
	message, err := service.messages.GetMessage(context.Background(), submission.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if message.Body != "a durable caption" || message.Direction != sqlite.MessageDirectionOutgoing ||
		message.RemoteMessageID != item.TransportRequestID || message.ReplyToRemoteID != nil {
		t.Fatalf("optimistic media message = %+v", message)
	}
	attachment, err := service.outbox.GetOutboxAttachment(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment(): %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(content))
	if attachment.OutboxID != submission.OutboxID || attachment.Ordinal != 0 ||
		attachment.BlobHash != wantHash || attachment.SizeBytes != int64(len(content)) ||
		attachment.MIME != "application/x-openmessage-test" || attachment.Filename != "photo.test" {
		t.Fatalf("outbox attachment = %+v", attachment)
	}
	reader, err := blobs.Open(blob.BlobRef{Hash: attachment.BlobHash})
	if err != nil {
		t.Fatalf("Open(blob): %v", err)
	}
	gotContent, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read blob = %v, close = %v", readErr, closeErr)
	}
	if !bytes.Equal(gotContent, content) {
		t.Fatalf("blob content = %q, want %q", gotContent, content)
	}
	if files := messagingBlobFiles(t, blobRoot); len(files) != 1 {
		t.Fatalf("blob files = %v, want one", files)
	}
}

func TestDuplicateSendMediaReturnsSameSubmissionAndOneDurableSet(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs, blobRoot := newMessagingTestBlobStore(t)
	registry := newScriptedRegistry("media-deduplicate", nil)
	registry.setMediaSender(&scriptedMediaSender{})
	service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
	content := []byte("deduplicate this attachment")
	command := func(mime, filename string) SendMediaCommand {
		return SendMediaCommand{
			CommonCommand: testCommonCommand("key-media-same"),
			Content:       bytes.NewReader(content),
			Filename:      filename,
			MIME:          mime,
			Caption:       "same caption",
		}
	}

	first := mustSendMedia(t, service, command(" image/test ", " image.bin "))
	second := mustSendMedia(t, service, command("image/test", "image.bin"))
	if first != second {
		t.Fatalf("duplicate media submissions differ: first=%+v second=%+v", first, second)
	}
	if files := messagingBlobFiles(t, blobRoot); len(files) != 1 {
		t.Fatalf("duplicate media left blob files %v, want one", files)
	}
	if _, err := service.outbox.FindByID(context.Background(), "id-004"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("duplicate candidate outbox error = %v, want ErrNotFound", err)
	}
	if _, err := service.messages.GetMessage(context.Background(), "id-005"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("duplicate candidate message error = %v, want ErrNotFound", err)
	}
}

func TestSendMediaIdempotencyConflictCoversCaptionAndFilename(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SendMediaCommand)
	}{
		{name: "caption", mutate: func(cmd *SendMediaCommand) { cmd.Caption = "changed caption" }},
		{name: "filename", mutate: func(cmd *SendMediaCommand) { cmd.Filename = "changed.bin" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			blobs, _ := newMessagingTestBlobStore(t)
			registry := newScriptedRegistry("media-conflict", nil)
			registry.setMediaSender(&scriptedMediaSender{})
			service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
			content := []byte("same content")
			command := SendMediaCommand{
				CommonCommand: testCommonCommand("key-media-conflict"),
				Content:       bytes.NewReader(content),
				Filename:      "original.bin",
				MIME:          "application/test",
				Caption:       "original caption",
			}
			mustSendMedia(t, service, command)

			test.mutate(&command)
			command.Content = bytes.NewReader(content)
			if _, err := service.SendMedia(context.Background(), command); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("SendMedia(changed %s) error = %v, want ErrIdempotencyConflict", test.name, err)
			}
		})
	}
}

func TestSendMediaRejectsTooLargeAndEmptyContent(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		max     int64
	}{
		{name: "too large", content: []byte("12345"), max: 4},
		{name: "empty", content: nil, max: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			blobs, blobRoot := newMessagingTestBlobStore(t)
			registry := newScriptedRegistry("media-size", nil)
			registry.setMediaSender(&scriptedMediaSender{})
			service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
			service.maxMediaBytes = test.max

			_, err := service.SendMedia(context.Background(), SendMediaCommand{
				CommonCommand: testCommonCommand("key-media-size"),
				Content:       bytes.NewReader(test.content),
				MIME:          "application/test",
			})
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("SendMedia() error = %v, want ErrInvalidCommand", err)
			}
			if _, err := service.outbox.FindByID(context.Background(), "id-001"); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("invalid media outbox error = %v, want ErrNotFound", err)
			}
			if test.name == "too large" {
				if files := messagingBlobFiles(t, blobRoot); len(files) != 0 {
					t.Fatalf("oversize media left blob files: %v", files)
				}
			}
		})
	}
}

func TestSendMediaCapabilityGatePrecedesBlobAndConversationWork(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs, blobRoot := newMessagingTestBlobStore(t)
	service := newMessagingTestServiceWithBlobs(
		t,
		store,
		newScriptedRegistry("media-unsupported", nil),
		blobs,
		clock,
	)
	content := []byte("must not be consumed")
	reader := bytes.NewReader(content)
	command := SendMediaCommand{
		CommonCommand: CommonCommand{
			AccountID:      "account-1",
			ConversationID: "missing-conversation",
			IdempotencyKey: "key-media-unsupported",
		},
		Content: reader,
		MIME:    "application/test",
	}

	if _, err := service.SendMedia(context.Background(), command); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SendMedia(unsupported) error = %v, want ErrUnsupported", err)
	}
	if reader.Len() != len(content) {
		t.Fatalf("unsupported SendMedia consumed %d bytes, want 0", len(content)-reader.Len())
	}
	if files := messagingBlobFiles(t, blobRoot); len(files) != 0 {
		t.Fatalf("unsupported SendMedia left blob files: %v", files)
	}
	if _, err := service.outbox.FindByID(context.Background(), "id-001"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unsupported media outbox error = %v, want ErrNotFound", err)
	}
}

func TestSendMediaDefaultsMIMEAndAllowsEmptyCaption(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs, _ := newMessagingTestBlobStore(t)
	registry := newScriptedRegistry("media-defaults", nil)
	registry.setMediaSender(&scriptedMediaSender{})
	service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)

	submission := mustSendMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("key-media-defaults"),
		Content:       bytes.NewReader([]byte("content")),
		Filename:      "  no-caption.bin  ",
		MIME:          " \t ",
	})
	attachment, err := service.outbox.GetOutboxAttachment(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment(): %v", err)
	}
	if attachment.MIME != "application/octet-stream" || attachment.Filename != "no-caption.bin" {
		t.Fatalf("normalized attachment = %+v", attachment)
	}
	message, err := service.messages.GetMessage(context.Background(), submission.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if message.Body != "" {
		t.Fatalf("empty caption body = %q, want empty", message.Body)
	}
}

func TestSendMediaValidatesContentAndFilename(t *testing.T) {
	tests := []struct {
		name     string
		content  io.Reader
		filename string
	}{
		{name: "nil content", content: nil},
		{name: "filename too long", content: bytes.NewReader([]byte("x")), filename: strings.Repeat("x", 513)},
		{name: "filename contains NUL", content: bytes.NewReader([]byte("x")), filename: "bad\x00name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			blobs, blobRoot := newMessagingTestBlobStore(t)
			registry := newScriptedRegistry("media-validation", nil)
			registry.setMediaSender(&scriptedMediaSender{})
			service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)

			_, err := service.SendMedia(context.Background(), SendMediaCommand{
				CommonCommand: testCommonCommand("key-media-validation"),
				Content:       test.content,
				Filename:      test.filename,
			})
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("SendMedia() error = %v, want ErrInvalidCommand", err)
			}
			if files := messagingBlobFiles(t, blobRoot); len(files) != 0 {
				t.Fatalf("invalid command left blob files: %v", files)
			}
		})
	}
}

func TestSendMediaWithoutBlobStoreReturnsConfigurationError(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("media-no-blob-store", nil)
	registry.setMediaSender(&scriptedMediaSender{})
	service, err := NewMessageService(store, registry, nil, clock, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewMessageService(nil blobs): %v", err)
	}

	_, err = service.SendMedia(context.Background(), SendMediaCommand{
		CommonCommand: testCommonCommand("key-media-no-blob-store"),
		Content:       bytes.NewReader([]byte("content")),
	})
	if err == nil || !strings.Contains(err.Error(), "blob store is not configured") {
		t.Fatalf("SendMedia(nil blobs) error = %v, want configuration error", err)
	}
}

func TestSendReactionValidatesCommand(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("reaction-validation", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)

	tests := []struct {
		name   string
		mutate func(*SendReactionCommand)
	}{
		{name: "empty emoji", mutate: func(cmd *SendReactionCommand) { cmd.Emoji = " \t " }},
		{name: "emoji too long", mutate: func(cmd *SendReactionCommand) { cmd.Emoji = strings.Repeat("x", 65) }},
		{name: "bad action", mutate: func(cmd *SendReactionCommand) { cmd.Action = bridge.ReactionAction("toggle") }},
		{name: "empty target", mutate: func(cmd *SendReactionCommand) { cmd.TargetMessageID = " " }},
		{name: "invalid schedule", mutate: func(cmd *SendReactionCommand) { cmd.NotBefore = time.UnixMilli(0) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := SendReactionCommand{
				CommonCommand:   testCommonCommand("key-reaction-validation-" + test.name),
				TargetMessageID: "target-message",
				Emoji:           "👍",
				Action:          bridge.ReactionAdd,
			}
			test.mutate(&command)

			if _, err := service.SendReaction(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("SendReaction() error = %v, want ErrInvalidCommand", err)
			}
		})
	}
}

func TestMarkReadValidatesCommand(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("read-validation", nil)
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)

	tests := []struct {
		name   string
		mutate func(*MarkReadCommand)
	}{
		{name: "empty device", mutate: func(cmd *MarkReadCommand) { cmd.DeviceID = " " }},
		{name: "empty target", mutate: func(cmd *MarkReadCommand) { cmd.LastReadMessageID = "\t" }},
		{name: "invalid read time", mutate: func(cmd *MarkReadCommand) { cmd.LastReadAt = time.UnixMilli(0) }},
		{name: "invalid schedule", mutate: func(cmd *MarkReadCommand) { cmd.NotBefore = time.UnixMilli(0) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := MarkReadCommand{
				CommonCommand:     testCommonCommand("key-read-validation-" + test.name),
				DeviceID:          "device-1",
				LastReadMessageID: "target-message",
				LastReadAt:        clock.Now(),
			}
			test.mutate(&command)

			if _, err := service.MarkRead(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("MarkRead() error = %v, want ErrInvalidCommand", err)
			}
		})
	}
}

func TestReactionAndReadCapabilityGatesPrecedeGraphWork(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(
		t,
		store,
		newScriptedRegistry("unsupported-intents", nil),
		clock,
	)

	if _, err := service.SendReaction(context.Background(), SendReactionCommand{
		CommonCommand: CommonCommand{
			AccountID:      "account-1",
			ConversationID: "missing-conversation",
			IdempotencyKey: "key-reaction-unsupported",
		},
		TargetMessageID: "missing-message",
		Emoji:           "👍",
	}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SendReaction(unsupported) error = %v, want ErrUnsupported", err)
	}
	if _, err := service.MarkRead(context.Background(), MarkReadCommand{
		CommonCommand: CommonCommand{
			AccountID:      "account-1",
			ConversationID: "missing-conversation",
			IdempotencyKey: "key-read-unsupported",
		},
		DeviceID:          "missing-device",
		LastReadMessageID: "missing-message",
	}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("MarkRead(unsupported) error = %v, want ErrUnsupported", err)
	}
}

func TestMarkReadChecksTargetBeforeDevice(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("read-check-order", nil)
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)

	_, err := service.MarkRead(context.Background(), MarkReadCommand{
		CommonCommand:     testCommonCommand("key-read-check-order"),
		DeviceID:          "missing-device",
		LastReadMessageID: "missing-message",
	})
	if !errors.Is(err, sqlite.ErrNotFound) || !strings.Contains(err.Error(), "read cursor target") {
		t.Fatalf("MarkRead(missing target and device) error = %v, want target not-found first", err)
	}
}

func TestReactionAndReadPayloadHashesCoverIntentFields(t *testing.T) {
	reaction, err := reactionPayloadHash("target-a", "👍", bridge.ReactionAdd)
	if err != nil {
		t.Fatalf("reactionPayloadHash(): %v", err)
	}
	for _, candidate := range []struct {
		target string
		emoji  string
		action bridge.ReactionAction
	}{
		{target: "target-b", emoji: "👍", action: bridge.ReactionAdd},
		{target: "target-a", emoji: "🔥", action: bridge.ReactionAdd},
		{target: "target-a", emoji: "👍", action: bridge.ReactionRemove},
	} {
		got, hashErr := reactionPayloadHash(candidate.target, candidate.emoji, candidate.action)
		if hashErr != nil {
			t.Fatalf("reactionPayloadHash(candidate): %v", hashErr)
		}
		if got == reaction {
			t.Fatalf("reaction hash did not cover candidate %+v", candidate)
		}
	}

	read, err := readPayloadHash("device-a", "message-a")
	if err != nil {
		t.Fatalf("readPayloadHash(): %v", err)
	}
	for _, candidate := range [][2]string{{"device-b", "message-a"}, {"device-a", "message-b"}} {
		got, hashErr := readPayloadHash(candidate[0], candidate[1])
		if hashErr != nil {
			t.Fatalf("readPayloadHash(candidate): %v", hashErr)
		}
		if got == read {
			t.Fatalf("read hash did not cover device/message %q/%q", candidate[0], candidate[1])
		}
	}
}

func TestSendReactionWritesDurableIntentWithoutMessageProjection(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("reaction-submit", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reaction-target"),
		Body:          "react to me",
	})

	submission := mustSendReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("key-reaction-submit"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "  👍  ",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)
	if item.Kind != sqlite.OutboxKindReaction || item.Operation != reactionOperation ||
		item.LocalMessageID != nil || submission.LocalMessageID != "" {
		t.Fatalf("reaction outbox item = %+v, submission = %+v", item, submission)
	}
	reaction, err := service.outbox.GetOutboxReaction(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReaction(): %v", err)
	}
	if reaction.TargetMessageID != target.LocalMessageID || reaction.Emoji != "👍" ||
		reaction.Action != string(bridge.ReactionAdd) || reaction.CreatedAtMS != clock.Now().UnixMilli() {
		t.Fatalf("outbox reaction = %+v", reaction)
	}
	if _, err := service.messages.GetMessage(context.Background(), "id-005"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("reaction candidate message error = %v, want ErrNotFound", err)
	}
}

func TestSendReactionDeduplicatesAndHashesEmojiAndAction(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("reaction-dedup", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reaction-dedup-target"),
		Body:          "target",
	})
	command := SendReactionCommand{
		CommonCommand:   testCommonCommand("key-reaction-dedup"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "🔥",
		Action:          bridge.ReactionAdd,
	}

	first := mustSendReaction(t, service, command)
	second := mustSendReaction(t, service, command)
	if first != second {
		t.Fatalf("duplicate reaction submissions differ: first=%+v second=%+v", first, second)
	}
	if _, err := service.outbox.FindByID(context.Background(), "id-007"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("duplicate candidate reaction error = %v, want ErrNotFound", err)
	}

	for _, mutate := range []func(*SendReactionCommand){
		func(cmd *SendReactionCommand) { cmd.Emoji = "👍" },
		func(cmd *SendReactionCommand) { cmd.Action = bridge.ReactionRemove },
	} {
		conflict := command
		mutate(&conflict)
		if _, err := service.SendReaction(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("SendReaction(conflict) error = %v, want ErrIdempotencyConflict", err)
		}
	}
}

func TestSendReactionKeepsRapidToggleAsTwoOrderedIntents(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("reaction-toggle", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reaction-toggle-target"),
		Body:          "target",
	})

	add := mustSendReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("key-reaction-add"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	clock.Advance(time.Millisecond)
	remove := mustSendReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("key-reaction-remove"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionRemove,
	})
	addItem := mustOutboxItem(t, service, add.OutboxID)
	removeItem := mustOutboxItem(t, service, remove.OutboxID)
	if add.OutboxID == remove.OutboxID || addItem.CreatedAtMS >= removeItem.CreatedAtMS ||
		addItem.State != sqlite.OutboxQueued || removeItem.State != sqlite.OutboxQueued {
		t.Fatalf("toggle intents = (%+v, %+v), want two ordered queued rows", addItem, removeItem)
	}
}

func TestSendReactionRejectsConversationAndTargetOwnershipViolations(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedSecondMessagingAccount(t, store, clock.Now())
	registry := newScriptedRegistry("reaction-ownership", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)
	foreignTarget := seedMessagingMessage(t, store, clock, sqlite.Message{
		MessageID:       "foreign-message",
		ConversationID:  "conversation-2",
		AccountID:       "account-2",
		RemoteMessageID: "remote-foreign-message",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "foreign",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	})

	_, err := service.SendReaction(context.Background(), SendReactionCommand{
		CommonCommand: CommonCommand{
			AccountID:      "account-1",
			ConversationID: "conversation-2",
			IdempotencyKey: "key-reaction-foreign-conversation",
		},
		TargetMessageID: foreignTarget,
		Emoji:           "👍",
	})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("SendReaction(foreign conversation) error = %v, want ErrInvalidCommand", err)
	}

	_, err = service.SendReaction(context.Background(), SendReactionCommand{
		CommonCommand:   testCommonCommand("key-reaction-foreign-target"),
		TargetMessageID: foreignTarget,
		Emoji:           "👍",
	})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("SendReaction(foreign target) error = %v, want ErrInvalidCommand", err)
	}
}

func TestReactionRejectsDeletedTargetButReadAllowsDeletedCursorTarget(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("deleted-target", nil)
	registry.setReactionSender(&scriptedReactionSender{})
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-deleted-target"),
		Body:          "delete me",
	})
	markMessagingMessageDeleted(t, store, clock, target.LocalMessageID)
	seedMessagingDevice(t, store, "device-1", "account-1", clock.Now())

	if _, err := service.SendReaction(context.Background(), SendReactionCommand{
		CommonCommand:   testCommonCommand("key-deleted-reaction"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("SendReaction(deleted target) error = %v, want ErrInvalidCommand", err)
	}
	read := mustMarkRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("key-deleted-read"),
		DeviceID:          "device-1",
		LastReadMessageID: target.LocalMessageID,
	})
	if read.State != OutboxQueued {
		t.Fatalf("MarkRead(deleted target) = %+v, want queued", read)
	}
}

func TestMarkReadWritesReceiptAndMonotoneCursorWithoutMessageProjection(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedMessagingDevice(t, store, "device-1", "account-1", clock.Now())
	registry := newScriptedRegistry("read-submit", nil)
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-read-target"),
		Body:          "read me",
	})

	submission := mustMarkRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("key-read-submit"),
		DeviceID:          "device-1",
		LastReadMessageID: target.LocalMessageID,
	})
	item := mustOutboxItem(t, service, submission.OutboxID)
	if item.Kind != sqlite.OutboxKindRead || item.Operation != readOperation ||
		item.LocalMessageID != nil || submission.LocalMessageID != "" {
		t.Fatalf("read outbox item = %+v, submission = %+v", item, submission)
	}
	receipt, err := service.outbox.GetOutboxReadReceipt(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReadReceipt(): %v", err)
	}
	if receipt.DeviceID != "device-1" || receipt.LastReadMessageID != target.LocalMessageID ||
		receipt.ReadAtMS != clock.Now().UnixMilli() || receipt.CreatedAtMS != clock.Now().UnixMilli() {
		t.Fatalf("outbox read receipt = %+v", receipt)
	}
	cursor, err := store.GetReadCursor("device-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if cursor.AccountID != "account-1" || cursor.LastReadMessageID == nil ||
		*cursor.LastReadMessageID != target.LocalMessageID || cursor.LastReadAtMS != clock.Now().UnixMilli() ||
		cursor.UpdatedAtMS != clock.Now().UnixMilli() {
		t.Fatalf("read cursor = %+v", cursor)
	}
	if _, err := service.messages.GetMessage(context.Background(), "id-005"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("read candidate message error = %v, want ErrNotFound", err)
	}
}

func TestMarkReadDeduplicatesSameCursorWithReadAtExcluded(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedMessagingDevice(t, store, "device-1", "account-1", clock.Now())
	registry := newScriptedRegistry("read-dedup", nil)
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-read-dedup-target"),
		Body:          "target",
	})
	command := MarkReadCommand{
		CommonCommand:     testCommonCommand("key-read-dedup"),
		DeviceID:          "device-1",
		LastReadMessageID: target.LocalMessageID,
		LastReadAt:        clock.Now(),
	}

	first := mustMarkRead(t, service, command)
	clock.Advance(time.Minute)
	command.LastReadAt = clock.Now()
	second := mustMarkRead(t, service, command)
	if first != second {
		t.Fatalf("duplicate read submissions differ: first=%+v second=%+v", first, second)
	}
	receipt, err := service.outbox.GetOutboxReadReceipt(context.Background(), first.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReadReceipt(): %v", err)
	}
	if receipt.ReadAtMS != messagingTestTime.UnixMilli() {
		t.Fatalf("deduplicated receipt read_at_ms = %d, want original %d", receipt.ReadAtMS, messagingTestTime.UnixMilli())
	}
	cursor, err := store.GetReadCursor("device-1", "conversation-1")
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if cursor.LastReadAtMS != messagingTestTime.UnixMilli() || cursor.UpdatedAtMS != messagingTestTime.UnixMilli() {
		t.Fatalf("deduplicated cursor changed = %+v, want original timestamps", cursor)
	}
}

func TestMarkReadRejectsMissingAndCrossAccountDeviceAndForeignTarget(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedSecondMessagingAccount(t, store, clock.Now())
	seedMessagingDevice(t, store, "device-1", "account-1", clock.Now())
	seedMessagingDevice(t, store, "device-2", "account-2", clock.Now())
	registry := newScriptedRegistry("read-ownership", nil)
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)
	localTarget := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-read-local-target"),
		Body:          "local",
	})
	foreignTarget := seedMessagingMessage(t, store, clock, sqlite.Message{
		MessageID:       "foreign-read-message",
		ConversationID:  "conversation-2",
		AccountID:       "account-2",
		RemoteMessageID: "remote-foreign-read-message",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "foreign",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	})

	tests := []struct {
		name     string
		deviceID string
		targetID string
	}{
		{name: "missing device", deviceID: "missing-device", targetID: localTarget.LocalMessageID},
		{name: "cross-account device", deviceID: "device-2", targetID: localTarget.LocalMessageID},
		{name: "foreign target", deviceID: "device-1", targetID: foreignTarget},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.MarkRead(context.Background(), MarkReadCommand{
				CommonCommand:     testCommonCommand("key-read-ownership-" + test.name),
				DeviceID:          test.deviceID,
				LastReadMessageID: test.targetID,
			})
			if err == nil {
				t.Fatal("MarkRead() succeeded, want ownership/not-found error")
			}
			if test.name != "missing device" && !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("MarkRead() error = %v, want ErrInvalidCommand", err)
			}
		})
	}
}

func TestCancelAndDeferredAPIs(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(
		t,
		store,
		newScriptedRegistry("cancel-platform", &scriptedTextSender{}),
		clock,
	)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("key-cancel"),
		Body:          "cancel me",
	}
	command.NotBefore = clock.Now().Add(time.Hour)
	submission := mustSendText(t, service, command)
	if _, err := service.RetryNotDispatched(context.Background(), submission.OutboxID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("RetryNotDispatched(queued) error = %v, want ErrInvalidState", err)
	}

	canceled, err := service.Cancel(context.Background(), submission.OutboxID)
	if err != nil || canceled.State != OutboxCanceled {
		t.Fatalf("Cancel() = %+v, %v", canceled, err)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(canceled) = %d, %v", processed, err)
	}
	if _, err := service.SendAgain(context.Background(), submission.OutboxID, "new-key"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("SendAgain() error = %v, want ErrNotImplemented", err)
	}
	if err := service.ObserveTransportEcho(context.Background(), TransportEcho{}); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("ObserveTransportEcho() error = %v, want ErrNotImplemented", err)
	}
}

type sendStep struct {
	result bridge.SendResult
	err    error
}

type scriptedTextSender struct {
	mu       sync.Mutex
	steps    []sendStep
	requests []bridge.TextRequest
	onSend   func()
}

func (s *scriptedTextSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	var step sendStep
	if len(s.steps) == 0 {
		s.mu.Unlock()
		if s.onSend != nil {
			s.onSend()
		}
		return bridge.SendResult{}, nil
	}
	step = s.steps[0]
	s.steps = s.steps[1:]
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	return step.result, step.err
}

func (s *scriptedTextSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedTextSender) snapshotRequests() []bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.TextRequest(nil), s.requests...)
}

type scriptedMediaSender struct {
	mu       sync.Mutex
	steps    []sendStep
	requests []bridge.MediaRequest
	onSend   func()
}

func (s *scriptedMediaSender) SendMedia(
	_ context.Context,
	request bridge.MediaRequest,
) (bridge.SendResult, error) {
	content, readErr := io.ReadAll(request.Reader)
	request.Reader = bytes.NewReader(content)

	s.mu.Lock()
	s.requests = append(s.requests, request)
	var step sendStep
	if len(s.steps) > 0 {
		step = s.steps[0]
		s.steps = s.steps[1:]
	}
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	if readErr != nil {
		return bridge.SendResult{}, readErr
	}
	return step.result, step.err
}

func (s *scriptedMediaSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedMediaSender) snapshotRequests() []bridge.MediaRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.MediaRequest(nil), s.requests...)
}

type scriptedReactionSender struct {
	mu       sync.Mutex
	steps    []sendStep
	requests []bridge.ReactionRequest
	onSend   func()
}

func (s *scriptedReactionSender) SendReaction(
	_ context.Context,
	request bridge.ReactionRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	var step sendStep
	if len(s.steps) > 0 {
		step = s.steps[0]
		s.steps = s.steps[1:]
	}
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	return step.result, step.err
}

func (s *scriptedReactionSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedReactionSender) snapshotRequests() []bridge.ReactionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.ReactionRequest(nil), s.requests...)
}

type readReceiptStep struct {
	err error
}

type scriptedReadReceiptSender struct {
	mu       sync.Mutex
	steps    []readReceiptStep
	requests []bridge.ReadReceiptRequest
	onSend   func()
}

func (s *scriptedReadReceiptSender) MarkRead(
	_ context.Context,
	request bridge.ReadReceiptRequest,
) error {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	var step readReceiptStep
	if len(s.steps) > 0 {
		step = s.steps[0]
		s.steps = s.steps[1:]
	}
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	return step.err
}

func (s *scriptedReadReceiptSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedReadReceiptSender) snapshotRequests() []bridge.ReadReceiptRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.ReadReceiptRequest(nil), s.requests...)
}

type scriptedRegistry struct {
	mu                   sync.Mutex
	platform             bridge.Platform
	sender               bridge.TextSender
	media                bridge.MediaSender
	reaction             bridge.ReactionSender
	readReceipt          bridge.ReadReceiptSender
	mediaSendDeclared    bool
	reactionsDeclared    bool
	readReceiptsDeclared bool
	available            bool
}

func newScriptedRegistry(platform bridge.Platform, sender bridge.TextSender) *scriptedRegistry {
	return &scriptedRegistry{platform: platform, sender: sender}
}

func (r *scriptedRegistry) setAvailable(available bool) {
	r.mu.Lock()
	r.available = available
	r.mu.Unlock()
}

func (r *scriptedRegistry) setMediaSender(sender bridge.MediaSender) {
	r.mu.Lock()
	r.media = sender
	r.mediaSendDeclared = sender != nil
	r.mu.Unlock()
}

func (r *scriptedRegistry) setMediaSendDeclared(declared bool) {
	r.mu.Lock()
	r.mediaSendDeclared = declared
	r.mu.Unlock()
}

func (r *scriptedRegistry) setReactionSender(sender bridge.ReactionSender) {
	r.mu.Lock()
	r.reaction = sender
	r.reactionsDeclared = sender != nil
	r.mu.Unlock()
}

func (r *scriptedRegistry) setReactionsDeclared(declared bool) {
	r.mu.Lock()
	r.reactionsDeclared = declared
	r.mu.Unlock()
}

func (r *scriptedRegistry) setReadReceiptSender(sender bridge.ReadReceiptSender) {
	r.mu.Lock()
	r.readReceipt = sender
	r.readReceiptsDeclared = sender != nil
	r.mu.Unlock()
}

func (r *scriptedRegistry) setReadReceiptsDeclared(declared bool) {
	r.mu.Lock()
	r.readReceiptsDeclared = declared
	r.mu.Unlock()
}

func (r *scriptedRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != "account-1" {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{
		AccountID:  accountID,
		Platform:   r.platform,
		Generation: 7,
	}, true
}

func (r *scriptedRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID != "account-1" || !r.available {
		return nil, fmt.Errorf("%w: %s", bridge.ErrAccountNotRegistered, accountID)
	}
	lease := &bridge.DispatchLease{
		AccountID:  accountID,
		Platform:   r.platform,
		Generation: 7,
		Ctx:        ctx,
	}
	switch capability {
	case bridge.CapabilityTextSend:
		if r.sender == nil {
			return nil, bridge.ErrCapabilityUnavailable
		}
		lease.Text = r.sender
	case bridge.CapabilityMediaSend:
		if r.media == nil {
			return nil, bridge.ErrCapabilityUnavailable
		}
		lease.Media = r.media
	case bridge.CapabilityReactions:
		if r.reaction == nil {
			return nil, bridge.ErrCapabilityUnavailable
		}
		lease.Reaction = r.reaction
	case bridge.CapabilityReadReceipts:
		if r.readReceipt == nil {
			return nil, bridge.ErrCapabilityUnavailable
		}
		lease.ReadReceipt = r.readReceipt
	default:
		return nil, bridge.ErrCapabilityUnavailable
	}
	return lease, nil
}

func (r *scriptedRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bridge.CapabilitySet{
		TextSend:     accountID == "account-1" && r.sender != nil,
		MediaSend:    accountID == "account-1" && r.mediaSendDeclared,
		Reactions:    accountID == "account-1" && r.reactionsDeclared,
		ReadReceipts: accountID == "account-1" && r.readReceiptsDeclared,
	}
}

type sequentialIDs struct {
	mu   sync.Mutex
	next int
}

func (s *sequentialIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("id-%03d", s.next), nil
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(now time.Time) *manualClock { return &manualClock{now: now} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func (c *manualClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

func newMessagingTestService(
	t *testing.T,
	store *sqlite.Store,
	registry bridge.Registry,
	clock Clock,
) *MessageService {
	t.Helper()
	blobs, _ := newMessagingTestBlobStore(t)
	return newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
}

func newMessagingTestServiceWithBlobs(
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

func newMessagingTestBlobStore(t *testing.T) (*blob.BlobStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "blobs")
	store, err := blob.New(root)
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	return store, root
}

func messagingBlobFiles(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "*", "*"))
	if err != nil {
		t.Fatalf("Glob(blob files): %v", err)
	}
	return files
}

func openMessagingTestStore(t *testing.T, now time.Time) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "message.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedMessagingStore(t, store, now)
	return store
}

func seedMessagingStore(t *testing.T, store *sqlite.Store, now time.Time) {
	t.Helper()
	nowMS := now.UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "account-1",
		BridgeKey:   "scripted",
		DisplayName: "Scripted account",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "conversation-1",
		AccountID:            "account-1",
		RemoteConversationID: "remote-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Test conversation",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
}

func testCommonCommand(key string) CommonCommand {
	return CommonCommand{
		AccountID:      "account-1",
		ConversationID: "conversation-1",
		IdempotencyKey: key,
	}
}

func mustSendText(t *testing.T, service *MessageService, command SendTextCommand) Submission {
	t.Helper()
	submission, err := service.SendText(context.Background(), command)
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	return submission
}

func mustSendMedia(t *testing.T, service *MessageService, command SendMediaCommand) Submission {
	t.Helper()
	submission, err := service.SendMedia(context.Background(), command)
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	return submission
}

func mustSendReaction(t *testing.T, service *MessageService, command SendReactionCommand) Submission {
	t.Helper()
	submission, err := service.SendReaction(context.Background(), command)
	if err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	return submission
}

func mustMarkRead(t *testing.T, service *MessageService, command MarkReadCommand) Submission {
	t.Helper()
	submission, err := service.MarkRead(context.Background(), command)
	if err != nil {
		t.Fatalf("MarkRead(): %v", err)
	}
	return submission
}

func mustDelivery(t *testing.T, service *MessageService, outboxID string) Delivery {
	t.Helper()
	delivery, err := service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("Get(%q): %v", outboxID, err)
	}
	return delivery
}

func mustOutboxItem(t *testing.T, service *MessageService, outboxID string) sqlite.OutboxItem {
	t.Helper()
	item, err := service.outbox.FindByID(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("FindByID(%q): %v", outboxID, err)
	}
	return item
}

func seedMessagingDevice(
	t *testing.T,
	store *sqlite.Store,
	deviceID string,
	accountID string,
	now time.Time,
) {
	t.Helper()
	if err := store.UpsertDevice(sqlite.Device{
		DeviceID:    deviceID,
		AccountID:   accountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "Test device",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: now.UnixMilli(),
		UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertDevice(%q): %v", deviceID, err)
	}
}

func seedSecondMessagingAccount(t *testing.T, store *sqlite.Store, now time.Time) {
	t.Helper()
	nowMS := now.UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "account-2",
		BridgeKey:   "scripted",
		DisplayName: "Second account",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(account-2): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "conversation-2",
		AccountID:            "account-2",
		RemoteConversationID: "remote-conversation-2",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Second conversation",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(conversation-2): %v", err)
	}
}

func seedMessagingMessage(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	message sqlite.Message,
) string {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	inboxID := "inbox-" + message.MessageID + "-" + string(message.State)
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    message.AccountID,
		Generation:   1,
		DedupeKey:    inboxID,
		Codec:        "test.frame",
		CodecVersion: 1,
		Payload:      []byte("frame"),
	}); err != nil {
		t.Fatalf("AppendInbox(%q): %v", inboxID, err)
	}
	if err := repository.ProjectMessage(context.Background(), sqlite.MessageProjection{
		InboxID: inboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(%q): %v", message.MessageID, err)
	}
	return message.MessageID
}

func markMessagingMessageDeleted(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	messageID string,
) {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	message, err := repository.GetMessage(context.Background(), messageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", messageID, err)
	}
	message.State = sqlite.MessageStateDeleted
	seedMessagingMessage(t, store, clock, message)
}
