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
// (or less) reads every row. Nothing deletes inbox rows in normal operation,
// so new rows get larger rowids; callers still rescan now and then in case
// one is reused.
func (s *Store) InboxReceiptsAfterRow(ctx context.Context, afterRowID int64) (map[string]int64, int64, error) {
	query, args := inboxReceiptsAfterRowQuery, []any{afterRowID}
	if afterRowID <= 0 {
		query, args = inboxLatestReceiptByCodecQuery, nil
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	rows, err := s.db.QueryContext(ctx, inboxReceiptsBetweenQuery(len(codecs)), args...)
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

// inboxReceiptsAfterRowQuery reads only the rows past a high-water mark.
// NOT INDEXED keeps it on the rowid range: offered inbox_codec_received_idx,
// SQLite prefers scanning that whole index to avoid sorting for GROUP BY,
// which turns a read of the few rows appended since the last refresh into a
// read of every row. NOT INDEXED still allows the rowid lookup.
const inboxReceiptsAfterRowQuery = `
	SELECT codec, MAX(received_at_ms), MAX(rowid)
	FROM inbox NOT INDEXED
	WHERE rowid > ?
	GROUP BY codec
`

// inboxLatestReceiptByCodecQuery is the full rescan. It steps through the
// distinct codecs in inbox_codec_received_idx, seeking past each one, and
// reads each codec's newest receipt from the end of its index range, so it
// touches a few index pages per codec however large the inbox grows. It
// returns nothing for an empty inbox. SQLite numbers inbox rowids from 1
// (AppendInbox never sets one), so every row is past a high-water mark of 0.
const inboxLatestReceiptByCodecQuery = `
	WITH RECURSIVE codecs(codec) AS (
		SELECT MIN(codec) FROM inbox
		UNION ALL
		SELECT (SELECT MIN(codec) FROM inbox WHERE codec > codecs.codec)
		FROM codecs
		WHERE codecs.codec IS NOT NULL
	)
	SELECT
		codec,
		(SELECT MAX(received_at_ms) FROM inbox WHERE inbox.codec = codecs.codec),
		(SELECT MAX(rowid) FROM inbox)
	FROM codecs
	WHERE codec IS NOT NULL
`

// inboxReceiptsBetweenQuery selects receipt times for codecCount codecs inside
// an inclusive window. With one codec, inbox_codec_received_idx answers it
// from the index alone, already in receipt order.
func inboxReceiptsBetweenQuery(codecCount int) string {
	return `
		SELECT received_at_ms
		FROM inbox
		WHERE codec IN (` + strings.TrimSuffix(strings.Repeat("?,", codecCount), ",") + `)
			AND received_at_ms BETWEEN ? AND ?
		ORDER BY received_at_ms
	`
}
