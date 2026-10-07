package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// LatestInboxReceipts returns the newest inbox receipt time (ms) per codec.
// Freshness reads it to tell how long each transport has been silent: the
// inbox holds every frame a transport delivered, decoded or not.
func (s *Store) LatestInboxReceipts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT codec, MAX(received_at_ms)
		FROM inbox
		GROUP BY codec
	`)
	if err != nil {
		return nil, fmt.Errorf("latest inbox receipts: %w", err)
	}
	defer rows.Close()
	latest := map[string]int64{}
	for rows.Next() {
		var codec string
		var receivedAtMS int64
		if err := rows.Scan(&codec, &receivedAtMS); err != nil {
			return nil, fmt.Errorf("latest inbox receipts: scan: %w", err)
		}
		latest[codec] = receivedAtMS
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest inbox receipts: %w", err)
	}
	return latest, nil
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
