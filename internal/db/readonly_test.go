package db

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyReadsWithoutMutatingLegacyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	writable, err := New(path)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := writable.UpsertConversation(&Conversation{
		ConversationID: "signal:+16505550100",
		Name:           "Signal Peer",
		LastMessageTS:  1_700_000_001_000,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(writable): %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(): %v", err)
	}
	conversations, err := readOnly.ListConversationsByPlatform("signal", 10)
	if err != nil {
		t.Fatalf("ListConversationsByPlatform(): %v", err)
	}
	if len(conversations) != 1 ||
		conversations[0].ConversationID != "signal:+16505550100" {
		t.Fatalf("read-only conversations = %+v", conversations)
	}
	if err := readOnly.UpsertConversation(&Conversation{
		ConversationID: "must-not-write",
		SourcePlatform: "signal",
	}); err == nil {
		t.Fatal("read-only store accepted a write")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatalf("Close(read-only): %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OpenReadOnly changed legacy database bytes")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingLegacyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly() succeeded for a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing path was created: %v", err)
	}
}
