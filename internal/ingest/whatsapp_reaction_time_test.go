package ingest

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// Review of #227: the envelope time has whole-second precision, while this
// account's own reactions are confirmed at their millisecond sender
// timestamp. A removal made on the phone in the same second as a confirmed
// add was therefore dropped as stale. Reactions now carry their own sender
// timestamp, and fall back to the envelope time only without one.
func TestWhatsAppReactionUsesItsOwnSenderTimestamp(t *testing.T) {
	envelope := waBaseMessageEnvelope()
	envelope.MessageID = "reaction-envelope"
	envelope.TimestampMS = (waTimestampMS / 1000) * 1000 // whole seconds, as whatsmeow decodes it
	senderMS := envelope.TimestampMS + 950

	for _, tc := range []struct {
		name string
		ms   *int64
		want int64
	}{
		{name: "sender timestamp present", ms: proto.Int64(senderMS), want: senderMS},
		{name: "no sender timestamp", ms: nil, want: envelope.TimestampMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
				Key:               &waCommon.MessageKey{ID: proto.String("reaction-target")},
				Text:              proto.String(""),
				SenderTimestampMS: tc.ms,
			}}
			events, err := NewWhatsAppDecoder().decodeMessageProto(waWorkerAccountID, envelope, message, false)
			if err != nil || len(events) != 1 || events[0].Reaction == nil {
				t.Fatalf("decode = %+v, %v; want one reaction", events, err)
			}
			if got := events[0].Reaction.OccurredAt; !got.Equal(time.UnixMilli(tc.want)) {
				t.Fatalf("reaction occurred at %d, want %d", got.UnixMilli(), tc.want)
			}
		})
	}
}
