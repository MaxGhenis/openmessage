package ingest

// Signal Desktop history codec tests. Every address, name and body here is
// synthetic.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	sdhAccountID = "signal-primary"
	sdhSelf      = "+15555550100"
	sdhPeerE164  = "+15555550101"
	sdhPeerACI   = "11111111-2222-4333-8444-555555555555"
	sdhOtherACI  = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	sdhGroupID   = "c3ludGhldGljLWdyb3VwLWlkLWZvci10ZXN0cy0wMQ=="
	sdhBaseMS    = int64(1_790_000_000_000)
)

var sdhReceivedAt = time.UnixMilli(sdhBaseMS + 3_600_000)

func sdhRecord(t *testing.T, snapshot SignalDesktopHistorySnapshot) bridge.RawIngressRecord {
	t.Helper()
	if snapshot.AccountAddress == "" {
		snapshot.AccountAddress = sdhSelf
	}
	record, err := SignalDesktopHistoryRecord(sdhAccountID, 0, snapshot, sdhReceivedAt)
	if err != nil {
		t.Fatalf("SignalDesktopHistoryRecord: %v", err)
	}
	return record
}

func sdhDecode(t *testing.T, record bridge.RawIngressRecord) []bridge.Event {
	t.Helper()
	events, err := NewSignalDesktopHistoryDecoder().Decode(context.Background(), record)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return events
}

func sdhDirect(peer string, messages ...SignalDesktopHistoryMessage) SignalDesktopHistorySnapshot {
	return SignalDesktopHistorySnapshot{
		RemoteConversationID: "signal:" + peer,
		Participants:         []SignalDesktopHistoryParticipant{{Address: peer, Name: "Synthetic Peer"}},
		Messages:             messages,
	}
}

func sdhGroup(messages ...SignalDesktopHistoryMessage) SignalDesktopHistorySnapshot {
	return SignalDesktopHistorySnapshot{
		RemoteConversationID: "signal-group:" + sdhGroupID,
		Title:                "Synthetic Group",
		Participants: []SignalDesktopHistoryParticipant{
			{Address: sdhPeerE164, Name: "Synthetic Peer"},
			{Address: sdhOtherACI, Name: "Synthetic Other"},
		},
		Messages: messages,
	}
}

func TestSignalDesktopHistoryCodecIDsAndBodiesFollowLiveRules(t *testing.T) {
	direct := sdhDirect(sdhPeerE164,
		SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, AuthorName: "Synthetic Peer", SentAtMS: sdhBaseMS + 1, Body: "  synthetic hello  "},
		SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 2, Body: "synthetic reply"},
		SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 3, Attachments: []SignalDesktopHistoryAttachment{{ContentType: "image/jpeg", FileName: "synthetic.jpg"}}},
		SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 4, Sticker: true},
	)
	events := sdhDecode(t, sdhRecord(t, direct))
	if len(events) != 5 || events[0].Kind != bridge.EventConversation {
		t.Fatalf("events = %+v, want a snapshot and four messages", events)
	}
	conversation := events[0].Conversation
	if conversation.RemoteConversationID != "signal:"+sdhPeerE164 || conversation.Kind != "direct" ||
		len(conversation.Participants) != 1 || conversation.Participants[0].Identity.Raw != sdhPeerE164 {
		t.Fatalf("conversation = %+v", conversation)
	}
	conversationID := "signal:" + sdhPeerE164
	want := []struct {
		remoteID  string
		direction string
		body      string
	}{
		{v2keys.SignalIncomingSourceID(conversationID, sdhPeerE164, sdhBaseMS+1), "incoming", "synthetic hello"},
		{strconv.FormatInt(sdhBaseMS+2, 10), "outgoing", "synthetic reply"},
		{v2keys.SignalIncomingSourceID(conversationID, sdhPeerE164, sdhBaseMS+3), "incoming", "[Photo]"},
		{v2keys.SignalIncomingSourceID(conversationID, sdhPeerE164, sdhBaseMS+4), "incoming", "[Sticker]"},
	}
	for index, expected := range want {
		message := events[index+1].Message
		if message == nil || message.RemoteConversationID != conversationID || message.RemoteMessageID != expected.remoteID ||
			message.Direction != expected.direction || message.Body != expected.body ||
			!message.OccurredAt.Equal(time.UnixMilli(sdhBaseMS+int64(index)+1)) || len(message.Attachments) != 0 {
			t.Fatalf("message %d = %+v, want %+v", index, message, expected)
		}
	}
	if events[2].Message.Sender.IsSelf != true || events[1].Message.Sender.Raw != sdhPeerE164 || events[1].Message.Sender.Name != "Synthetic Peer" {
		t.Fatalf("senders = %+v / %+v", events[1].Message.Sender, events[2].Message.Sender)
	}

	group := sdhGroup(
		SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 10, Body: "synthetic group hello"},
		SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 11, Body: "synthetic group reply"},
	)
	events = sdhDecode(t, sdhRecord(t, group))
	groupID := "signal-group:" + sdhGroupID
	if events[0].Conversation.Kind != "group" || events[0].Conversation.Title != "Synthetic Group" || len(events[0].Conversation.Participants) != 2 {
		t.Fatalf("group snapshot = %+v", events[0].Conversation)
	}
	if got := events[1].Message.RemoteMessageID; got != v2keys.SignalIncomingSourceID(groupID, sdhOtherACI, sdhBaseMS+10) {
		t.Fatalf("group incoming remote ID = %q", got)
	}
	if got := events[2].Message.RemoteMessageID; got != strconv.FormatInt(sdhBaseMS+11, 10) || events[2].Message.RemoteConversationID != groupID {
		t.Fatalf("group outgoing = %+v", events[2].Message)
	}
}

