package app

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
)

// The empty-stub policy decides which contentless Google messages are dropped.
// Legacy applies it on two paths (the live EventHandler and backfill's
// storeMessage) and v2 applies it in the Google decoder. The tests below run
// all three on the same generated messages and require the same keep/drop
// decision. Tapback-shaped bodies are outside the domain on purpose: legacy
// folds them into a reaction while v2 keeps the message and emits the reaction
// (counted as tapback_messages), an intended difference.

// googleStubParity runs one message through the three implementations.
type googleStubParity struct {
	backfill *App
	live     *client.EventHandler
	decoder  *ingest.GoogleDecoder
	seq      int
}

func newGoogleStubParity(t *testing.T) *googleStubParity {
	t.Helper()
	liveStore, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(live): %v", err)
	}
	t.Cleanup(func() { liveStore.Close() })
	return &googleStubParity{
		backfill: newTestApp(t, &mockGMClient{}),
		live:     &client.EventHandler{Store: liveStore, Logger: zerolog.Nop()},
		decoder:  ingest.NewGoogleDecoder(nil),
	}
}

type googleStubDecisions struct {
	legacyLive, legacyBackfill, v2 bool
}

func (d googleStubDecisions) agree() bool {
	return d.legacyLive == d.legacyBackfill && d.legacyBackfill == d.v2
}

func (p *googleStubParity) decide(t *testing.T, template *gmproto.Message) googleStubDecisions {
	t.Helper()
	p.seq++
	message := proto.Clone(template).(*gmproto.Message)
	message.MessageID = fmt.Sprintf("parity-%d", p.seq)
	message.ConversationID = "parity-conversation"
	message.Timestamp = 1_700_000_000_000_000 + int64(p.seq)

	p.backfill.storeMessage(message)
	p.live.Handle(&libgm.WrappedMessage{Message: proto.Clone(message).(*gmproto.Message)})

	payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: message})
	if err != nil {
		t.Fatalf("MarshalGoogleMessageFrame: %v", err)
	}
	events, err := p.decoder.Decode(context.Background(), bridge.RawIngressRecord{
		AccountID:    "parity-account",
		Codec:        ingest.GoogleCodec,
		CodecVersion: ingest.GoogleCodecVersion,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	v2Keeps := false
	for _, event := range events {
		if event.Kind == bridge.EventMessage {
			v2Keeps = true
		}
	}
	return googleStubDecisions{
		legacyLive:     storedLegacyMessage(t, p.live.Store, message.MessageID),
		legacyBackfill: storedLegacyMessage(t, p.backfill.Store, message.MessageID),
		v2:             v2Keeps,
	}
}

func storedLegacyMessage(t *testing.T, store *db.Store, messageID string) bool {
	t.Helper()
	stored, err := store.GetMessageByID(messageID)
	if err != nil {
		t.Fatalf("GetMessageByID(%q): %v", messageID, err)
	}
	return stored != nil
}

// googleStubStatuses is every MessageStatus a frame can carry: none at all,
// each named enum value, and one value outside the enum.
func googleStubStatuses() []*gmproto.MessageStatus {
	values := make([]int, 0, len(gmproto.MessageStatusType_name))
	for value := range gmproto.MessageStatusType_name {
		values = append(values, int(value))
	}
	sort.Ints(values)
	statuses := []*gmproto.MessageStatus{nil}
	for _, value := range values {
		statuses = append(statuses, &gmproto.MessageStatus{Status: gmproto.MessageStatusType(value)})
	}
	return append(statuses, &gmproto.MessageStatus{Status: gmproto.MessageStatusType(9999)})
}

func googleStubStatusName(status *gmproto.MessageStatus) string {
	if status == nil {
		return "nil"
	}
	return status.GetStatus().String()
}

type googleStubShape struct {
	name        string
	contentless bool
	message     func() *gmproto.Message
}

func googleStubShapes() []googleStubShape {
	actionID := "action-1"
	return []googleStubShape{
		{"no message info", true, func() *gmproto.Message { return &gmproto.Message{} }},
		{"action-only info", true, func() *gmproto.Message {
			// The shape of an MMS download placeholder seen in live frames.
			return &gmproto.Message{MessageInfo: []*gmproto.MessageInfo{{ActionMessageID: &actionID}}}
		}},
		{"empty text", true, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleTextInfo("")}
		}},
		{"whitespace text", true, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleTextInfo(" \t\n ")}
		}},
		{"media without ids", true, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleMediaInfo(&gmproto.MediaContent{MimeType: "image/jpeg"})}
		}},
		{"reaction without emoji", true, func() *gmproto.Message {
			return &gmproto.Message{Reactions: []*gmproto.ReactionEntry{{
				Data:           &gmproto.ReactionData{},
				ParticipantIDs: []string{"participant-1"},
			}}}
		}},
		{"reply target only", true, func() *gmproto.Message {
			return &gmproto.Message{ReplyMessage: &gmproto.ReplyMessage{MessageID: "target-1"}}
		}},
		{"text", false, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleTextInfo("hello")}
		}},
		{"zero-width text", false, func() *gmproto.Message {
			// U+200B is not Unicode White_Space, so TrimSpace keeps it: content.
			return &gmproto.Message{MessageInfo: googleTextInfo("​")}
		}},
		{"media id", false, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleMediaInfo(&gmproto.MediaContent{MediaID: "media-1"})}
		}},
		{"thumbnail id only", false, func() *gmproto.Message {
			return &gmproto.Message{MessageInfo: googleMediaInfo(&gmproto.MediaContent{ThumbnailMediaID: "thumb-1"})}
		}},
		{"reaction", false, func() *gmproto.Message {
			return &gmproto.Message{Reactions: []*gmproto.ReactionEntry{{
				Data:           &gmproto.ReactionData{Unicode: "❤️"},
				ParticipantIDs: []string{"participant-1"},
			}}}
		}},
	}
}

