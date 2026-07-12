-- Durable outbound intents are claimed with short leases. The
-- transport_called_at_ms marker records the point after which retrying the
-- same intent could duplicate a remote operation.
CREATE TABLE outbox (
    outbox_id                 TEXT PRIMARY KEY CHECK (trim(outbox_id) <> ''),
    account_id                TEXT NOT NULL,
    conversation_id           TEXT NOT NULL CHECK (trim(conversation_id) <> ''),
    kind                      TEXT NOT NULL
                              CHECK (kind IN ('text', 'media', 'reaction', 'read')),
    idempotency_key           TEXT NOT NULL CHECK (trim(idempotency_key) <> ''),
    payload_hash              TEXT NOT NULL CHECK (trim(payload_hash) <> ''),
    operation                 TEXT NOT NULL CHECK (trim(operation) <> ''),
    state                     TEXT NOT NULL CHECK (
        state IN (
            'queued', 'dispatching', 'not_dispatched', 'uncertain',
            'confirmed', 'store_failed', 'rejected', 'canceled'
        )
    ),
    local_message_id          TEXT CHECK (
        local_message_id IS NULL OR trim(local_message_id) <> ''
    ),
    transport_request_id      TEXT NOT NULL
                              CHECK (trim(transport_request_id) <> ''),
    result_remote_id          TEXT CHECK (
        result_remote_id IS NULL OR trim(result_remote_id) <> ''
    ),
    error_class               TEXT,
    error_code                TEXT,
    error_detail              TEXT,
    attempt_count             INTEGER NOT NULL DEFAULT 0
                              CHECK (attempt_count >= 0),
    lease_owner               TEXT CHECK (
        lease_owner IS NULL OR trim(lease_owner) <> ''
    ),
    lease_token               TEXT CHECK (
        lease_token IS NULL OR trim(lease_token) <> ''
    ),
    lease_expires_at_ms       INTEGER CHECK (
        lease_expires_at_ms IS NULL OR lease_expires_at_ms > 0
    ),
    transport_called_at_ms    INTEGER CHECK (
        transport_called_at_ms IS NULL OR transport_called_at_ms > 0
    ),
    scheduled_for_ms          INTEGER NOT NULL CHECK (scheduled_for_ms > 0),
    next_attempt_at_ms        INTEGER CHECK (
        next_attempt_at_ms IS NULL OR next_attempt_at_ms > 0
    ),
    created_at_ms             INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms             INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    FOREIGN KEY (account_id)
        REFERENCES accounts(account_id) ON DELETE CASCADE,
    UNIQUE (account_id, idempotency_key),
    UNIQUE (account_id, transport_request_id),
    CHECK (
        (state = 'dispatching'
         AND lease_owner IS NOT NULL
         AND lease_token IS NOT NULL
         AND lease_expires_at_ms IS NOT NULL)
        OR
        (state <> 'dispatching'
         AND lease_owner IS NULL
         AND lease_token IS NULL
         AND lease_expires_at_ms IS NULL
         AND transport_called_at_ms IS NULL)
    ),
    CHECK (state <> 'not_dispatched' OR next_attempt_at_ms IS NOT NULL)
) STRICT;

CREATE INDEX outbox_due_idx
    ON outbox(state, scheduled_for_ms, next_attempt_at_ms);

CREATE INDEX outbox_expired_lease_idx
    ON outbox(lease_expires_at_ms)
    WHERE state = 'dispatching';
