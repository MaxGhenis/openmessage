-- Google Messages conversation ids are device-local row ids: a phone swap,
-- factory reset or backup restore re-keys every thread (a new "ID space"), so
-- ingest re-binds ids by participant identity. Two live threads can also share
-- a roster (two groups with the same members, two 1:1 threads with one
-- person), and those must never take each other's row. These columns let
-- ingest tell the two apart.
--
-- accounts.remote_idspace_epoch counts the device ID spaces ingest has
-- detected for the account.
--
-- conversations.remote_announced_epoch is the latest epoch in which the
-- transport announced the conversation (a ConversationEvent was applied to the
-- row). NULL marks a provisional thread: ingest minted it from a message frame
-- and no ConversationEvent has reached it yet. Rows written by anything else
-- (pre-existing rows, legacy migration, mirroring, repair) default to epoch 0.
--
-- conversations.remote_bound_epoch is the epoch in which the row's current
-- wire id was bound to it.
ALTER TABLE accounts ADD COLUMN remote_idspace_epoch INTEGER NOT NULL DEFAULT 0
    CHECK (remote_idspace_epoch >= 0);

ALTER TABLE conversations ADD COLUMN remote_announced_epoch INTEGER DEFAULT 0
    CHECK (remote_announced_epoch IS NULL OR remote_announced_epoch >= 0);

ALTER TABLE conversations ADD COLUMN remote_bound_epoch INTEGER NOT NULL DEFAULT 0
    CHECK (remote_bound_epoch >= 0);
