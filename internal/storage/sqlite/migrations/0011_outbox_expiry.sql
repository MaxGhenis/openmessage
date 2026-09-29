-- Interactive sends may carry a hard expiry. An intent that has not crossed
-- the transport boundary by expires_at_ms is canceled instead of transmitted
-- stale (2026-08-05: an overnight-queued send flushed ~15 hours later,
-- seconds behind the day-of retry, double-texting the recipient). NULL means
-- the intent never expires, which preserves the behavior of every existing
-- row and of app-initiated sends.
ALTER TABLE outbox
    ADD COLUMN expires_at_ms INTEGER CHECK (
        expires_at_ms IS NULL OR expires_at_ms > 0
    );

CREATE INDEX outbox_expiry_idx
    ON outbox(expires_at_ms)
    WHERE expires_at_ms IS NOT NULL
      AND state IN ('queued', 'not_dispatched');
