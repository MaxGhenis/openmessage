package client

import (
	"reflect"
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

func TestExtractMediaInfo_Characterization(t *testing.T) {
	// Guard the exact media shape and fallbacks before the Wave 1 supervisor refactor.
	tests := []struct {
		name  string
		media *gmproto.MediaContent
		want  *MediaInfo
	}{
		{
			name: "complete metadata",
			media: &gmproto.MediaContent{
				Format:                 gmproto.MediaFormats_IMAGE_JPEG,
				MediaID:                "mid-123",
				MediaName:              "photo.jpg",
				Size:                   12345,
				MimeType:               "image/jpeg",
				DecryptionKey:          []byte{0xde, 0xad, 0xbe, 0xef},
				ThumbnailMediaID:       "thumb-123",
				ThumbnailDecryptionKey: []byte{0xca, 0xfe},
				MediaData:              []byte{0x01, 0x02, 0x03},
			},
			want: &MediaInfo{
				MediaID:                "mid-123",
				MimeType:               "image/jpeg",
				MediaName:              "photo.jpg",
				DecryptionKey:          []byte{0xde, 0xad, 0xbe, 0xef},
				Size:                   12345,
				ThumbnailMediaID:       "thumb-123",
				ThumbnailDecryptionKey: []byte{0xca, 0xfe},
				InlineData:             []byte{0x01, 0x02, 0x03},
			},
		},
		{
			name: "thumbnail fallback",
			media: &gmproto.MediaContent{
				MimeType:               "image/webp",
				DecryptionKey:          []byte{0xff},
				ThumbnailMediaID:       "thumb-only",
				ThumbnailDecryptionKey: []byte{0x10, 0x20},
			},
			want: &MediaInfo{
				MediaID:                "thumb-only",
				MimeType:               "image/webp",
				DecryptionKey:          []byte{0x10, 0x20},
				ThumbnailMediaID:       "thumb-only",
				ThumbnailDecryptionKey: []byte{0x10, 0x20},
			},
		},
		{
			name: "image MIME inference",
			media: &gmproto.MediaContent{
				Format:  gmproto.MediaFormats_IMAGE_PNG,
				MediaID: "image-without-mime",
			},
			want: &MediaInfo{
				MediaID:  "image-without-mime",
				MimeType: "image/jpeg",
			},
		},
		{
			name: "default MIME inference",
			media: &gmproto.MediaContent{
				Format:  gmproto.MediaFormats_VIDEO_MP4,
				MediaID: "video-without-mime",
			},
			want: &MediaInfo{
				MediaID:  "video-without-mime",
				MimeType: "application/octet-stream",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &gmproto.Message{
				MessageInfo: []*gmproto.MessageInfo{{
					Data: &gmproto.MessageInfo_MediaContent{MediaContent: tt.media},
				}},
			}
			if got := ExtractMediaInfo(msg); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExtractMediaInfo() = %+v, want %+v", got, tt.want)
			}
		})
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
	// Actors carry the reactor participant IDs so the UI can name who reacted.
	if got := reactions[0].Actors; len(got) != 3 || got[0] != "p1" || got[1] != "p2" || got[2] != "p3" {
		t.Errorf("expected actors [p1 p2 p3], got %v", got)
	}
	if got := reactions[1].Actors; len(got) != 1 || got[0] != "p1" {
		t.Errorf("expected actors [p1], got %v", got)
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

func TestExtractSenderInfo_Characterization(t *testing.T) {
	// Guard sender identity precedence and fallbacks before the Wave 1 supervisor refactor.
	tests := []struct {
		name        string
		participant *gmproto.Participant
		wantName    string
		wantNumber  string
	}{
		{
			name: "full name and ID number take precedence",
			participant: &gmproto.Participant{
				FullName:        "Ada Lovelace",
				FirstName:       "Ada",
				ID:              &gmproto.SmallInfo{Number: "+15551234567"},
				FormattedNumber: "(555) 123-4567",
			},
			wantName:   "Ada Lovelace",
			wantNumber: "+15551234567",
		},
		{
			name: "first name and formatted number fall back",
			participant: &gmproto.Participant{
				FirstName:       "Ada",
				ID:              &gmproto.SmallInfo{},
				FormattedNumber: "(555) 123-4567",
			},
			wantName:   "Ada",
			wantNumber: "(555) 123-4567",
		},
		{
			name: "nil sender",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotNumber := ExtractSenderInfo(&gmproto.Message{SenderParticipant: tt.participant})
			if gotName != tt.wantName || gotNumber != tt.wantNumber {
				t.Fatalf("ExtractSenderInfo() = (%q, %q), want (%q, %q)", gotName, gotNumber, tt.wantName, tt.wantNumber)
			}
		})
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
