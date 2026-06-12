package importer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// ContactsImportResult summarizes a macOS address-book import.
type ContactsImportResult struct {
	ContactsImported int
	PhoneNumbers     int
	Emails           int
	Sources          int
	Errors           []string
}

// MacOSContacts imports the local macOS address book into the contacts and
// unified_contacts tables. It reads the AddressBook SQLite databases directly
// (like the iMessage importer reads chat.db) and therefore needs Full Disk
// Access to ~/Library/Application Support/AddressBook.
//
// Photos are intentionally not imported here: macOS keeps contact images
// outside these databases (often fetched from iCloud on demand), so the Go
// importer can't reliably read them. The native macOS app still renders contact
// photos live via the Contacts framework.
type MacOSContacts struct {
	// Paths optionally lists abcddb files to read. When empty, the importer
	// auto-discovers them under ~/Library/Application Support/AddressBook.
	Paths []string
}

type abContact struct {
	name    string
	numbers []string
	emails  []string
}

// ImportFromAddressBook reads every discovered address-book database, merges
// duplicate people across sources, and upserts them into the store.
func (m *MacOSContacts) ImportFromAddressBook(store *db.Store) (*ContactsImportResult, error) {
	paths, err := m.dbPaths()
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no macOS AddressBook database found (Full Disk Access may be required)")
	}

	result := &ContactsImportResult{}
	merged := map[string]*abContact{}
	var order []string

	for _, p := range paths {
		contacts, err := readAddressBookDB(p)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", filepath.Base(filepath.Dir(p)), err))
			continue
		}
		result.Sources++
		for _, c := range contacts {
			key := contactDedupKey(c)
			if key == "" {
				continue
			}
			if existing, ok := merged[key]; ok {
				existing.numbers = mergeUnique(existing.numbers, c.numbers)
				existing.emails = mergeUnique(existing.emails, c.emails)
				if existing.name == "" {
					existing.name = c.name
				}
				continue
			}
			cp := c
			merged[key] = &cp
			order = append(order, key)
		}
	}

	for _, key := range order {
		if err := storeContact(store, merged[key], result); err != nil {
			result.Errors = append(result.Errors, err.Error())
		}
	}
	return result, nil
}

func (m *MacOSContacts) dbPaths() ([]string, error) {
	if len(m.Paths) > 0 {
		return m.Paths, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, "Library", "Application Support", "AddressBook")
	var paths []string
	top := filepath.Join(base, "AddressBook-v22.abcddb")
	if fileExists(top) {
		paths = append(paths, top)
	}
	matches, _ := filepath.Glob(filepath.Join(base, "Sources", "*", "AddressBook-v22.abcddb"))
	paths = append(paths, matches...)
	return paths, nil
}

func readAddressBookDB(path string) ([]abContact, error) {
	conn, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	phones, err := loadOwnedValues(conn, "ZABCDPHONENUMBER", "ZFULLNUMBER")
	if err != nil {
		return nil, fmt.Errorf("phones: %w", err)
	}
	emails, err := loadOwnedValues(conn, "ZABCDEMAILADDRESS", "ZADDRESS")
	if err != nil {
		return nil, fmt.Errorf("emails: %w", err)
	}

	rows, err := conn.Query(`SELECT Z_PK, ZFIRSTNAME, ZMIDDLENAME, ZLASTNAME, ZNICKNAME, ZORGANIZATION FROM ZABCDRECORD`)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	defer rows.Close()

	var contacts []abContact
	for rows.Next() {
		var pk int64
		var first, middle, last, nick, org sql.NullString
		if err := rows.Scan(&pk, &first, &middle, &last, &nick, &org); err != nil {
			return nil, err
		}
		name := buildContactName(first.String, middle.String, last.String, nick.String, org.String)
		nums := phones[pk]
		mails := emails[pk]
		// Skip contacts with no way to reach them — a bare name is useless for
		// a messaging inbox and would just clutter the picker.
		if len(nums) == 0 && len(mails) == 0 {
			continue
		}
		contacts = append(contacts, abContact{name: name, numbers: nums, emails: mails})
	}
	return contacts, rows.Err()
}

