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

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// GoogleInboxFrame is one durable Google inbox row as the SMS-path check
// reads it: the v1 envelope payload and when the adapter received it.
type GoogleInboxFrame struct {
	ReceivedAt time.Time
	Payload    []byte
}

// GoogleTransportStats counts what GoogleIncomingTransports saw, so a reader
// can tell an empty result from an unreadable one.
type GoogleTransportStats struct {
	Frames        int `json:"frames"`
	Malformed     int `json:"malformed"`
	Conversations int `json:"conversation_frames"`
	Messages      int `json:"message_frames"`
	// Untimed and Stubs count incoming frames skipped because they carry no
	// timestamp or no content.
	Untimed int `json:"untimed"`
	Stubs   int `json:"stubs"`
	// Incoming counts distinct incoming messages, after dedupe.
	Incoming int `json:"incoming"`
	SMS      int `json:"sms"`
	RCS      int `json:"rcs"`
	Unknown  int `json:"unknown"`
}

// GoogleMessageTransport maps gmproto.Message.Type (protobuf field 11) to a
// transport: 1 SMS, 2 downloaded MMS, 3 not-yet-downloaded MMS, 4 RCS. SMS and
// MMS both ride the carrier messaging path. Any other value is unknown.
//
// The gmproto source marks 4 as uncertain ("4 = rcs?"); the data agrees. On
// the install this was calibrated against, every incoming message frame from
// 2026-07-19 to 2026-10-07 carried one of these four values, and of the 1,450
// incoming message frames in conversations typed RCS (field 22), 1,399
// carried 4.
func GoogleMessageTransport(messageType int64) freshness.Transport {
	switch messageType {
	case 1, 2, 3:
		return freshness.TransportSMS
	case 4:
		return freshness.TransportRCS
	default:
		return freshness.TransportUnknown
	}
}

// GoogleConversationTransport maps gmproto.Conversation.Type (protobuf field
// 22) to a transport. It describes the conversation, not each message: a new
// one-to-one chat can report SMS for its first seconds and then switch to RCS,
// and an RCS chat can fall back to SMS for a single message. So it only
// labels messages whose own type is missing.
func GoogleConversationTransport(conversationType gmproto.ConversationType) freshness.Transport {
	switch conversationType {
	case gmproto.ConversationType_SMS:
		return freshness.TransportSMS
	case gmproto.ConversationType_RCS:
		return freshness.TransportRCS
	default:
		return freshness.TransportUnknown
	}
}

// googleMessageIsIncoming reports whether a message frame is an inbound
// delivery: not sent by this account, with an INCOMING_* status. Tombstones
// (chat created, protocol switched) and statusless update frames are not
// deliveries.
func googleMessageIsIncoming(message *gmproto.Message) bool {
	if client.MessageIsFromMe(message) {
		return false
	}
	status := message.GetMessageStatus()
	if status == nil {
		return false
	}
	return strings.HasPrefix(status.GetStatus().String(), "INCOMING")
}

type googleConversationTypeAt struct {
	receivedAt time.Time
	transport  freshness.Transport
}

type googleIncomingKey struct {
	conversationID string
	messageID      string
}

// googleSameMessageSpan bounds how far apart two frames with the same IDs may be
// timed and still be the same message. A message's frames differ by seconds
// (an MMS is re-stamped by up to 15 s when it finishes downloading), while
// device-local IDs reused after a phone swap name messages days apart.
const googleSameMessageSpan = time.Hour

type googleIncoming struct {
	key googleIncomingKey
	// at is the earliest time among the message's frames and latest the
	// newest; a frame joins only if the pair stays within
	// googleSameMessageSpan, so merging cannot creep forward frame by frame.
	at         time.Time
	latest     time.Time
	receivedAt time.Time
	// messageTransport comes from the message's own type; conversationID
	// resolves the fallback once every conversation frame has been read.
	messageTransport freshness.Transport
}

