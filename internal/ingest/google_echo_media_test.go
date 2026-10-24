package ingest

// Images sent from OpenMessage, echoed without a TmpID. Before sends carried
// their image and media echoes were paired, each sent image showed as an
// "Empty message" (the local row, with no attachment) beside its echo (the
// image, often only as a thumbnail): all six media sends on the live store
// on 2026-10-24. The Google adapter sends an image as a message with no
// text and any caption as a separate text message, so a captioned image has
// two echoes. These tests drive the real service, worker and store.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// sentImage is the image every test sends; the sent message must end up
// holding exactly these bytes.
var sentImage = []byte("\x89PNG\r\n\x1a\n the original, full-size image")

func sentImageHash() string {
	sum := sha256.Sum256(sentImage)
	return hex.EncodeToString(sum[:])
}

// untaggedImageEcho is the echo of an image: an outgoing message with one
// media part and no text, and no TmpID.
func untaggedImageEcho(remoteID string, status gmproto.MessageStatusType, occurredAt time.Time) *gmproto.Message {
	message := googleEchoMessage(remoteID, "", "", true, occurredAt)
	message.MessageStatus = &gmproto.MessageStatus{Status: status}
	message.MessageInfo = []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
			MediaID:       "media-" + remoteID,
			MediaName:     "image.png",
			MimeType:      "image/png",
			Size:          7547,
			DecryptionKey: []byte{0x0a},
		}},
	}}
	return message
}

// imageEcho delivers an image echo this machine receives at arrivedAt.
func (h *googleEchoHarness) imageEcho(t *testing.T, remoteID string, status gmproto.MessageStatusType, arrivedAt time.Time) {
	t.Helper()
	if arrivedAt.Before(h.clock.Now()) {
		t.Fatalf("echo arrives at %v, before the clock's %v", arrivedAt, h.clock.Now())
	}
	h.clock.Set(arrivedAt)
	h.appendMessage(t, untaggedImageEcho(remoteID, status, arrivedAt.Add(phoneClockOffset)))
	h.waitDrained(t)
	if errors := h.counters.Snapshot(googleEchoAccountID).EchoErrors; errors != 0 {
		t.Fatalf("echo reconcile failed %d times", errors)
	}
}

func (h *googleEchoHarness) enqueueImage(t *testing.T, key, caption string) messaging.Submission {
	t.Helper()
	submission, err := h.service.SendMedia(context.Background(), messaging.SendMediaCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      googleEchoAccountID,
			ConversationID: googleEchoConversationID,
			IdempotencyKey: "google-echo-image-" + key,
		},
		Content:  bytes.NewReader(sentImage),
		Filename: "image.png",
		MIME:     "image/png",
		Caption:  caption,
	})
	if err != nil {
		t.Fatalf("MessageService.SendMedia(): %v", err)
	}
	return submission
}

func (h *googleEchoHarness) sendImageAt(t *testing.T, key, caption string, createdAt, sentAt time.Time) messaging.Submission {
	t.Helper()
	h.clock.Set(createdAt)
	submission := h.enqueueImage(t, key, caption)
	h.clock.Set(sentAt)
	h.confirm(t, submission.OutboxID)
	return submission
}

// shownMessage is what a message row shows: its text and its attachments, as
// "state:blob hash" per ordinal.
type shownMessage struct {
	body        string
	attachments []string
}

