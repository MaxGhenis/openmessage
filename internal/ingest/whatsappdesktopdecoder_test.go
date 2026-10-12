package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

func TestWhatsAppDesktopHistoryCodecMatchesLiveBodies(t *testing.T) {
	for _, test := range []struct {
		typ           int
		caption, text string
		voice         bool
		body          string
	}{
		{0, "", " desktop text ", false, "desktop text"},
		{7, "", "https://example.invalid/link", false, "https://example.invalid/link"},
		{1, "", "", false, "[Photo]"}, {1, " image caption ", "", false, "image caption"},
		{2, "", "", false, "[Video]"}, {2, " video caption ", "", false, "video caption"},
		{11, "", "", false, "[Video]"},
		{3, "", "", false, "[Audio]"}, {3, "", "", true, "[Voice note]"},
		{8, "", "", false, "[Document]"}, {8, "document caption", "", false, "document caption"},
		{15, "", "", false, "[Sticker]"},
	} {
		t.Run(fmt.Sprintf("type_%d_%s", test.typ, test.body), func(t *testing.T) {
			captured := WhatsAppDesktopHistoryMessage{StanzaID: "synthetic-stanza", TimestampMS: 1780000000000, Type: test.typ, Text: test.text, Caption: test.caption, VoiceNote: test.voice}
			snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Title: "synthetic title", Messages: []WhatsAppDesktopHistoryMessage{captured}}
			record := wadhRecord(t, snapshot)
			events, err := NewWhatsAppDesktopHistoryDecoder().Decode(context.Background(), record)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[0].Kind != bridge.EventConversation || events[1].Message.Body != test.body {
				t.Fatalf("events=%+v", events)
			}
			if len(events[1].Message.Attachments) != 0 {
				t.Fatal("media without a captured local file produced an attachment")
			}
			var envelope whatsAppDesktopHistoryEnvelope
			if err := json.Unmarshal(record.Payload, &envelope); err != nil {
				t.Fatal(err)
			}
			liveRecord := record
			liveRecord.Codec = WhatsAppCodec
			liveRecord.CodecVersion = WhatsAppCodecVersion
			liveRecord.Payload = waJSON(t, envelope.Messages[0].whatsAppFrameEnvelope)
			live, err := NewWhatsAppDecoder().Decode(context.Background(), liveRecord)
			if err != nil {
				t.Fatal(err)
			}
			// Bodies, identities, direction and IDs must match the live decoder
			// exactly. Attachments differ by design: the live decoder turns any
			// media protobuf into a remote ref, while desktop history attaches
			// only a verified local file (none here).
			if len(live) != 1 || live[0].Message == nil {
				t.Fatalf("live decode = %+v", live)
			}
			liveMessage := *live[0].Message
			liveMessage.Attachments = nil
			if !reflect.DeepEqual(*events[1].Message, liveMessage) || events[1].Kind != live[0].Kind {
				t.Fatalf("desktop message differs from live decode: desktop=%+v live=%+v", events[1].Message, liveMessage)
			}
		})
	}
}

func TestWhatsAppDesktopHistoryCodecLocalAttachmentAndCanonicalParticipants(t *testing.T) {
	localRef := whatsappmedia.EncodeLocalMediaRef("synthetic/image.jpg")
	snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: "123456789@g.us", Title: "synthetic group", Participants: []WhatsAppDesktopHistoryParticipant{{JID: "15555550101@s.whatsapp.net"}, {JID: "555555555@lid"}, {JID: "15555550999:7@s.whatsapp.net", IsSelf: true}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "image", SenderJID: "555555555@lid", TimestampMS: 1780000000000, Type: 1, LocalMediaRef: localRef, MIMEType: "image/jpeg", FileName: "image.jpg", Size: 123}}}
	events, err := NewWhatsAppDesktopHistoryDecoder().Decode(context.Background(), wadhRecord(t, snapshot))
	if err != nil {
		t.Fatal(err)
	}
	conversation := events[0].Conversation
	if conversation.Kind != "group" || conversation.RemoteConversationID != "whatsapp:123456789@g.us" {
		t.Fatalf("conversation=%+v", conversation)
	}
	refs := map[string]bridge.IdentityRef{}
	for _, p := range conversation.Participants {
		refs[p.Identity.Raw] = p.Identity
	}
	if refs["+15555550101"].Raw == "" || refs["555555555@lid"].Raw == "" || !refs["+15555550999"].IsSelf {
		t.Fatalf("participants=%+v", conversation.Participants)
	}
	attachment := events[1].Message.Attachments[0]
	if err := whatsappadapter.ValidateDownloadOpaque(attachment.RemoteRef); err != nil {
		t.Fatal(err)
	}
	if attachment.RemoteID != localRef || attachment.MIME != "image/jpeg" || attachment.Filename != "image.jpg" || attachment.Size != 123 {
		t.Fatalf("attachment=%+v", attachment)
	}
}