func TestSignalDesktopHistoryCodecReactionsTargetTheirMessage(t *testing.T) {
	snapshot := sdhGroup(
		SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 10, Body: "synthetic group hello", Reactions: []SignalDesktopHistoryReaction{
			{ActorAddress: sdhPeerE164, Emoji: "👍", TimestampMS: sdhBaseMS + 20},
			{IsSelf: true, Emoji: "❤️", TimestampMS: sdhBaseMS + 21},
		}},
		SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 11, Body: "synthetic mine", Reactions: []SignalDesktopHistoryReaction{
			{ActorAddress: sdhOtherACI, Emoji: "😂"},
		}},
	)
	events := sdhDecode(t, sdhRecord(t, snapshot))
	groupID := "signal-group:" + sdhGroupID
	var reactions []*bridge.ReactionEvent
	for _, event := range events {
		if event.Kind == bridge.EventReaction {
			reactions = append(reactions, event.Reaction)
		}
	}
	if len(reactions) != 3 {
		t.Fatalf("reactions = %d, want 3", len(reactions))
	}
	incomingTarget := v2keys.SignalIncomingSourceID(groupID, sdhOtherACI, sdhBaseMS+10)
	if !reactions[0].Actor.IsSelf || reactions[0].Emoji != "❤️" || reactions[0].TargetRemoteMessageID != incomingTarget {
		t.Fatalf("self reaction = %+v", reactions[0])
	}
	if reactions[1].Actor.Raw != sdhPeerE164 || reactions[1].TargetRemoteMessageID != incomingTarget || reactions[1].Action != bridge.ReactionAdd {
		t.Fatalf("peer reaction = %+v", reactions[1])
	}
	if reactions[2].TargetRemoteMessageID != strconv.FormatInt(sdhBaseMS+11, 10) || !reactions[2].OccurredAt.Equal(time.UnixMilli(sdhBaseMS+11)) {
		t.Fatalf("reaction on own message = %+v", reactions[2])
	}
}

