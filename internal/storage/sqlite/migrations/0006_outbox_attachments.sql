-- Media metadata for outbox intents of kind 'media'. Caption and reply-to ride
-- the optimistic outgoing message row, exactly as text bodies do. blob_hash
-- references a content-addressed file under the private BlobStore root; blob
-- existence is filesystem-owned, so there is no SQL FK for it.
CREATE TABLE outbox_attachments (
    outbox_id      TEXT NOT NULL,
    ordinal        INTEGER NOT NULL CHECK (ordinal >= 0),
    blob_hash      TEXT NOT NULL CHECK (
        length(blob_hash) = 64
        AND blob_hash = lower(blob_hash)
        AND blob_hash NOT GLOB '*[^0-9a-f]*'
    ),
    size_bytes     INTEGER NOT NULL CHECK (size_bytes > 0),
    mime           TEXT NOT NULL CHECK (trim(mime) <> ''),
    filename       TEXT NOT NULL DEFAULT '',
    created_at_ms  INTEGER NOT NULL CHECK (created_at_ms > 0),
    PRIMARY KEY (outbox_id, ordinal),
    FOREIGN KEY (outbox_id)
        REFERENCES outbox(outbox_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX outbox_attachments_blob_hash_idx
    ON outbox_attachments(blob_hash);
