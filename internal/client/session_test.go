package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSaveAndLoadSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	// Save
	data := &SessionData{
		AuthDataJSON: []byte(`{"session_id":"test-123"}`),
		PushKeysJSON: []byte(`{"url":"https://example.com"}`),
	}
	err := SaveSession(path, data)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	// Load
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Compare as parsed JSON (whitespace-insensitive)
	var authMap map[string]any
	if err := json.Unmarshal(loaded.AuthDataJSON, &authMap); err != nil {
		t.Fatalf("parse auth data: %v", err)
	}
	if authMap["session_id"] != "test-123" {
		t.Errorf("auth data session_id mismatch: %v", authMap)
	}

	var pushMap map[string]any
	if err := json.Unmarshal(loaded.PushKeysJSON, &pushMap); err != nil {
		t.Fatalf("parse push keys: %v", err)
	}
	if pushMap["url"] != "https://example.com" {
		t.Errorf("push keys url mismatch: %v", pushMap)
	}
}

func TestLoadSessionNotFound(t *testing.T) {
	_, err := LoadSession("/nonexistent/path/session.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// SaveSession rewrites the paired credentials every few minutes now that
// rotated cookies are persisted, so a reader must never observe a partial
// file and a rewrite must leave no debris or loosened permissions behind.
func TestSaveSessionRewritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	const writers = 8
	const rounds = 25
	failures := make(chan error, writers+1)
	writersDone := make(chan struct{})
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		for {
			select {
			case <-writersDone:
				return
			default:
			}
			loaded, err := LoadSession(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				// A torn write surfaces here as a JSON parse failure.
				failures <- fmt.Errorf("read session: %w", err)
				return
			}
			var auth map[string]any
			if err := json.Unmarshal(loaded.AuthDataJSON, &auth); err != nil {
				failures <- fmt.Errorf("parse auth data: %w", err)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				data := &SessionData{
					AuthDataJSON: json.RawMessage(
						fmt.Sprintf(`{"session_id":"writer-%d-round-%d"}`, writer, round),
					),
				}
				if err := SaveSession(path, data); err != nil {
					failures <- fmt.Errorf("save session: %w", err)
					return
				}
			}
		}(writer)
	}
	wg.Wait()
	close(writersDone)
	<-readerDone

	select {
	case err := <-failures:
		t.Fatalf("concurrent SaveSession produced an unreadable session: %v", err)
	default:
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "session.json" {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat session: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("session permissions = %04o, want 0600", got)
	}
}

// A failed save must not damage the session already on disk — that file is the
// only copy of the paired credentials.
func TestSaveSessionFailureKeepsPreviousSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := SaveSession(path, &SessionData{AuthDataJSON: []byte(`{"session_id":"good"}`)}); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	// json.Marshal rejects invalid RawMessage, so the save fails after the
	// target already exists.
	err := SaveSession(path, &SessionData{AuthDataJSON: []byte(`{oops`)})
	if err == nil {
		t.Fatal("SaveSession() with invalid auth data succeeded, want error")
	}

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("load after failed save: %v", err)
	}
	if got := string(loaded.AuthDataJSON); !strings.Contains(got, "good") {
		t.Fatalf("session after failed save = %s, want the previous contents", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "session.json" {
			t.Fatalf("temp file left behind after failed save: %s", entry.Name())
		}
	}
}

func TestSaveSessionCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "session.json")

	data := &SessionData{
		AuthDataJSON: []byte(`{}`),
	}
	err := SaveSession(path, data)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}
