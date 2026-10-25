-- Trigram full-text indexes behind the v2 substring searches: message bodies
-- (SearchMessages) and conversation titles, participant names and identity
-- names and addresses (SearchConversationsByName).
--
-- Each index is an FTS5 external-content table over one table. It stores only
-- trigram postings keyed by the source row's rowid and reads the text back
-- from the source row. A query `x LIKE '%' || ? || '%'` on an indexed column
-- reads the postings of every trigram in the pattern's literal runs of three
-- or more characters, and SQLite then applies the LIKE itself to each
-- candidate row, so a search returns exactly the rows the LIKE alone would.
-- A pattern with no such run (a one- or two-character query) gets no help
-- from the index; the Go callers keep their plain LIKE statements for those.
--
-- tokenize: case_sensitive 0 folds case in the index, which is what lets
-- SQLite hand a case-insensitive LIKE to it; the index folds more than LIKE
-- does (non-ASCII letters too), which only adds candidates that the LIKE then
-- rejects. Diacritics are not removed, matching LIKE.
-- detail=none stores no positions: the trigrams of a run are matched as an AND
-- rather than a phrase, a looser candidate set the LIKE also narrows, for an
-- index a fraction of the size. columnsize=0 drops the per-row token counts
-- only relevance ranking would read; searches order by recency.
--
-- The triggers keep each index equal to its table on every write, whatever
-- statement or foreign-key cascade makes it. A REPLACE is covered because the
-- store's connections turn recursive_triggers on; without it SQLite skips the
-- delete triggers of the rows a REPLACE removes. The triggers are keyed by
-- rowid: these tables have implicit rowids that no code assigns or changes,
-- and VACUUM and VACUUM INTO keep the rowids of a table that has an index
-- (each of these has its primary-key index); tests pin both.

CREATE VIRTUAL TABLE messages_fts USING fts5(
    body,
    content = 'messages',
    content_rowid = 'rowid',
    tokenize = 'trigram case_sensitive 0',
    detail = none,
    columnsize = 0
);

CREATE TRIGGER messages_fts_after_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts (rowid, body) VALUES (new.rowid, new.body);
END;

CREATE TRIGGER messages_fts_after_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body)
    VALUES ('delete', old.rowid, old.body);
END;

CREATE TRIGGER messages_fts_after_update AFTER UPDATE ON messages
WHEN new.rowid IS NOT old.rowid OR new.body IS NOT old.body BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body)
    VALUES ('delete', old.rowid, old.body);
    INSERT INTO messages_fts (rowid, body) VALUES (new.rowid, new.body);
END;

INSERT INTO messages_fts (messages_fts) VALUES ('rebuild');

CREATE VIRTUAL TABLE conversations_fts USING fts5(
    title,
    content = 'conversations',
    content_rowid = 'rowid',
    tokenize = 'trigram case_sensitive 0',
    detail = none,
    columnsize = 0
);

CREATE TRIGGER conversations_fts_after_insert AFTER INSERT ON conversations BEGIN
    INSERT INTO conversations_fts (rowid, title) VALUES (new.rowid, new.title);
END;

CREATE TRIGGER conversations_fts_after_delete AFTER DELETE ON conversations BEGIN
    INSERT INTO conversations_fts (conversations_fts, rowid, title)
    VALUES ('delete', old.rowid, old.title);
END;

CREATE TRIGGER conversations_fts_after_update AFTER UPDATE ON conversations
WHEN new.rowid IS NOT old.rowid OR new.title IS NOT old.title BEGIN
    INSERT INTO conversations_fts (conversations_fts, rowid, title)
    VALUES ('delete', old.rowid, old.title);
    INSERT INTO conversations_fts (rowid, title) VALUES (new.rowid, new.title);
END;

INSERT INTO conversations_fts (conversations_fts) VALUES ('rebuild');

CREATE VIRTUAL TABLE conversation_participants_fts USING fts5(
    display_name,
    content = 'conversation_participants',
    content_rowid = 'rowid',
    tokenize = 'trigram case_sensitive 0',
    detail = none,
    columnsize = 0
);

CREATE TRIGGER conversation_participants_fts_after_insert
AFTER INSERT ON conversation_participants BEGIN
    INSERT INTO conversation_participants_fts (rowid, display_name)
    VALUES (new.rowid, new.display_name);
END;

CREATE TRIGGER conversation_participants_fts_after_delete
AFTER DELETE ON conversation_participants BEGIN
    INSERT INTO conversation_participants_fts (
        conversation_participants_fts, rowid, display_name
    ) VALUES ('delete', old.rowid, old.display_name);
END;

CREATE TRIGGER conversation_participants_fts_after_update
AFTER UPDATE ON conversation_participants
WHEN new.rowid IS NOT old.rowid OR new.display_name IS NOT old.display_name BEGIN
    INSERT INTO conversation_participants_fts (
        conversation_participants_fts, rowid, display_name
    ) VALUES ('delete', old.rowid, old.display_name);
    INSERT INTO conversation_participants_fts (rowid, display_name)
    VALUES (new.rowid, new.display_name);
END;

INSERT INTO conversation_participants_fts (conversation_participants_fts)
VALUES ('rebuild');

-- detail=none keeps no column information either, so a LIKE on one of these
-- two columns reads the postings of both; the LIKE on the named column then
-- keeps only that column's matches.
CREATE VIRTUAL TABLE identities_fts USING fts5(
    display_name,
    canonical_value,
    content = 'identities',
    content_rowid = 'rowid',
    tokenize = 'trigram case_sensitive 0',
    detail = none,
    columnsize = 0
);

CREATE TRIGGER identities_fts_after_insert AFTER INSERT ON identities BEGIN
    INSERT INTO identities_fts (rowid, display_name, canonical_value)
    VALUES (new.rowid, new.display_name, new.canonical_value);
END;

CREATE TRIGGER identities_fts_after_delete AFTER DELETE ON identities BEGIN
    INSERT INTO identities_fts (identities_fts, rowid, display_name, canonical_value)
    VALUES ('delete', old.rowid, old.display_name, old.canonical_value);
END;

CREATE TRIGGER identities_fts_after_update AFTER UPDATE ON identities
WHEN new.rowid IS NOT old.rowid
  OR new.display_name IS NOT old.display_name
  OR new.canonical_value IS NOT old.canonical_value BEGIN
    INSERT INTO identities_fts (identities_fts, rowid, display_name, canonical_value)
    VALUES ('delete', old.rowid, old.display_name, old.canonical_value);
    INSERT INTO identities_fts (rowid, display_name, canonical_value)
    VALUES (new.rowid, new.display_name, new.canonical_value);
END;

INSERT INTO identities_fts (identities_fts) VALUES ('rebuild');