func (h *googleEchoHarness) shown(t *testing.T, messageID string) shownMessage {
	t.Helper()
	inspection, err := sql.Open("sqlite", h.storePath)
	if err != nil {
		t.Fatalf("sql.Open(inspect): %v", err)
	}
	defer inspection.Close()
	var shown shownMessage
	if err := inspection.QueryRow(`SELECT body FROM messages WHERE message_id = ?`, messageID).Scan(&shown.body); err != nil {
		t.Fatalf("read message %s: %v", messageID, err)
	}
	rows, err := inspection.Query(`
		SELECT state, COALESCE(blob_hash, '')
		FROM message_attachments
		WHERE message_id = ?
		ORDER BY ordinal
	`, messageID)
	if err != nil {
		t.Fatalf("read attachments of %s: %v", messageID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var state, hash string
		if err := rows.Scan(&state, &hash); err != nil {
			t.Fatalf("scan attachment: %v", err)
		}
		shown.attachments = append(shown.attachments, state+":"+hash)
	}
	return shown
}

// messageWithRemoteID returns the ID of the test conversation's row holding
// remoteID.
func (h *googleEchoHarness) messageWithRemoteID(t *testing.T, remoteID string) string {
	t.Helper()
	for messageID, remote := range h.remoteIDs(t) {
		if remote == remoteID {
			return messageID
		}
	}
	t.Fatalf("no message holds %q (rows %v)", remoteID, h.remoteIDs(t))
	return ""
}

// assertShowsSentImage checks that messageID shows the image it sent, the
// original bytes, with caption as its text.
func assertShowsSentImage(t *testing.T, h *googleEchoHarness, messageID, caption string) {
	t.Helper()
	want := "downloaded:" + sentImageHash()
	if got := h.shown(t, messageID); got.body != caption || len(got.attachments) != 1 || got.attachments[0] != want {
		t.Fatalf("message %s shows %q %v, want %q with the sent image %s", messageID, got.body, got.attachments, caption, want)
	}
}

// dropSentAttachments puts sent images back as builds before this fix stored
// them: a message with no attachment.
func (h *googleEchoHarness) dropSentAttachments(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", h.storePath)
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		DELETE FROM message_attachments
		WHERE message_id IN (SELECT local_message_id FROM outbox WHERE kind = 'media')
	`); err != nil {
		t.Fatalf("drop sent attachments: %v", err)
	}
}

// The live sequence of 2026-10-24: an image sent with no caption, its echo
// first seen 0.51 s after the send went out, then re-delivered.
func TestGoogleImageEchoMergesIntoTheSentImage(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	send := h.enqueueImage(t, "image", "")
	// Queued, the message already shows the image it is sending.
	assertShowsSentImage(t, h, send.LocalMessageID, "")
	h.clock.Set(googleEchoNow.Add(2541 * time.Millisecond))
	h.confirm(t, send.OutboxID)
	h.imageEcho(t, "89928", complete, googleEchoNow.Add(3052*time.Millisecond))
	h.imageEcho(t, "89928", delivered, googleEchoNow.Add(5*time.Second))

	h.assertBound(t, send, "89928")
	if got := h.countV2Messages(t); got != 1 {
		t.Fatalf("messages = %d, want one", got)
	}
	// It keeps the original it sent, not the echo's reference.
	assertShowsSentImage(t, h, send.LocalMessageID, "")
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 1 || snapshot.EchoErrors != 0 {
		t.Fatalf("counters = %+v, want one content match and no errors", snapshot)
	}
}

// A captioned image is two messages on the phone: the image, then the
// caption. The sent message shows both until the caption's echo arrives, then
// the caption is its own message, whichever echo comes first.
func TestGoogleCaptionEchoBecomesItsOwnMessage(t *testing.T) {
	for _, captionFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("captionFirst=%v", captionFirst), func(t *testing.T) {
			h := newGoogleEchoHarness(t, nil)
			send := h.sendImageAt(t, "captioned", "  look at this ", googleEchoNow, googleEchoNow.Add(time.Second))
			if len(h.sender.captions) != 1 || h.sender.captions[0] != "look at this" {
				t.Fatalf("captions sent = %q, want the trimmed caption", h.sender.captions)
			}
			assertShowsSentImage(t, h, send.LocalMessageID, "look at this")

			image := func(at time.Duration) { h.imageEcho(t, "89928", complete, googleEchoNow.Add(at)) }
			caption := func(at time.Duration) { h.echo(t, "89929", "look at this", complete, googleEchoNow.Add(at)) }
			if captionFirst {
				caption(1400 * time.Millisecond)
				image(1500 * time.Millisecond)
			} else {
				image(1400 * time.Millisecond)
				caption(1500 * time.Millisecond)
			}

			h.assertBound(t, send, "89928")
			if got := h.countV2Messages(t); got != 2 {
				t.Fatalf("messages = %d (%v), want the image and its caption", got, h.remoteIDs(t))
			}
			assertShowsSentImage(t, h, send.LocalMessageID, "")
			if got := h.shown(t, h.messageWithRemoteID(t, "89929")); got.body != "look at this" || len(got.attachments) != 0 {
				t.Fatalf("caption message shows %q %v, want the caption alone", got.body, got.attachments)
			}
		})
	}
}

// An image's echo is never taken for a text's, nor a text's for an image's,
// however well its timing fits: each send waits for an echo of its own kind.
func TestGoogleEchoKindsNeverCross(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	text := h.sendAt(t, "text", "ok", googleEchoNow, googleEchoNow)
	// An image with the same text, in one message, sent from the phone.
	mms := untaggedImageEcho("91001", complete, googleEchoNow.Add(500*time.Millisecond+phoneClockOffset))
	mms.MessageInfo = append(mms.MessageInfo, &gmproto.MessageInfo{
		Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: "ok"}},
	})
	h.clock.Set(googleEchoNow.Add(500 * time.Millisecond))
	h.appendMessage(t, mms)
	h.waitDrained(t)
	assertUnpaired(t, h, text, "91001")
	h.echo(t, "91002", "ok", complete, googleEchoNow.Add(800*time.Millisecond))
	h.assertBound(t, text, "91002")

	image := h.sendImageAt(t, "image", "", googleEchoNow.Add(time.Minute), googleEchoNow.Add(time.Minute))
	h.echo(t, "91003", "ok", complete, googleEchoNow.Add(time.Minute+500*time.Millisecond)) // typed on the phone
	if delivery := h.delivery(t, image.OutboxID); delivery.RemoteMessageID == "91003" {
		t.Fatalf("image send paired with a text: %+v", delivery)
	}
	h.imageEcho(t, "91004", complete, googleEchoNow.Add(time.Minute+800*time.Millisecond))
	h.assertBound(t, image, "91004")
	if got := h.countV2Messages(t); got != 4 {
		t.Fatalf("messages = %d (%v), want two sends and two phone messages", got, h.remoteIDs(t))
	}
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoErrors != 0 {
		t.Fatalf("counters = %+v, want no reconcile errors", snapshot)
	}
}

// Images sent before this fix: the sent message has no attachment and the
// echo projected beside it. The startup repair gives the message its image,
// merges the echo into it and moves any caption out, once.
func TestRepairMergesImageEchoesIntoSentImages(t *testing.T) {
	h := newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	plain := h.sendImageAt(t, "plain", "", googleEchoNow, googleEchoNow)
	h.imageEcho(t, "92001", complete, googleEchoNow.Add(511*time.Millisecond))
	captioned := h.sendImageAt(t, "captioned", "here", googleEchoNow.Add(time.Minute), googleEchoNow.Add(time.Minute))
	h.imageEcho(t, "92002", complete, googleEchoNow.Add(time.Minute+453*time.Millisecond))
	h.echo(t, "92003", "here", complete, googleEchoNow.Add(time.Minute+600*time.Millisecond))
	h.dropSentAttachments(t)
	if got := h.countV2Messages(t); got != 5 {
		t.Fatalf("messages before repair = %d, want each send beside its echoes", got)
	}

	report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || report.Attached != 2 || report.Merged != 3 || report.Failed != 0 {
		t.Fatalf("repair = %+v, %v; want 2 attached and 3 pairs (two images, one caption)", report, err)
	}
	h.assertBound(t, plain, "92001")
	h.assertBound(t, captioned, "92002")
	assertShowsSentImage(t, h, plain.LocalMessageID, "")
	assertShowsSentImage(t, h, captioned.LocalMessageID, "")
	if got := h.shown(t, h.messageWithRemoteID(t, "92003")); got.body != "here" || len(got.attachments) != 0 {
		t.Fatalf("caption message shows %q %v, want the caption alone", got.body, got.attachments)
	}
	if got := h.countV2Messages(t); got != 3 {
		t.Fatalf("messages after repair = %d (%v), want two images and a caption", got, h.remoteIDs(t))
	}

	if again, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background()); err != nil || again != (sqlite.EchoDuplicateRepairReport{}) {
		t.Fatalf("second repair = %+v, %v; want nothing to do", again, err)
	}
}

// mediaEchoScenario interleaves texts and images sent from OpenMessage
// (images with or without a caption) with texts and images sent from the
// phone, at least 2 s apart, as echoScenario does for texts. A send's echo is
// received 0.2-0.8 s after its confirm, or (outran) that long before it with
// a DELIVERED re-delivery after; a caption's echo follows its image's by
// 30-230 ms. Captions and texts share their words, so a caption must be told
// from a text by timing alone.
type mediaEchoScenario struct {
	events []mediaEchoEvent
}

type mediaEchoEvent struct {
	fromApp bool
	image   bool
	outran  bool
	body    string // a text's body or an image's caption
	at      time.Duration
	lag     time.Duration
	gap     time.Duration // caption echo after the image's
}

func (mediaEchoScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(6)
	var scenario mediaEchoScenario
	at := time.Duration(0)
	words := []string{"ok", "on my way"}
	for i := 0; i < n; i++ {
		at += time.Duration(2+r.Intn(40)) * time.Second
		event := mediaEchoEvent{
			fromApp: r.Intn(3) != 0,
			image:   r.Intn(2) == 0,
			outran:  r.Intn(4) == 0,
			body:    words[r.Intn(2)],
			at:      at,
			lag:     time.Duration(200+r.Intn(600)) * time.Millisecond,
			gap:     time.Duration(30+r.Intn(200)) * time.Millisecond,
		}
		if event.image && (!event.fromApp || r.Intn(2) == 0) {
			event.body = "" // phone images, and half the sent ones, have no caption
		}
		scenario.events = append(scenario.events, event)
	}
	return reflect.ValueOf(scenario)
}

func mediaEchoRemoteID(event int, caption bool) string {
	if caption {
		return fmt.Sprintf("94%03d", event)
	}
	return fmt.Sprintf("93%03d", event)
}

// runMediaEchoScenario plays the scenario through the live correlation, or
// through a pre-fix build (no content correlation, sent images without their
// attachment) followed by the startup repair, and returns the harness and
// each send.
func runMediaEchoScenario(t *testing.T, scenario mediaEchoScenario, viaRepair bool) (*googleEchoHarness, map[int]messaging.Submission) {
	t.Helper()
	var h *googleEchoHarness
	if viaRepair {
		h = newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	} else {
		h = newGoogleEchoHarness(t, nil)
	}
	echoes := func(i int, event mediaEchoEvent, status gmproto.MessageStatusType, arrivedAt time.Time) {
		if !event.image {
			h.echo(t, mediaEchoRemoteID(i, false), event.body, status, arrivedAt)
			return
		}
		h.imageEcho(t, mediaEchoRemoteID(i, false), status, arrivedAt)
		if event.body != "" {
			h.echo(t, mediaEchoRemoteID(i, true), event.body, status, arrivedAt.Add(event.gap))
		}
	}
	enqueue := func(i int, event mediaEchoEvent) messaging.Submission {
		key := fmt.Sprintf("send-%d", i)
		if event.image {
			return h.enqueueImage(t, key, event.body)
		}
		submission, _ := h.enqueue(t, key, event.body)
		return submission
	}
	sends := map[int]messaging.Submission{}
	for i, event := range scenario.events {
		at := googleEchoNow.Add(event.at)
		switch {
		case !event.fromApp:
			echoes(i, event, complete, at)
		case event.outran:
			h.clock.Set(at.Add(-event.lag - 300*time.Millisecond))
			sends[i] = enqueue(i, event)
			echoes(i, event, complete, at.Add(-event.lag-250*time.Millisecond))
			h.clock.Set(at)
			h.confirm(t, sends[i].OutboxID)
			echoes(i, event, delivered, at.Add(event.lag))
		default:
			h.clock.Set(at)
			sends[i] = enqueue(i, event)
			h.confirm(t, sends[i].OutboxID)
			echoes(i, event, complete, at.Add(event.lag))
		}
	}
	if viaRepair {
		h.dropSentAttachments(t)
		if _, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background()); err != nil {
			t.Fatalf("RepairUncorrelatedEchoDuplicates(): %v", err)
		}
	}
	return h, sends
}

// TestUncorrelatedEchoPairsImagesAndCaptionsProperty: for any scenario,
// through the live path and through the pre-fix path plus repair,
//   - every send ends bound to its own echo (an image to its image echo),
//   - every sent image shows the original it sent, and no caption once the
//     caption's echo is in, and
//   - every message is one row: one per text or image, one per caption.
func TestUncorrelatedEchoPairsImagesAndCaptionsProperty(t *testing.T) {
	property := func(scenario mediaEchoScenario) bool {
		for _, viaRepair := range []bool{false, true} {
			h, sends := runMediaEchoScenario(t, scenario, viaRepair)
			ids := h.remoteIDs(t)
			want := len(scenario.events)
			for i, event := range scenario.events {
				if event.image && event.body != "" {
					want++
					caption := h.shown(t, h.messageWithRemoteID(t, mediaEchoRemoteID(i, true)))
					if caption.body != event.body || len(caption.attachments) != 0 {
						t.Logf("viaRepair=%v: caption %d shows %q %v (%+v)", viaRepair, i, caption.body, caption.attachments, scenario.events)
						return false
					}
				}
				send, ok := sends[i]
				if !ok {
					continue
				}
				if got := ids[send.LocalMessageID]; got != mediaEchoRemoteID(i, false) {
					t.Logf("viaRepair=%v: send %d bound to %q, want its own echo %q (%+v)", viaRepair, i, got, mediaEchoRemoteID(i, false), scenario.events)
					return false
				}
				if event.image {
					shown := h.shown(t, send.LocalMessageID)
					if shown.body != "" || len(shown.attachments) != 1 || shown.attachments[0] != "downloaded:"+sentImageHash() {
						t.Logf("viaRepair=%v: sent image %d shows %q %v (%+v)", viaRepair, i, shown.body, shown.attachments, scenario.events)
						return false
					}
				}
			}
			if len(ids) != want {
				t.Logf("viaRepair=%v: rows = %d, want %d (%v; %+v)", viaRepair, len(ids), want, ids, scenario.events)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}
