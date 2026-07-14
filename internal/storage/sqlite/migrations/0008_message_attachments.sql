-- Inbound attachment lifecycle. One row per attachment carried by an inbound
-- message projection. blob_hash is NULL until the media download service
-- materializes bytes into the private BlobStore; blob existence is
-- filesystem-owned, so (like outbox_attachments, 0006) there is no SQL FK for it.
CREATE TABLE message_attachments (
    message_id     TEXT NOT NULL,
    ordinal        INTEGER NOT NULL CHECK (ordinal >= 0),
    remote_id      TEXT NOT NULL DEFAULT '',
    remote_ref     BLOB NOT NULL DEFAULT x'',        -- bridge.Attachment.RemoteRef (opaque)
    filename       TEXT NOT NULL DEFAULT '',
    mime           TEXT NOT NULL DEFAULT 'application/octet-stream'
                   CHECK (trim(mime) <> ''),
    size_bytes     INTEGER CHECK (size_bytes IS NULL OR size_bytes >= 0),
    state          TEXT NOT NULL DEFAULT 'pending'
                   CHECK (state IN ('pending', 'downloaded')),
    blob_hash      TEXT CHECK (
        blob_hash IS NULL OR (
            length(blob_hash) = 64
            AND blob_hash = lower(blob_hash)
            AND blob_hash NOT GLOB '*[^0-9a-f]*'
        )
    ),
    last_error     TEXT,
    created_at_ms  INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms  INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    PRIMARY KEY (message_id, ordinal),
    FOREIGN KEY (message_id) REFERENCES messages(message_id) ON DELETE CASCADE,
    CHECK (
        (state = 'pending'    AND blob_hash IS NULL)
        OR (state = 'downloaded' AND blob_hash IS NOT NULL)
    )
) STRICT;

-- Partial index feeds the GC union (only downloaded rows pin a blob).
CREATE INDEX message_attachments_blob_hash_idx
    ON message_attachments(blob_hash)
    WHERE blob_hash IS NOT NULL;
