package viz

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestFindMediaMessages(t *testing.T) {
	messages := []*db.Message{
		{MessageID: "1", MediaID: "media-1", MimeType: "image/jpeg", Body: "photo"},
		{MessageID: "2", MediaID: "", MimeType: "", Body: "just text"},
		{MessageID: "3", MediaID: "media-3", MimeType: "video/mp4", Body: "video"},
		{MessageID: "4", MediaID: "media-4", MimeType: "audio/ogg", Body: "voice note"},
		{MessageID: "5", MediaID: "media-5", MimeType: "image/png", Body: "screenshot"},
		{MessageID: "6", MediaID: "media-6", MimeType: "application/pdf", Body: "document"},
		{MessageID: "7", MediaID: "", MimeType: "image/gif", Body: "no media id"},
	}

	result := FindMediaMessages(messages)

	if len(result) != 3 {
		t.Fatalf("got %d media messages, want 3", len(result))
	}

	wantIDs := []string{"1", "3", "5"}
	for i, want := range wantIDs {
		if result[i].MessageID != want {
			t.Errorf("result[%d].MessageID = %q, want %q", i, result[i].MessageID, want)
		}
	}
}

func TestFindMediaMessagesEmpty(t *testing.T) {
	// nil input
	if got := FindMediaMessages(nil); got != nil {
		t.Errorf("FindMediaMessages(nil) = %v, want nil", got)
	}

	// empty input
	if got := FindMediaMessages([]*db.Message{}); got != nil {
		t.Errorf("FindMediaMessages([]) = %v, want nil", got)
	}

	// no media messages
	noMedia := []*db.Message{
		{MessageID: "1", Body: "hello"},
		{MessageID: "2", Body: "world"},
	}
	if got := FindMediaMessages(noMedia); got != nil {
		t.Errorf("FindMediaMessages(text-only) = %v, want nil", got)
	}
}

func TestIsVisualMedia(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"image/heic", true},
		{"video/mp4", true},
		{"video/quicktime", true},
		{"video/webm", true},
		{"audio/ogg", false},
		{"audio/mpeg", false},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := isVisualMedia(tt.mime); got != tt.want {
				t.Errorf("isVisualMedia(%q) = %v, want %v", tt.mime, got, tt.want)
			}
		})
	}
}
