-- Receipt times by codec. Silence detection (internal/freshness) reads the
-- receipt times of one codec inside a time window, and inbox rows are never
-- deleted, so the table grows for as long as the store exists. The only index
-- on received_at_ms is partial (unprocessed frames), which left those reads
-- scanning every row and sorting the matches. This index answers
-- codec = ? AND received_at_ms BETWEEN ? AND ? from the index alone, already
-- in receipt order, and lets the newest receipt per codec be found by seeking
-- each codec's last entry.
CREATE INDEX inbox_codec_received_idx
    ON inbox(codec, received_at_ms);
