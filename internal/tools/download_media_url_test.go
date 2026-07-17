package tools

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestDownloadMediaReturnsLegacyMediaURLWithoutDownloadingOrWriting(t *testing.T) {
	a := testApp(t)
	const messageID = "whatsapp:thread/with space:message"
	if err := a.Store.UpsertMessage(&db.Message{
		MessageID:      messageID,
		ConversationID: "whatsapp:thread/with space",
		TimestampMS:    time.Now().UnixMilli(),
		MediaID:        "v2blob:" + strings.Repeat("a", 64),
		MimeType:       "image/png",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed projected v2 media message: %v", err)
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": messageID}
	result, err := downloadMediaHandler(a)(context.Background(), req)
	if err != nil {
		t.Fatalf("downloadMediaHandler(): %v", err)
	}
	if result.IsError {
		t.Fatalf("download_media returned tool error: %#v", result.Content)
	}
	payload := structuredMap(t, result)
	if got, want := payload["url"], "/api/media/"+url.PathEscape(messageID); got != want {
		t.Fatalf("url = %#v, want %#v", got, want)
	}
	if got, want := payload["filename"], "openmessage-whatsapp_thread_with_space_message.png"; got != want {
		t.Fatalf("filename = %#v, want %#v", got, want)
	}
	if got := payload["mime_type"]; got != "image/png" {
		t.Fatalf("mime_type = %#v, want image/png", got)
	}
	if size, ok := payload["size_bytes"]; !ok || size != nil {
		t.Fatalf("size_bytes = %#v (present=%v), want explicit unknown null", size, ok)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir(temp): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("download_media wrote temp files: %#v", entries)
	}
}

func TestDownloadMediaToolIsReadOnlyServingURL(t *testing.T) {
	tool := downloadMediaTool()
	if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
		t.Fatalf("readOnlyHint = %#v, want true", tool.Annotations.ReadOnlyHint)
	}
	if strings.Contains(strings.ToLower(tool.Description), "save to a local file") {
		t.Fatalf("description still promises a local-file mutation: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "/api/media/") {
		t.Fatalf("description does not document the serving URL: %q", tool.Description)
	}
}
