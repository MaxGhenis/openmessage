-- Reduced copies of attachments (2026-10-24). Google often names only an
-- image's thumbnail and names the full-size original only after the phone is
-- asked for it. remote_preview marks a remote reference that names such a
-- reduced copy; blob_preview marks downloaded bytes that are one. With both,
-- a downloaded thumbnail can take the original's reference when an update
-- names it (projection) and then have its bytes replaced (media service).
ALTER TABLE message_attachments
    ADD COLUMN remote_preview INTEGER NOT NULL DEFAULT 0
        CHECK (remote_preview IN (0, 1));

ALTER TABLE message_attachments
    ADD COLUMN blob_preview INTEGER NOT NULL DEFAULT 0
        CHECK (blob_preview IN (0, 1) AND (blob_preview = 0 OR blob_hash IS NOT NULL));
