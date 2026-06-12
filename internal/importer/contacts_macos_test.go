package importer

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// makeFakeAddressBook writes a minimal abcddb with the tables/columns the
// importer reads, so we can exercise it without a real macOS address book.
func makeFakeAddressBook(t *testing.T, path string, stmts []string) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fake abcddb: %v", err)
	}
	defer conn.Close()
	schema := []string{
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZMIDDLENAME TEXT, ZLASTNAME TEXT, ZNICKNAME TEXT, ZORGANIZATION TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZADDRESS TEXT)`,
	}
	for _, s := range append(schema, stmts...) {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func TestMacOSContactsImport(t *testing.T) {
	dir := t.TempDir()
	abPath := filepath.Join(dir, "AddressBook-v22.abcddb")
	makeFakeAddressBook(t, abPath, []string{
		// Sarah Chen: two numbers + one email.
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1,'Sarah','Chen')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (10,1,'+1 (415) 555-1234')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (11,1,'+14155559999')`,
		`INSERT INTO ZABCDEMAILADDRESS (Z_PK, ZOWNER, ZADDRESS) VALUES (20,1,'sarah@example.com')`,
		// Organization-only contact with one number -> name falls back to org.
		`INSERT INTO ZABCDRECORD (Z_PK, ZORGANIZATION) VALUES (2,'Acme Inc')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (12,2,'+18005551212')`,
		// Name only, no reachable identifier -> skipped.
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME) VALUES (3,'Ghost')`,
	})

	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	res, err := (&MacOSContacts{Paths: []string{abPath}}).ImportFromAddressBook(store)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if res.ContactsImported != 2 {
		t.Errorf("ContactsImported = %d, want 2 (Ghost has no number/email and is skipped)", res.ContactsImported)
	}
	if res.PhoneNumbers != 3 {
		t.Errorf("PhoneNumbers = %d, want 3", res.PhoneNumbers)
	}
	if res.Emails != 1 {
		t.Errorf("Emails = %d, want 1", res.Emails)
	}

	// Sarah is searchable in the contacts table (drives the compose picker / list_contacts).
	contacts, err := store.ListContacts("Sarah", 10)
	if err != nil {
		t.Fatalf("list contacts: %v", err)
	}
	if len(contacts) != 2 || contacts[0].Name != "Sarah Chen" {
		t.Errorf("expected 2 Sarah Chen rows (one per number), got %d", len(contacts))
	}

	// Unified contact carries both phone and email identifiers.
	us, err := store.ListUnifiedContacts("Sarah", 10)
	if err != nil {
		t.Fatalf("list unified: %v", err)
	}
	if len(us) != 1 {
		t.Fatalf("unified contacts for Sarah = %d, want 1", len(us))
	}
	if !strings.Contains(us[0].Identifiers, "sarah@example.com") || !strings.Contains(us[0].Identifiers, `"phone"`) {
		t.Errorf("identifiers missing phone/email: %s", us[0].Identifiers)
	}
}

func TestMacOSContactsImport_DedupesAcrossSources(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.abcddb")
	b := filepath.Join(dir, "b.abcddb")
	makeFakeAddressBook(t, a, []string{
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1,'Max','G')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (10,1,'+13105550000')`,
	})
	// Same person in a second source, formatted differently + an extra number.
	makeFakeAddressBook(t, b, []string{
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1,'Max','G')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (10,1,'+1 (310) 555-0000')`,
		`INSERT INTO ZABCDPHONENUMBER (Z_PK, ZOWNER, ZFULLNUMBER) VALUES (11,1,'+13105551111')`,
	})

	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	res, err := (&MacOSContacts{Paths: []string{a, b}}).ImportFromAddressBook(store)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.ContactsImported != 1 {
		t.Errorf("ContactsImported = %d, want 1 (same person merged across sources)", res.ContactsImported)
	}
}
