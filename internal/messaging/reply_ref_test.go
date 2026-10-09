package messaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestReplyToIncomingMessageCarriesItsStoredDescription(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	identityID := "identity-quoted-author"
	seedDispatchIdentity(t, store, identityID, "+15551234567", clock.Now())
	target := projectReplyTarget(t, store, clock, sqlite.Message{
		MessageID:        "message-quoted-incoming",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "4f2b7c9d0e1a3b5c7d9e1f2a4b6c8d0e2f4a6b8c",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "  look at this  ",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     1_700_000_000_123,
	}, "video/mp4", "image/jpeg")

	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "reply-text"}}}}
	media := &scriptedMediaSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "reply-media"}}}}
	registry := newScriptedRegistry("reply-incoming", text)
	registry.setMediaSender(media)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	mustSendText(t, service, SendTextCommand{
		CommonCommand:    testCommonCommand("reply-incoming-text"),
		Body:             "replying in text",
		ReplyToMessageID: target.MessageID,
	})
	mustSendMedia(t, service, SendMediaCommand{
		CommonCommand:    testCommonCommand("reply-incoming-media"),
		Content:          bytes.NewReader([]byte("reply photo")),
		Filename:         "reply.png",
		MIME:             "image/png",
		ReplyToMessageID: target.MessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue() = %d, %v; want 2, nil", processed, err)
	}

	want := bridge.MessageRef{
		RemoteID:       target.RemoteMessageID,
		AuthorID:       "+15551234567",
		SentAt:         time.UnixMilli(1_700_000_000_123),
		Text:           "  look at this  ",
		HasAttachment:  true,
		AttachmentMIME: "video/mp4",
	}
	textRequests := text.snapshotRequests()
	if len(textRequests) != 1 || !messageRefEqual(textRequests[0].ReplyTo, &want) {
		t.Fatalf("text reply ref = %+v, want %+v", textRequests, want)
	}
	mediaRequests := media.snapshotRequests()
	if len(mediaRequests) != 1 || !messageRefEqual(mediaRequests[0].ReplyTo, &want) {
		t.Fatalf("media reply ref = %+v, want %+v", mediaRequests, want)
	}
}

func TestReplyToOwnConfirmedSendCarriesTransportIDAndSelfAuthor(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "1700000000555"}},
		{result: bridge.SendResult{RemoteMessageID: "1700000000556"}},
	}}
	registry := newScriptedRegistry("reply-own", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	quoted := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("reply-own-quoted"),
		Body:          "sent through the outbox",
	})
	submittedAt := clock.Now()
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(quoted) = %d, %v; want 1, nil", processed, err)
	}
	clock.Advance(time.Minute)
	mustSendText(t, service, SendTextCommand{
		CommonCommand:    testCommonCommand("reply-own-reply"),
		Body:             "replying to myself",
		ReplyToMessageID: quoted.LocalMessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reply) = %d, %v; want 1, nil", processed, err)
	}

	requests := text.snapshotRequests()
	want := bridge.MessageRef{
		RemoteID: "1700000000555",
		SentAt:   time.UnixMilli(submittedAt.UnixMilli()),
		Text:     "sent through the outbox",
	}
	if len(requests) != 2 || !messageRefEqual(requests[1].ReplyTo, &want) {
		t.Fatalf("own reply ref = %+v, want %+v", requests, want)
	}
}