// GoogleIncomingTransports turns Google inbox frames into one TransportEvent
// per distinct incoming message, labelled SMS or RCS.
//
// A message is labelled by its own type (field 11) and, only when that is
// missing, by its conversation's type (field 22) as of the newest conversation
// frame received at or before the message (unknown if there is none).
//
// Each event is timed at the message's own timestamp (field 5), capped at its
// receipt so a fast phone clock cannot hide a silence. The timestamp matters:
// Google re-sends months-old messages when a thread refreshes, and timing them
// at receipt would make old SMS look like current delivery. For the same
// reason a frame with no timestamp is skipped, as is a contentless stub (the
// empty message group activity can leak into a one-to-one thread), which the
// decoder never projects either; an MMS notification is never a stub. Frames
// the server replays after a long-poll reconnect (is_old) are real deliveries
// and count the same way. A message relayed in several frames (one per status
// change) counts once, at its earliest time; frames with the same IDs whose
// times would spread over googleSameMessageSpan or more are different
// messages, because Google IDs are device-local and a phone swap reuses them.
// Malformed frames are counted and skipped. The result depends only on which
// frames there are, not their order.
func GoogleIncomingTransports(frames []GoogleInboxFrame) ([]freshness.TransportEvent, GoogleTransportStats) {
	stats := GoogleTransportStats{Frames: len(frames)}
	// Receipt order decides which conversation type was in force; frames
	// received in the same millisecond are ordered by their bytes so the
	// result depends only on which frames there are, not their input order.
	ordered := append([]GoogleInboxFrame(nil), frames...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].ReceivedAt.Equal(ordered[j].ReceivedAt) {
			return ordered[i].ReceivedAt.Before(ordered[j].ReceivedAt)
		}
		return bytes.Compare(ordered[i].Payload, ordered[j].Payload) < 0
	})

	conversationTypes := map[string][]googleConversationTypeAt{}
	incoming := map[googleIncomingKey][]*googleIncoming{}
	for _, frame := range ordered {
		var envelope googleFrameEnvelope
		if err := json.Unmarshal(frame.Payload, &envelope); err != nil || len(envelope.ProtoB64) == 0 {
			stats.Malformed++
			continue
		}
		switch envelope.Kind {
		case googleFrameConversation:
			var conversation gmproto.Conversation
			if err := proto.Unmarshal(envelope.ProtoB64, &conversation); err != nil {
				stats.Malformed++
				continue
			}
			stats.Conversations++
			id := conversation.GetConversationID()
			if id == "" {
				continue
			}
			conversationTypes[id] = append(conversationTypes[id], googleConversationTypeAt{
				receivedAt: frame.ReceivedAt,
				transport:  GoogleConversationTransport(conversation.GetType()),
			})
		case googleFrameMessage:
			var message gmproto.Message
			if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
				stats.Malformed++
				continue
			}
			stats.Messages++
			if !googleMessageIsIncoming(&message) {
				continue
			}
			key := googleIncomingKey{
				conversationID: message.GetConversationID(),
				messageID:      message.GetMessageID(),
			}
			if key.messageID == "" {
				continue
			}
			micros := message.GetTimestamp()
			if micros <= 0 {
				stats.Untimed++
				continue
			}
			if googleMessageIsDeliveryStub(&message) {
				stats.Stubs++
				continue
			}
			at := time.UnixMicro(micros)
			if frame.ReceivedAt.Before(at) {
				at = frame.ReceivedAt
			}
			transport := GoogleMessageTransport(message.GetType())
			var seen *googleIncoming
			for _, candidate := range incoming[key] {
				earliest, latest := candidate.at, candidate.latest
				if at.Before(earliest) {
					earliest = at
				}
				if at.After(latest) {
					latest = at
				}
				if latest.Sub(earliest) < googleSameMessageSpan {
					seen = candidate
					break
				}
			}
			if seen == nil {
				incoming[key] = append(incoming[key], &googleIncoming{
					key: key, at: at, latest: at, receivedAt: frame.ReceivedAt, messageTransport: transport,
				})
				continue
			}
			if at.Before(seen.at) {
				seen.at = at
			}
			if at.After(seen.latest) {
				seen.latest = at
			}
			if seen.messageTransport == freshness.TransportUnknown {
				seen.messageTransport = transport
			}
		default:
			stats.Malformed++
		}
	}

	events := make([]freshness.TransportEvent, 0, len(incoming))
	for _, messages := range incoming {
		for _, message := range messages {
			events = append(events, freshness.TransportEvent{
				At:        message.at,
				Transport: message.transport(conversationTypes),
			})
		}
	}
	for _, event := range events {
		switch event.Transport {
		case freshness.TransportSMS:
			stats.SMS++
		case freshness.TransportRCS:
			stats.RCS++
		default:
			stats.Unknown++
		}
	}
	stats.Incoming = len(events)
	sort.Slice(events, func(i, j int) bool {
		if !events[i].At.Equal(events[j].At) {
			return events[i].At.Before(events[j].At)
		}
		return events[i].Transport < events[j].Transport
	})
	return events, stats
}

