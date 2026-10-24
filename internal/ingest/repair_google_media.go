package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Google media repair: attachment references as today's decoder and
// projection would have stored them.
//
// Before migration 0012 a reference named a thumbnail without saying so, and
// carried nothing to ask the phone for the full-size image with; a thumbnail
// that was downloaded was stored as if it were the image. Before #263 an
// update of a message that named no media wiped the reference of a pending
// attachment. The raw frames in the inbox still say what each update named,
// so the repair replays a message's frames through the decoder and gives each
// attachment the reference the rules now keep: the last full-size image any
// frame named, else the last thumbnail, marked as one. A downloaded attachment
// whose stored reference is a thumbnail is marked as holding a reduced copy,
// so the media service replaces it with the original (or asks the phone for
// it) the next time it is read. Downloaded originals, and attachments no
// frame describes, are left alone.

// Google media repair outcomes. Only the first four change the store.
const (
	googleMediaRestored        = "restore-reference"    // pending, its reference was empty
	googleMediaPendingOriginal = "pending-original"     // pending, a frame named the original
	googleMediaPendingPreview  = "pending-thumbnail"    // pending, mark the thumbnail and its part
	googleMediaHeldPreview     = "downloaded-thumbnail" // downloaded bytes are a thumbnail
	googleMediaUnchanged       = "unchanged"
	googleMediaNoFrames        = "no-frames"
	googleMediaNeverNamed      = "never-named"
	googleMediaUnknownBytes    = "downloaded-unknown-reference"
	googleMediaKeptOriginal    = "kept-reference"
)

// GoogleMediaRepairOptions scopes the repair to one Google account.
type GoogleMediaRepairOptions struct {
	AccountID string
}

// GoogleMediaRepair is one attachment the repair changes.
type GoogleMediaRepair struct {
	Outcome         string `json:"outcome"`
	MessageID       string `json:"message_id"`
	RemoteMessageID string `json:"remote_message_id"`
	Ordinal         int64  `json:"ordinal"`
	State           string `json:"state"`
	FromRemoteID    string `json:"from_remote_id"`
	ToRemoteID      string `json:"to_remote_id"`
	ToPreview       bool   `json:"to_preview"`
	BlobPreview     bool   `json:"blob_preview"`

	apply sqlite.AttachmentReferenceRepair
}

// GoogleMediaRepairReport is a dry-run plan; ApplyGoogleMediaRepair applies
// Repairs.
type GoogleMediaRepairReport struct {
	AccountID      string              `json:"account_id"`
	Frames         int                 `json:"frames"`
	Malformed      int                 `json:"malformed_frames"`
	MediaMessages  int                 `json:"messages_with_media_frames"`
	Attachments    int                 `json:"attachments"`
	Outcomes       map[string]int      `json:"outcomes"`
	Repairs        []GoogleMediaRepair `json:"repairs"`
	AppliedRepairs int                 `json:"applied_repairs,omitempty"`
}

type googleMediaKey struct {
	conversationID string
	messageID      string
}

// googleMediaFrame is what one frame of a message said about its media.
type googleMediaFrame struct {
	timestampMS int64
	info        *client.MediaInfo
	attachment  bridge.Attachment // as the decoder packs it; RemoteID may be empty
}

// PlanGoogleMediaRepair reads accountID's Google inbox frames and attachments
// and plans the repair. It writes nothing.
func PlanGoogleMediaRepair(
	ctx context.Context,
	store *sqlite.Store,
	options GoogleMediaRepairOptions,
) (GoogleMediaRepairReport, error) {
	report := GoogleMediaRepairReport{AccountID: options.AccountID, Outcomes: map[string]int{}}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, time.Now)
	if err != nil {
		return report, err
	}
	frames, err := readGoogleMediaFrames(ctx, store, options.AccountID, &report)
	if err != nil {
		return report, err
	}
	report.MediaMessages = len(frames)
	rows, err := attachments.ListAccountAttachments(ctx, options.AccountID)
	if err != nil {
		return report, err
	}
	report.Attachments = len(rows)
	for _, row := range rows {
		repair, outcome := planGoogleMediaRepair(row, frames[googleMediaKey{
			conversationID: wireConversationID(row.RemoteConversationID),
			messageID:      row.RemoteMessageID,
		}])
		report.Outcomes[outcome]++
		if repair != nil {
			report.Repairs = append(report.Repairs, *repair)
		}
	}
	return report, nil
}

