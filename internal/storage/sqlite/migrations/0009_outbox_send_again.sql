-- M5 SendAgain linkage: a new outbound intent created by "Send Again" records
-- the ambiguous/rejected prior intent it supersedes. outbox_id is globally
-- unique (PRIMARY KEY, 0005), so a single-column self reference suffices;
-- account scoping is enforced in the service. Nullable, no default: SQLite
-- forbids ADD COLUMN of a REFERENCES column with a non-NULL default under
-- enforced foreign keys, and PRAGMA foreign_key_check (run post-migration)
-- passes trivially because every existing row gets NULL.
ALTER TABLE outbox
    ADD COLUMN send_again_of_outbox_id TEXT
    REFERENCES outbox(outbox_id) ON DELETE SET NULL;

CREATE INDEX outbox_send_again_of_idx
    ON outbox(send_again_of_outbox_id)
    WHERE send_again_of_outbox_id IS NOT NULL;
