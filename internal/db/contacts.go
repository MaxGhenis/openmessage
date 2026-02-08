package db

import "encoding/json"

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
func (s *Store) ListContactsFromConversations(query string, limit int) ([]*Contact, error) {
	rows, err := s.db.Query(`
		SELECT conversation_id, name, participants FROM conversations
		ORDER BY last_message_ts DESC
	`)
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
	queryLower := toLower(query)

	for rows.Next() {
		var convID, name, participantsJSON string
		if err := rows.Scan(&convID, &name, &participantsJSON); err != nil {
			continue
		}

		var participants []participant
		if err := json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
			// Fall back to conversation name if participants can't be parsed
			if name != "" && !seen[name] {
				if query == "" || containsLower(name, queryLower) {
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
			if query != "" && !containsLower(displayName, queryLower) && !containsLower(p.Number, queryLower) {
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

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func containsLower(s, sub string) bool {
	s = toLower(s)
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
