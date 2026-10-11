package sqlite

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

func TestRemoteMessageIDsPresentIsAccountWideAndBatchSafe(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, _ := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageConversation(t, store, "conversation-b", "account-a")
	seedMessageAccount(t, store, "account-other", "whatsmeow")
	seedMessageConversation(t, store, "conversation-other", "account-other")
	seedOutboxTestMessage(t, store, "in-a", "account-a", "conversation-a")
	seedOutboxTestMessage(t, store, "in-b", "account-a", "conversation-b")
	seedOutboxTestMessage(t, store, "other", "account-other", "conversation-other")
	repository := mustMessageRepository(t, store, outboxTestTimeMS)

	// 900 IDs crosses the 400-ID statement batch twice.
	ids := []string{"remote-in-a", "remote-in-b", "remote-other", " remote-in-a ", ""}
	for i := 0; i < 900; i++ {
		ids = append(ids, "absent-"+string(rune('a'+i%26))+string(rune('a'+i/26%26)))
	}
	present, err := repository.RemoteMessageIDsPresent(context.Background(), "account-a", ids)
	if err != nil {
		t.Fatalf("RemoteMessageIDsPresent(): %v", err)
	}
	if !reflect.DeepEqual(present, map[string]bool{"remote-in-a": true, "remote-in-b": true}) {
		t.Fatalf("present = %v, want both account-a conversations and nothing from another account", present)
	}
}

func TestSelfIdentityValuesListsEverySelfRowNewestFirst(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, _ := openOutboxTestRepository(t, clock.Now)
	for _, row := range []Identity{
		{IdentityID: "self-old", AccountID: "account-a", Kind: "e164", CanonicalValue: "+15550100002", RawValue: "+15550100002", IsSelf: true, MetadataJSON: "{}", CreatedAtMS: 100, UpdatedAtMS: 100},
		{IdentityID: "self-new", AccountID: "account-a", Kind: "jid", CanonicalValue: "991122@lid", RawValue: "991122@lid", IsSelf: true, MetadataJSON: "{}", CreatedAtMS: 100, UpdatedAtMS: 300},
		{IdentityID: "peer", AccountID: "account-a", Kind: "e164", CanonicalValue: "+15550100003", RawValue: "+15550100003", MetadataJSON: "{}", CreatedAtMS: 100, UpdatedAtMS: 400},
	} {
		mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(row))
	}
	values, err := store.SelfIdentityValues("account-a")
	if err != nil {
		t.Fatalf("SelfIdentityValues(): %v", err)
	}
	want := []SelfIdentityValue{{Kind: "jid", CanonicalValue: "991122@lid"}, {Kind: "e164", CanonicalValue: "+15550100002"}}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %+v, want %+v", values, want)
	}
}

func TestMessageOutboxTransportRequestIDsListsTextAndMediaSendsOfTheAccount(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	mustEnqueueOutbox(t, repository, outboxTestItem("first"))
	mustEnqueueOutbox(t, repository, outboxTestItem("second"))
	ids, err := store.MessageOutboxTransportRequestIDs("account-a")
	if err != nil {
		t.Fatalf("MessageOutboxTransportRequestIDs(): %v", err)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"request-first", "request-second"}) {
		t.Fatalf("ids = %v", ids)
	}
	if other, err := store.MessageOutboxTransportRequestIDs("account-other"); err != nil || len(other) != 0 {
		t.Fatalf("other account ids = %v, %v", other, err)
	}
}
