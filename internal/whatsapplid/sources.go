package whatsapplid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Source names, as reports and evidence list them.
const (
	SourceSessionLIDMap   = "session_lid_map"
	SourceSessionDevice   = "session_device"
	SourceSessionContacts = "session_contacts"
	SourceDesktopChats    = "desktop_chats"
	SourceDesktopMembers  = "desktop_members"
	SourceDesktopMessages = "desktop_messages"
	SourceDesktopLIDPairs = "desktop_lid_pairs"
)

// SourceReport says what one database contributed. A missing file is not an
// error: Read is false and Missing true. Tables or columns an older schema
// lacks are skipped and listed in Skipped.
type SourceReport struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Read    bool     `json:"read"`
	Missing bool     `json:"missing,omitempty"`
	Error   string   `json:"error,omitempty"`
	Rows    int      `json:"rows"`
	Skipped []string `json:"skipped,omitempty"`
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// OpenReadOnly opens a SQLite database read-only. mode=ro keeps the reader from
// writing the file; it still sees rows in the WAL, which immutable=1 would
// hide.
func OpenReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolute),
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readFile(ctx context.Context, name, path string, read func(context.Context, *sql.DB, *SourceReport) error) SourceReport {
	report := SourceReport{Name: name, Path: path}
	if strings.TrimSpace(path) == "" {
		report.Missing = true
		return report
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		report.Missing = true
		return report
	} else if err != nil {
		report.Error = err.Error()
		return report
	}
	db, err := OpenReadOnly(path)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	defer db.Close()
	if err := read(ctx, db, &report); err != nil {
		report.Error = err.Error()
		return report
	}
	report.Read = true
	return report
}

func columns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found[strings.ToUpper(name)] = true
	}
	return found, rows.Err()
}

// scanPairs runs a two-column query and hands each row to add.
func scanPairs(ctx context.Context, q queryer, query string, add func(first, second string)) (int, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var first, second sql.NullString
		if err := rows.Scan(&first, &second); err != nil {
			return count, err
		}
		add(first.String, second.String)
		count++
	}
	return count, rows.Err()
}

// ReadSessionStore reads the whatsmeow session store (whatsapp-session.db):
// the LID map whatsmeow keeps from the server (whatsmeow_lid_map, user parts
// only), this account's own phone-number JID and LID (whatsmeow_device), and
// the JIDs of its contacts (whatsmeow_contacts.their_jid). The store holds
// what whatsmeow has learned since it was last paired, so an unpaired or
// newly paired store knows little.
func ReadSessionStore(ctx context.Context, path string, evidence *Evidence) SourceReport {
	return readFile(ctx, "session", path, func(ctx context.Context, db *sql.DB, report *SourceReport) error {
		type table struct {
			name, source string
			needed       []string
			query        string
			add          func(first, second string)
		}
		tables := []table{
			{
				name: "whatsmeow_lid_map", source: SourceSessionLIDMap, needed: []string{"LID", "PN"},
				query: `SELECT lid, pn FROM whatsmeow_lid_map`,
				add: func(lid, pn string) {
					lidUser, lidOK := lidDigits(lid, true)
					pnUser, pnOK := phoneDigits(pn)
					if lidOK && pnOK {
						evidence.AddPair(lidUser, pnUser, SourceSessionLIDMap)
					}
				},
			},
			{
				name: "whatsmeow_device", source: SourceSessionDevice, needed: []string{"JID", "LID"},
				query: `SELECT jid, lid FROM whatsmeow_device`,
				add:   func(jid, lid string) { evidence.AddJIDPair(jid, lid, SourceSessionDevice) },
			},
			{
				name: "whatsmeow_contacts", source: SourceSessionContacts, needed: []string{"THEIR_JID"},
				query: `SELECT their_jid, '' FROM whatsmeow_contacts`,
				add:   func(jid, _ string) { evidence.AddJID(jid, SourceSessionContacts) },
			},
		}
		for _, t := range tables {
			have, err := columns(ctx, db, t.name)
			if err != nil {
				return fmt.Errorf("read %s columns: %w", t.name, err)
			}
			missing := false
			for _, column := range t.needed {
				missing = missing || !have[column]
			}
			if missing {
				report.Skipped = append(report.Skipped, t.name)
				continue
			}
			rows, err := scanPairs(ctx, db, t.query, t.add)
			report.Rows += rows
			if err != nil {
				return fmt.Errorf("read %s: %w", t.name, err)
			}
		}
		return nil
	})
}

