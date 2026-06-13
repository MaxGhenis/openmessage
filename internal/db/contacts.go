package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func (s *Store) UpsertContact(c *Contact) error {
	_, err := s.db.Exec(`
		INSERT INTO contacts (contact_id, name, number)
		VALUES (?, ?, ?)
		ON CONFLICT(contact_id) DO UPDATE SET
			name=excluded.name,
			number=excluded.number
	`, c.ContactID, c.Name, c.Number)
	return err
}

func (s *Store) ListContacts(query string, limit int) ([]*Contact, error) {
	var rows_query string
	var args []any

	if query != "" {
		rows_query = `
			SELECT contact_id, name, number FROM contacts
			WHERE name LIKE ? OR number LIKE ?
			ORDER BY name
			LIMIT ?
		`
		like := "%" + query + "%"
		args = []any{like, like, limit}
	} else {
		rows_query = `
			SELECT contact_id, name, number FROM contacts
			ORDER BY name
			LIMIT ?
		`
		args = []any{limit}
	}

	rows, err := s.db.Query(rows_query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		c := &Contact{}
		if err := rows.Scan(&c.ContactID, &c.Name, &c.Number); err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

// ListContactsFromConversations extracts contacts from conversation participants
// as a fallback when the contacts table is empty.
//
// The SQL prefilter is a strict superset of the per-participant matching
// below (the participants JSON contains every name and number verbatim), so
// it never hides a match — it just keeps a filtered autocomplete keystroke
// from JSON-decoding every conversation in the store. The row LIMIT bounds
// worst-case work; it is generous because one row can fan out to many
// participants and duplicates are dropped.
func (s *Store) ListContactsFromConversations(query string, limit int) ([]*Contact, error) {
	queryFilter := strings.ToLower(query)
	rows, err := s.db.Query(`
		SELECT conversation_id, name, participants FROM conversations
		WHERE ? = '' OR instr(lower(name), ?) > 0 OR instr(lower(participants), ?) > 0
		ORDER BY last_message_ts DESC
		LIMIT ?
	`, queryFilter, queryFilter, queryFilter, limit*20)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type participant struct {
		Name   string `json:"name"`
		Number string `json:"number"`
		IsMe   bool   `json:"is_me"`
	}

	seen := map[string]bool{}
	var contacts []*Contact
	queryLower := strings.ToLower(query)

	for rows.Next() {
		var convID, name, participantsJSON string
		if err := rows.Scan(&convID, &name, &participantsJSON); err != nil {
			continue
		}

		var participants []participant
		if err := json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
			// Fall back to conversation name if participants can't be parsed
			if name != "" && !seen[name] {
				if query == "" || containsInsensitive(name, queryLower) {
					seen[name] = true
					contacts = append(contacts, &Contact{
						ContactID: convID,
						Name:      name,
					})
				}
			}
			continue
		}

		for _, p := range participants {
			if p.IsMe {
				continue
			}
			displayName := p.Name
			if displayName == "" {
				displayName = p.Number
			}
			if displayName == "" {
				continue
			}
			key := displayName + "|" + p.Number
			if seen[key] {
				continue
			}
			if query != "" && !containsInsensitive(displayName, queryLower) && !containsInsensitive(p.Number, queryLower) {
				continue
			}
			seen[key] = true
			contacts = append(contacts, &Contact{
				ContactID: convID,
				Name:      displayName,
				Number:    p.Number,
			})
		}

		if len(contacts) >= limit {
			contacts = contacts[:limit]
			break
		}
	}
	return contacts, rows.Err()
}

// UpsertUnifiedContact creates or updates a unified contact.
func (s *Store) UpsertUnifiedContact(c *UnifiedContact) error {
	_, err := s.db.Exec(`
		INSERT INTO unified_contacts (unified_id, display_name, identifiers)
		VALUES (?, ?, ?)
		ON CONFLICT(unified_id) DO UPDATE SET
			display_name=excluded.display_name,
			identifiers=excluded.identifiers
	`, c.UnifiedID, c.DisplayName, c.Identifiers)
	return err
}

// GetUnifiedContact retrieves a unified contact by ID.
func (s *Store) GetUnifiedContact(id string) (*UnifiedContact, error) {
	c := &UnifiedContact{}
	err := s.db.QueryRow(`
		SELECT unified_id, display_name, identifiers
		FROM unified_contacts WHERE unified_id = ?
	`, id).Scan(&c.UnifiedID, &c.DisplayName, &c.Identifiers)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

// ListUnifiedContacts lists unified contacts, optionally filtered by name.
func (s *Store) ListUnifiedContacts(query string, limit int) ([]*UnifiedContact, error) {
	var q string
	var args []any
	if query != "" {
		q = `SELECT unified_id, display_name, identifiers FROM unified_contacts WHERE display_name LIKE ? ORDER BY display_name LIMIT ?`
		args = []any{"%" + query + "%", limit}
	} else {
		q = `SELECT unified_id, display_name, identifiers FROM unified_contacts ORDER BY display_name LIMIT ?`
		args = []any{limit}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var contacts []*UnifiedContact
	for rows.Next() {
		c := &UnifiedContact{}
		if err := rows.Scan(&c.UnifiedID, &c.DisplayName, &c.Identifiers); err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func containsInsensitive(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), sub)
}