// ApplyGoogleMediaRepair applies report's repairs in one transaction. A
// repair whose attachment changed since the plan is skipped.
func ApplyGoogleMediaRepair(ctx context.Context, store *sqlite.Store, report *GoogleMediaRepairReport) error {
	attachments, err := sqlite.NewMessageAttachmentRepository(store, time.Now)
	if err != nil {
		return err
	}
	repairs := make([]sqlite.AttachmentReferenceRepair, 0, len(report.Repairs))
	for _, repair := range report.Repairs {
		repairs = append(repairs, repair.apply)
	}
	applied, err := attachments.ApplyAttachmentReferenceRepairs(ctx, repairs)
	if err != nil {
		return err
	}
	report.AppliedRepairs = applied
	return nil
}

// wireConversationID is the remote conversation ID a conversation's frames
// carry. A conversation whose binding another thread took over keeps its
// history under sqlite.DisplacedRemoteIDPrefix + "<remote id>:<conversation
// id>"; its frames still carry the remote ID, and planGoogleMediaRepair's
// time window keeps the other thread's frames out.
func wireConversationID(remoteConversationID string) string {
	rest, displaced := strings.CutPrefix(remoteConversationID, sqlite.DisplacedRemoteIDPrefix)
	if !displaced {
		return remoteConversationID
	}
	if index := strings.LastIndex(rest, ":"); index > 0 {
		return rest[:index]
	}
	return remoteConversationID
}

// readGoogleMediaFrames returns, per message, the media its frames named, in
// the order the frames were received. Live and catch-up frames both count:
// any frame that names a message's media names it correctly.
func readGoogleMediaFrames(
	ctx context.Context,
	store *sqlite.Store,
	accountID string,
	report *GoogleMediaRepairReport,
) (map[googleMediaKey][]googleMediaFrame, error) {
	var records []sqlite.InboxRecord
	for _, codec := range []string{GoogleCodec, GoogleHistoryCodec} {
		listed, err := store.ListInboxByCodecSince(ctx, codec, 0)
		if err != nil {
			return nil, err
		}
		for _, record := range listed {
			if record.AccountID == accountID {
				records = append(records, record)
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ReceivedAtMS != records[j].ReceivedAtMS {
			return records[i].ReceivedAtMS < records[j].ReceivedAtMS
		}
		return records[i].InboxID < records[j].InboxID
	})
	frames := map[googleMediaKey][]googleMediaFrame{}
	for _, record := range records {
		report.Frames++
		var envelope googleFrameEnvelope
		if err := json.Unmarshal(record.Payload, &envelope); err != nil || len(envelope.ProtoB64) == 0 {
			report.Malformed++
			continue
		}
		if envelope.Kind != googleFrameMessage {
			continue
		}
		var message gmproto.Message
		if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
			report.Malformed++
			continue
		}
		info := client.ExtractMediaInfo(&message)
		if info == nil || message.GetMessageID() == "" {
			continue
		}
		packed, err := googleAttachments(message.GetMessageID(), info)
		if err != nil || len(packed) != 1 {
			report.Malformed++
			continue
		}
		key := googleMediaKey{conversationID: message.GetConversationID(), messageID: message.GetMessageID()}
		frames[key] = append(frames[key], googleMediaFrame{
			timestampMS: message.GetTimestamp() / 1000,
			info:        info,
			attachment:  packed[0],
		})
	}
	return frames, nil
}