func TestWhatsAppDesktopHistoryCodecI3RejectsLIDConversationsAndUnsupportedRows(t *testing.T) {
	for _, jid := range []string{"555555@lid", "status@broadcast", "155555@broadcast", "0@s.whatsapp.net", "13135550002@s.whatsapp.net", "newsletter@newsletter", "invalid", ""} {
		t.Run(jid, func(t *testing.T) {
			if _, err := WhatsAppDesktopHistoryRecord("account", 0, WhatsAppDesktopHistorySnapshot{ChatJID: jid}, time.Now()); err == nil {
				t.Fatal("builder accepted unsupported conversation")
			}
			record := bridge.RawIngressRecord{Codec: WhatsAppDesktopHistoryCodec, CodecVersion: 1, Payload: waJSON(t, whatsAppDesktopHistoryEnvelope{ChatJID: jid})}
			if _, err := NewWhatsAppDesktopHistoryDecoder().Decode(context.Background(), record); err == nil {
				t.Fatal("decoder accepted unsupported conversation")
			}
		})
	}
	if _, err := WhatsAppDesktopHistoryRecord("account", 0, WhatsAppDesktopHistorySnapshot{ChatJID: "0123@g.us"}, time.Now()); err != nil {
		t.Fatalf("valid nonzero group beginning with zero rejected: %v", err)
	}
	for _, typ := range []int{6, 10, 14, 41, 43, 66, 75, 999} {
		if _, err := WhatsAppDesktopHistoryRecord("account", 0, WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "system", TimestampMS: 1780000000000, Type: typ}}}, time.Now()); err == nil {
			t.Fatalf("accepted message type %d", typ)
		}
	}
	if _, err := WhatsAppDesktopHistoryRecord("account", 0, WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Messages: make([]WhatsAppDesktopHistoryMessage, 101)}, time.Now()); err == nil {
		t.Fatal("accepted >100 rows")
	}
}

func TestWhatsAppDesktopHistoryCodecValidation(t *testing.T) {
	record := wadhRecord(t, WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "text", TimestampMS: 1780000000000, Type: 0, Text: "synthetic"}}})
	for _, mutate := range []func(*bridge.RawIngressRecord){
		func(r *bridge.RawIngressRecord) { r.Codec = WhatsAppCodec },
		func(r *bridge.RawIngressRecord) { r.CodecVersion = 2 },
		func(r *bridge.RawIngressRecord) { r.Payload = []byte("{") },
		func(r *bridge.RawIngressRecord) {
			var e whatsAppDesktopHistoryEnvelope
			json.Unmarshal(r.Payload, &e)
			e.Messages[0].ChatJID = "different@g.us"
			r.Payload = waJSON(t, e)
		},
		func(r *bridge.RawIngressRecord) {
			var e whatsAppDesktopHistoryEnvelope
			json.Unmarshal(r.Payload, &e)
			e.Messages[0].LocalMediaRef = whatsappmedia.EncodeLocalMediaRef("../../outside")
			r.Payload = waJSON(t, e)
		},
	} {
		bad := record
		mutate(&bad)
		if _, err := NewWhatsAppDesktopHistoryDecoder().Decode(context.Background(), bad); err == nil {
			t.Fatal("malformed history frame accepted")
		}
	}
	if _, err := NewWhatsAppDesktopHistoryDecoder().Decode(nil, record); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewWhatsAppDesktopHistoryDecoder().Decode(ctx, record); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestWhatsAppDesktopHistoryRecordDeterministicAndDisjointFromLive(t *testing.T) {
	snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Participants: []WhatsAppDesktopHistoryParticipant{{JID: waSenderJID}, {JID: waCanonicalChatJID}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "second", TimestampMS: 1780000001000, Type: 0, Text: "synthetic second"}, {StanzaID: "first", TimestampMS: 1780000000000, Type: 0, Text: "synthetic first"}}}
	a := wadhRecord(t, snapshot)
	snapshot.Participants[0], snapshot.Participants[1] = snapshot.Participants[1], snapshot.Participants[0]
	snapshot.Messages[0], snapshot.Messages[1] = snapshot.Messages[1], snapshot.Messages[0]
	b, err := WhatsAppDesktopHistoryRecord(waWorkerAccountID, 99, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.DedupeKey != b.DedupeKey || string(a.Payload) != string(b.Payload) || !strings.HasPrefix(a.DedupeKey, "wadesk:") {
		t.Fatal("snapshot order, generation or receipt time altered payload/key")
	}
	snapshot.Messages[0].Text = "synthetic edit"
	if c := wadhRecord(t, snapshot); c.DedupeKey == a.DedupeKey {
		t.Fatal("changed payload did not change dedupe key")
	}
}

