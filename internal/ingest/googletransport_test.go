package ingest_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
)

// Google Messages protobuf type values (gmproto Message field 11).
const (
	googleTypeUnset int64 = 0
	googleTypeSMS   int64 = 1
	googleTypeMMS   int64 = 2
	googleTypeMMSDL int64 = 3 // MMS not yet downloaded
	googleTypeRCS   int64 = 4
)

var transportBase = time.Date(2026, 10, 3, 20, 16, 47, 0, time.UTC)

type googleMessageSpec struct {
	conversationID string
	messageID      string
	messageType    int64
	status         gmproto.MessageStatusType
	sentAt         time.Time
	fromMe         bool
	isOld          bool
}

func googleMessageFrame(t *testing.T, receivedAt time.Time, spec googleMessageSpec) ingest.GoogleInboxFrame {
	t.Helper()
	message := &gmproto.Message{
		MessageID:      spec.messageID,
		ConversationID: spec.conversationID,
		Type:           spec.messageType,
		MessageStatus:  &gmproto.MessageStatus{Status: spec.status},
		MessageInfo:    textInfo("body " + spec.messageID),
	}
	if !spec.sentAt.IsZero() {
		message.Timestamp = spec.sentAt.UnixMicro()
	}
	if spec.fromMe {
		message.SenderParticipant = &gmproto.Participant{IsMe: true}
	}
	payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: message, IsOld: spec.isOld})
	if err != nil {
		t.Fatalf("MarshalGoogleMessageFrame: %v", err)
	}
	return ingest.GoogleInboxFrame{ReceivedAt: receivedAt, Payload: payload}
}

func googleConversationFrame(
	t *testing.T,
	receivedAt time.Time,
	conversationID string,
	conversationType gmproto.ConversationType,
) ingest.GoogleInboxFrame {
	t.Helper()
	payload, _, err := ingest.MarshalGoogleConversationFrame(&gmproto.Conversation{
		ConversationID: conversationID,
		Name:           "Thread " + conversationID,
		Type:           conversationType,
	})
	if err != nil {
		t.Fatalf("MarshalGoogleConversationFrame: %v", err)
	}
	return ingest.GoogleInboxFrame{ReceivedAt: receivedAt, Payload: payload}
}

func incoming(conversationID, messageID string, messageType int64, at time.Time) googleMessageSpec {
	return googleMessageSpec{
		conversationID: conversationID,
		messageID:      messageID,
		messageType:    messageType,
		status:         gmproto.MessageStatusType_INCOMING_COMPLETE,
		sentAt:         at,
	}
}

// sameEvents compares events by instant and transport, ignoring the zone a
// time was built in.
func sameEvents(got, want []freshness.TransportEvent) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].At.Equal(want[i].At) || got[i].Transport != want[i].Transport {
			return false
		}
	}
	return true
}

func TestGoogleMessageTransportMapsProtobufField11(t *testing.T) {
	t.Parallel()
	cases := map[int64]freshness.Transport{
		googleTypeUnset: freshness.TransportUnknown,
		googleTypeSMS:   freshness.TransportSMS,
		googleTypeMMS:   freshness.TransportSMS,
		googleTypeMMSDL: freshness.TransportSMS,
		googleTypeRCS:   freshness.TransportRCS,
		5:               freshness.TransportUnknown,
		-1:              freshness.TransportUnknown,
	}
	for messageType, want := range cases {
		if got := ingest.GoogleMessageTransport(messageType); got != want {
			t.Errorf("GoogleMessageTransport(%d) = %s, want %s", messageType, got, want)
		}
	}
	conversationCases := map[gmproto.ConversationType]freshness.Transport{
		gmproto.ConversationType_UNKNOWN_CONVERSATION_TYPE: freshness.TransportUnknown,
		gmproto.ConversationType_SMS:                       freshness.TransportSMS,
		gmproto.ConversationType_RCS:                       freshness.TransportRCS,
	}
	for conversationType, want := range conversationCases {
		if got := ingest.GoogleConversationTransport(conversationType); got != want {
			t.Errorf("GoogleConversationTransport(%s) = %s, want %s", conversationType, got, want)
		}
	}
}

