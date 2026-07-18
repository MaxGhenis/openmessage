-- Inbound reaction read model (M3/D2). One row per person per message; emoji is a
-- mutable attribute. Removals are tombstones (state='removed'), never deletes, so
-- replayed/out-of-order frames converge by last-writer-wins. Distinct from the
-- send-side outbox_reactions table in 0007.
CREATE TABLE reactions (
    message_id           TEXT NOT NULL,
    reactor_key          TEXT NOT NULL CHECK (trim(reactor_key) <> ''),
    account_id           TEXT NOT NULL,
    conversation_id      TEXT NOT NULL,
    reactor_identity_id  TEXT,                       -- FK identities; NULL for self / unattributed
    reactor_is_self      INTEGER NOT NULL DEFAULT 0 CHECK (reactor_is_self IN (0, 1)),
    reactor_label        TEXT NOT NULL DEFAULT '',   -- raw actor token for display when identity is NULL and not self
    emoji                TEXT NOT NULL CHECK (
        (state = 'removed' OR trim(emoji) != '') AND length(emoji) <= 64
    ),
    state                TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'removed')),
    occurred_at_ms       INTEGER NOT NULL CHECK (occurred_at_ms > 0),
    source_seq_ms        INTEGER NOT NULL DEFAULT 0, -- frame ReceivedAt; monotone guard for snapshot reconcile
    created_at_ms        INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms        INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    PRIMARY KEY (message_id, reactor_key),
    FOREIGN KEY (message_id)
        REFERENCES messages(message_id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, conversation_id)
        REFERENCES conversations(account_id, conversation_id) ON DELETE CASCADE,
    FOREIGN KEY (reactor_identity_id)
        REFERENCES identities(identity_id)
) STRICT;

-- Read-path: active reactions for a batch of messages (v2read DTO aggregation).
CREATE INDEX reactions_message_state_idx ON reactions(message_id, state);
-- Conversation-scoped maintenance / CASCADE support.
CREATE INDEX reactions_conversation_idx ON reactions(conversation_id);

-- Per-message monotone fence for Google embedded-snapshot reconcile. This is
-- separate because an empty snapshot writes no reaction rows yet must still
-- advance the fence. Delta-mode platforms never touch it.
CREATE TABLE reaction_snapshot_fences (
    message_id     TEXT NOT NULL PRIMARY KEY
        REFERENCES messages(message_id) ON DELETE CASCADE,
    source_seq_ms  INTEGER NOT NULL CHECK (source_seq_ms >= 0),
    updated_at_ms  INTEGER NOT NULL CHECK (updated_at_ms > 0)
) STRICT;
