package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func NormalizeAvatarPhone(phone string) string {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return strings.TrimSpace(phone)
	}
	value := digits.String()
	if len(value) == 10 {
		return "+1" + value
	}
	if len(value) == 11 && strings.HasPrefix(value, "1") {
		return "+" + value
	}
	if strings.HasPrefix(strings.TrimSpace(phone), "+") {
		return "+" + value
	}
	return value
}

func avatarCandidateKey(c ContactAvatarCandidate) (source, participantID, contactID, phone string) {
	source = strings.ToLower(strings.TrimSpace(c.SourcePlatform))
	if source == "" {
		source = "sms"
	}
	participantID = strings.TrimSpace(c.ParticipantID)
	contactID = strings.TrimSpace(c.ContactID)
	phone = NormalizeAvatarPhone(c.PhoneNumber)
	return source, participantID, contactID, phone
}

func ContactAvatarID(c ContactAvatarCandidate) string {
	source, participantID, contactID, phone := avatarCandidateKey(c)
	switch {
	case participantID != "":
		return fmt.Sprintf("%s:participant:%s", source, participantID)
	case contactID != "":
		return fmt.Sprintf("%s:contact:%s", source, contactID)
	case phone != "":
		return fmt.Sprintf("%s:phone:%s", source, phone)
	default:
		return ""
	}
}

func (s *Store) UpsertContactAvatar(candidate ContactAvatarCandidate, imageData []byte, mimeType, imageHash string, nowMS int64) error {
	source, participantID, contactID, phone := avatarCandidateKey(candidate)
	avatarID := ContactAvatarID(candidate)
	if avatarID == "" {
		return nil
	}
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	displayName := strings.TrimSpace(candidate.DisplayName)
	_, err := s.db.Exec(`
		INSERT INTO contact_avatars (
			avatar_id, source_platform, participant_id, contact_id, phone_number,
			display_name, mime_type, image_data, image_hash, updated_at_ms, last_checked_at_ms
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(avatar_id) DO UPDATE SET
			source_platform=excluded.source_platform,
			participant_id=excluded.participant_id,
			contact_id=excluded.contact_id,
			phone_number=excluded.phone_number,
			display_name=CASE WHEN excluded.display_name != '' THEN excluded.display_name ELSE contact_avatars.display_name END,
			mime_type=CASE WHEN excluded.image_hash != contact_avatars.image_hash THEN excluded.mime_type ELSE contact_avatars.mime_type END,
			image_data=CASE WHEN excluded.image_hash != contact_avatars.image_hash THEN excluded.image_data ELSE contact_avatars.image_data END,
			image_hash=CASE WHEN excluded.image_hash != contact_avatars.image_hash THEN excluded.image_hash ELSE contact_avatars.image_hash END,
			updated_at_ms=CASE WHEN excluded.image_hash != contact_avatars.image_hash THEN excluded.updated_at_ms ELSE contact_avatars.updated_at_ms END,
			last_checked_at_ms=excluded.last_checked_at_ms
	`, avatarID, source, participantID, contactID, phone, displayName, mimeType, imageData, imageHash, nowMS, nowMS)
	return err
}

func (s *Store) MarkContactAvatarChecked(candidate ContactAvatarCandidate, nowMS int64) error {
	source, participantID, contactID, phone := avatarCandidateKey(candidate)
	avatarID := ContactAvatarID(candidate)
	if avatarID == "" {
		return nil
	}
	displayName := strings.TrimSpace(candidate.DisplayName)
	_, err := s.db.Exec(`
		INSERT INTO contact_avatars (
			avatar_id, source_platform, participant_id, contact_id, phone_number,
			display_name, last_checked_at_ms
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(avatar_id) DO UPDATE SET
			display_name=CASE WHEN excluded.display_name != '' THEN excluded.display_name ELSE contact_avatars.display_name END,
			last_checked_at_ms=excluded.last_checked_at_ms
	`, avatarID, source, participantID, contactID, phone, displayName, nowMS)
	return err
}

func (s *Store) GetContactAvatar(sourcePlatform, participantID, contactID, phoneNumber string) (*ContactAvatar, error) {
	source := strings.ToLower(strings.TrimSpace(sourcePlatform))
	if source == "" {
		source = "sms"
	}
	participantID = strings.TrimSpace(participantID)
	contactID = strings.TrimSpace(contactID)
	phoneNumber = NormalizeAvatarPhone(phoneNumber)
	queries := []struct {
		where string
		args  []any
	}{
		{"source_platform = ? AND participant_id = ? AND participant_id != ''", []any{source, participantID}},
		{"source_platform = ? AND contact_id = ? AND contact_id != ''", []any{source, contactID}},
		{"source_platform = ? AND phone_number = ? AND phone_number != ''", []any{source, phoneNumber}},
	}
	for _, q := range queries {
		if len(q.args) < 2 || q.args[1] == "" {
			continue
		}
		row := s.db.QueryRow(`
			SELECT avatar_id, source_platform, participant_id, contact_id, phone_number,
				display_name, mime_type, image_data, image_hash, updated_at_ms, last_checked_at_ms
			FROM contact_avatars
			WHERE `+q.where+`
			ORDER BY CASE WHEN image_hash != '' THEN 0 ELSE 1 END, updated_at_ms DESC
			LIMIT 1
		`, q.args...)
		avatar := &ContactAvatar{}
		err := row.Scan(
			&avatar.AvatarID,
			&avatar.SourcePlatform,
			&avatar.ParticipantID,
			&avatar.ContactID,
			&avatar.PhoneNumber,
			&avatar.DisplayName,
			&avatar.MimeType,
			&avatar.ImageData,
			&avatar.ImageHash,
			&avatar.UpdatedAtMS,
			&avatar.LastCheckedAtMS,
		)
		if err == nil {
			return avatar, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return nil, nil
}