// Categories come from high generator bits; counts assert every body branch
// occurs. The small seeded generator uses no external property dependency.
func TestWhatsAppDesktopHistoryCodecMatchesLiveProperty(t *testing.T) {
	random := rand.New(rand.NewSource(19071))
	types := []int{0, 7, 1, 2, 3, 8, 11, 15}
	counts := make([]int, len(types))
	directions := [2]int{}
	audioKinds := [2]int{}
	captionKinds := [2]int{}
	for i := 0; i < 96; i++ {
		state := random.Uint64()
		category := int((state >> 33) % uint64(len(types)))
		counts[category]++
		captured := WhatsAppDesktopHistoryMessage{StanzaID: fmt.Sprintf("synthetic-%d", i), TimestampMS: 1780000000000 + int64(i)*1000, Type: types[category], IsFromMe: (state>>48)&1 == 1, VoiceNote: (state>>50)&1 == 1}
		if captured.IsFromMe {
			directions[1]++
		} else {
			directions[0]++
		}
		if captured.Type == 3 {
			if captured.VoiceNote {
				audioKinds[1]++
			} else {
				audioKinds[0]++
			}
		}
		if types[category] == 0 || types[category] == 7 {
			captured.Text = fmt.Sprintf("synthetic body %d", i)
		}
		if (state>>52)&1 == 1 {
			captured.Caption = fmt.Sprintf("synthetic caption %d", i)
		}
		if captured.Caption == "" {
			captionKinds[0]++
		} else {
			captionKinds[1]++
		}
		snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Messages: []WhatsAppDesktopHistoryMessage{captured}}
		record := wadhRecord(t, snapshot)
		var e whatsAppDesktopHistoryEnvelope
		if err := json.Unmarshal(record.Payload, &e); err != nil {
			t.Fatal(err)
		}
		liveRecord := record
		liveRecord.Codec = WhatsAppCodec
		liveRecord.Payload = waJSON(t, e.Messages[0].whatsAppFrameEnvelope)
		live, err := NewWhatsAppDecoder().Decode(context.Background(), liveRecord)
		if err != nil {
			t.Fatal(err)
		}
		history, err := NewWhatsAppDesktopHistoryDecoder().Decode(context.Background(), record)
		if err != nil {
			t.Fatal(err)
		}
		// Everything but attachments must equal the live decode; desktop
		// history never carries the remote media fields the live decoder reads.
		if len(history) != 2 || len(live) != 1 || history[1].Kind != live[0].Kind || len(history[1].Message.Attachments) != 0 {
			t.Fatalf("case %d: history=%+v live=%+v", i, history, live)
		}
		liveMessage := *live[0].Message
		liveMessage.Attachments = nil
		if !reflect.DeepEqual(*history[1].Message, liveMessage) {
			t.Fatalf("case %d differs", i)
		}
	}
	for i, count := range counts {
		if count == 0 {
			t.Fatalf("type %d never exercised", types[i])
		}
	}
	for label, categories := range map[string][2]int{"directions": directions, "audio kinds": audioKinds, "caption kinds": captionKinds} {
		if categories[0] == 0 || categories[1] == 0 {
			t.Fatalf("%s branches not both exercised: %v", label, categories)
		}
	}
}

func wadhRecord(t *testing.T, snapshot WhatsAppDesktopHistorySnapshot) bridge.RawIngressRecord {
	t.Helper()
	record, err := WhatsAppDesktopHistoryRecord(waWorkerAccountID, 0, snapshot, waWorkerNow)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
