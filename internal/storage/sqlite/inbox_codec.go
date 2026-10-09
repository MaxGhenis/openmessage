package sqlite

import (
	"context"
	"fmt"
)

// ListInboxByCodecSince returns every frame of codec received at or after
// sinceMS, processed or not, in receipt order. Projection only stamps
// processed_at_ms and never deletes a payload, so the inbox is the durable
// record of what each transport delivered, including details the projected
// tables drop, such as whether a Google message travelled as SMS or RCS.
func (s *Store) ListInboxByCodecSince(
	ctx context.Context,
	codec string,
	sinceMS int64,
) ([]InboxRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+inboxColumns+`
		FROM inbox
		WHERE codec = ? AND received_at_ms >= ?
		ORDER BY received_at_ms, inbox_id
	`, codec, sinceMS)
	if err != nil {
		return nil, fmt.Errorf("list %s inbox records since %d: %w", codec, sinceMS, err)
	}
	records, err := collectRows(rows, scanInboxRecord)
	if err != nil {
		return nil, fmt.Errorf("list %s inbox records since %d: %w", codec, sinceMS, err)
	}
	return records, nil
}