func TestGoogleIncomingTransportsPrefersTheMessageTypeOverTheConversationType(t *testing.T) {
	t.Parallel()
	at := transportBase
	frames := []ingest.GoogleInboxFrame{
		// A new one-to-one chat: the message is RCS (type 4) and the first
		// conversation frame still says SMS, as on 2026-10-05 08:22.
		googleMessageFrame(t, at, incoming("new-chat", "m1", googleTypeRCS, at)),
		googleConversationFrame(t, at.Add(2*time.Second), "new-chat", gmproto.ConversationType_SMS),
		googleConversationFrame(t, at.Add(38*time.Second), "new-chat", gmproto.ConversationType_RCS),
		// An RCS chat whose message fell back to SMS (type 1).
		googleConversationFrame(t, at.Add(time.Minute), "rcs-chat", gmproto.ConversationType_RCS),
		googleMessageFrame(t, at.Add(2*time.Minute), incoming("rcs-chat", "m2", googleTypeSMS, at.Add(2*time.Minute))),
		// An MMS in an SMS group.
		googleConversationFrame(t, at.Add(3*time.Minute), "mms-group", gmproto.ConversationType_SMS),
		googleMessageFrame(t, at.Add(4*time.Minute), incoming("mms-group", "m3", googleTypeMMSDL, at.Add(4*time.Minute))),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	want := []freshness.TransportEvent{
		{At: at, Transport: freshness.TransportRCS},
		{At: at.Add(2 * time.Minute), Transport: freshness.TransportSMS},
		{At: at.Add(4 * time.Minute), Transport: freshness.TransportSMS},
	}
	if !sameEvents(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	if stats.Incoming != 3 || stats.SMS != 2 || stats.RCS != 1 || stats.Unknown != 0 || stats.Conversations != 4 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGoogleIncomingTransportsFallsBackToTheConversationTypeInForce(t *testing.T) {
	t.Parallel()
	at := transportBase
	untyped := func(messageID string, receivedAt time.Time) ingest.GoogleInboxFrame {
		return googleMessageFrame(t, receivedAt, incoming("chat", messageID, googleTypeUnset, receivedAt))
	}
	frames := []ingest.GoogleInboxFrame{
		untyped("before-any-conversation-frame", at),
		googleConversationFrame(t, at.Add(time.Minute), "chat", gmproto.ConversationType_SMS),
		untyped("while-sms", at.Add(2*time.Minute)),
		googleConversationFrame(t, at.Add(3*time.Minute), "chat", gmproto.ConversationType_RCS),
		untyped("while-rcs", at.Add(4*time.Minute)),
		googleMessageFrame(t, at.Add(5*time.Minute), incoming("no-conversation-frames", "orphan", googleTypeUnset, at.Add(5*time.Minute))),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	want := []freshness.TransportEvent{
		{At: at, Transport: freshness.TransportUnknown}, // no conversation frame yet
		{At: at.Add(2 * time.Minute), Transport: freshness.TransportSMS},
		{At: at.Add(4 * time.Minute), Transport: freshness.TransportRCS},
		{At: at.Add(5 * time.Minute), Transport: freshness.TransportUnknown},
	}
	if !sameEvents(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	if stats.SMS != 1 || stats.RCS != 1 || stats.Unknown != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGoogleIncomingTransportsBreaksReceiptTiesByFrameNotInputOrder(t *testing.T) {
	t.Parallel()
	at := transportBase
	sms := googleConversationFrame(t, at, "chat", gmproto.ConversationType_SMS)
	rcs := googleConversationFrame(t, at, "chat", gmproto.ConversationType_RCS)
	message := googleMessageFrame(t, at.Add(time.Minute), incoming("chat", "untyped", googleTypeUnset, at.Add(time.Minute)))
	first, _ := ingest.GoogleIncomingTransports([]ingest.GoogleInboxFrame{sms, rcs, message})
	second, _ := ingest.GoogleIncomingTransports([]ingest.GoogleInboxFrame{rcs, sms, message})
	if len(first) != 1 || !sameEvents(first, second) {
		t.Fatalf("tied conversation frames: %+v vs %+v, want one event independent of input order", first, second)
	}
}

// Google IDs are device-local, so a phone swap reuses them. A new SMS that
// reuses an old message's IDs is a new delivery, not a re-relay of the old
// one, and must not be hidden behind the old message's time.
func TestGoogleIncomingTransportsSeparatesReusedIDsByTime(t *testing.T) {
	t.Parallel()
	old := transportBase.Add(-30 * 24 * time.Hour)
	fresh := transportBase
	frames := []ingest.GoogleInboxFrame{
		googleMessageFrame(t, old, incoming("1969", "88017", googleTypeSMS, old)),
		googleMessageFrame(t, fresh, incoming("1969", "88017", googleTypeSMS, fresh)),
		// The same new message re-relayed 59 minutes later is still one.
		googleMessageFrame(t, fresh.Add(59*time.Minute), incoming("1969", "88017", googleTypeSMS, fresh.Add(59*time.Minute))),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	want := []freshness.TransportEvent{
		{At: old, Transport: freshness.TransportSMS},
		{At: fresh, Transport: freshness.TransportSMS},
	}
	if !sameEvents(events, want) || stats.Incoming != 2 {
		t.Fatalf("events = %+v stats = %+v, want the old and the new message", events, stats)
	}
}

// A contentless stub (the empty message group activity leaks into a
// one-to-one thread) is not a delivery; the decoder never projects it either.
func TestGoogleIncomingTransportsSkipsContentlessStubs(t *testing.T) {
	t.Parallel()
	at := transportBase
	payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: &gmproto.Message{
		MessageID: "stub", ConversationID: "chat", Type: googleTypeSMS, Timestamp: at.UnixMicro(),
		MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
	}})
	if err != nil {
		t.Fatal(err)
	}
	events, stats := ingest.GoogleIncomingTransports([]ingest.GoogleInboxFrame{{ReceivedAt: at, Payload: payload}})
	if len(events) != 0 || stats.Stubs != 1 {
		t.Fatalf("events = %+v stats = %+v, want the stub skipped", events, stats)
	}
}

// Frames sharing IDs merge only while all their times fit inside an hour;
// the merged time cannot creep forward frame by frame, so the result does not
// depend on which frame arrived first.
func TestGoogleIncomingTransportsBoundsTheMergedSpan(t *testing.T) {
	t.Parallel()
	at := transportBase
	frame := func(offset time.Duration) ingest.GoogleInboxFrame {
		return googleMessageFrame(t, at.Add(3*time.Hour), incoming("chat", "reused", googleTypeSMS, at.Add(offset)))
	}
	ascending := []ingest.GoogleInboxFrame{frame(0), frame(50 * time.Minute), frame(100 * time.Minute)}
	descending := []ingest.GoogleInboxFrame{frame(100 * time.Minute), frame(50 * time.Minute), frame(0)}
	first, _ := ingest.GoogleIncomingTransports(ascending)
	second, _ := ingest.GoogleIncomingTransports(descending)
	if len(first) != 2 || !sameEvents(first, second) {
		t.Fatalf("ascending %+v, descending %+v, want the same two messages", first, second)
	}
}

// An MMS notification is carrier evidence whatever its download state, even
// with no content yet.
func TestGoogleIncomingTransportsCountsMMSNotificationsWithoutContent(t *testing.T) {
	t.Parallel()
	at := transportBase
	var frames []ingest.GoogleInboxFrame
	for i, status := range []gmproto.MessageStatusType{
		gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED_SIM_HAS_NO_DATA,
		gmproto.MessageStatusType_INCOMING_EXPIRED_OR_NOT_AVAILABLE,
	} {
		payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: &gmproto.Message{
			MessageID: fmt.Sprintf("mms-%d", i), ConversationID: "chat", Type: googleTypeMMSDL,
			Timestamp: at.UnixMicro(), MessageStatus: &gmproto.MessageStatus{Status: status},
		}})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, ingest.GoogleInboxFrame{ReceivedAt: at, Payload: payload})
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	if len(events) != 4 || stats.SMS != 4 || stats.Stubs != 0 {
		t.Fatalf("events = %+v stats = %+v, want four carrier deliveries", events, stats)
	}
}

func TestGoogleIncomingTransportsCountsOnlyIncomingDeliveries(t *testing.T) {
	t.Parallel()
	at := transportBase
	spec := func(messageID string, status gmproto.MessageStatusType) googleMessageSpec {
		s := incoming("chat", messageID, googleTypeSMS, at)
		s.status = status
		return s
	}
	fromMe := spec("from-me-incoming-status", gmproto.MessageStatusType_INCOMING_COMPLETE)
	fromMe.fromMe = true
	frames := []ingest.GoogleInboxFrame{
		googleMessageFrame(t, at, spec("outgoing-complete", gmproto.MessageStatusType_OUTGOING_COMPLETE)),
		googleMessageFrame(t, at, spec("outgoing-delivered", gmproto.MessageStatusType_OUTGOING_DELIVERED)),
		googleMessageFrame(t, at, spec("outgoing-failed", gmproto.MessageStatusType_OUTGOING_FAILED_GENERIC)),
		googleMessageFrame(t, at, spec("tombstone", gmproto.MessageStatusType_TOMBSTONE_ONE_ON_ONE_SMS_CREATED)),
		googleMessageFrame(t, at, spec("unknown-status", gmproto.MessageStatusType_STATUS_UNKNOWN)),
		googleMessageFrame(t, at, fromMe),
		googleMessageFrame(t, at, spec("incoming-downloading", gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING)),
		googleMessageFrame(t, at, spec("incoming-displayed", gmproto.MessageStatusType_INCOMING_DISPLAYED)),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	if len(events) != 2 || stats.Incoming != 2 || stats.Messages != len(frames) {
		t.Fatalf("events = %+v stats = %+v, want only the two INCOMING_* deliveries", events, stats)
	}
}

func TestGoogleIncomingTransportsTimesEachMessageOnceAtItsOwnTimestamp(t *testing.T) {
	t.Parallel()
	at := transportBase
	monthsOld := at.Add(-200 * 24 * time.Hour)
	future := incoming("chat", "fast-clock", googleTypeSMS, at.Add(time.Hour))
	untimed := incoming("chat", "untimed", googleTypeSMS, time.Time{})
	replay := incoming("chat", "replayed", googleTypeSMS, at.Add(-3*time.Hour))
	replay.isOld = true
	downloading := incoming("chat", "mms", googleTypeMMSDL, at)
	downloading.status = gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING

	frames := []ingest.GoogleInboxFrame{
		// Google re-sends a months-old SMS when a thread refreshes: it must
		// count at its own time, not as current delivery.
		googleMessageFrame(t, at, incoming("chat", "old", googleTypeSMS, monthsOld)),
		googleMessageFrame(t, at, future),
		// With no timestamp there is no telling when it arrived: skipped.
		googleMessageFrame(t, at, untimed),
		// A reconnect replay is a real delivery.
		googleMessageFrame(t, at, replay),
		// One MMS relayed twice as its status changes: counted once, typed by
		// the first frame that knows its type, at the earliest time.
		googleMessageFrame(t, at.Add(time.Minute), downloading),
		googleMessageFrame(t, at.Add(2*time.Minute), incoming("chat", "mms", googleTypeUnset, at.Add(time.Minute))),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	want := []freshness.TransportEvent{
		{At: monthsOld, Transport: freshness.TransportSMS},
		{At: at.Add(-3 * time.Hour), Transport: freshness.TransportSMS},
		{At: at, Transport: freshness.TransportSMS}, // fast clock, capped at receipt
		{At: at, Transport: freshness.TransportSMS}, // MMS
	}
	if !sameEvents(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	if stats.Incoming != 4 || stats.Messages != 6 || stats.Untimed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGoogleIncomingTransportsSkipsMalformedFrames(t *testing.T) {
	t.Parallel()
	at := transportBase
	frames := []ingest.GoogleInboxFrame{
		{ReceivedAt: at, Payload: []byte("not json")},
		{ReceivedAt: at, Payload: []byte(`{"kind":"message","proto_b64":""}`)},
		{ReceivedAt: at, Payload: []byte(`{"kind":"reaction","proto_b64":"CgE="}`)},
		{ReceivedAt: at, Payload: []byte(`{"kind":"message","proto_b64":"/////w==","is_old":false}`)},
		googleMessageFrame(t, at, incoming("chat", "good", googleTypeRCS, at)),
	}
	events, stats := ingest.GoogleIncomingTransports(frames)
	if len(events) != 1 || events[0].Transport != freshness.TransportRCS {
		t.Fatalf("events = %+v, want the one good RCS message", events)
	}
	if stats.Malformed != 4 || stats.Frames != 5 {
		t.Fatalf("stats = %+v, want 4 malformed of 5", stats)
	}
}

// TestGoogleIncomingTransportsDetectsTheOctoberIMSOutage replays the shape of
// the 2026-10-03 to 10-07 outage through real protobuf frames: two weeks of
// mixed traffic, then RCS only, including the new chat whose first
// conversation frame said SMS. The check must flag once, keyed on the last SMS,
// and the mislabelled conversation must not look like SMS recovering.
func TestGoogleIncomingTransportsDetectsTheOctoberIMSOutage(t *testing.T) {
	t.Parallel()
	lastSMS := time.Date(2026, 10, 3, 20, 16, 47, 0, time.UTC) // 16:16:47 EDT
	var frames []ingest.GoogleInboxFrame
	n := 0
	add := func(conversationID string, conversationType gmproto.ConversationType, messageType int64, at time.Time) {
		n++
		frames = append(frames,
			googleConversationFrame(t, at, conversationID, conversationType),
			googleMessageFrame(t, at.Add(2*time.Second), incoming(conversationID, fmt.Sprintf("m%d", n), messageType, at)),
		)
	}
	for at := lastSMS.Add(-30 * 24 * time.Hour); at.Before(lastSMS); at = at.Add(2 * time.Hour) {
		add("short-code-22000", gmproto.ConversationType_SMS, googleTypeSMS, at)
		add("friend", gmproto.ConversationType_RCS, googleTypeRCS, at.Add(time.Hour))
	}
	add("short-code-22000", gmproto.ConversationType_SMS, googleTypeSMS, lastSMS)
	for at := lastSMS.Add(40 * time.Minute); at.Before(lastSMS.Add(57 * time.Hour)); at = at.Add(70 * time.Minute) {
		add("friend", gmproto.ConversationType_RCS, googleTypeRCS, at)
	}
	// 2026-10-05 08:22 EDT: a new chat, RCS message, conversation frame says SMS.
	newChat := time.Date(2026, 10, 5, 12, 22, 55, 0, time.UTC)
	frames = append(frames,
		googleMessageFrame(t, newChat, incoming("new-chat", "first", googleTypeRCS, newChat)),
		googleConversationFrame(t, newChat.Add(2*time.Second), "new-chat", gmproto.ConversationType_SMS),
		googleConversationFrame(t, newChat.Add(38*time.Second), "new-chat", gmproto.ConversationType_RCS),
	)

	events, _ := ingest.GoogleIncomingTransports(frames)
	cfg := freshness.DefaultSMSPathConfig
	cfg.Location = time.UTC
	episodes := map[int64]int{}
	var firstStall time.Time
	for now := lastSMS; now.Before(lastSMS.Add(57 * time.Hour)); now = now.Add(5 * time.Minute) {
		verdict := freshness.EvaluateSMSPath(events, now, cfg)
		if verdict.Stalled {
			episodes[verdict.LastSMS.UnixMilli()]++
			if firstStall.IsZero() {
				firstStall = now
			}
		}
	}
	if len(episodes) != 1 || episodes[lastSMS.UnixMilli()] == 0 {
		t.Fatalf("episodes = %v, want one keyed on %s", episodes, lastSMS)
	}
	// Twelve arrivals a day make six expected in 12 hours, so the 24-hour
	// floor decides.
	if want := lastSMS.Add(24 * time.Hour); firstStall.Before(want) || firstStall.Sub(want) >= 5*time.Minute {
		t.Fatalf("first stall %s, want the first tick at or after %s", firstStall, want)
	}
	after := freshness.EvaluateSMSPath(events, newChat.Add(time.Minute), cfg)
	if !after.Stalled || !after.LastSMS.Equal(lastSMS) {
		t.Fatalf("after the mislabelled new chat: %+v, want still stalled since %s", after, lastSMS)
	}
}

// TestGoogleIncomingTransportsIgnoresFrameOrderAndDuplicates checks, over
// random frame sets, that shuffling frames or relaying them twice never
// changes the result, and that every distinct incoming message yields exactly
// one event.
func TestGoogleIncomingTransportsIgnoresFrameOrderAndDuplicates(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(20261003, 1616))
	statuses := []gmproto.MessageStatusType{
		gmproto.MessageStatusType_INCOMING_COMPLETE,
		gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_OUTGOING_COMPLETE,
		gmproto.MessageStatusType_TOMBSTONE_PROTOCOL_SWITCH_TO_TEXT,
	}
	conversationTypes := []gmproto.ConversationType{
		gmproto.ConversationType_UNKNOWN_CONVERSATION_TYPE,
		gmproto.ConversationType_SMS,
		gmproto.ConversationType_RCS,
	}
	for round := 0; round < 200; round++ {
		var frames []ingest.GoogleInboxFrame
		incomingKeys := map[string]bool{}
		sentAtByMessage := map[string]time.Time{}
		for i := 0; i < 1+r.IntN(40); i++ {
			// Few distinct receipt times, so many frames tie.
			receivedAt := transportBase.Add(time.Duration(r.IntN(6)) * time.Minute)
			conversationID := fmt.Sprintf("c%d", r.IntN(4))
			if r.IntN(3) == 0 {
				frames = append(frames, googleConversationFrame(t, receivedAt, conversationID, conversationTypes[r.IntN(3)]))
				continue
			}
			// Each message keeps one timestamp across its frames, as real
			// relays do; ID reuse is covered by its own example test.
			messageID := fmt.Sprintf("m%d", r.IntN(12))
			sentAt, ok := sentAtByMessage[conversationID+"/"+messageID]
			if !ok {
				sentAt = transportBase.Add(-time.Duration(r.IntN(120)) * time.Minute)
				sentAtByMessage[conversationID+"/"+messageID] = sentAt
			}
			spec := incoming(conversationID, messageID, int64(r.IntN(5)), sentAt)
			spec.status = statuses[r.IntN(len(statuses))]
			spec.isOld = r.IntN(4) == 0
			if spec.status == gmproto.MessageStatusType_INCOMING_COMPLETE ||
				spec.status == gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING {
				incomingKeys[spec.conversationID+"/"+spec.messageID] = true
			}
			frames = append(frames, googleMessageFrame(t, receivedAt, spec))
		}
		events, stats := ingest.GoogleIncomingTransports(frames)
		if len(events) != len(incomingKeys) || stats.Incoming != len(incomingKeys) {
			t.Fatalf("round %d: %d events, want one per distinct incoming message (%d)", round, len(events), len(incomingKeys))
		}
		if stats.SMS+stats.RCS+stats.Unknown != stats.Incoming {
			t.Fatalf("round %d: stats do not add up: %+v", round, stats)
		}

		shuffled := append([]ingest.GoogleInboxFrame(nil), frames...)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got, _ := ingest.GoogleIncomingTransports(shuffled); !reflect.DeepEqual(got, events) {
			t.Fatalf("round %d: shuffled frames gave %+v, want %+v", round, got, events)
		}
		doubled := append(append([]ingest.GoogleInboxFrame(nil), frames...), frames...)
		if got, _ := ingest.GoogleIncomingTransports(doubled); !reflect.DeepEqual(got, events) {
			t.Fatalf("round %d: duplicated frames gave %+v, want %+v", round, got, events)
		}
	}
}
