-- Inbox payload retention. A processed frame keeps its row, and with it the
-- (account_id, dedupe_key) that stops a replayed frame from projecting twice,
-- for as long as the store exists. Only its payload bytes age out: the pruner
-- empties them and stamps payload_pruned_at_ms.
--
-- quarantined_at_ms marks a frame the worker gave up on. Its payload is the
-- only record of what the frame carried, so it is never pruned. Frames
-- quarantined before this migration were not marked and age out like any
-- other processed frame.
ALTER TABLE inbox ADD COLUMN quarantined_at_ms INTEGER CHECK (
    quarantined_at_ms IS NULL
    OR (quarantined_at_ms > 0 AND processed_at_ms IS NOT NULL)
);

ALTER TABLE inbox ADD COLUMN payload_pruned_at_ms INTEGER CHECK (
    payload_pruned_at_ms IS NULL
    OR (
        payload_pruned_at_ms > 0
        AND processed_at_ms IS NOT NULL
        AND quarantined_at_ms IS NULL
        AND length(payload) = 0
    )
);

-- The pruner walks processed, unquarantined frames that still hold a payload,
-- oldest processing time first.
CREATE INDEX inbox_payload_prune_idx
    ON inbox(processed_at_ms, inbox_id)
    WHERE processed_at_ms IS NOT NULL
      AND quarantined_at_ms IS NULL
      AND payload_pruned_at_ms IS NULL;
