package ingest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func repairThumbnail(id string) *gmproto.MediaContent {
	return &gmproto.MediaContent{MimeType: "image/jpeg", ThumbnailMediaID: id, ThumbnailDecryptionKey: []byte{0x0a}}
}

func repairOriginal(id string) *gmproto.MediaContent {
	return &gmproto.MediaContent{
		MimeType: "image/jpeg", MediaName: "IMG.jpeg", Size: 4_000_000,
		MediaID: id, DecryptionKey: []byte{0x0b},
		ThumbnailMediaID: "thumb-of-" + id, ThumbnailDecryptionKey: []byte{0x0c},
	}
}

func repairNameless() *gmproto.MediaContent {
	return &gmproto.MediaContent{MimeType: "image/jpeg"}
}

// repairMediaFrame sends one update of message id in conversation with media
// as its second part, as Google does. The sender is the thread's peer: Karl
// in hwt-r, Shoshana in hwt-d.
func repairMediaFrame(t *testing.T, h *hwtHarness, conversation, id string, at time.Time, media *gmproto.MediaContent) {
	t.Helper()
	from := hwtKarl
	if conversation == "hwt-d" {
		from = hwtShoshana
	}
	// A caption keeps an update that names no media from being an empty
	// stub, which the decoder drops.
	message := hwtMessage{id: id, conversation: conversation, body: "look at " + id, from: from, at: at}.proto()
	message.MessageInfo = append(message.MessageInfo, &gmproto.MessageInfo{
		ActionMessageID: proto.String("part-" + id),
		Data:            &gmproto.MessageInfo_MediaContent{MediaContent: media},
	})
	h.liveMessage(t, message)
	h.pump(t)
}

// oldGoogleRef is the reference the decoder packed before migration 0012.
func oldGoogleRef(id, key string) []byte {
	if id == "" {
		key = ""
	}
	return []byte(fmt.Sprintf(`{"v":1,"media_id":%q,"decryption_key":%q}`, id, key))
}

type repairRowState struct {
	remoteID      string
	remoteRef     []byte
	remotePreview bool
	state         string
	blobPreview   bool
}

func repairRow(t *testing.T, h *hwtHarness, attachments *sqlite.MessageAttachmentRepository, messageID string) repairRowState {
	t.Helper()
	row, err := attachments.GetForDownload(context.Background(), messageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(%s): %v", messageID, err)
	}
	return repairRowState{row.RemoteID, row.RemoteRef, row.RemotePreview, row.State, row.BlobPreview}
}

