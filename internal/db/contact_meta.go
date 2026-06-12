package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ContactMeta holds per-person CRM metadata: free-form tags, an optional
// reach-out cadence (in days), and a cached relationship summary.
type ContactMeta struct {
	PersonKey    string   `json:"person_key"`
	DisplayName  string   `json:"display_name"`
	Tags         []string `json:"tags"`
	ReachOutDays int      `json:"reach_out_days"`
	Summary      string   `json:"summary"`
	SummaryAt    int64    `json:"summary_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

// PersonKey normalizes a display name into a stable key used to group a person
// across conversations and to key their CRM metadata.
func PersonKey(name string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// GetContactMeta returns the metadata for a person key, or a zero-value
// ContactMeta (with empty tags) if none has been saved yet.
func (s *Store) GetContactMeta(key string) (*ContactMeta, error) {
	m := &ContactMeta{PersonKey: key, Tags: []string{}}
	var tagsJSON string
	err := s.db.QueryRow(`
		SELECT display_name, tags, reach_out_days, summary, summary_at, updated_at
		FROM contact_meta WHERE person_key = ?
	`, key).Scan(&m.DisplayName, &tagsJSON, &m.ReachOutDays, &m.Summary, &m.SummaryAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return m, nil
		}
		return nil, err
	}
	m.Tags = parseTags(tagsJSON)
	return m, nil
}

// GetContactMetaMap loads all saved metadata at once, keyed by person key, for
// efficient list rendering.
func (s *Store) GetContactMetaMap() (map[string]*ContactMeta, error) {
	rows, err := s.db.Query(`SELECT person_key, display_name, tags, reach_out_days, summary, summary_at, updated_at FROM contact_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*ContactMeta{}
	for rows.Next() {
		m := &ContactMeta{}
		var tagsJSON string
		if err := rows.Scan(&m.PersonKey, &m.DisplayName, &tagsJSON, &m.ReachOutDays, &m.Summary, &m.SummaryAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Tags = parseTags(tagsJSON)
		out[m.PersonKey] = m
	}
	return out, rows.Err()
}

func (s *Store) ensureContactMeta(key, displayName string) error {
	_, err := s.db.Exec(`
		INSERT INTO contact_meta (person_key, display_name, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(person_key) DO UPDATE SET
			display_name = CASE WHEN excluded.display_name != '' THEN excluded.display_name ELSE contact_meta.display_name END
	`, key, displayName, time.Now().UnixMilli())
	return err
}

// SetContactTags replaces the tag list for a person.
func (s *Store) SetContactTags(key, displayName string, tags []string) error {
	if err := s.ensureContactMeta(key, displayName); err != nil {
		return err
	}
	clean := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		clean = append(clean, t)
	}
	b, _ := json.Marshal(clean)
	_, err := s.db.Exec(`UPDATE contact_meta SET tags = ?, updated_at = ? WHERE person_key = ?`, string(b), time.Now().UnixMilli(), key)
	return err
}

// SetContactReachOut sets how often (in days) the user wants to reach out to a
// person. 0 disables the reminder.
func (s *Store) SetContactReachOut(key, displayName string, days int) error {
	if days < 0 {
		days = 0
	}
	if err := s.ensureContactMeta(key, displayName); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE contact_meta SET reach_out_days = ?, updated_at = ? WHERE person_key = ?`, days, time.Now().UnixMilli(), key)
	return err
}

// SetContactSummary caches a generated relationship summary.
func (s *Store) SetContactSummary(key, displayName, summary string, at int64) error {
	if err := s.ensureContactMeta(key, displayName); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE contact_meta SET summary = ?, summary_at = ?, updated_at = ? WHERE person_key = ?`, summary, at, time.Now().UnixMilli(), key)
	return err
}

func parseTags(jsonStr string) []string {
	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" || jsonStr == "null" {
		return []string{}
	}
	var tags []string
	if err := json.Unmarshal([]byte(jsonStr), &tags); err != nil {
		return []string{}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}
