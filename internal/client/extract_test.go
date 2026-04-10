package client

import (
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestExtractMediaInfo_NoMedia(t *testing.T) {
	msg := &gmproto.Message{
		MessageInfo: []*gmproto.MessageInfo{
			{Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "hello"},
			}},
		},
	}
	info := ExtractMediaInfo(msg)
	if info != nil {
		t.Fatalf("expected nil for text-only message, got %+v", info)
	}
}

func TestExtractMediaInfo_WithImage(t *testing.T) {
	msg := &gmproto.Message{
		MessageInfo: []*gmproto.MessageInfo{
			{
				ActionMessageID: strPtr("action-1"),
				Data: &gmproto.MessageInfo_MediaContent{
					MediaContent: &gmproto.MediaContent{
						Format:        gmproto.MediaFormats_IMAGE_JPEG,
						MediaID:       "mid-123",
						MediaName:     "photo.jpg",
						Size:          12345,
						MimeType:      "image/jpeg",
						DecryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
					},
				},
			},
		},
	}
	info := ExtractMediaInfo(msg)
	if info == nil {
		t.Fatal("expected media info, got nil")
	}
	if info.MediaID != "mid-123" {
		t.Errorf("expected MediaID 'mid-123', got %q", info.MediaID)
	}
	if info.MimeType != "image/jpeg" {
		t.Errorf("expected MimeType 'image/jpeg', got %q", info.MimeType)
	}
	if info.MediaName != "photo.jpg" {
		t.Errorf("expected MediaName 'photo.jpg', got %q", info.MediaName)
	}
	if len(info.DecryptionKey) != 4 {
		t.Errorf("expected 4-byte key, got %d bytes", len(info.DecryptionKey))
	}
}

func TestExtractMediaInfo_TextAndImage(t *testing.T) {
	// Messages can have both text content and media content in different MessageInfo entries
	msg := &gmproto.Message{
		MessageInfo: []*gmproto.MessageInfo{
			{Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "Check this out"},
			}},
			{
				ActionMessageID: strPtr("act-2"),
				Data: &gmproto.MessageInfo_MediaContent{
					MediaContent: &gmproto.MediaContent{
						MediaID:       "mid-456",
						MimeType:      "image/png",
						DecryptionKey: []byte{0x01, 0x02},
					},
				},
			},
		},
	}

	// Text should still be extractable
	body := ExtractMessageBody(msg)
	if body != "Check this out" {
		t.Errorf("expected body 'Check this out', got %q", body)
	}

	// Media should also be extractable
	info := ExtractMediaInfo(msg)
	if info == nil {
		t.Fatal("expected media info, got nil")
	}
	if info.MediaID != "mid-456" {
		t.Errorf("expected MediaID 'mid-456', got %q", info.MediaID)
	}
}

func TestExtractReactions_None(t *testing.T) {
	msg := &gmproto.Message{}
	reactions := ExtractReactions(msg)
	if reactions != nil {
		t.Fatalf("expected nil, got %+v", reactions)
	}
}

func TestExtractReactions_WithEmojis(t *testing.T) {
	msg := &gmproto.Message{
		Reactions: []*gmproto.ReactionEntry{
			{
				Data:           &gmproto.ReactionData{Unicode: "😂"},
				ParticipantIDs: []string{"p1", "p2", "p3"},
			},
			{
				Data:           &gmproto.ReactionData{Unicode: "❤️"},
				ParticipantIDs: []string{"p1"},
			},
		},
	}
	reactions := ExtractReactions(msg)
	if len(reactions) != 2 {
		t.Fatalf("expected 2 reactions, got %d", len(reactions))
	}
	if reactions[0].Emoji != "😂" {
		t.Errorf("expected emoji 😂, got %q", reactions[0].Emoji)
	}
	if reactions[0].Count != 3 {
		t.Errorf("expected count 3, got %d", reactions[0].Count)
	}
	if reactions[1].Emoji != "❤️" {
		t.Errorf("expected emoji ❤️, got %q", reactions[1].Emoji)
	}
	if reactions[1].Count != 1 {
		t.Errorf("expected count 1, got %d", reactions[1].Count)
	}
}

func TestExtractReplyToID_None(t *testing.T) {
	msg := &gmproto.Message{}
	if id := ExtractReplyToID(msg); id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestExtractReplyToID_WithReply(t *testing.T) {
	msg := &gmproto.Message{
		ReplyMessage: &gmproto.ReplyMessage{
			MessageID: "original-msg-123",
		},
	}
	if id := ExtractReplyToID(msg); id != "original-msg-123" {
		t.Errorf("expected 'original-msg-123', got %q", id)
	}
}

func TestMessageIsFromMeFallsBackToOutgoingStatus(t *testing.T) {
	msg := &gmproto.Message{
		MessageStatus: &gmproto.MessageStatus{
			Status: gmproto.MessageStatusType_OUTGOING_COMPLETE,
		},
	}
	if !MessageIsFromMe(msg) {
		t.Fatal("expected outgoing message status to be treated as from me")
	}
}

func TestMessageIsFromMePrefersIncomingStatusWhenParticipantMissing(t *testing.T) {
	msg := &gmproto.Message{
		MessageStatus: &gmproto.MessageStatus{
			Status: gmproto.MessageStatusType_INCOMING_COMPLETE,
		},
	}
	if MessageIsFromMe(msg) {
		t.Fatal("expected incoming message status without sender participant to remain not-from-me")
	}
}

func strPtr(s string) *string { return &s }
