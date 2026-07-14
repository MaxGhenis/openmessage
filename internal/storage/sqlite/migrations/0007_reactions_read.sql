-- Reaction intent payload for outbox rows of kind 'reaction'. The target is a
-- local message; its remote identity is resolved at dispatch time.
CREATE TABLE outbox_reactions (
    outbox_id          TEXT NOT NULL,
    target_message_id  TEXT NOT NULL,
    emoji              TEXT NOT NULL CHECK (trim(emoji) <> '' AND length(emoji) <= 64),
    action             TEXT NOT NULL CHECK (action IN ('add', 'remove', 'switch')),
    created_at_ms      INTEGER NOT NULL CHECK (created_at_ms > 0),
    PRIMARY KEY (outbox_id),
    FOREIGN KEY (outbox_id) REFERENCES outbox(outbox_id) ON DELETE CASCADE,
    FOREIGN KEY (target_message_id) REFERENCES messages(message_id)
) STRICT;

CREATE INDEX outbox_reactions_target_idx ON outbox_reactions(target_message_id);

-- Read-receipt intent payload for outbox rows of kind 'read'.
CREATE TABLE outbox_read_receipts (
    outbox_id             TEXT NOT NULL,
    device_id             TEXT NOT NULL,
    last_read_message_id  TEXT NOT NULL,
    read_at_ms            INTEGER NOT NULL CHECK (read_at_ms > 0),
    created_at_ms         INTEGER NOT NULL CHECK (created_at_ms > 0),
    PRIMARY KEY (outbox_id),
    FOREIGN KEY (outbox_id) REFERENCES outbox(outbox_id) ON DELETE CASCADE,
    FOREIGN KEY (device_id) REFERENCES devices(device_id),
    FOREIGN KEY (last_read_message_id) REFERENCES messages(message_id)
) STRICT;

CREATE INDEX outbox_read_receipts_message_idx
    ON outbox_read_receipts(last_read_message_id);

-- S5b partial absorption: device-scoped monotone read cursors (plan §2.4).
-- The derived FTS migration and unread-derivation queries remain S5b/S8 scope.
-- The unique index makes the composite cursor FK legal against the merged 0004
-- messages table (which has only PK message_id and
-- UNIQUE(account_id, conversation_id, remote_message_id)).
CREATE UNIQUE INDEX messages_conversation_message_uq
    ON messages(conversation_id, message_id);

CREATE TABLE read_cursors (
    account_id            TEXT NOT NULL,
    device_id             TEXT NOT NULL,
    conversation_id       TEXT NOT NULL,
    last_read_message_id  TEXT,
    last_read_at_ms       INTEGER NOT NULL DEFAULT 0 CHECK (last_read_at_ms >= 0),
    source_updated_at_ms  INTEGER CHECK (
        source_updated_at_ms IS NULL OR source_updated_at_ms >= 0
    ),
    updated_at_ms         INTEGER NOT NULL CHECK (updated_at_ms > 0),
    PRIMARY KEY (device_id, conversation_id),
    FOREIGN KEY (account_id, device_id)
        REFERENCES devices(account_id, device_id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, conversation_id)
        REFERENCES conversations(account_id, conversation_id) ON DELETE CASCADE,
    FOREIGN KEY (conversation_id, last_read_message_id)
        REFERENCES messages(conversation_id, message_id),
    CHECK (last_read_message_id IS NULL OR last_read_at_ms > 0)
) STRICT;

CREATE INDEX read_cursors_conversation_idx
    ON read_cursors(conversation_id, last_read_at_ms DESC);
