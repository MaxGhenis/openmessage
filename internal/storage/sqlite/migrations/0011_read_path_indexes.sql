-- Indexes that keep hot read paths off whole-table scans (plan audit,
-- 2026-10-09). Each serves a read that otherwise visits every row of a table
-- that grows with message history.

-- /api/status refreshes every platform's newest message and newest incoming
-- message every 30 s while the app is open. MAX(occurred_at_ms) for one
-- (account_id, direction) is a single seek here.
CREATE INDEX messages_account_direction_time_idx
    ON messages(account_id, direction, occurred_at_ms);

-- Newest-first listings across conversations (MCP get_messages, a search with
-- an empty query, optionally inside a date window) read the first rows of this
-- index instead of sorting the whole table to keep a few.
CREATE INDEX messages_time_idx
    ON messages(occurred_at_ms, message_id);

-- Every outgoing message a thread renders asks for its newest outbox row by
-- local_message_id; without this index each lookup reads all of the account's
-- outbox rows. local_message_id = ? implies the partial predicate.
CREATE INDEX outbox_local_message_idx
    ON outbox(account_id, local_message_id, created_at_ms, outbox_id)
    WHERE local_message_id IS NOT NULL;