func TestSignalDesktopHistoryRecordRejectsWhatLiveWouldNotKey(t *testing.T) {
	valid := sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS, Body: "synthetic"})
	cases := map[string]func(*SignalDesktopHistorySnapshot){
		"account not E.164":       func(s *SignalDesktopHistorySnapshot) { s.AccountAddress = sdhPeerACI },
		"conversation PNI":        func(s *SignalDesktopHistorySnapshot) { s.RemoteConversationID = "signal:PNI:" + sdhPeerACI },
		"conversation bad prefix": func(s *SignalDesktopHistorySnapshot) { s.RemoteConversationID = "whatsapp:" + sdhPeerE164 },
		"uppercase ACI": func(s *SignalDesktopHistorySnapshot) {
			s.RemoteConversationID = "signal:" + strings.ToUpper(sdhPeerACI)
		},
		"group ID not base64": func(s *SignalDesktopHistorySnapshot) { s.RemoteConversationID = "signal-group:not base64!" },
		"author not peer":     func(s *SignalDesktopHistorySnapshot) { s.Messages[0].AuthorAddress = sdhOtherACI },
		"author missing":      func(s *SignalDesktopHistorySnapshot) { s.Messages[0].AuthorAddress = "" },
		"no sent time":        func(s *SignalDesktopHistorySnapshot) { s.Messages[0].SentAtMS = 0 },
		"participant PNI":     func(s *SignalDesktopHistorySnapshot) { s.Participants[0].Address = "PNI:" + sdhPeerACI },
		"reaction not peer": func(s *SignalDesktopHistorySnapshot) {
			s.Messages[0].Reactions = []SignalDesktopHistoryReaction{{ActorAddress: sdhOtherACI, Emoji: "👍"}}
		},
		"reaction no emoji": func(s *SignalDesktopHistorySnapshot) {
			s.Messages[0].Reactions = []SignalDesktopHistoryReaction{{IsSelf: true}}
		},
		"over 100 messages": func(s *SignalDesktopHistorySnapshot) {
			for index := 0; index < 100; index++ {
				s.Messages = append(s.Messages, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + int64(index) + 1, Body: "synthetic"})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := valid
			snapshot.AccountAddress = sdhSelf
			snapshot.Participants = append([]SignalDesktopHistoryParticipant(nil), valid.Participants...)
			snapshot.Messages = append([]SignalDesktopHistoryMessage(nil), valid.Messages...)
			mutate(&snapshot)
			if _, err := SignalDesktopHistoryRecord(sdhAccountID, 0, snapshot, sdhReceivedAt); err == nil {
				t.Fatalf("SignalDesktopHistoryRecord accepted %s", name)
			}
		})
	}
}

// A frame is data at rest: the decoder re-checks it rather than trusting the
// feeder, so a tampered or mismatched payload is refused at decode time too.
func TestSignalDesktopHistoryDecoderRejectsMismatchedPayloads(t *testing.T) {
	record := sdhRecord(t, sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS, Body: "synthetic"}))
	mutate := func(edit func(map[string]any)) bridge.RawIngressRecord {
		var payload map[string]any
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		edit(payload)
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		changed := record
		changed.Payload = encoded
		return changed
	}
	cases := map[string]bridge.RawIngressRecord{
		"conversation swapped": mutate(func(p map[string]any) { p["conversation_id"] = "signal:" + sdhOtherACI }),
		"kind mismatch":        mutate(func(p map[string]any) { p["kind"] = "group" }),
		"account not E.164":    mutate(func(p map[string]any) { p["account"] = sdhPeerACI }),
		"bad participant":      mutate(func(p map[string]any) { p["participants"] = []any{map[string]any{"address": "not-an-address"}} }),
		"edit line": mutate(func(p map[string]any) {
			p["lines"] = []any{map[string]any{"line": map[string]any{"envelope": map[string]any{
				"source": sdhPeerE164, "timestamp": sdhBaseMS + 5,
				"editMessage": map[string]any{"targetSentTimestamp": sdhBaseMS, "dataMessage": map[string]any{"timestamp": sdhBaseMS + 5, "message": "synthetic edit"}},
			}}}}
		}),
	}
	wrongCodec := record
	wrongCodec.CodecVersion = 2
	cases["codec version"] = wrongCodec
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSignalDesktopHistoryDecoder().Decode(context.Background(), changed); err == nil {
				t.Fatalf("Decode accepted %s", name)
			}
		})
	}
}