// transport labels a message by its own type, else by its conversation's type
// in force when it was received.
func (m *googleIncoming) transport(conversationTypes map[string][]googleConversationTypeAt) freshness.Transport {
	if m.messageTransport != freshness.TransportUnknown {
		return m.messageTransport
	}
	return conversationTransportAt(conversationTypes[m.key.conversationID], m.receivedAt)
}

// conversationTransportAt returns the conversation type in force when a
// message was received: the newest frame at or before receivedAt. With none,
// the message stays unknown rather than borrowing a later frame: a new chat's
// first conversation frame can say SMS for seconds before it switches to RCS.
// history is in receipt order.
func conversationTransportAt(history []googleConversationTypeAt, receivedAt time.Time) freshness.Transport {
	chosen := freshness.TransportUnknown
	for _, entry := range history {
		if entry.receivedAt.After(receivedAt) {
			break
		}
		chosen = entry.transport
	}
	return chosen
}

// googleMessageIsDeliveryStub reports a contentless frame that is not evidence
// of a delivery, using the decoder's own stub rule. An MMS notification is
// exempt: whether or not its download succeeded or was started, its arrival
// shows the carrier path delivered.
func googleMessageIsDeliveryStub(message *gmproto.Message) bool {
	switch message.GetType() {
	case 2, 3:
		return false
	}
	status := message.GetMessageStatus().GetStatus().String()
	if strings.Contains(status, "DOWNLOAD") || strings.Contains(status, "EXPIRED") {
		return false
	}
	return googleMessageIsEmptyStub(
		message,
		client.ExtractMessageBody(message),
		client.ExtractMediaInfo(message),
		client.ExtractReactions(message),
	)
}

// GoogleInboxReader is the slice of the v2 store the SMS-path check reads.
type GoogleInboxReader interface {
	ListInboxByCodecSince(ctx context.Context, codec string, sinceMS int64) ([]sqlite.InboxRecord, error)
}

// GoogleIncomingTransportsSince classifies the Google inbox frames received at
// or after since. It reads the inbox, not the projected messages, because
// only the raw protobuf still says whether a message travelled as SMS or RCS.
// Frames of envelope versions this decoder does not understand are skipped.
func GoogleIncomingTransportsSince(
	ctx context.Context,
	store GoogleInboxReader,
	since time.Time,
) ([]freshness.TransportEvent, GoogleTransportStats, error) {
	if store == nil {
		return nil, GoogleTransportStats{}, fmt.Errorf("load Google transports: store is nil")
	}
	records, err := store.ListInboxByCodecSince(ctx, GoogleCodec, since.UnixMilli())
	if err != nil {
		return nil, GoogleTransportStats{}, fmt.Errorf("load Google transports: %w", err)
	}
	frames := make([]GoogleInboxFrame, 0, len(records))
	for _, record := range records {
		if record.CodecVersion != int64(GoogleCodecVersion) {
			continue
		}
		frames = append(frames, GoogleInboxFrame{
			ReceivedAt: time.UnixMilli(record.ReceivedAtMS),
			Payload:    record.Payload,
		})
	}
	events, stats := GoogleIncomingTransports(frames)
	return events, stats, nil
}

// GoogleSMSPathLoader reads the SMS-path check's history from the v2 inbox,
// for freshness.NewSMSPathMonitor.
func GoogleSMSPathLoader(store GoogleInboxReader) freshness.SMSPathLoader {
	return func(ctx context.Context, since time.Time) (freshness.SMSPathLoad, error) {
		events, stats, err := GoogleIncomingTransportsSince(ctx, store, since)
		if err != nil {
			return freshness.SMSPathLoad{}, err
		}
		return freshness.SMSPathLoad{
			Events:    events,
			Frames:    stats.Frames,
			Malformed: stats.Malformed,
			Unknown:   stats.Unknown,
		}, nil
	}
}