// ReadDesktop reads WhatsApp Desktop's ChatStorage.sqlite inside one read
// transaction, so it sees one consistent snapshot while Desktop writes: every
// chat, group-member and message-sender JID (ZWACHATSESSION.ZCONTACTJID,
// ZWAGROUPMEMBER.ZMEMBERJID, ZWAMESSAGE.ZFROMJID), each of which keeps its
// server, and the phone number Desktop pairs a chat or member with when its
// schema has ZCONTACTIDENTIFIER.
func ReadDesktop(ctx context.Context, path string, evidence *Evidence) SourceReport {
	return readFile(ctx, "desktop", path, func(ctx context.Context, db *sql.DB, report *SourceReport) error {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := ReadDesktopDB(ctx, tx, evidence, true, &report.Skipped)
		report.Rows = rows
		return err
	})
}

// ReadDesktopDB reads an open ChatStorage.sqlite as ReadDesktop does. With
// senders false it reads only chats and members, the rows that pair a LID
// with a number, and skips the message table.
func ReadDesktopDB(ctx context.Context, q queryer, evidence *Evidence, senders bool, skipped *[]string) (int, error) {
	total := 0
	for _, t := range []struct {
		table, jid, source string
	}{
		{"ZWACHATSESSION", "ZCONTACTJID", SourceDesktopChats},
		{"ZWAGROUPMEMBER", "ZMEMBERJID", SourceDesktopMembers},
	} {
		have, err := columns(ctx, q, t.table)
		if err != nil {
			return total, fmt.Errorf("read %s columns: %w", t.table, err)
		}
		if !have[t.jid] {
			*skipped = append(*skipped, t.table)
			continue
		}
		identifier := "''"
		if have["ZCONTACTIDENTIFIER"] {
			identifier = "ZCONTACTIDENTIFIER"
		} else {
			*skipped = append(*skipped, t.table+".ZCONTACTIDENTIFIER")
		}
		source := t.source
		rows, err := scanPairs(ctx, q,
			`SELECT `+t.jid+`, `+identifier+` FROM `+t.table,
			func(jid, identifier string) { evidence.AddJIDPair(jid, identifier, source) })
		total += rows
		if err != nil {
			return total, fmt.Errorf("read %s: %w", t.table, err)
		}
	}
	if !senders {
		return total, nil
	}
	have, err := columns(ctx, q, "ZWAMESSAGE")
	if err != nil {
		return total, fmt.Errorf("read ZWAMESSAGE columns: %w", err)
	}
	if !have["ZFROMJID"] {
		*skipped = append(*skipped, "ZWAMESSAGE")
		return total, nil
	}
	rows, err := scanPairs(ctx, q,
		`SELECT DISTINCT ZFROMJID, '' FROM ZWAMESSAGE WHERE ZFROMJID IS NOT NULL`,
		func(jid, _ string) { evidence.AddJID(jid, SourceDesktopMessages) })
	total += rows
	if err != nil {
		return total, fmt.Errorf("read ZWAMESSAGE: %w", err)
	}
	return total, nil
}

// DefaultDesktopPath is where WhatsApp Desktop for macOS keeps
// ChatStorage.sqlite.
func DefaultDesktopPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

// DesktopLIDPath is where WhatsApp Desktop keeps LID.sqlite: beside
// ChatStorage.sqlite.
func DesktopLIDPath(chatStoragePath string) string {
	return filepath.Join(filepath.Dir(chatStoragePath), "LID.sqlite")
}

// ReadDesktopLIDPairs reads the LID-to-number pairs WhatsApp Desktop keeps in
// LID.sqlite (ZWAPHONENUMBERLIDPAIR).
func ReadDesktopLIDPairs(ctx context.Context, path string, evidence *Evidence) SourceReport {
	return readFile(ctx, "desktop_lid", path, func(ctx context.Context, db *sql.DB, report *SourceReport) error {
		have, err := columns(ctx, db, "ZWAPHONENUMBERLIDPAIR")
		if err != nil {
			return fmt.Errorf("read ZWAPHONENUMBERLIDPAIR columns: %w", err)
		}
		if !have["ZLID"] || !have["ZPHONENUMBER"] {
			report.Skipped = append(report.Skipped, "ZWAPHONENUMBERLIDPAIR")
			return nil
		}
		rows, err := scanPairs(ctx, db, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`, func(lid, pn string) {
			lidUser, lidOK := lidDigits(lid, true)
			pnUser, pnOK := phoneDigits(pn)
			if lidOK && pnOK {
				evidence.AddPair(lidUser, pnUser, SourceDesktopLIDPairs)
			}
		})
		report.Rows = rows
		return err
	})
}