func TestReplySubmittedWhileQuotedSendWasPendingFollowsItsConfirmation(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "1700000000777"}},
		{result: bridge.SendResult{RemoteMessageID: "1700000000778"}},
	}}
	registry := newScriptedRegistry("reply-pending", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	quoted := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("reply-pending-quoted"),
		Body:          "still sending",
	})
	quotedRow := mustOutboxItem(t, service, quoted.OutboxID)
	replyCommand := testCommonCommand("reply-pending-reply")
	replyCommand.NotBefore = clock.Now().Add(time.Second)
	reply := mustSendText(t, service, SendTextCommand{
		CommonCommand:    replyCommand,
		Body:             "quick reply",
		ReplyToMessageID: quoted.LocalMessageID,
	})
	stored, err := service.messages.GetMessage(context.Background(), reply.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(reply): %v", err)
	}
	if stored.ReplyToRemoteID == nil || *stored.ReplyToRemoteID != quotedRow.TransportRequestID {
		t.Fatalf("stored reply target = %v, want the pending request ID %q", stored.ReplyToRemoteID, quotedRow.TransportRequestID)
	}

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(quoted) = %d, %v; want 1, nil", processed, err)
	}
	clock.Advance(time.Second)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reply) = %d, %v; want 1, nil", processed, err)
	}

	requests := text.snapshotRequests()
	want := bridge.MessageRef{
		RemoteID: "1700000000777",
		SentAt:   time.UnixMilli(messagingTestTime.UnixMilli()),
		Text:     "still sending",
	}
	if len(requests) != 2 || !messageRefEqual(requests[1].ReplyTo, &want) {
		t.Fatalf("reply ref after the quoted send confirmed = %+v, want %+v", requests, want)
	}
}

func TestReplyToUnsentOwnMessageCarriesOnlyItsRequestID(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "reply-after-cancel"}}}}
	registry := newScriptedRegistry("reply-unsent", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	quoted := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("reply-unsent-quoted"),
		Body:          "never sent",
	})
	quotedRow := mustOutboxItem(t, service, quoted.OutboxID)
	if _, err := service.Cancel(context.Background(), quoted.OutboxID); err != nil {
		t.Fatalf("Cancel(quoted): %v", err)
	}
	mustSendText(t, service, SendTextCommand{
		CommonCommand:    testCommonCommand("reply-unsent-reply"),
		Body:             "reply to a canceled send",
		ReplyToMessageID: quoted.LocalMessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	requests := text.snapshotRequests()
	want := bridge.MessageRef{RemoteID: quotedRow.TransportRequestID}
	if len(requests) != 1 || !messageRefEqual(requests[0].ReplyTo, &want) {
		t.Fatalf("reply ref to an unsent message = %+v, want bare %+v", requests, want)
	}
}

func TestReplyToRemoteIDTheStoreLacksStaysBare(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "reply-unknown"}}}}
	registry := newScriptedRegistry("reply-unknown", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	unknown := "remote-id-the-store-does-not-hold"
	if _, _, err := service.outbox.EnqueueOutgoingMessage(context.Background(), sqlite.NewOutboxItem{
		OutboxID:           "outbox-unknown-reply",
		AccountID:          "account-1",
		ConversationID:     "conversation-1",
		Kind:               sqlite.OutboxKindText,
		IdempotencyKey:     "reply-unknown",
		PayloadHash:        "hash-unknown-reply",
		Operation:          textOperation,
		LocalMessageID:     "message-unknown-reply",
		TransportRequestID: "request-unknown-reply",
		ScheduledFor:       clock.Now(),
	}, sqlite.Message{
		MessageID:       "message-unknown-reply",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "request-unknown-reply",
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "reply to a message only the transport knows",
		ReplyToRemoteID: &unknown,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("EnqueueOutgoingMessage(): %v", err)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	requests := text.snapshotRequests()
	want := bridge.MessageRef{RemoteID: unknown}
	if len(requests) != 1 || !messageRefEqual(requests[0].ReplyTo, &want) {
		t.Fatalf("reply ref to an unheld remote ID = %+v, want bare %+v", requests, want)
	}
}

func TestReplyToOutboxMediaWithoutCaptionCarriesItsAttachment(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "reply-to-photo"}}}}
	media := &scriptedMediaSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "1700000000999"}}}}
	registry := newScriptedRegistry("reply-outbox-media", text)
	registry.setMediaSender(media)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	photo := mustSendMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("reply-outbox-media-photo"),
		Content:       bytes.NewReader([]byte("png bytes")),
		Filename:      "photo.png",
		MIME:          "image/png",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(photo) = %d, %v; want 1, nil", processed, err)
	}
	mustSendText(t, service, SendTextCommand{
		CommonCommand:    testCommonCommand("reply-outbox-media-reply"),
		Body:             "nice photo",
		ReplyToMessageID: photo.LocalMessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reply) = %d, %v; want 1, nil", processed, err)
	}

	requests := text.snapshotRequests()
	want := bridge.MessageRef{
		RemoteID:       "1700000000999",
		SentAt:         time.UnixMilli(messagingTestTime.UnixMilli()),
		HasAttachment:  true,
		AttachmentMIME: "image/png",
	}
	if len(requests) != 1 || !messageRefEqual(requests[0].ReplyTo, &want) {
		t.Fatalf("reply ref to an outbox photo = %+v, want %+v", requests, want)
	}
}

