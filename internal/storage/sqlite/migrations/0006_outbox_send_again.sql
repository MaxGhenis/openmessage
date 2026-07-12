-- A replacement outbound intent keeps a durable reference to the earlier
-- terminal or uncertain intent. Deleting the earlier retained row does not
-- delete the replacement.
ALTER TABLE outbox
    ADD COLUMN send_again_of_outbox_id TEXT
        REFERENCES outbox(outbox_id) ON DELETE SET NULL
        CHECK (
            send_again_of_outbox_id IS NULL
            OR trim(send_again_of_outbox_id) <> ''
        );

CREATE INDEX outbox_send_again_of_idx
    ON outbox(send_again_of_outbox_id)
    WHERE send_again_of_outbox_id IS NOT NULL;
