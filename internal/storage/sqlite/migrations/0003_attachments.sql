CREATE TABLE attachments (
    attachment_id    TEXT PRIMARY KEY CHECK (trim(attachment_id) <> ''),
    blob_hash        TEXT NOT NULL CHECK (
        length(blob_hash) = 64
        AND blob_hash = lower(blob_hash)
        AND blob_hash NOT GLOB '*[^0-9a-f]*'
    ),
    size             INTEGER NOT NULL CHECK (size >= 0),
    mime             TEXT NOT NULL CHECK (trim(mime) <> ''),
    filename         TEXT NOT NULL DEFAULT '',
    created_at_ms    INTEGER NOT NULL CHECK (created_at_ms > 0)
) STRICT;

CREATE INDEX attachments_blob_hash_idx ON attachments(blob_hash);
