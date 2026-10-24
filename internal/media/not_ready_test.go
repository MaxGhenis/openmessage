package media

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Google sends an image's message before it names the media and names it in
// an update about a second later (live, 2026-10-24). A download asked for in
// between must leave nothing behind that outlives the update.
func TestResolveNotReadyRecordsNothingAndSucceedsOnceTheReferenceArrives(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-early", []sqlite.MessageAttachment{{
		Ordinal: 0, Filename: "IMG_1583.jpeg", MIME: "image/jpeg",
	}})
	notReady := fmt.Errorf("%w: no media ID yet", bridge.ErrMediaNotReady)
	downloader := &scriptedDownloader{steps: []downloadStep{
		{err: notReady},
		byteStreamStep([]byte("jpeg bytes"), "image/jpeg"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-early", Ordinal: 0}

	_, err := service.Resolve(context.Background(), command)
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, bridge.ErrMediaNotReady) {
		t.Fatalf("Resolve(before the media ID) error = %v, want ErrNotReady wrapping bridge.ErrMediaNotReady", err)
	}
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrGone) {
		t.Fatalf("Resolve(before the media ID) error = %v, must not be terminal", err)
	}
	assertPendingWithoutBlob(t, env, "message-early", 0)
	if lastError := attachmentLastError(t, env.dbPath, "message-early", 0); lastError.Valid {
		t.Fatalf("not-ready download recorded last_error = %q, want nothing", lastError.String)
	}
	if _, unavailable := getMediaAttachment(t, env, "message-early", 0).UnavailableReasonAt(env.clock.Now()); unavailable {
		t.Fatal("not-ready download marked the attachment unavailable")
	}

	reprojectMediaMessage(t, env, "message-early", "named", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "media-1", RemoteRef: []byte(`{"v":1,"media_id":"media-1"}`),
		Filename: "IMG_1583.jpeg", MIME: "image/jpeg",
	}})
	descriptor, err := service.Resolve(context.Background(), command)
	if err != nil {
		t.Fatalf("Resolve(after the media ID): %v", err)
	}
	if descriptor.Ref.Hash == "" || getMediaAttachment(t, env, "message-early", 0).State != "downloaded" {
		t.Fatalf("after the media ID: descriptor %+v, row %+v", descriptor, getMediaAttachment(t, env, "message-early", 0))
	}
	requests := downloader.snapshotRequests()
	if len(requests) != 2 || requests[0].ref.RemoteID != "" || requests[1].ref.RemoteID != "media-1" {
		t.Fatalf("download requests = %+v, want the empty reference then media-1", requests)
	}
}

// A not-ready answer must not overwrite a receive-time or unavailable note
// either: it says nothing about the attachment.
func TestResolveNotReadyKeepsAnEarlierLastError(t *testing.T) {
	env := newMediaTestEnvironment(t)
	earlier := "earlier transport failure"
	seedMediaMessage(t, env, "message-noted", []sqlite.MessageAttachment{{
		Ordinal: 0, Filename: "noted.jpeg", MIME: "image/jpeg", LastError: &earlier,
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{{err: bridge.ErrMediaNotReady}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))

	_, err := service.Resolve(context.Background(), DownloadCommand{
		AccountID: "account-1", MessageID: "message-noted", Ordinal: 0,
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Resolve() error = %v, want ErrNotReady", err)
	}
	if lastError := attachmentLastError(t, env.dbPath, "message-noted", 0); !lastError.Valid || lastError.String != earlier {
		t.Fatalf("last_error = %+v, want %q kept", lastError, earlier)
	}
}

func reprojectMediaMessage(
	t *testing.T,
	env *mediaTestEnvironment,
	messageID string,
	update string,
	attachments []sqlite.MessageAttachment,
) {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(env.store, env.clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	inboxID := "inbox-" + messageID + "-" + update
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID: inboxID, AccountID: "account-1", Generation: 1,
		DedupeKey: "dedupe-" + messageID + "-" + update, Codec: "test", CodecVersion: 1,
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
		t.Fatalf("ProjectMessage(update %s): %v", update, err)
	}
}

// An attachment still unnamed NotReadyGrace after it arrived answers ErrGone,
// so readers stop waiting for it, and still records nothing: a reference that
// arrives later is downloaded as usual.
func TestResolveNotReadyPastTheGraceIsGoneAndRecordsNothing(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-unnamed", []sqlite.MessageAttachment{{
		Ordinal: 0, Filename: "IMG_2001.jpeg", MIME: "image/jpeg",
	}})
	notReady := fmt.Errorf("%w: no media ID yet", bridge.ErrMediaNotReady)
	downloader := &scriptedDownloader{steps: []downloadStep{
		{err: notReady},
		{err: notReady},
		byteStreamStep([]byte("late jpeg"), "image/jpeg"),
	}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-unnamed", Ordinal: 0}

	// Nameless updates keep coming (they move updated_at_ms); the grace still
	// runs from when the attachment arrived.
	env.clock.advance(NotReadyGrace - time.Minute)
	reprojectMediaMessage(t, env, "message-unnamed", "still-unnamed", []sqlite.MessageAttachment{{
		Ordinal: 0, Filename: "IMG_2001.jpeg", MIME: "image/jpeg",
	}})
	env.clock.advance(time.Minute - time.Second)
	if _, err := service.Resolve(context.Background(), command); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Resolve(just inside the grace) error = %v, want ErrNotReady", err)
	}
	env.clock.advance(time.Second)
	_, err := service.Resolve(context.Background(), command)
	if !errors.Is(err, ErrGone) || errors.Is(err, ErrNotReady) || !errors.Is(err, bridge.ErrMediaNotReady) {
		t.Fatalf("Resolve(at the grace) error = %v, want ErrGone wrapping bridge.ErrMediaNotReady", err)
	}
	assertPendingWithoutBlob(t, env, "message-unnamed", 0)
	if lastError := attachmentLastError(t, env.dbPath, "message-unnamed", 0); lastError.Valid {
		t.Fatalf("gone-after-grace recorded last_error = %q, want nothing", lastError.String)
	}

	reprojectMediaMessage(t, env, "message-unnamed", "named-late", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "media-late", RemoteRef: []byte(`{"v":1,"media_id":"media-late"}`),
		Filename: "IMG_2001.jpeg", MIME: "image/jpeg",
	}})
	if _, err := service.Resolve(context.Background(), command); err != nil {
		t.Fatalf("Resolve(after a late reference): %v", err)
	}
	if got := getMediaAttachment(t, env, "message-unnamed", 0); got.State != "downloaded" {
		t.Fatalf("after a late reference: row %+v, want downloaded", got)
	}
}