// planGoogleMediaRepair decides what to do with one attachment given its
// message's frames, and returns the repair (nil when nothing changes) with
// its outcome.
func planGoogleMediaRepair(row sqlite.AccountAttachment, all []googleMediaFrame) (*GoogleMediaRepair, string) {
	// Google message IDs are device-local, and a phone swap reuses them for
	// messages days apart; a message's own frames are seconds apart.
	var frames []googleMediaFrame
	for _, frame := range all {
		if delta := frame.timestampMS - row.OccurredAtMS; delta > -googleSameMessageSpan.Milliseconds() && delta < googleSameMessageSpan.Milliseconds() {
			frames = append(frames, frame)
		}
	}
	if len(frames) == 0 {
		return nil, googleMediaNoFrames
	}
	var (
		lastOriginal  *bridge.Attachment
		lastThumbnail *bridge.Attachment
		thumbnails    = map[string]bridge.Attachment{}
		originals     = map[string]bool{}
	)
	for index := range frames {
		attachment, info := frames[index].attachment, frames[index].info
		if info.IsThumbnail {
			thumbnails[attachment.RemoteID] = attachment
		} else if info.ThumbnailMediaID != "" && info.ThumbnailMediaID != info.MediaID {
			// Named beside an original, so this frame packed the original.
			if _, ok := thumbnails[info.ThumbnailMediaID]; !ok {
				thumbnails[info.ThumbnailMediaID] = bridge.Attachment{}
			}
		}
		switch {
		case attachment.RemoteID == "":
		case attachment.Preview:
			lastThumbnail = &frames[index].attachment
		default:
			originals[attachment.RemoteID] = true
			lastOriginal = &frames[index].attachment
		}
	}
	target := lastOriginal
	if target == nil {
		target = lastThumbnail
	}
	repair := &GoogleMediaRepair{
		MessageID: row.MessageID, RemoteMessageID: row.RemoteMessageID, Ordinal: row.Ordinal,
		State: row.State, FromRemoteID: row.RemoteID,
	}
	expect := sqlite.AttachmentReferenceRepair{
		MessageID: row.MessageID, Ordinal: row.Ordinal,
		ExpectRemoteID: row.RemoteID, ExpectRemoteRef: row.RemoteRef, ExpectBlobHash: row.BlobHash,
	}

	if row.State != "downloaded" {
		if target == nil {
			if row.RemoteID == "" {
				return nil, googleMediaNeverNamed
			}
			return nil, googleMediaUnchanged
		}
		if _, knownThumbnail := thumbnails[row.RemoteID]; target.Preview && row.RemoteID != "" && !knownThumbnail {
			// The row names media no frame here calls a thumbnail; a
			// thumbnail does not replace it.
			return nil, googleMediaKeptOriginal
		}
		if row.RemoteID == target.RemoteID && bytes.Equal(row.RemoteRef, target.RemoteRef) && row.RemotePreview == target.Preview {
			return nil, googleMediaUnchanged
		}
		outcome := googleMediaPendingPreview
		switch {
		case row.RemoteID == "":
			outcome = googleMediaRestored
		case !target.Preview:
			outcome = googleMediaPendingOriginal
		}
		size := target.Size
		expect.RemoteID, expect.RemoteRef, expect.RemotePreview = target.RemoteID, target.RemoteRef, target.Preview
		expect.Filename, expect.MIME, expect.SizeBytes = target.Filename, target.MIME, &size
		repair.Outcome, repair.ToRemoteID, repair.ToPreview, repair.apply = outcome, target.RemoteID, target.Preview, expect
		return repair, outcome
	}

	// Downloaded: the bytes are whatever the stored reference named.
	thumbnail, isThumbnail := thumbnails[row.RemoteID]
	if !isThumbnail || originals[row.RemoteID] {
		if originals[row.RemoteID] {
			return nil, googleMediaUnchanged
		}
		return nil, googleMediaUnknownBytes
	}
	reference := thumbnail
	if lastOriginal != nil {
		reference = *lastOriginal
	}
	if row.BlobPreview && row.RemoteID == reference.RemoteID && bytes.Equal(row.RemoteRef, reference.RemoteRef) && row.RemotePreview == reference.Preview {
		return nil, googleMediaUnchanged
	}
	expect.RemoteID, expect.RemoteRef, expect.RemotePreview, expect.BlobPreview = reference.RemoteID, reference.RemoteRef, reference.Preview, true
	repair.Outcome, repair.ToRemoteID, repair.ToPreview, repair.BlobPreview, repair.apply =
		googleMediaHeldPreview, reference.RemoteID, reference.Preview, true, expect
	return repair, googleMediaHeldPreview
}

// String summarizes a repair for the CLI.
func (r GoogleMediaRepair) String() string {
	to := r.ToRemoteID
	if r.ToPreview {
		to += " (thumbnail)"
	}
	return fmt.Sprintf("%-20s message %s (%s) %s: %q -> %q", r.Outcome, r.RemoteMessageID, r.MessageID, r.State, r.FromRemoteID, to)
}