func TestReplyTargetLoadFailureFailsBeforeTransport(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	target := projectReplyTarget(t, store, clock, sqlite.Message{
		MessageID:       "message-load-failure",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "remote-load-failure",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "quoted",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().Add(-time.Minute).UnixMilli(),
	})
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "never"}}}}
	registry := newScriptedRegistry("reply-load-failure", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	service.messages = failingAttachmentRepository{messageRepository: service.messages}

	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand:    testCommonCommand("reply-load-failure"),
		Body:             "reply",
		ReplyToMessageID: target.MessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxNotDispatched || row.ErrorCode == nil ||
		*row.ErrorCode != "load_reply_target_attachment" || row.TransportCalledAtMS != nil {
		t.Fatalf("outbox row after reply-target load failure = %+v, want not_dispatched before the transport call", row)
	}
	if got := text.requestCount(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

// TestReplyRefInvariants checks, over seeded random conversations, every
// reply the dispatcher maps:
//
//   - The ref names the quoted message's current remote ID, or the stored
//     remote ID when the store has no such message.
//   - SentAt is zero exactly when the dispatcher has no description: the
//     message is not held, or it is an outgoing one the transport has not
//     accepted.
//   - A described ref equals the stored message: AuthorID is its sender's
//     canonical value ("" for a nil sender), SentAt its occurred time, Text
//     its body, and the attachment fields its first attachment.
//   - The mapping is deterministic: a retried attempt carries the same ref.
func TestReplyRefInvariants(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			checkReplyRefInvariants(t, seed)
		})
	}
}

type replyRefCase struct {
	name       string
	quotedID   string // local message ID the reply quotes
	wantRemote string
	described  bool
	author     string
	occurredMS int64
	body       string
	mime       string
	hasMedia   bool
}

