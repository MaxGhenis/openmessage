-- Inbox payload retention. A processed frame keeps its row, and with it the
-- (account_id, dedupe_key) that stops a replayed frame from projecting twice,
-- for as long as the store exists. Only its payload bytes age out: the pruner
-- empties them and stamps payload_pruned_at_ms.
--
-- quarantined_at_ms marks a frame the worker gave up on. Its payload is the
-- only record of what the frame carried, so it is never pruned.
ALTER TABLE inbox ADD COLUMN quarantined_at_ms INTEGER CHECK (
    quarantined_at_ms IS NULL
    OR (quarantined_at_ms > 0 AND processed_at_ms IS NOT NULL)
);

-- applied_at_ms marks a frame whose every event the worker applied. Processed
-- does not imply applied: a frame's first message projection marks it
-- processed, and a crash, shutdown, or failure can stop the worker before the
-- rest of its events land. Only applied frames are pruned.
ALTER TABLE inbox ADD COLUMN applied_at_ms INTEGER CHECK (
    applied_at_ms IS NULL
    OR (applied_at_ms > 0 AND processed_at_ms IS NOT NULL)
);

-- Frames processed before this migration recorded neither outcome. They are
-- taken as applied, so they age out like any other processed frame; a frame
-- quarantined or interrupted before now cannot be told apart from them.
UPDATE inbox
SET applied_at_ms = processed_at_ms
WHERE processed_at_ms IS NOT NULL;

ALTER TABLE inbox ADD COLUMN payload_pruned_at_ms INTEGER CHECK (
    payload_pruned_at_ms IS NULL
    OR (
        payload_pruned_at_ms > 0
        AND applied_at_ms IS NOT NULL
        AND quarantined_at_ms IS NULL
        AND length(payload) = 0
    )
);

-- The pruner walks applied, unquarantined frames that still hold a payload,
-- oldest processing time first.
CREATE INDEX inbox_payload_prune_idx
    ON inbox(processed_at_ms, inbox_id)
    WHERE applied_at_ms IS NOT NULL
      AND quarantined_at_ms IS NULL
      AND payload_pruned_at_ms IS NULL;
