package db

func (s *Store) UpsertDraft(d *Draft) error {
	_, err := s.db.Exec(`
		INSERT INTO drafts (draft_id, conversation_id, body, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(draft_id) DO UPDATE SET
			conversation_id=excluded.conversation_id,
			body=excluded.body,
			created_at=excluded.created_at
	`, d.DraftID, d.ConversationID, d.Body, d.CreatedAt)
	return err
}

func (s *Store) ListDrafts(conversationID string) ([]*Draft, error) {
	rows, err := s.db.Query(`
		SELECT draft_id, conversation_id, body, created_at
		FROM drafts
		WHERE conversation_id = ?
		ORDER BY created_at DESC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var drafts []*Draft
	for rows.Next() {
		d := &Draft{}
		if err := rows.Scan(&d.DraftID, &d.ConversationID, &d.Body, &d.CreatedAt); err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	return drafts, rows.Err()
}

func (s *Store) GetDraft(draftID string) (*Draft, error) {
	row := s.db.QueryRow(`
		SELECT draft_id, conversation_id, body, created_at
		FROM drafts WHERE draft_id = ?
	`, draftID)
	d := &Draft{}
	err := row.Scan(&d.DraftID, &d.ConversationID, &d.Body, &d.CreatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (s *Store) DeleteDraft(draftID string) error {
	_, err := s.db.Exec(`DELETE FROM drafts WHERE draft_id = ?`, draftID)
	return err
}
