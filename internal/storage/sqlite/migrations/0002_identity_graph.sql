CREATE TABLE accounts (
    account_id        TEXT PRIMARY KEY CHECK (trim(account_id) <> ''),
    bridge_key        TEXT NOT NULL CHECK (trim(bridge_key) <> ''),
    remote_account_id TEXT CHECK (
        remote_account_id IS NULL OR trim(remote_account_id) <> ''
    ),
    display_name      TEXT NOT NULL DEFAULT '',
    mode              TEXT NOT NULL DEFAULT 'live'
                      CHECK (mode IN ('live', 'archive')),
    enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    config_json       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config_json)),
    created_at_ms     INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms     INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms)
) STRICT;

CREATE UNIQUE INDEX accounts_remote_uq
    ON accounts(bridge_key, remote_account_id)
    WHERE remote_account_id IS NOT NULL;

CREATE TABLE devices (
    device_id         TEXT PRIMARY KEY CHECK (trim(device_id) <> ''),
    account_id        TEXT NOT NULL,
    remote_device_id  TEXT CHECK (
        remote_device_id IS NULL OR trim(remote_device_id) <> ''
    ),
    kind              TEXT NOT NULL
                      CHECK (kind IN ('local_installation', 'remote_endpoint')),
    display_name      TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL DEFAULT 'active'
                      CHECK (state IN ('active', 'revoked', 'unknown')),
    is_current        INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0, 1)),
    last_seen_at_ms   INTEGER CHECK (
        last_seen_at_ms IS NULL OR last_seen_at_ms >= 0
    ),
    created_at_ms     INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms     INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (account_id)
        REFERENCES accounts(account_id) ON DELETE CASCADE,
    UNIQUE (account_id, device_id)
) STRICT;

CREATE UNIQUE INDEX devices_remote_uq
    ON devices(account_id, remote_device_id)
    WHERE remote_device_id IS NOT NULL;

CREATE UNIQUE INDEX devices_current_local_uq
    ON devices(account_id)
    WHERE kind = 'local_installation' AND is_current = 1;

CREATE TABLE identities (
    identity_id       TEXT PRIMARY KEY CHECK (trim(identity_id) <> ''),
    account_id        TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (trim(kind) <> ''),
    canonical_value   TEXT NOT NULL CHECK (trim(canonical_value) <> ''),
    raw_value         TEXT NOT NULL CHECK (trim(raw_value) <> ''),
    display_name      TEXT NOT NULL DEFAULT '',
    is_self           INTEGER NOT NULL DEFAULT 0 CHECK (is_self IN (0, 1)),
    metadata_json     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    created_at_ms     INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms     INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (account_id)
        REFERENCES accounts(account_id) ON DELETE CASCADE,
    UNIQUE (account_id, kind, canonical_value),
    UNIQUE (account_id, identity_id)
) STRICT;

CREATE INDEX identities_value_idx
    ON identities(kind, canonical_value);

-- People and their identity links are created explicitly. Display names are
-- deliberately non-unique and never cause identities or people to be merged.
CREATE TABLE people (
    person_id             TEXT PRIMARY KEY CHECK (trim(person_id) <> ''),
    display_name          TEXT NOT NULL DEFAULT '',
    sort_name             TEXT NOT NULL DEFAULT '',
    merged_into_person_id TEXT,
    created_at_ms         INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms         INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (merged_into_person_id)
        REFERENCES people(person_id) ON DELETE RESTRICT,
    CHECK (
        merged_into_person_id IS NULL OR merged_into_person_id <> person_id
    )
) STRICT;

CREATE TABLE person_identities (
    identity_id       TEXT PRIMARY KEY,
    person_id         TEXT NOT NULL,
    provenance        TEXT NOT NULL CHECK (
        provenance IN (
            'explicit', 'address_book', 'bridge_alias', 'import', 'heuristic'
        )
    ),
    confidence        REAL NOT NULL CHECK (confidence BETWEEN 0.0 AND 1.0),
    is_primary        INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
    linked_at_ms      INTEGER NOT NULL CHECK (linked_at_ms > 0),
    FOREIGN KEY (identity_id)
        REFERENCES identities(identity_id) ON DELETE CASCADE,
    FOREIGN KEY (person_id)
        REFERENCES people(person_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX person_identities_person_idx
    ON person_identities(person_id);

CREATE UNIQUE INDEX person_primary_identity_uq
    ON person_identities(person_id)
    WHERE is_primary = 1;

CREATE TABLE conversations (
    conversation_id        TEXT PRIMARY KEY CHECK (trim(conversation_id) <> ''),
    account_id             TEXT NOT NULL,
    remote_conversation_id TEXT NOT NULL CHECK (trim(remote_conversation_id) <> ''),
    kind                   TEXT NOT NULL
                           CHECK (kind IN ('direct', 'group', 'broadcast', 'system')),
    title                  TEXT NOT NULL DEFAULT '',
    remote_revision        TEXT,
    notification_mode      TEXT NOT NULL DEFAULT 'all'
                           CHECK (notification_mode IN ('all', 'mentions', 'muted')),
    is_favorite            INTEGER NOT NULL DEFAULT 0 CHECK (is_favorite IN (0, 1)),
    archived_at_ms         INTEGER CHECK (
        archived_at_ms IS NULL OR archived_at_ms >= 0
    ),
    last_message_at_ms     INTEGER NOT NULL DEFAULT 0
                           CHECK (last_message_at_ms >= 0),
    metadata_json          TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    created_at_ms          INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms          INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (account_id)
        REFERENCES accounts(account_id) ON DELETE CASCADE,
    UNIQUE (account_id, remote_conversation_id),
    UNIQUE (account_id, conversation_id)
) STRICT;

CREATE INDEX conversations_recency_idx
    ON conversations(archived_at_ms, last_message_at_ms DESC, conversation_id);

-- account_id is intentionally repeated here. These two composite foreign keys
-- require each participant's conversation and identity to share that account,
-- so both cross-account participants and orphan identities fail declaratively.
CREATE TABLE conversation_participants (
    account_id        TEXT NOT NULL,
    conversation_id   TEXT NOT NULL,
    identity_id       TEXT NOT NULL,
    role              TEXT NOT NULL DEFAULT 'member'
                      CHECK (role IN ('member', 'admin', 'owner', 'unknown')),
    display_name      TEXT NOT NULL DEFAULT '',
    is_active         INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
    joined_at_ms      INTEGER CHECK (joined_at_ms IS NULL OR joined_at_ms >= 0),
    left_at_ms        INTEGER CHECK (left_at_ms IS NULL OR left_at_ms >= 0),
    PRIMARY KEY (conversation_id, identity_id),
    FOREIGN KEY (account_id, conversation_id)
        REFERENCES conversations(account_id, conversation_id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, identity_id)
        REFERENCES identities(account_id, identity_id) ON DELETE RESTRICT
) STRICT;

CREATE INDEX conversation_participants_identity_idx
    ON conversation_participants(identity_id, conversation_id);
