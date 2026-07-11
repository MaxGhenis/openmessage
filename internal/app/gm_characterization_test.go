package app

import (
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"
)

func TestNewContactNumbersCharacterization(t *testing.T) {
	tests := []struct {
		name   string
		phones []string
		want   []*gmproto.ContactNumber
	}{
		{
			name:   "nil input",
			phones: nil,
			want:   []*gmproto.ContactNumber{},
		},
		{
			name:   "empty input",
			phones: []string{},
			want:   []*gmproto.ContactNumber{},
		},
		{
			name:   "numbers are copied verbatim",
			phones: []string{"+15551234567", " 555 0100 ", ""},
			want: []*gmproto.ContactNumber{
				{MysteriousInt: ContactNumberMysteriousInt, Number: "+15551234567", Number2: "+15551234567"},
				{MysteriousInt: ContactNumberMysteriousInt, Number: " 555 0100 ", Number2: " 555 0100 "},
				{MysteriousInt: ContactNumberMysteriousInt, Number: "", Number2: ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewContactNumbers(tt.phones)
			if got == nil {
				t.Fatal("NewContactNumbers returned nil; current protocol shape uses a non-nil slice")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(NewContactNumbers()) = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if !proto.Equal(got[i], tt.want[i]) {
					t.Errorf("NewContactNumbers()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExtractSIMAndParticipantCharacterization(t *testing.T) {
	participantSIM := &gmproto.SIMPayload{Two: 1, SIMNumber: 1}
	fallbackSIM := &gmproto.SIMPayload{Two: 2, SIMNumber: 2}
	secondParticipantSIM := &gmproto.SIMPayload{Two: 3, SIMNumber: 3}

	tests := []struct {
		name              string
		conversation      *gmproto.Conversation
		wantParticipantID string
		wantSIM           *gmproto.SIMPayload
	}{
		{
			name: "first self participant and its SIM win",
			conversation: &gmproto.Conversation{
				Participants: []*gmproto.Participant{
					{ID: &gmproto.SmallInfo{Number: "+10000000000"}, SimPayload: fallbackSIM},
					{
						ID:         &gmproto.SmallInfo{Number: "+15551234567", ParticipantID: "participant-id-is-not-selected"},
						IsMe:       true,
						SimPayload: participantSIM,
					},
					{
						ID:         &gmproto.SmallInfo{Number: "+19999999999"},
						IsMe:       true,
						SimPayload: secondParticipantSIM,
					},
				},
				SimCard: &gmproto.SIMCard{SIMData: &gmproto.SIMData{SIMPayload: fallbackSIM}},
			},
			wantParticipantID: "+15551234567",
			wantSIM:           participantSIM,
		},
		{
			name: "self participant without SIM uses conversation fallback",
			conversation: &gmproto.Conversation{
				Participants: []*gmproto.Participant{{
					ID:   &gmproto.SmallInfo{Number: "+15557654321"},
					IsMe: true,
				}},
				SimCard: &gmproto.SIMCard{SIMData: &gmproto.SIMData{SIMPayload: fallbackSIM}},
			},
			wantParticipantID: "+15557654321",
			wantSIM:           fallbackSIM,
		},
		{
			name: "conversation fallback works without self participant",
			conversation: &gmproto.Conversation{
				Participants: []*gmproto.Participant{{ID: &gmproto.SmallInfo{Number: "+15550001111"}}},
				SimCard:      &gmproto.SIMCard{SIMData: &gmproto.SIMData{SIMPayload: fallbackSIM}},
			},
			wantSIM: fallbackSIM,
		},
		{
			name:         "nil conversation",
			conversation: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			participantID, sim := ExtractSIMAndParticipant(tt.conversation)
			if participantID != tt.wantParticipantID {
				t.Errorf("participantID = %q, want %q", participantID, tt.wantParticipantID)
			}
			// The selected libgm payload must be forwarded, not copied or rebuilt.
			if sim != tt.wantSIM {
				t.Errorf("SIM payload pointer = %p, want %p", sim, tt.wantSIM)
			}
		})
	}
}

func TestSendPayloadWithTmpIDShapeCharacterization(t *testing.T) {
	textSIM := &gmproto.SIMPayload{Two: 1, SIMNumber: 1}
	mediaSIM := &gmproto.SIMPayload{Two: 2, SIMNumber: 2}
	media := &gmproto.MediaContent{
		Format:                 gmproto.MediaFormats_IMAGE_PNG,
		MediaID:                "media-id",
		MediaName:              "photo.png",
		Size:                   12345,
		Dimensions:             &gmproto.Dimensions{Width: 640, Height: 480},
		MediaData:              []byte{0x01, 0x02},
		ThumbnailMediaID:       "thumbnail-id",
		DecryptionKey:          []byte{0x03, 0x04},
		ThumbnailDecryptionKey: []byte{0x05, 0x06},
		MimeType:               "image/png",
	}

	tests := []struct {
		name string
		got  *gmproto.SendMessageRequest
		want *gmproto.SendMessageRequest
	}{
		{
			name: "text with reply",
			got:  BuildSendPayloadWithTmpID("conversation-id", "hello", "reply-id", "+15551234567", textSIM, "transport-request-id"),
			want: &gmproto.SendMessageRequest{
				ConversationID: "conversation-id",
				MessagePayload: &gmproto.MessagePayload{
					TmpID: "transport-request-id",
					MessageInfo: []*gmproto.MessageInfo{{
						Data: &gmproto.MessageInfo_MessageContent{
							MessageContent: &gmproto.MessageContent{Content: "hello"},
						},
					}},
					ConversationID: "conversation-id",
					ParticipantID:  "+15551234567",
					TmpID2:         "transport-request-id",
				},
				SIMPayload: &gmproto.SIMPayload{Two: 1, SIMNumber: 1},
				TmpID:      "transport-request-id",
				Reply:      &gmproto.ReplyPayload{MessageID: "reply-id"},
			},
		},
		{
			name: "media",
			got:  BuildSendMediaPayloadWithTmpID("conversation-id", media, "+15557654321", mediaSIM, "media-request-id"),
			want: &gmproto.SendMessageRequest{
				ConversationID: "conversation-id",
				MessagePayload: &gmproto.MessagePayload{
					TmpID: "media-request-id",
					MessageInfo: []*gmproto.MessageInfo{{
						Data: &gmproto.MessageInfo_MediaContent{
							MediaContent: &gmproto.MediaContent{
								Format:                 gmproto.MediaFormats_IMAGE_PNG,
								MediaID:                "media-id",
								MediaName:              "photo.png",
								Size:                   12345,
								Dimensions:             &gmproto.Dimensions{Width: 640, Height: 480},
								MediaData:              []byte{0x01, 0x02},
								ThumbnailMediaID:       "thumbnail-id",
								DecryptionKey:          []byte{0x03, 0x04},
								ThumbnailDecryptionKey: []byte{0x05, 0x06},
								MimeType:               "image/png",
							},
						},
					}},
					ConversationID: "conversation-id",
					ParticipantID:  "+15557654321",
					TmpID2:         "media-request-id",
				},
				SIMPayload: &gmproto.SIMPayload{Two: 2, SIMNumber: 2},
				TmpID:      "media-request-id",
			},
		},
	}

	// API tests already lock the three TmpID locations. Full proto equality adds
	// the remaining wire-shape defaults and complete media metadata for Wave 1.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !proto.Equal(tt.got, tt.want) {
				t.Fatalf("payload = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestBuildReactionPayloadEmojiMappingCharacterization(t *testing.T) {
	sim := &gmproto.SIMPayload{SIMNumber: 2}
	tests := []struct {
		emoji string
		want  gmproto.EmojiType
	}{
		{"👍", gmproto.EmojiType_LIKE},
		{"😍", gmproto.EmojiType_LOVE},
		{"😂", gmproto.EmojiType_LAUGH},
		{"😮", gmproto.EmojiType_SURPRISED},
		{"😥", gmproto.EmojiType_SAD},
		{"😠", gmproto.EmojiType_ANGRY},
		{"👎", gmproto.EmojiType_DISLIKE},
		{"🤔", gmproto.EmojiType_QUESTIONING},
		{"😢", gmproto.EmojiType_CRYING_FACE},
		{"😡", gmproto.EmojiType_POUTING_FACE},
		{"❤", gmproto.EmojiType_RED_HEART},
		{"❤️", gmproto.EmojiType_RED_HEART},
		{"🫡", gmproto.EmojiType_CUSTOM},
	}

	for _, tt := range tests {
		t.Run(tt.emoji, func(t *testing.T) {
			// The emoji enum is a wire detail guarded ahead of the Wave 1 supervisor refactor.
			got := BuildReactionPayload("message-id", tt.emoji, "SwItCh", sim)
			want := &gmproto.SendReactionRequest{
				MessageID:    "message-id",
				ReactionData: &gmproto.ReactionData{Unicode: tt.emoji, Type: tt.want},
				Action:       gmproto.SendReactionRequest_SWITCH,
				SIMPayload:   &gmproto.SIMPayload{SIMNumber: 2},
			}
			if !proto.Equal(got, want) {
				t.Fatalf("reaction payload = %v, want %v", got, want)
			}
			if got.GetSIMPayload() != sim {
				t.Error("SIM payload must be forwarded without replacement")
			}
		})
	}
}
