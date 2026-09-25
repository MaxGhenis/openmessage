package db

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/contactsync"
)

func directoryPerson(id, name string, phones ...string) contactsync.Person {
	p := contactsync.Person{ResourceName: "people/" + id, Names: []contactsync.Name{{DisplayName: name}}, Organizations: []contactsync.Organization{{Name: "Example Company", Title: "Engineer"}}, EmailAddresses: []contactsync.Value{{Value: "test@example.com", Type: "work"}}}
	for _, n := range phones {
		p.PhoneNumbers = append(p.PhoneNumbers, contactsync.Value{Value: n, Type: "mobile"})
	}
	return p
}
func seedDirectoryConversation(t *testing.T, s *Store, id, name, phone string, group bool) {
	t.Helper()
	ps, _ := json.Marshal([]map[string]any{{"name": "Me", "number": "+12025550000", "is_me": true}, {"name": phone, "number": phone, "id": "remote-peer", "contact_id": "phone-local-id", "extra": true}})
	if err := s.UpsertConversation(&Conversation{ConversationID: id, Name: name, Participants: string(ps), IsGroup: group, LastMessageTS: 1234, UnreadCount: 2, IsFavorite: true, NotificationMode: "muted"}); err != nil {
		t.Fatal(err)
	}
}
func TestContactDirectoryReconcilesAndRetainsMetadata(t *testing.T) {
	s := newTestStore(t)
	phone := "+12025550101"
	seedDirectoryConversation(t, s, "raw", phone, phone, false)
	seedDirectoryConversation(t, s, "custom", "My dentist", phone, false)
	seedDirectoryConversation(t, s, "group", "Team lunch", phone, true)
	if err := s.UpsertContact(&Contact{ContactID: "phone-original", Name: "Original", Number: "+12025550999"}); err != nil {
		t.Fatal(err)
	}
	p := directoryPerson("a", "Alice Example", phone, "+12025550102")
	phones, changed, err := s.ReplaceContactDirectory([]contactsync.Person{p})
	if err != nil || phones != 2 || changed != 3 {
		t.Fatalf("%d %d %v", phones, changed, err)
	}
	for id, want := range map[string]string{"raw": "Alice Example", "custom": "My dentist", "group": "Team lunch"} {
		c, _ := s.GetConversation(id)
		if c.Name != want || c.LastMessageTS != 1234 || c.UnreadCount != 2 || !c.IsFavorite || c.NotificationMode != "muted" {
			t.Fatalf("%+v", c)
		}
		var ps []map[string]any
		json.Unmarshal([]byte(c.Participants), &ps)
		if ps[1]["extra"] != true || ps[1]["contact_id"] != "phone-local-id" || ps[1]["name"] != "Alice Example" {
			t.Fatalf("%v", ps)
		}
	}
	var raw string
	if err = s.db.QueryRow("SELECT data_json FROM google_contact_directory").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved contactsync.Person
	json.Unmarshal([]byte(raw), &saved)
	if len(saved.PhoneNumbers) != 2 || saved.Organizations[0].Name != "Example Company" || saved.EmailAddresses[0].Value != "test@example.com" {
		t.Fatalf("%+v", saved)
	}
	contacts, _ := s.ListContacts("Alice", 10)
	if len(contacts) != 2 {
		t.Fatal(contacts)
	}
	// A later phone snapshot still containing a raw number cannot undo resolution.
	seedDirectoryConversation(t, s, "raw", phone, phone, false)
	c, _ := s.GetConversation("raw")
	if c.Name != "Alice Example" {
		t.Fatal(c.Name)
	}
	p.Names[0].DisplayName = "Alice Renamed"
	if _, _, err = s.ReplaceContactDirectory([]contactsync.Person{p}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetConversation("raw")
	if c.Name != "Alice Renamed" {
		t.Fatal(c.Name)
	}
	if _, _, err = s.ReplaceContactDirectory(nil); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetConversation("raw")
	if c.Name != phone {
		t.Fatal("deleted identity retained", c.Name)
	}
	contacts, _ = s.ListContacts("", 10)
	if len(contacts) != 1 || contacts[0].ContactID != "phone-original" {
		t.Fatal(contacts)
	}
}
func TestContactDirectoryAmbiguityRollbackAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	phone := "+12025550101"
	seedDirectoryConversation(t, s, "raw", phone, phone, false)
	p := directoryPerson("a", "Alice", phone)
	if _, _, err = s.ReplaceContactDirectory([]contactsync.Person{p, p}); err == nil {
		t.Fatal("duplicate IDs should rollback")
	}
	contacts, _ := s.ListContacts("Alice", 10)
	if len(contacts) != 0 {
		t.Fatal("partial write")
	}
	if _, _, err = s.ReplaceContactDirectory([]contactsync.Person{p, directoryPerson("b", "Bob", phone)}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetConversation("raw")
	if c.Name != phone {
		t.Fatal("assigned ambiguous identity")
	}
	if _, _, err = s.ReplaceContactDirectory([]contactsync.Person{p}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	seedDirectoryConversation(t, s, "new", phone, phone, false)
	c, _ = s.GetConversation("new")
	if c.Name != "Alice" {
		t.Fatal("directory not loaded on restart")
	}
	if _, _, err = s.ReplaceContactDirectory([]contactsync.Person{p, directoryPerson("b", "Bob", phone)}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetConversation("raw")
	if c.Name != phone {
		t.Fatal("new ambiguity retained old name")
	}
}
func TestDirectoryPhone(t *testing.T) {
	for in, want := range map[string]string{"(202) 555-0101": "+12025550101", "+44 20 7946 0000": "+442079460000", "+49 30 123456": "+4930123456", "2025550101 ext 2": "", "user@example.com": "", "12345": ""} {
		if got := DirectoryPhone(in); got != want {
			t.Fatalf("%q => %q, want %q", in, got, want)
		}
	}
}
