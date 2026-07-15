package whatsapplive

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"

	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

func TestDownloadMediaRefRemoteDecodesHexAndControlsAllowNoHash(t *testing.T) {
	tests := []struct {
		name            string
		fileSHA256      string
		wantFileSHA256  []byte
		wantAllowNoHash bool
	}{
		{name: "hash present is strict", fileSHA256: "0506", wantFileSHA256: []byte{0x05, 0x06}, wantAllowNoHash: false},
		{name: "hash absent allows legacy download", fileSHA256: "", wantFileSHA256: nil, wantAllowNoHash: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := &Bridge{connected: true, client: &whatsmeow.Client{}}
			originalDownload := downloadMediaWithPath
			originalIsConnected := clientIsConnected
			t.Cleanup(func() {
				downloadMediaWithPath = originalDownload
				clientIsConnected = originalIsConnected
			})
			clientIsConnected = func(*whatsmeow.Client) bool { return true }
			downloadMediaWithPath = func(_ *whatsmeow.Client, _ context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, mediaType whatsmeow.MediaType, mmsType string, allowNoHash bool) ([]byte, error) {
				if directPath != "/mms/image" || mmsType != "" {
					t.Fatalf("download path/type = %q/%q", directPath, mmsType)
				}
				if string(encFileHash) != string([]byte{0x03, 0x04}) || string(fileHash) != string(test.wantFileSHA256) || string(mediaKey) != string([]byte{0x01, 0x02}) {
					t.Fatalf("decoded enc/hash/key = %x/%x/%x", encFileHash, fileHash, mediaKey)
				}
				if mediaType != whatsmeow.MediaImage || allowNoHash != test.wantAllowNoHash {
					t.Fatalf("media type/allowNoHash = %v/%v, want image/%v", mediaType, allowNoHash, test.wantAllowNoHash)
				}
				return []byte("image-bytes"), nil
			}

			data, mime, err := bridge.DownloadMediaRef("remote", storedMediaRef{
				DirectPath:    "/mms/image",
				FileSHA256:    test.fileSHA256,
				FileEncSHA256: "0304",
				FileLength:    11,
			}, "0102", "image/png", "unused")
			if err != nil {
				t.Fatalf("DownloadMediaRef(): %v", err)
			}
			if string(data) != "image-bytes" || mime != "image/png" {
				t.Fatalf("download = %q/%q, want image-bytes/image/png", data, mime)
			}
		})
	}
}

func TestDownloadMediaRefLocalRoutesWithoutTransport(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	path := filepath.Join(tempHome, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "Message", "Media", "voice.opus")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("opus-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	bridge := &Bridge{}
	data, mime, err := bridge.DownloadMediaRef(
		"local",
		storedMediaRef{DirectPath: "ignored", FileEncSHA256: "not-hex"},
		"not-hex",
		"",
		whatsappmedia.EncodeLocalMediaRef("Media/voice.opus"),
	)
	if err != nil {
		t.Fatalf("DownloadMediaRef(): %v", err)
	}
	if string(data) != "opus-bytes" || mime != "audio/ogg" {
		t.Fatalf("local download = %q/%q, want opus-bytes/audio/ogg", data, mime)
	}
}
