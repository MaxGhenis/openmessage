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

// TestOutgoingSendKeyFailedClaimBlocksResend locks in the invariant the Google
// ambiguous-send handling relies on: when a send errors ambiguously (the request
// may have reached the platform), the claim is COMPLETED as failed — and a
// retry with the same key must still be deduped, never re-granted, or it would
// re-dispatch and deliver a duplicate. A key that was RELEASED (the send
// provably did not dispatch, e.g. auth rejection) can be claimed again.
func TestOutgoingSendKeyFailedClaimBlocksResend(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if claimed, _, err := store.ClaimOutgoingSendKey("tmp_ambig", "conv1"); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	// Ambiguous send error → keep the claim as failed.
	if err := store.CompleteOutgoingSendKey("tmp_ambig", "", OutgoingSendStatusFailed); err != nil {
		t.Fatal(err)
	}
	claimed, item, err := store.ClaimOutgoingSendKey("tmp_ambig", "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("a retained (failed) claim must dedup a retry, not re-grant the claim")
	}
	if item == nil || item.Status != OutgoingSendStatusFailed {
		t.Fatalf("expected retained failed claim, got %#v", item)
	}

	// A released key (provably-not-dispatched path) can be claimed again so a
	// post-re-pair retry sends the message exactly once.
	if err := store.ReleaseOutgoingSendKey("tmp_ambig"); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := store.ClaimOutgoingSendKey("tmp_ambig", "conv1"); err != nil || !claimed {
		t.Fatalf("post-release re-claim: claimed=%v err=%v", claimed, err)
	}
}