func checkReplyRefInvariants(t *testing.T, seed int64) {
	random := rand.New(rand.NewSource(seed))
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{}
	media := &scriptedMediaSender{}
	registry := newScriptedRegistry(bridge.Platform(fmt.Sprintf("reply-invariants-%d", seed)), text)
	registry.setMediaSender(media)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	bodies := []string{"", "  ", "hello", "  padded  ", "multi\nline"}
	mimes := []string{"image/jpeg", "video/mp4", "audio/aac", "application/pdf"}
	var cases []replyRefCase

	identityCount := 1 + random.Intn(3)
	for index := 0; index < identityCount; index++ {
		seedDispatchIdentity(t, store, fmt.Sprintf("identity-%d", index), fmt.Sprintf("+1555000%04d", index), clock.Now())
	}
	incomingCount := 1 + random.Intn(4)
	for index := 0; index < incomingCount; index++ {
		message := sqlite.Message{
			MessageID:       fmt.Sprintf("message-incoming-%d", index),
			ConversationID:  "conversation-1",
			AccountID:       "account-1",
			RemoteMessageID: fmt.Sprintf("%040x", random.Uint64()),
			Direction:       sqlite.MessageDirectionIncoming,
			Body:            bodies[random.Intn(len(bodies))],
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    1_700_000_000_000 + random.Int63n(1_000_000_000),
		}
		author := ""
		if random.Intn(4) != 0 {
			identity := random.Intn(identityCount)
			identityID := fmt.Sprintf("identity-%d", identity)
			message.SenderIdentityID = &identityID
			author = fmt.Sprintf("+1555000%04d", identity)
		}
		var attachmentMIMEs []string
		for count := random.Intn(3); count > 0; count-- {
			attachmentMIMEs = append(attachmentMIMEs, mimes[random.Intn(len(mimes))])
		}
		projectReplyTarget(t, store, clock, message, attachmentMIMEs...)
		mime := ""
		if len(attachmentMIMEs) > 0 {
			mime = attachmentMIMEs[0]
		}
		cases = append(cases, replyRefCase{
			name: message.MessageID, quotedID: message.MessageID, wantRemote: message.RemoteMessageID,
			described: true, author: author, occurredMS: message.OccurredAtMS, body: message.Body,
			mime: mime, hasMedia: len(attachmentMIMEs) > 0,
		})
	}

	outgoingCount := 1 + random.Intn(4)
	for index := 0; index < outgoingCount; index++ {
		occurredMS := clock.Now().UnixMilli()
		body := bodies[random.Intn(len(bodies))]
		isMedia := random.Intn(3) == 0
		var submission Submission
		mime := ""
		if isMedia {
			mime = mimes[random.Intn(len(mimes))]
			submission = mustSendMedia(t, service, SendMediaCommand{
				CommonCommand: testCommonCommand(fmt.Sprintf("outgoing-%d", index)),
				Content:       bytes.NewReader([]byte(fmt.Sprintf("bytes-%d", index))),
				Filename:      "attachment.bin",
				MIME:          mime,
				Caption:       body,
			})
		} else {
			if strings.TrimSpace(body) == "" {
				body = "outgoing text"
			}
			submission = mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand(fmt.Sprintf("outgoing-%d", index)),
				Body:          body,
			})
		}
		row := mustOutboxItem(t, service, submission.OutboxID)
		confirmed := random.Intn(3) != 0
		wantRemote := row.TransportRequestID
		if confirmed {
			wantRemote = fmt.Sprintf("%d", 1_700_000_000_000+int64(index))
			if isMedia {
				media.mu.Lock()
				media.steps = append(media.steps, sendStep{result: bridge.SendResult{RemoteMessageID: wantRemote}})
				media.mu.Unlock()
			} else {
				text.mu.Lock()
				text.steps = append(text.steps, sendStep{result: bridge.SendResult{RemoteMessageID: wantRemote}})
				text.mu.Unlock()
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("seed %d: DispatchDue(outgoing %d) = %d, %v", seed, index, processed, err)
			}
		} else if _, err := service.Cancel(context.Background(), submission.OutboxID); err != nil {
			t.Fatalf("seed %d: Cancel(outgoing %d): %v", seed, index, err)
		}
		cases = append(cases, replyRefCase{
			name: submission.LocalMessageID, quotedID: submission.LocalMessageID, wantRemote: wantRemote,
			described: confirmed, occurredMS: occurredMS, body: body, mime: mime, hasMedia: isMedia,
		})
		clock.Advance(time.Second)
	}

	random.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
	for index, want := range cases {
		// The first attempt fails before dispatch, so the reply is mapped twice.
		text.mu.Lock()
		text.steps = append(text.steps,
			sendStep{err: bridge.OpError{
				Class: bridge.FailureTransient, Operation: "send_text",
				Dispatch: bridge.DispatchNotCalled, Cause: errors.New("scripted retry"),
			}},
			sendStep{result: bridge.SendResult{RemoteMessageID: fmt.Sprintf("reply-%d", index)}},
		)
		before := len(text.requests)
		text.mu.Unlock()
		mustSendText(t, service, SendTextCommand{
			CommonCommand:    testCommonCommand(fmt.Sprintf("reply-%d", index)),
			Body:             "reply",
			ReplyToMessageID: want.quotedID,
		})
		for attempt := 0; attempt < 2; attempt++ {
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("seed %d case %s attempt %d: DispatchDue = %d, %v", seed, want.name, attempt, processed, err)
			}
			clock.Advance(defaultRetryDelay + time.Second)
		}
		requests := text.snapshotRequests()[before:]
		if len(requests) != 2 {
			t.Fatalf("seed %d case %s: reply attempts = %d, want 2", seed, want.name, len(requests))
		}
		if !messageRefEqual(requests[0].ReplyTo, requests[1].ReplyTo) {
			t.Fatalf("seed %d case %s: retried ref %+v != first %+v", seed, want.name, requests[1].ReplyTo, requests[0].ReplyTo)
		}
		got := requests[1].ReplyTo
		if got == nil || got.RemoteID != want.wantRemote {
			t.Fatalf("seed %d case %s: ref = %+v, want remote %q", seed, want.name, got, want.wantRemote)
		}
		if got.SentAt.IsZero() == want.described {
			t.Fatalf("seed %d case %s: ref = %+v, described = %v", seed, want.name, got, want.described)
		}
		if !want.described {
			if *got != (bridge.MessageRef{RemoteID: want.wantRemote}) {
				t.Fatalf("seed %d case %s: undescribed ref = %+v, want bare", seed, want.name, got)
			}
			continue
		}
		wantRef := bridge.MessageRef{
			RemoteID: want.wantRemote, AuthorID: want.author, SentAt: time.UnixMilli(want.occurredMS),
			Text: want.body, HasAttachment: want.hasMedia, AttachmentMIME: want.mime,
		}
		if !messageRefEqual(got, &wantRef) {
			t.Fatalf("seed %d case %s: ref = %+v, want %+v", seed, want.name, got, wantRef)
		}
	}
}

