-- Transport frames are committed here before decoding or projection. An empty
-- dedupe key is allowed for transports that do not expose a stable event ID.
CREATE TABLE inbox (
    inbox_id         TEXT PRIMARY KEY CHECK (trim(inbox_id) <> ''),
    account_id       TEXT NOT NULL,
    generation       INTEGER NOT NULL CHECK (generation >= 0),
    dedupe_key       TEXT NOT NULL DEFAULT '' CHECK (
        dedupe_key = '' OR trim(dedupe_key) <> ''
    ),
    codec            TEXT NOT NULL CHECK (trim(codec) <> ''),
    codec_version    INTEGER NOT NULL CHECK (codec_version >= 0),
    received_at_ms   INTEGER NOT NULL CHECK (received_at_ms > 0),
    payload          BLOB NOT NULL,
    processed_at_ms  INTEGER CHECK (
        processed_at_ms IS NULL OR processed_at_ms >= received_at_ms
    ),
    FOREIGN KEY (account_id)
        REFERENCES accounts(account_id) ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX inbox_dedupe_uq
    ON inbox(account_id, dedupe_key)
    WHERE dedupe_key <> '';

CREATE INDEX inbox_unprocessed_idx
    ON inbox(received_at_ms, inbox_id)
    WHERE processed_at_ms IS NULL;

-- account_id participates in both foreign keys. SQLite therefore rejects a
-- message whose conversation or sender belongs to a different account, as
-- well as references to rows that do not exist.
CREATE TABLE messages (
    message_id          TEXT PRIMARY KEY CHECK (trim(message_id) <> ''),
    conversation_id     TEXT NOT NULL,
    account_id          TEXT NOT NULL,
    remote_message_id   TEXT NOT NULL CHECK (trim(remote_message_id) <> ''),
    sender_identity_id  TEXT,
    direction           TEXT NOT NULL
                        CHECK (direction IN ('incoming', 'outgoing')),
    body                TEXT NOT NULL DEFAULT '',
    reply_to_remote_id  TEXT CHECK (
        reply_to_remote_id IS NULL OR trim(reply_to_remote_id) <> ''
    ),
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active', 'edited', 'deleted')),
    occurred_at_ms      INTEGER NOT NULL CHECK (occurred_at_ms > 0),
    created_at_ms       INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms       INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (account_id, conversation_id)
        REFERENCES conversations(account_id, conversation_id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, sender_identity_id)
        REFERENCES identities(account_id, identity_id) ON DELETE RESTRICT,
    UNIQUE (account_id, conversation_id, remote_message_id)
) STRICT;

CREATE INDEX messages_conversation_time_idx
    ON messages(conversation_id, occurred_at_ms DESC, message_id DESC);

CREATE INDEX messages_sender_time_idx
    ON messages(sender_identity_id, occurred_at_ms DESC)
    WHERE sender_identity_id IS NOT NULL;
