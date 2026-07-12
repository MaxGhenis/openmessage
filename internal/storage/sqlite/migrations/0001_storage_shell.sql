PRAGMA application_id = 0x4F4D5347;

CREATE TABLE schema_migrations (
    version           INTEGER PRIMARY KEY CHECK (version > 0),
    name              TEXT NOT NULL UNIQUE CHECK (trim(name) <> ''),
    checksum_sha256   TEXT NOT NULL CHECK (length(checksum_sha256) = 64),
    applied_at_ms     INTEGER NOT NULL CHECK (applied_at_ms > 0),
    app_version       TEXT NOT NULL CHECK (trim(app_version) <> ''),
    execution_ms      INTEGER NOT NULL CHECK (execution_ms >= 0)
) STRICT;

CREATE TABLE store_metadata (
    singleton         INTEGER PRIMARY KEY CHECK (singleton = 1),
    store_instance_id TEXT NOT NULL UNIQUE
                      CHECK (length(store_instance_id) BETWEEN 20 AND 64),
    revision          INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at_ms     INTEGER NOT NULL CHECK (created_at_ms > 0)
) STRICT;

INSERT INTO store_metadata (singleton, store_instance_id, created_at_ms)
VALUES (1, :store_instance_id, :created_at_ms);
