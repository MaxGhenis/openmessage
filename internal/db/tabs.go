package db

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Tab is a user-created folder that conversations can be filed under.
// The built-in Recent (inbox) and Archive tabs are implicit and not stored here.
type Tab struct {
	TabID     string `json:"tab_id"`
	Name      string `json:"name"`
	Position  int    `json:"position"`
	CreatedAt int64  `json:"created_at"`
}

func newTabID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "tab_" + hex.EncodeToString(buf), nil
}

// CreateTab creates a new custom tab, appended after existing tabs.
func (s *Store) CreateTab(name string) (*Tab, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("tab name is required")
	}
	id, err := newTabID()
	if err != nil {
		return nil, err
	}
	var maxPos *int
	if err := s.db.QueryRow(`SELECT MAX(position) FROM tabs`).Scan(&maxPos); err != nil {
		return nil, err
	}
	pos := 0
	if maxPos != nil {
		pos = *maxPos + 1
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(
		`INSERT INTO tabs (tab_id, name, position, created_at) VALUES (?, ?, ?, ?)`,
		id, name, pos, now,
	); err != nil {
		return nil, err
	}
	return &Tab{TabID: id, Name: name, Position: pos, CreatedAt: now}, nil
}

// ListTabs returns custom tabs ordered by position.
func (s *Store) ListTabs() ([]*Tab, error) {
	rows, err := s.db.Query(`SELECT tab_id, name, position, created_at FROM tabs ORDER BY position ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tabs []*Tab
	for rows.Next() {
		t := &Tab{}
		if err := rows.Scan(&t.TabID, &t.Name, &t.Position, &t.CreatedAt); err != nil {
			return nil, err
		}
		tabs = append(tabs, t)
	}
	return tabs, rows.Err()
}

// RenameTab updates a custom tab's display name.
func (s *Store) RenameTab(tabID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tab name is required")
	}
	_, err := s.db.Exec(`UPDATE tabs SET name = ? WHERE tab_id = ?`, name, tabID)
	return err
}

// DeleteTab removes a custom tab and returns any conversations filed under it to
// Recent (inbox), so threads are never orphaned in a tab that no longer exists.
func (s *Store) DeleteTab(tabID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE conversations SET tab = '' WHERE tab = ?`, tabID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tabs WHERE tab_id = ?`, tabID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
