package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// InboxReceiptsAfterRow returns, per codec, the newest receipt time (ms) among
// inbox rows whose rowid exceeds afterRowID, and the largest rowid seen (or
// afterRowID when there are none). Freshness calls it with its high-water mark
// so each refresh reads only rows appended since the last one; afterRowID 0
// scans everything. Nothing deletes inbox rows in normal operation, so new
// rows get larger rowids; callers still rescan now and then in case one is
// reused.
func (s *Store) InboxReceiptsAfterRow(ctx context.Context, afterRowID int64) (map[string]int64, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT codec, MAX(received_at_ms), MAX(rowid)
		FROM inbox
		WHERE rowid > ?
		GROUP BY codec
	`, afterRowID)
	if err != nil {
		return nil, 0, fmt.Errorf("inbox receipts after row: %w", err)
	}
	defer rows.Close()
	latest := map[string]int64{}
	maxRowID := afterRowID
	for rows.Next() {
		var codec string
		var receivedAtMS, rowID int64
		if err := rows.Scan(&codec, &receivedAtMS, &rowID); err != nil {
			return nil, 0, fmt.Errorf("inbox receipts after row: scan: %w", err)
		}
		latest[codec] = receivedAtMS
		if rowID > maxRowID {
			maxRowID = rowID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("inbox receipts after row: %w", err)
	}
	return latest, maxRowID, nil
}

// InboxReceiptsBetween returns the receipt times (ms, ascending) of inbox
// frames with one of codecs received inside [fromMS, toMS].
func (s *Store) InboxReceiptsBetween(
	ctx context.Context,
	codecs []string,
	fromMS int64,
	toMS int64,
) ([]int64, error) {
	if len(codecs) == 0 || toMS < fromMS {
		return nil, nil
	}
	args := make([]any, 0, len(codecs)+2)
	for _, codec := range codecs {
		args = append(args, codec)
	}
	args = append(args, fromMS, toMS)
	rows, err := s.db.QueryContext(ctx, `
		SELECT received_at_ms
		FROM inbox
		WHERE codec IN (`+strings.TrimSuffix(strings.Repeat("?,", len(codecs)), ",")+`)
			AND received_at_ms BETWEEN ? AND ?
		ORDER BY received_at_ms
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox receipts between: %w", err)
	}
	defer rows.Close()
	var receipts []int64
	for rows.Next() {
		var receivedAtMS int64
		if err := rows.Scan(&receivedAtMS); err != nil {
			return nil, fmt.Errorf("inbox receipts between: scan: %w", err)
		}
		receipts = append(receipts, receivedAtMS)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inbox receipts between: %w", err)
	}
	return receipts, nil
}
