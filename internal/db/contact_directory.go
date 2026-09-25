package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/maxghenis/openmessage/internal/contactsync"
)

const directoryPrefix = "google-contacts:"

// DirectoryPhone rejects extensions, emails and short codes rather than guessing
// an identity. Match full numbers only; retain every original phone in the JSON.
func DirectoryPhone(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.ContainsRune("+().- ", r) && !unicode.IsSpace(r) {
			return ""
		}
	}
	d := b.String()
	if len(d) == 10 && !strings.HasPrefix(s, "+") {
		d = "1" + d
	}
	if len(d) > 15 || (strings.HasPrefix(s, "+") && len(d) < 7) || (!strings.HasPrefix(s, "+") && len(d) < 11) {
		return ""
	}
	return "+" + d
}
func directoryNames(people []contactsync.Person) map[string]string {
	names := map[string]string{}
	owners := map[string]string{}
	for _, p := range people {
		for _, n := range p.PhoneNumbers {
			phone := DirectoryPhone(n.Value)
			if phone == "" {
				continue
			}
			if owner, ok := owners[phone]; ok && owner != p.ResourceName {
				names[phone] = ""
				continue
			}
			owners[phone] = p.ResourceName
			names[phone] = p.DisplayName()
		}
	}
	return names
}
func (s *Store) loadContactDirectory() error {
	rows, err := s.db.Query("SELECT data_json FROM google_contact_directory")
	if err != nil {
		return err
	}
	defer rows.Close()
	var people []contactsync.Person
	for rows.Next() {
		var raw string
		var p contactsync.Person
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		people = append(people, p)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.directoryNames = directoryNames(people)
	return nil
}

// ReplaceContactDirectory atomically replaces only this provider's rows after a
// complete fetch. Existing thread identity, history, titles and read state survive.
func (s *Store) ReplaceContactDirectory(people []contactsync.Person) (int, int, error) {
	s.directoryMu.Lock()
	defer s.directoryMu.Unlock()
	next := directoryNames(people)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM google_contact_directory"); err != nil {
		return 0, 0, err
	}
	if _, err = tx.Exec("DELETE FROM contacts WHERE substr(contact_id,1,?)=?", len(directoryPrefix), directoryPrefix); err != nil {
		return 0, 0, err
	}
	phones := 0
	for _, p := range people {
		raw, e := json.Marshal(p)
		if e != nil {
			return 0, 0, e
		}
		if _, err = tx.Exec("INSERT INTO google_contact_directory(resource_name,data_json) VALUES(?,?)", p.ResourceName, string(raw)); err != nil {
			return 0, 0, err
		}
		seen := map[string]bool{}
		for _, n := range p.PhoneNumbers {
			phone := DirectoryPhone(n.Value)
			if phone == "" || seen[phone] {
				continue
			}
			seen[phone] = true
			name := p.DisplayName()
			if name == "" {
				name = phone
			}
			if _, err = tx.Exec("INSERT INTO contacts(contact_id,name,number) VALUES(?,?,?)", directoryPrefix+p.ResourceName+":"+phone, name, phone); err != nil {
				return 0, 0, err
			}
			phones++
		}
	}
	rows, err := tx.Query("SELECT conversation_id,name,is_group,participants,source_platform FROM conversations WHERE source_platform IN ('sms','')")
	if err != nil {
		return 0, 0, err
	}
	var changed []*Conversation
	for rows.Next() {
		c := &Conversation{}
		if err = rows.Scan(&c.ConversationID, &c.Name, &c.IsGroup, &c.Participants, &c.SourcePlatform); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if resolveDirectoryNames(c, s.directoryNames, next) {
			changed = append(changed, c)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	for _, c := range changed {
		if _, err = tx.Exec("UPDATE conversations SET name=?,participants=? WHERE conversation_id=?", c.Name, c.Participants, c.ConversationID); err != nil {
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit contact directory: %w", err)
	}
	s.directoryNames = next
	return phones, len(changed), nil
}

func replaceDirectoryName(name, phone string, old, next map[string]string) string {
	current := next[phone]
	if current != "" && (strings.TrimSpace(name) == "" || DirectoryPhone(name) == phone || old[phone] != "" && name == old[phone]) {
		return current
	}
	// Remove a name supplied by the previous directory if the number was deleted
	// or became shared/ambiguous. Preserve labels that did not come from it.
	if current == "" && old[phone] != "" && name == old[phone] {
		return phone
	}
	return name
}
func resolveDirectoryNames(c *Conversation, old, next map[string]string) bool {
	if c.SourcePlatform != "" && c.SourcePlatform != "sms" {
		return false
	}
	var ps []map[string]json.RawMessage
	if json.Unmarshal([]byte(c.Participants), &ps) != nil {
		return false
	}
	changed := false
	peers := map[string]bool{}
	for _, p := range ps {
		var me bool
		_ = json.Unmarshal(p["is_me"], &me)
		if me {
			continue
		}
		var number, name string
		_ = json.Unmarshal(p["number"], &number)
		_ = json.Unmarshal(p["name"], &name)
		phone := DirectoryPhone(number)
		if phone == "" {
			continue
		}
		peers[phone] = true
		newName := replaceDirectoryName(name, phone, old, next)
		if newName != name {
			p["name"], _ = json.Marshal(newName)
			changed = true
		}
	}
	if !c.IsGroup && len(peers) == 1 {
		for phone := range peers {
			name := replaceDirectoryName(c.Name, phone, old, next)
			if name != c.Name {
				c.Name = name
				changed = true
			}
		}
	}
	if changed {
		raw, _ := json.Marshal(ps)
		c.Participants = string(raw)
	}
	return changed
}
