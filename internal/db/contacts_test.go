package db

import (
	"fmt"
	"testing"
)

func TestUpsertContact_InsertAndUpdate(t *testing.T) {
	store := newTestStore(t)

	t.Run("insert new contact", func(t *testing.T) {
		contact := &Contact{
			ContactID: "c1",
			Name:      "Alice",
			Number:    "+15551234567",
		}
		if err := store.UpsertContact(contact); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		contacts, err := store.ListContacts("Alice", 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(contacts) != 1 {
			t.Fatalf("count: got %d, want 1", len(contacts))
		}
		if contacts[0].Name != "Alice" {
			t.Errorf("name: got %q, want %q", contacts[0].Name, "Alice")
		}
		if contacts[0].Number != "+15551234567" {
			t.Errorf("number: got %q, want %q", contacts[0].Number, "+15551234567")
		}
	})

	t.Run("update existing contact", func(t *testing.T) {
		contact := &Contact{
			ContactID: "c1",
			Name:      "Alice Smith",
			Number:    "+15559999999",
		}
		if err := store.UpsertContact(contact); err != nil {
			t.Fatalf("upsert update: %v", err)
		}

		contacts, err := store.ListContacts("Alice Smith", 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(contacts) != 1 {
			t.Fatalf("count: got %d, want 1", len(contacts))
		}
		if contacts[0].Name != "Alice Smith" {
			t.Errorf("name after update: got %q, want %q", contacts[0].Name, "Alice Smith")
		}
		if contacts[0].Number != "+15559999999" {
			t.Errorf("number after update: got %q, want %q", contacts[0].Number, "+15559999999")
		}
	})

	t.Run("upsert does not create duplicate", func(t *testing.T) {
		contacts, err := store.ListContacts("", 100)
		if err != nil {
			t.Fatalf("list all: %v", err)
		}
		if len(contacts) != 1 {
			t.Errorf("total contacts: got %d, want 1 (no duplicates)", len(contacts))
		}
	})
}

func TestListContacts_QueryFilter(t *testing.T) {
	store := newTestStore(t)

	contacts := []Contact{
		{ContactID: "c1", Name: "Alice Johnson", Number: "+15551111111"},
		{ContactID: "c2", Name: "Bob Smith", Number: "+15552222222"},
		{ContactID: "c3", Name: "Charlie Brown", Number: "+15553333333"},
		{ContactID: "c4", Name: "Alice Cooper", Number: "+15554444444"},
		{ContactID: "c5", Name: "Dave", Number: "+15551111999"},
	}
	for i := range contacts {
		if err := store.UpsertContact(&contacts[i]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("filter by name substring", func(t *testing.T) {
		got, err := store.ListContacts("alice", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("count: got %d, want 2", len(got))
		}
	})

	t.Run("filter by phone number substring", func(t *testing.T) {
		got, err := store.ListContacts("5551111", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		// Matches "+15551111111" and "+15551111999".
		if len(got) != 2 {
			t.Errorf("count: got %d, want 2", len(got))
		}
	})

	t.Run("empty query returns all contacts", func(t *testing.T) {
		got, err := store.ListContacts("", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 5 {
			t.Errorf("count: got %d, want 5", len(got))
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		got, err := store.ListContacts("zzzzz", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("count: got %d, want 0", len(got))
		}
	})

	t.Run("limit constrains results", func(t *testing.T) {
		got, err := store.ListContacts("", 2)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("count: got %d, want 2", len(got))
		}
	})

	t.Run("results ordered by name", func(t *testing.T) {
		got, err := store.ListContacts("", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for i := 1; i < len(got); i++ {
			if got[i].Name < got[i-1].Name {
				t.Errorf("not sorted: %q < %q at index %d", got[i].Name, got[i-1].Name, i)
			}
		}
	})
}

func TestContactAvatarCacheLookup(t *testing.T) {
	store := newTestStore(t)
	now := int64(1700000000000)
	candidate := ContactAvatarCandidate{
		SourcePlatform: "sms",
		ParticipantID:  "participant-1",
		ContactID:      "contact-1",
		PhoneNumber:    "+1 (555) 123-4567",
		DisplayName:    "Alice",
	}
	if err := store.UpsertContactAvatar(candidate, []byte("avatar-bytes"), "image/png", "hash-1", now); err != nil {
		t.Fatalf("upsert avatar: %v", err)
	}

	for _, tc := range []struct {
		name          string
		participantID string
		contactID     string
		phone         string
	}{
		{name: "participant", participantID: "participant-1"},
		{name: "contact", contactID: "contact-1"},
		{name: "phone", phone: "5551234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.GetContactAvatar("sms", tc.participantID, tc.contactID, tc.phone)
			if err != nil {
				t.Fatalf("get avatar: %v", err)
			}
			if got == nil {
				t.Fatal("got nil avatar")
			}
			if string(got.ImageData) != "avatar-bytes" {
				t.Fatalf("image data = %q, want avatar-bytes", string(got.ImageData))
			}
			if got.ImageHash != "hash-1" {
				t.Fatalf("image hash = %q, want hash-1", got.ImageHash)
			}
		})
	}
}

func TestContactAvatarContactOnlyThenParticipantContactDoesNotConflict(t *testing.T) {
	store := newTestStore(t)
	now := int64(1700000000000)

	if err := store.MarkContactAvatarChecked(ContactAvatarCandidate{
		SourcePlatform: "sms",
		ContactID:      "contact-1",
		PhoneNumber:    "+15551234567",
		DisplayName:    "Alice",
	}, now); err != nil {
		t.Fatalf("mark checked: %v", err)
	}
	if err := store.UpsertContactAvatar(ContactAvatarCandidate{
		SourcePlatform: "sms",
		ParticipantID:  "participant-1",
		ContactID:      "contact-1",
		PhoneNumber:    "+15551234567",
		DisplayName:    "Alice",
	}, []byte("avatar-bytes"), "image/jpeg", "hash-2", now+1); err != nil {
		t.Fatalf("upsert participant avatar after contact row: %v", err)
	}

	got, err := store.GetContactAvatar("sms", "", "contact-1", "")
	if err != nil {
		t.Fatalf("get by contact: %v", err)
	}
	if got == nil || got.ImageHash != "hash-2" {
		t.Fatalf("got avatar %#v, want image hash hash-2", got)
	}
}

func TestListContacts_Empty(t *testing.T) {
	store := newTestStore(t)

	got, err := store.ListContacts("", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("count: got %d, want 0", len(got))
	}
}

func TestListContacts_SpecialCharacters(t *testing.T) {
	store := newTestStore(t)

	// Contacts with special characters in names.
	contacts := []Contact{
		{ContactID: "sp1", Name: "O'Brien", Number: "+15551111111"},
		{ContactID: "sp2", Name: "Dr. Smith (MD)", Number: "+15552222222"},
		{ContactID: "sp3", Name: "José García", Number: "+15553333333"},
	}
	for i := range contacts {
		if err := store.UpsertContact(&contacts[i]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("apostrophe in name", func(t *testing.T) {
		got, err := store.ListContacts("O'Brien", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
	})

	t.Run("parentheses in name", func(t *testing.T) {
		got, err := store.ListContacts("(MD)", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
	})

	t.Run("unicode in name", func(t *testing.T) {
		got, err := store.ListContacts("García", 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
	})
}

func TestUpsertContact_ManyContacts(t *testing.T) {
	store := newTestStore(t)

	for i := 0; i < 100; i++ {
		err := store.UpsertContact(&Contact{
			ContactID: fmt.Sprintf("c%d", i),
			Name:      fmt.Sprintf("Contact %03d", i),
			Number:    fmt.Sprintf("+1555%07d", i),
		})
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	got, err := store.ListContacts("", 1000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("count: got %d, want 100", len(got))
	}
}

func TestListContacts_QueryMatchesBothNameAndNumber(t *testing.T) {
	store := newTestStore(t)

	store.UpsertContact(&Contact{ContactID: "c1", Name: "Alice 555", Number: "+15551234567"})
	store.UpsertContact(&Contact{ContactID: "c2", Name: "Bob", Number: "+15559876543"})

	// "555" should match both: Alice by name ("Alice 555") and Bob by number ("+15559876543").
	// Also Alice by number (+15551234567).
	got, err := store.ListContacts("555", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("count: got %d, want 2 (matches both name and number)", len(got))
	}
}

func TestListContactsFromConversationsPrefilter(t *testing.T) {
	store := newTestStore(t)
	convos := []*Conversation{
		{ConversationID: "c1", Name: "Alice Smith", Participants: `[{"name":"Alice Smith","number":"+15551112222"}]`, LastMessageTS: 300},
		{ConversationID: "c2", Name: "Bob Jones", Participants: `[{"name":"Bob Jones","number":"+15553334444"}]`, LastMessageTS: 200},
		{ConversationID: "c3", Name: "carol ALICEson", Participants: `[{"name":"carol ALICEson","number":"+15555556666"}]`, LastMessageTS: 100},
	}
	for _, c := range convos {
		if err := store.UpsertConversation(c); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("case-insensitive name match", func(t *testing.T) {
		got, err := store.ListContactsFromConversations("alice", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d contacts, want 2 (Alice Smith + carol ALICEson): %+v", len(got), got)
		}
	})

	t.Run("number match", func(t *testing.T) {
		got, err := store.ListContactsFromConversations("5553334444", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "Bob Jones" {
			t.Fatalf("number search wrong: %+v", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		got, err := store.ListContactsFromConversations("zzzz", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d contacts, want 0", len(got))
		}
	})

	t.Run("empty query returns all up to limit", func(t *testing.T) {
		got, err := store.ListContactsFromConversations("", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d contacts, want limit 2", len(got))
		}
	})
}