// The repair takes rows in the state code before migration 0012 left them
// (thumbnail references unmarked and without their part, downloaded
// thumbnails taken for images, a reference wiped by a nameless update, also
// in a displaced conversation) to the state today's projection gives the same
// frames, and marks downloaded thumbnails so the media service replaces them.
// It leaves downloaded originals alone, and a second run plans nothing.
func TestPlanAndApplyGoogleMediaRepair(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hwt-r", "Repair", false, hwtKarl))
	h.liveConversation(t, hwtConversation("hwt-d", "Displaced", false, hwtShoshana))
	h.pump(t)
	at := hwtStart.Add(-time.Hour)

	repairMediaFrame(t, h, "hwt-r", "held-thumb", at, repairNameless())
	repairMediaFrame(t, h, "hwt-r", "held-thumb", at, repairThumbnail("T1"))
	repairMediaFrame(t, h, "hwt-r", "pending-thumb", at, repairThumbnail("T2"))
	repairMediaFrame(t, h, "hwt-r", "held-then-named", at, repairThumbnail("T3"))
	repairMediaFrame(t, h, "hwt-r", "wiped", at, repairThumbnail("T4"))
	repairMediaFrame(t, h, "hwt-r", "held-original", at, repairOriginal("F5"))
	repairMediaFrame(t, h, "hwt-d", "displaced-wiped", at, repairThumbnail("T6"))
	repairMediaFrame(t, h, "hwt-r", "never-named", at, repairNameless())

	attachments, err := sqlite.NewMessageAttachmentRepository(h.store, h.clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	ids := map[string]string{}
	for _, remote := range []string{"held-thumb", "pending-thumb", "held-then-named", "wiped", "held-original", "never-named"} {
		ids[remote] = h.message(t, "hwt-r", remote).MessageID
	}
	ids["displaced-wiped"] = h.message(t, "hwt-d", "displaced-wiped").MessageID
	// What today's projection stores from these frames.
	want := map[string]repairRowState{}
	for remote, id := range ids {
		want[remote] = repairRow(t, h, attachments, id)
	}
	if !want["pending-thumb"].remotePreview || !bytes.Contains(want["pending-thumb"].remoteRef, []byte(`"part_id":"part-pending-thumb"`)) {
		t.Fatalf("projected thumbnail = %+v %s, want it marked with its part", want["pending-thumb"], want["pending-thumb"].remoteRef)
	}

	// Take the store back to how code before migration 0012 left it.
	database := i01OpenInspector(t, h.path)
	defer database.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for remote, key := range map[string]string{"held-thumb": "0a", "pending-thumb": "0a", "held-then-named": "0a", "held-original": "0b"} {
		row := want[remote]
		exec(`UPDATE message_attachments SET remote_ref = ?, remote_preview = 0 WHERE message_id = ?`, oldGoogleRef(row.remoteID, key), ids[remote])
	}
	for _, remote := range []string{"wiped", "displaced-wiped"} {
		exec(`UPDATE message_attachments SET remote_id = '', remote_ref = ?, remote_preview = 0, mime = 'application/octet-stream' WHERE message_id = ?`, oldGoogleRef("", ""), ids[remote])
	}
	exec(`UPDATE conversations SET remote_conversation_id = ? || conversation_id WHERE remote_conversation_id = 'hwt-d'`, sqlite.DisplacedRemoteIDPrefix+"hwt-d:")
	ctx := context.Background()
	for remote, hash := range map[string]string{"held-thumb": "1", "held-then-named": "3", "held-original": "5"} {
		if err := attachments.MarkDownloaded(ctx, ids[remote], 0, strings.Repeat(hash, 64), 10, "image/jpeg", false); err != nil {
			t.Fatalf("MarkDownloaded(%s): %v", remote, err)
		}
	}
	// The original of a downloaded thumbnail, named after it downloaded;
	// before 0012 the projection did not touch a downloaded row.
	repairMediaFrame(t, h, "hwt-r", "held-then-named", at, repairOriginal("F3"))
	exec(`UPDATE message_attachments SET remote_id = 'T3', remote_ref = ?, remote_preview = 0, blob_preview = 0 WHERE message_id = ?`, oldGoogleRef("T3", "0a"), ids["held-then-named"])

	report, err := PlanGoogleMediaRepair(ctx, h.store, GoogleMediaRepairOptions{AccountID: i01AccountID})
	if err != nil {
		t.Fatalf("PlanGoogleMediaRepair(): %v", err)
	}
	wantOutcomes := map[string]int{
		googleMediaHeldPreview:    2, // held-thumb, held-then-named
		googleMediaPendingPreview: 1, // pending-thumb
		googleMediaRestored:       2, // wiped, displaced-wiped
		googleMediaUnchanged:      1, // held-original
		googleMediaNeverNamed:     1,
	}
	if fmt.Sprint(report.Outcomes) != fmt.Sprint(wantOutcomes) || report.Attachments != 7 || report.Malformed != 0 {
		t.Fatalf("plan outcomes = %v over %d attachments (%d malformed), want %v over 7", report.Outcomes, report.Attachments, report.Malformed, wantOutcomes)
	}
	before := map[string]repairRowState{}
	for remote, id := range ids {
		before[remote] = repairRow(t, h, attachments, id)
	}
	if err := ApplyGoogleMediaRepair(ctx, h.store, &report); err != nil {
		t.Fatalf("ApplyGoogleMediaRepair(): %v", err)
	}
	if report.AppliedRepairs != len(report.Repairs) || len(report.Repairs) != 5 {
		t.Fatalf("applied %d of %d repairs, want all 5", report.AppliedRepairs, len(report.Repairs))
	}

	after := map[string]repairRowState{}
	for remote, id := range ids {
		after[remote] = repairRow(t, h, attachments, id)
	}
	for _, remote := range []string{"pending-thumb", "wiped", "displaced-wiped"} {
		if got := after[remote]; got.remoteID != want[remote].remoteID || !bytes.Equal(got.remoteRef, want[remote].remoteRef) || got.remotePreview != want[remote].remotePreview {
			t.Errorf("%s after repair = %+v %s, want today's projection %+v %s", remote, got, got.remoteRef, want[remote], want[remote].remoteRef)
		}
	}
	if got := after["held-thumb"]; got.state != "downloaded" || !got.blobPreview || !got.remotePreview || got.remoteID != "T1" ||
		!bytes.Equal(got.remoteRef, want["held-thumb"].remoteRef) {
		t.Errorf("held-thumb after repair = %+v %s, want a downloaded thumbnail awaiting its original", got, got.remoteRef)
	}
	named := sqlite.MessageAttachment{State: after["held-then-named"].state, RemoteID: after["held-then-named"].remoteID,
		RemotePreview: after["held-then-named"].remotePreview, BlobPreview: after["held-then-named"].blobPreview}
	if !named.NeedsOriginal() || named.RemoteID != "F3" {
		t.Errorf("held-then-named after repair = %+v, want the thumbnail held and F3 named", after["held-then-named"])
	}
	for _, remote := range []string{"held-original", "never-named"} {
		if got := after[remote]; got.remoteID != before[remote].remoteID || !bytes.Equal(got.remoteRef, before[remote].remoteRef) || got.blobPreview != before[remote].blobPreview {
			t.Errorf("%s changed: %+v, want %+v", remote, got, before[remote])
		}
	}
	if wiped, err := attachments.GetForDownload(ctx, ids["wiped"], 0); err != nil || wiped.MIME != "image/jpeg" {
		t.Errorf("wiped attachment's MIME after repair = %q (%v), want the frame's image/jpeg", wiped.MIME, err)
	}

	again, err := PlanGoogleMediaRepair(ctx, h.store, GoogleMediaRepairOptions{AccountID: i01AccountID})
	if err != nil || len(again.Repairs) != 0 {
		t.Fatalf("second plan = %d repairs (%v), %v; want none", len(again.Repairs), again.Outcomes, err)
	}
}

// A repair applies only to the row it was planned for: a row that changed
// since is skipped.
func TestApplyGoogleMediaRepairSkipsARowThatChanged(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hwt-r", "Repair", false, hwtKarl))
	h.pump(t)
	repairMediaFrame(t, h, "hwt-r", "pending-thumb", hwtStart, repairThumbnail("T2"))
	id := h.message(t, "hwt-r", "pending-thumb").MessageID
	database := i01OpenInspector(t, h.path)
	defer database.Close()
	if _, err := database.Exec(`UPDATE message_attachments SET remote_ref = ?, remote_preview = 0 WHERE message_id = ?`, oldGoogleRef("T2", "0a"), id); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	report, err := PlanGoogleMediaRepair(ctx, h.store, GoogleMediaRepairOptions{AccountID: i01AccountID})
	if err != nil || len(report.Repairs) != 1 {
		t.Fatalf("plan = %+v, %v; want one repair", report, err)
	}
	if _, err := database.Exec(`UPDATE message_attachments SET remote_ref = ? WHERE message_id = ?`, oldGoogleRef("T2", "0d"), id); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGoogleMediaRepair(ctx, h.store, &report); err != nil {
		t.Fatalf("ApplyGoogleMediaRepair(): %v", err)
	}
	if report.AppliedRepairs != 0 {
		t.Fatalf("applied %d repairs to a row that changed, want 0", report.AppliedRepairs)
	}
}

func repairFrameOf(t *testing.T, messageID string, atMS int64, media *gmproto.MediaContent) googleMediaFrame {
	t.Helper()
	message := &gmproto.Message{MessageID: messageID, MessageInfo: []*gmproto.MessageInfo{{
		ActionMessageID: proto.String("part-" + messageID),
		Data:            &gmproto.MessageInfo_MediaContent{MediaContent: media},
	}}}
	info := client.ExtractMediaInfo(message)
	packed, err := googleAttachments(messageID, info)
	if err != nil {
		t.Fatal(err)
	}
	return googleMediaFrame{timestampMS: atMS, info: info, attachment: packed[0]}
}

func TestPlanGoogleMediaRepairCases(t *testing.T) {
	const at = int64(1_792_355_247_354)
	thumb := func(id string) bridge.Attachment {
		return repairFrameOf(t, "m", at, repairThumbnail(id)).attachment
	}
	pending := func(id string, ref []byte) sqlite.AccountAttachment {
		return sqlite.AccountAttachment{MessageAttachment: sqlite.MessageAttachment{
			MessageID: "message", RemoteID: id, RemoteRef: ref, State: "pending",
		}, RemoteMessageID: "m", OccurredAtMS: at}
	}
	hash := strings.Repeat("a", 64)
	downloaded := func(id string, ref []byte) sqlite.AccountAttachment {
		row := pending(id, ref)
		row.State, row.BlobHash = "downloaded", &hash
		return row
	}
	for name, tc := range map[string]struct {
		row     sqlite.AccountAttachment
		frames  []googleMediaFrame
		outcome string
		toID    string
	}{
		"no frames": {pending("T1", oldGoogleRef("T1", "0a")), nil, googleMediaNoFrames, ""},
		"frames of a reused ID days later are not this message's": {
			pending("", oldGoogleRef("", "")),
			[]googleMediaFrame{repairFrameOf(t, "m", at+2*24*time.Hour.Milliseconds(), repairThumbnail("T9"))},
			googleMediaNoFrames, "",
		},
		"a later thumbnail replaces an earlier one": {
			pending("T1", oldGoogleRef("T1", "0a")),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairThumbnail("T1")), repairFrameOf(t, "m", at, repairThumbnail("T2"))},
			googleMediaPendingPreview, "T2",
		},
		"an original beats a later thumbnail": {
			pending("T1", oldGoogleRef("T1", "0a")),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairOriginal("F1")), repairFrameOf(t, "m", at, repairThumbnail("T2"))},
			googleMediaPendingOriginal, "F1",
		},
		"a reference no frame calls a thumbnail is kept": {
			pending("legacy-id", []byte(`{"v":1,"media_id":"legacy-id","decryption_key":"0f"}`)),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairThumbnail("T2"))},
			googleMediaKeptOriginal, "",
		},
		"bytes no frame describes are left alone": {
			downloaded("legacy-id", nil),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairThumbnail("T2"))},
			googleMediaUnknownBytes, "",
		},
		"a downloaded original is left alone": {
			downloaded("F1", oldGoogleRef("F1", "0b")),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairOriginal("F1"))},
			googleMediaUnchanged, "",
		},
		"a thumbnail named only beside its original awaits that original": {
			downloaded("thumb-of-F1", oldGoogleRef("thumb-of-F1", "0c")),
			[]googleMediaFrame{repairFrameOf(t, "m", at, repairOriginal("F1"))},
			googleMediaHeldPreview, "F1",
		},
	} {
		repair, outcome := planGoogleMediaRepair(tc.row, tc.frames)
		if outcome != tc.outcome || (repair == nil) != (tc.toID == "") || (repair != nil && repair.apply.RemoteID != tc.toID) {
			t.Errorf("%s: outcome %s, repair %+v; want %s to %q", name, outcome, repair, tc.outcome, tc.toID)
		}
	}
	if got := thumb("T1"); !got.Preview || !bytes.Contains(got.RemoteRef, []byte(`"part_id":"part-m"`)) {
		t.Errorf("packed thumbnail = %+v %s", got, got.RemoteRef)
	}
	for in, want := range map[string]string{
		"2877": "2877", sqlite.DisplacedRemoteIDPrefix + "2877:1ce0c6dd": "2877", sqlite.DisplacedRemoteIDPrefix + "x": sqlite.DisplacedRemoteIDPrefix + "x",
	} {
		if got := wireConversationID(in); got != want {
			t.Errorf("wireConversationID(%q) = %q, want %q", in, got, want)
		}
	}
}