// projectReplyTarget stores message as a projected (ingested) message with
// one pending attachment per MIME type, in ordinal order.
func projectReplyTarget(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	message sqlite.Message,
	attachmentMIMEs ...string,
) sqlite.Message {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	inboxID := "inbox-" + message.MessageID
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    message.AccountID,
		Generation:   1,
		DedupeKey:    inboxID,
		Codec:        "reply-target-test",
		CodecVersion: 1,
		Payload:      []byte("reply target"),
	}); err != nil {
		t.Fatalf("AppendInbox(%q): %v", inboxID, err)
	}
	attachments := make([]sqlite.MessageAttachment, 0, len(attachmentMIMEs))
	for ordinal, mime := range attachmentMIMEs {
		size := int64(10 + ordinal)
		attachments = append(attachments, sqlite.MessageAttachment{
			RemoteID:  fmt.Sprintf("%s-attachment-%d", message.MessageID, ordinal),
			Ordinal:   int64(ordinal),
			RemoteRef: []byte("{}"),
			Filename:  fmt.Sprintf("attachment-%d", ordinal),
			MIME:      mime,
			SizeBytes: &size,
			State:     "pending",
		})
	}
	if err := repository.ProjectMessage(context.Background(), sqlite.MessageProjection{
		InboxID:     inboxID,
		Message:     message,
		Attachments: attachments,
	}); err != nil {
		t.Fatalf("ProjectMessage(%q): %v", message.MessageID, err)
	}
	projected, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", message.MessageID, err)
	}
	return projected
}

func messageRefEqual(got, want *bridge.MessageRef) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got.RemoteID == want.RemoteID &&
		got.AuthorID == want.AuthorID &&
		got.SentAt.Equal(want.SentAt) &&
		got.SentAt.IsZero() == want.SentAt.IsZero() &&
		got.Text == want.Text &&
		got.HasAttachment == want.HasAttachment &&
		got.AttachmentMIME == want.AttachmentMIME
}

type failingAttachmentRepository struct {
	messageRepository
}

func (failingAttachmentRepository) FirstAttachmentMIME(context.Context, string) (string, bool, error) {
	return "", false, errors.New("scripted attachment lookup failure")
}
