package signallive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/rs/zerolog"
)

func TestBridgeDownloadMediaRefRoutesRemoteRecipientAndGroup(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		isGroup  bool
		wantTail []string
	}{
		{
			name:     "recipient",
			target:   "+15551234567",
			wantTail: []string{"--recipient", "+15551234567"},
		},
		{
			name:     "group",
			target:   "group-token",
			isGroup:  true,
			wantTail: []string{"--group-id", "group-token"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
			originalRun := runSignalCLI
			t.Cleanup(func() { runSignalCLI = originalRun })

			var captured []string
			runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
				captured = append([]string{configDir}, args...)
				return []byte("cmVtb3RlLWJ5dGVz"), nil
			}

			data, mimeType, err := bridge.DownloadMediaRef(
				"+15551230000",
				"remote",
				"attachment-123",
				"",
				tc.target,
				tc.isGroup,
			)
			if err != nil {
				t.Fatalf("DownloadMediaRef(remote): %v", err)
			}
			if string(data) != "remote-bytes" || mimeType != "" {
				t.Fatalf("DownloadMediaRef(remote) = (%q, %q), want remote-bytes and empty transport MIME", data, mimeType)
			}
			want := []string{
				bridge.configDir,
				"-a", "+15551230000",
				"getAttachment", "--id", "attachment-123",
			}
			want = append(want, tc.wantTail...)
			if !slices.Equal(captured, want) {
				t.Fatalf("signal-cli call = %q, want %q", captured, want)
			}
		})
	}
}

func TestBridgeDownloadMediaRefRoutesLocalPathWithoutSignalCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local attachment.bin")
	if err := os.WriteFile(path, []byte("local-bytes"), 0o600); err != nil {
		t.Fatalf("write local attachment: %v", err)
	}
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	called := false
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, errors.New("signal-cli must not run for local attachment")
	}

	data, mimeType, err := bridge.DownloadMediaRef(
		"+15551230000",
		"local",
		"",
		path,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("DownloadMediaRef(local): %v", err)
	}
	if string(data) != "local-bytes" || mimeType != "" {
		t.Fatalf("DownloadMediaRef(local) = (%q, %q), want local-bytes and empty transport MIME", data, mimeType)
	}
	if called {
		t.Fatal("DownloadMediaRef(local) invoked signal-cli")
	}
}

func TestBridgeDownloadMediaRefClassifiesRemoteCommandFailure(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("authorization failed"), errors.New("exit status 1")
	}

	_, _, err := bridge.DownloadMediaRef(
		"+15551230000", "remote", "attachment-123", "", "+15551234567", false,
	)
	if err == nil || !IsCommandError(err) {
		t.Fatalf("DownloadMediaRef(remote) error = %v, want CommandError", err)
	}
}

func TestBridgeDownloadMediaRefRejectsUnknownKindBeforeIO(t *testing.T) {
	bridge := &Bridge{configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	called := false
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	_, _, err := bridge.DownloadMediaRef(
		"+15551230000", "archive", "attachment-123", "/tmp/a", "+15551234567", false,
	)
	if err == nil {
		t.Fatal("DownloadMediaRef(unknown kind) error = nil")
	}
	if called {
		t.Fatal("DownloadMediaRef(unknown kind) invoked signal-cli")
	}
}

func TestBridgeDownloadMediaLegacyGroupBehaviorSurvivesCoreExtraction(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		// Raw, unpadded base64 preserves the legacy decoder fallback as well as
		// the recipient/group command shape through the extracted core.
		return []byte("bGVnYWN5LWdyb3VwIQ"), nil
	}

	data, mimeType, err := bridge.DownloadMedia(&db.Message{
		ConversationID: "signal-group:group-token",
		MediaID:        "signalatt:attachment-legacy",
		MimeType:       "image/gif",
	})
	if err != nil {
		t.Fatalf("DownloadMedia(legacy group): %v", err)
	}
	if string(data) != "legacy-group!" || mimeType != "image/gif" {
		t.Fatalf("DownloadMedia(legacy group) = (%q, %q), want legacy-group!/image-gif", data, mimeType)
	}
	want := []string{
		bridge.configDir,
		"-a", "+15551230000",
		"getAttachment", "--id", "attachment-legacy",
		"--group-id", "group-token",
	}
	if !slices.Equal(captured, want) {
		t.Fatalf("legacy signal-cli call = %q, want %q", captured, want)
	}
}