func TestSignalDesktopHistoryRecordDedupeKeyIgnoresInputOrderAndReceiptTime(t *testing.T) {
	snapshot := sdhGroup(
		SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 10, Body: "synthetic one", Reactions: []SignalDesktopHistoryReaction{{ActorAddress: sdhPeerE164, Emoji: "👍"}, {IsSelf: true, Emoji: "❤️"}}},
		SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 11, Body: "synthetic two"},
		SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 11, Body: "synthetic three"},
	)
	snapshot.AccountAddress = sdhSelf
	want, err := SignalDesktopHistoryRecord(sdhAccountID, 0, snapshot, sdhReceivedAt)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(91011))
	for trial := 0; trial < 20; trial++ {
		shuffled := snapshot
		shuffled.Messages = append([]SignalDesktopHistoryMessage(nil), snapshot.Messages...)
		shuffled.Participants = append([]SignalDesktopHistoryParticipant(nil), snapshot.Participants...)
		random.Shuffle(len(shuffled.Messages), func(i, j int) {
			shuffled.Messages[i], shuffled.Messages[j] = shuffled.Messages[j], shuffled.Messages[i]
		})
		random.Shuffle(len(shuffled.Participants), func(i, j int) {
			shuffled.Participants[i], shuffled.Participants[j] = shuffled.Participants[j], shuffled.Participants[i]
		})
		for index := range shuffled.Messages {
			reactions := append([]SignalDesktopHistoryReaction(nil), shuffled.Messages[index].Reactions...)
			random.Shuffle(len(reactions), func(i, j int) { reactions[i], reactions[j] = reactions[j], reactions[i] })
			shuffled.Messages[index].Reactions = reactions
		}
		got, err := SignalDesktopHistoryRecord(sdhAccountID, 0, shuffled, sdhReceivedAt.Add(time.Duration(trial)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if got.DedupeKey != want.DedupeKey || string(got.Payload) != string(want.Payload) {
			t.Fatalf("trial %d: payload depends on input order or receipt time", trial)
		}
	}
	if !strings.HasPrefix(want.DedupeKey, "sigdesk:") || strings.HasPrefix(want.DedupeKey, "env:") {
		t.Fatalf("dedupe key %q is not in the history key space", want.DedupeKey)
	}
}

// signal-cli's own receive line for the same message, as the live bridge
// captures it: a phone-number source when signal-cli knows one, the ACI
// otherwise, plus attachment IDs it can fetch.
func sdhLiveLine(account, conversationID string, message SignalDesktopHistoryMessage) ([]byte, string, string) {
	group := strings.HasPrefix(conversationID, "signal-group:")
	data := map[string]any{"timestamp": message.SentAtMS}
	if message.Body != "" {
		data["message"] = message.Body
	}
	if group {
		data["groupInfo"] = map[string]any{"groupId": strings.TrimPrefix(conversationID, "signal-group:"), "type": "DELIVER"}
	}
	var attachments []any
	for index, attachment := range message.Attachments {
		attachments = append(attachments, map[string]any{"contentType": attachment.ContentType, "filename": attachment.FileName, "id": fmt.Sprintf("synthetic-attachment-%d", index)})
	}
	if attachments != nil {
		data["attachments"] = attachments
	}
	for flag, key := range map[*bool]string{&message.Sticker: "sticker", &message.Payment: "payment", &message.Poll: "pollCreate", &message.StoryReply: "storyContext"} {
		if *flag {
			data[key] = map[string]any{"synthetic": true}
		}
	}
	if message.Contact {
		data["contacts"] = []any{map[string]any{"synthetic": true}}
	}
	if message.Preview {
		data["previews"] = []any{map[string]any{"synthetic": true}}
	}
	if message.ViewOnce {
		data["viewOnce"] = true
	}
	envelope := map[string]any{"timestamp": message.SentAtMS}
	if message.Outgoing {
		destination := ""
		if !group {
			destination = strings.TrimPrefix(conversationID, "signal:")
			data["destinationNumber"] = destination
		}
		envelope["sourceNumber"] = account
		envelope["syncMessage"] = map[string]any{"sentMessage": data}
		line, _ := json.Marshal(map[string]any{"account": account, "envelope": envelope})
		return line, "", destination
	}
	if strings.HasPrefix(message.AuthorAddress, "+") {
		envelope["sourceNumber"] = message.AuthorAddress
	} else {
		envelope["sourceUuid"] = message.AuthorAddress
	}
	envelope["sourceName"] = message.AuthorName
	envelope["dataMessage"] = data
	line, _ := json.Marshal(map[string]any{"account": account, "envelope": envelope})
	return line, message.AuthorAddress, ""
}

// Differential: for any message the history codec emits, the live decoder fed
// signal-cli's own line for that message yields the same row identity, sender,
// direction, time and body. Only attachments differ, by design: a live line
// carries signal-cli attachment IDs, a Desktop one cannot.
func TestSignalDesktopHistoryCodecMatchesLiveDecoderProperty(t *testing.T) {
	random := rand.New(rand.NewSource(20261011))
	contentTypes := []string{"image/png", "video/mp4", "audio/aac", "application/pdf"}
	categories := make([]int, 8)
	for trial := 0; trial < 400; trial++ {
		state := random.Uint64()
		category := int((state >> 33) % 8)
		categories[category]++
		group := category&1 != 0
		outgoing := category&2 != 0
		aciAuthor := category&4 != 0
		author := sdhPeerE164
		if aciAuthor {
			author = sdhPeerACI
		}
		snapshot := sdhDirect(author)
		if group {
			snapshot = sdhGroup()
		}
		message := SignalDesktopHistoryMessage{Outgoing: outgoing, SentAtMS: sdhBaseMS + int64(random.Intn(1_000_000)) + 1}
		if !outgoing {
			message.AuthorAddress = author
			message.AuthorName = fmt.Sprintf("Synthetic %d", random.Intn(5))
		}
		if random.Intn(3) != 0 {
			message.Body = fmt.Sprintf("synthetic body %d", random.Intn(1000))
			if random.Intn(4) == 0 {
				message.Body = "  " + message.Body + "\n"
			}
		}
		if random.Intn(3) == 0 {
			message.Attachments = []SignalDesktopHistoryAttachment{{ContentType: contentTypes[random.Intn(len(contentTypes))], FileName: "synthetic.bin"}}
		}
		message.Sticker = random.Intn(5) == 0
		message.Contact = random.Intn(7) == 0
		message.Payment = random.Intn(9) == 0
		message.Poll = random.Intn(9) == 0
		message.Preview = random.Intn(6) == 0
		message.ViewOnce = random.Intn(8) == 0
		message.StoryReply = random.Intn(8) == 0
		snapshot.Messages = []SignalDesktopHistoryMessage{message}

		events := sdhDecode(t, sdhRecord(t, snapshot))
		if len(events) != 2 || events[1].Message == nil {
			t.Fatalf("trial %d: history events = %+v", trial, events)
		}
		history := *events[1].Message

		line, resolvedSource, resolvedDestination := sdhLiveLine(sdhSelf, snapshot.RemoteConversationID, message)
		live, _, err := BuildSignalIngress(sdhAccountID, 1, sdhSelf, line, resolvedSource, resolvedDestination, sdhReceivedAt)
		if err != nil {
			t.Fatalf("trial %d: BuildSignalIngress: %v", trial, err)
		}
		liveEvents, err := NewSignalDecoder().Decode(context.Background(), *live)
		if err != nil || len(liveEvents) != 1 || liveEvents[0].Message == nil {
			t.Fatalf("trial %d: live events = %+v err=%v", trial, liveEvents, err)
		}
		want := *liveEvents[0].Message
		if len(history.Attachments) != 0 || len(want.Attachments) != len(message.Attachments) {
			t.Fatalf("trial %d: attachments history=%d live=%d", trial, len(history.Attachments), len(want.Attachments))
		}
		history.Attachments, want.Attachments = nil, nil
		if !reflect.DeepEqual(history, want) {
			t.Fatalf("trial %d category %d: history %+v != live %+v", trial, category, history, want)
		}
	}
	for category, count := range categories {
		if count == 0 {
			t.Fatalf("category %d never exercised", category)
		}
	}
}