func googleTextInfo(body string) []*gmproto.MessageInfo {
	return []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: body},
		},
	}}
}

func googleMediaInfo(media *gmproto.MediaContent) []*gmproto.MessageInfo {
	return []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: media},
	}}
}

// TestGoogleEmptyStubPolicyLegacyV2Parity enumerates every status a frame can
// carry against every content shape. Invariants:
//   - the legacy live path, legacy backfill path, and v2 decoder make the same
//     keep/drop decision for every input;
//   - a message with content (text, media ID, or emoji reaction) is always kept;
//   - a contentless message with no MessageStatus is dropped, because legacy
//     records it as status "unknown", which is a terminal status.
func TestGoogleEmptyStubPolicyLegacyV2Parity(t *testing.T) {
	parity := newGoogleStubParity(t)
	for _, status := range googleStubStatuses() {
		for _, shape := range googleStubShapes() {
			message := shape.message()
			message.MessageStatus = status
			got := parity.decide(t, message)
			label := fmt.Sprintf("status=%s shape=%q", googleStubStatusName(status), shape.name)
			if !got.agree() {
				t.Errorf("%s: decisions diverge: %+v", label, got)
			}
			if !shape.contentless && !(got.legacyLive && got.legacyBackfill && got.v2) {
				t.Errorf("%s: message with content was dropped: %+v", label, got)
			}
			if shape.contentless && status == nil && (got.legacyLive || got.legacyBackfill || got.v2) {
				t.Errorf("%s: contentless nil-status message was kept: %+v", label, got)
			}
		}
	}
}

// googleStubSample is a randomly generated Google message for the property
// test: random status presence and value (including values outside the enum),
// a body drawn from whitespace and non-whitespace runes, and optional media
// and reaction content.
type googleStubSample struct {
	HasStatus      bool
	Status         int32
	Body           string
	MediaID        bool
	ThumbnailID    bool
	ReactionEmoji  bool
	ReactionNoData bool
}

func (googleStubSample) Generate(r *rand.Rand, _ int) reflect.Value {
	alphabet := []string{" ", "\t", "\n", " ", "　", "​", "a", "Z", "1", "."}
	var body strings.Builder
	for i := r.Intn(4); i > 0; i-- {
		body.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	values := make([]int32, 0, len(gmproto.MessageStatusType_name))
	for value := range gmproto.MessageStatusType_name {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	status := values[r.Intn(len(values))]
	if r.Intn(8) == 0 {
		status = r.Int31n(1 << 16)
	}
	return reflect.ValueOf(googleStubSample{
		HasStatus:      r.Intn(3) != 0,
		Status:         status,
		Body:           body.String(),
		MediaID:        r.Intn(4) == 0,
		ThumbnailID:    r.Intn(6) == 0,
		ReactionEmoji:  r.Intn(5) == 0,
		ReactionNoData: r.Intn(5) == 0,
	})
}

func (s googleStubSample) message() *gmproto.Message {
	message := &gmproto.Message{MessageInfo: googleTextInfo(s.Body)}
	if s.HasStatus {
		message.MessageStatus = &gmproto.MessageStatus{Status: gmproto.MessageStatusType(s.Status)}
	}
	if s.MediaID || s.ThumbnailID {
		media := &gmproto.MediaContent{MimeType: "image/png"}
		if s.MediaID {
			media.MediaID = "media-1"
		}
		if s.ThumbnailID {
			media.ThumbnailMediaID = "thumb-1"
		}
		message.MessageInfo = append(message.MessageInfo, googleMediaInfo(media)...)
	}
	if s.ReactionEmoji {
		message.Reactions = append(message.Reactions, &gmproto.ReactionEntry{
			Data:           &gmproto.ReactionData{Unicode: "👍"},
			ParticipantIDs: []string{"participant-1"},
		})
	}
	if s.ReactionNoData {
		message.Reactions = append(message.Reactions, &gmproto.ReactionEntry{
			ParticipantIDs: []string{"participant-2"},
		})
	}
	return message
}

// TestGoogleEmptyStubPolicyLegacyV2ParityProperty checks the same agreement
// invariant on random messages, and that the shared decision matches the
// policy applied to the legacy status string directly.
func TestGoogleEmptyStubPolicyLegacyV2ParityProperty(t *testing.T) {
	parity := newGoogleStubParity(t)
	property := func(sample googleStubSample) bool {
		message := sample.message()
		got := parity.decide(t, message)
		if !got.agree() {
			t.Logf("sample %+v: decisions diverge: %+v", sample, got)
			return false
		}
		mediaID := ""
		if media := client.ExtractMediaInfo(message); media != nil {
			mediaID = media.MediaID
		}
		reactions := ""
		if len(client.ExtractReactions(message)) > 0 {
			reactions = "present"
		}
		stub := db.IsEmptyStubMessage(&db.Message{
			Body:      client.ExtractMessageBody(message),
			MediaID:   mediaID,
			Reactions: reactions,
			Status:    client.MessageStatusString(message),
		})
		if got.v2 == stub {
			t.Logf("sample %+v: kept=%v but policy stub=%v", sample, got.v2, stub)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261008))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}
