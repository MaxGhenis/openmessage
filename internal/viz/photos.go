package viz

import (
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
)

// FindMediaMessages returns messages that have media attachments (images/videos).
func FindMediaMessages(messages []*db.Message) []*db.Message {
	var media []*db.Message
	for _, m := range messages {
		if m.MediaID != "" && isVisualMedia(m.MimeType) {
			media = append(media, m)
		}
	}
	return media
}

// isVisualMedia checks if a MIME type is an image or video.
func isVisualMedia(mime string) bool {
	return strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/")
}