// loadOwnedValues returns a map of ZABCDRECORD.Z_PK -> []value for a child table
// (phone numbers or email addresses) keyed by its ZOWNER column.
func loadOwnedValues(conn *sql.DB, table, valueCol string) (map[int64][]string, error) {
	rows, err := conn.Query(fmt.Sprintf(`SELECT ZOWNER, %s FROM %s WHERE %s IS NOT NULL`, valueCol, table, valueCol))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var owner sql.NullInt64
		var value sql.NullString
		if err := rows.Scan(&owner, &value); err != nil {
			return nil, err
		}
		v := strings.TrimSpace(value.String)
		if !owner.Valid || v == "" {
			continue
		}
		out[owner.Int64] = appendUnique(out[owner.Int64], v)
	}
	return out, rows.Err()
}

func storeContact(store *db.Store, c *abContact, result *ContactsImportResult) error {
	c.numbers = dedupeNumbersByDigits(c.numbers)
	name := c.name
	if name == "" {
		if len(c.numbers) > 0 {
			name = c.numbers[0]
		} else if len(c.emails) > 0 {
			name = c.emails[0]
		}
	}
	id := "macos:" + shortHash(contactDedupKey(*c))

	type identifier struct {
		Platform string `json:"platform"`
		Value    string `json:"value"`
	}
	idents := make([]identifier, 0, len(c.numbers)+len(c.emails))
	for _, n := range c.numbers {
		idents = append(idents, identifier{Platform: "phone", Value: n})
	}
	for _, e := range c.emails {
		idents = append(idents, identifier{Platform: "email", Value: e})
	}
	identJSON, err := json.Marshal(idents)
	if err != nil {
		return err
	}
	if err := store.UpsertUnifiedContact(&db.UnifiedContact{
		UnifiedID:   id,
		DisplayName: name,
		Identifiers: string(identJSON),
	}); err != nil {
		return err
	}
	result.ContactsImported++

	for i, n := range c.numbers {
		if err := store.UpsertContact(&db.Contact{
			ContactID: fmt.Sprintf("%s:p%d", id, i),
			Name:      name,
			Number:    n,
		}); err != nil {
			return err
		}
		result.PhoneNumbers++
	}
	result.Emails += len(c.emails)
	return nil
}

func buildContactName(first, middle, last, nick, org string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{first, middle, last} {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if s := strings.TrimSpace(nick); s != "" {
		return s
	}
	return strings.TrimSpace(org)
}

// contactDedupKey produces a stable key so the same person across multiple
// address-book sources (iCloud, On My Mac, ...) merges into one entry. Named
// contacts merge by normalized name; nameless ones fall back to their numbers
// then emails.
func contactDedupKey(c abContact) string {
	if name := strings.ToLower(strings.TrimSpace(c.name)); name != "" {
		return "name:" + name
	}
	nums := make([]string, 0, len(c.numbers))
	for _, n := range c.numbers {
		if d := digitsOnly(n); d != "" {
			nums = append(nums, d)
		}
	}
	sort.Strings(nums)
	if joined := strings.Join(nums, ","); joined != "" {
		return "num:" + joined
	}
	mails := append([]string{}, c.emails...)
	for i := range mails {
		mails[i] = strings.ToLower(strings.TrimSpace(mails[i]))
	}
	sort.Strings(mails)
	if joined := strings.Join(mails, ","); joined != "" {
		return "email:" + joined
	}
	return ""
}

// dedupeNumbersByDigits collapses numbers that are the same once formatting is
// stripped (e.g. "+1 (310) 555-0000" and "+13105550000"), keeping the first
// formatting seen.
func dedupeNumbersByDigits(numbers []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(numbers))
	for _, n := range numbers {
		d := digitsOnly(n)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, n)
	}
	return out
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func shortHash(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}

func mergeUnique(a, b []string) []string {
	for _, v := range b {
		a = appendUnique(a, v)
	}
	return a
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
