package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/localapi"
)

func TestParseMediaImportArgs(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		args    []string
		want    mediaImportArgs
		wantErr string
	}{
		"message id":       {args: []string{"msg-1", "f.html"}, want: mediaImportArgs{messageID: "msg-1", path: "f.html"}},
		"media id":         {args: []string{"v2msg:msg-1:2", "f.html", "--json"}, want: mediaImportArgs{messageID: "msg-1", ordinal: 2, path: "f.html", json: true}},
		"ordinal flag":     {args: []string{"--ordinal", "1", "msg-1", "f.html", "--sha256", strings.ToUpper(digest)}, want: mediaImportArgs{messageID: "msg-1", ordinal: 1, path: "f.html", sha256: digest}},
		"agreeing ordinal": {args: []string{"v2msg:msg-1:1", "f.html", "--ordinal", "1"}, want: mediaImportArgs{messageID: "msg-1", ordinal: 1, path: "f.html"}},
		"contradiction":    {args: []string{"v2msg:msg-1:1", "f.html", "--ordinal", "2"}, wantErr: "contradicts"},
		"bad media id":     {args: []string{"v2msg:msg-1", "f.html"}, wantErr: "is not v2msg"},
		"bad digest":       {args: []string{"msg-1", "f.html", "--sha256", "xyz"}, wantErr: "--sha256"},
		"negative":         {args: []string{"msg-1", "f.html", "--ordinal", "-1"}, wantErr: "--ordinal"},
		"unknown flag":     {args: []string{"msg-1", "f.html", "--force"}, wantErr: "unknown flag"},
		"missing file":     {args: []string{"msg-1"}, wantErr: "Usage"},
	} {
		got, err := parseMediaImportArgs(tc.args)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %v, want %q", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: parse = %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

func TestRunMediaImportUploadsTheFileWithItsDigest(t *testing.T) {
	content := []byte("<html>copy saved from Signal Desktop</html>")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	var gotMethod, gotPath, gotDigest, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		gotDigest, gotAuth = r.Header.Get("X-Content-SHA256"), r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(localapi.AttachmentImport{
			MessageID: "msg-1", Ordinal: 0, State: "downloaded", BlobHash: digest,
			SizeBytes: int64(len(content)), MIME: "application/octet-stream", Filename: "Report.html", Imported: true,
		})
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "Report.html")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runMediaImport(context.Background(), &out, localapi.NewClient(server.URL, "tok"), []string{"v2msg:msg-1:0", file, "--sha256", digest}); err != nil {
		t.Fatalf("runMediaImport(): %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/messages/msg-1/attachments/0" ||
		gotDigest != digest || gotAuth != "Bearer tok" || !bytes.Equal(gotBody, content) {
		t.Fatalf("request = %s %s digest=%q auth=%q body=%q", gotMethod, gotPath, gotDigest, gotAuth, gotBody)
	}
	if want := "msg-1:0 imported: Report.html (application/octet-stream, 43 bytes, sha256 " + digest + ")\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestRunMediaImportRefusesAFileThatIsNotTheExpectedOne(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "f.html")
	if err := os.WriteFile(file, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runMediaImport(context.Background(), io.Discard, localapi.NewClient(server.URL, ""), []string{"msg-1", file, "--sha256", strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "not the expected") || called {
		t.Fatalf("runMediaImport(wrong file) = %v, called=%v; want a local refusal", err, called)
	}
}

func TestRunMediaImportSurfacesTheAppsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"media: imported content does not match the attachment: content is 3 bytes, the transport declared 85841"}`))
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "f.html")
	if err := os.WriteFile(file, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runMediaImport(context.Background(), io.Discard, localapi.NewClient(server.URL, ""), []string{"msg-1", file})
	if err == nil || !strings.Contains(err.Error(), "the transport declared 85841") {
		t.Fatalf("runMediaImport() = %v, want the app's reason", err)
	}
}
