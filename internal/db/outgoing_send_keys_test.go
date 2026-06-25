package db

import "testing"

func TestOutgoingSendKeyClaimCompleteRelease(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	claimed, item, err := store.ClaimOutgoingSendKey("tmp_key_1", "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("first claim was not granted")
	}
	if item == nil || item.Status != OutgoingSendStatusSending || item.ConversationID != "conv1" {
		t.Fatalf("unexpected first claim: %#v", item)
	}

	claimed, item, err = store.ClaimOutgoingSendKey("tmp_key_1", "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("duplicate claim was granted")
	}
	if item == nil || item.Status != OutgoingSendStatusSending {
		t.Fatalf("unexpected duplicate item: %#v", item)
	}

	if err := store.CompleteOutgoingSendKey("tmp_key_1", "msg1", OutgoingSendStatusSent); err != nil {
		t.Fatal(err)
	}
	item, err = store.GetOutgoingSendKey("tmp_key_1")
	if err != nil {
		t.Fatal(err)
	}
	if item == nil || item.MessageID != "msg1" || item.Status != OutgoingSendStatusSent {
		t.Fatalf("unexpected completed item: %#v", item)
	}

	if err := store.ReleaseOutgoingSendKey("tmp_key_1"); err != nil {
		t.Fatal(err)
	}
	item, err = store.GetOutgoingSendKey("tmp_key_1")
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Fatalf("released key still exists: %#v", item)
	}
}
