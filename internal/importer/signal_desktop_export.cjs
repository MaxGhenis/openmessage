const vm = require('node:vm');
const assert = require('node:assert');

function loadAddon(addonPath) {
  return require(addonPath);
}

class Statement {
  #needsTranslation;
  #cache;
  #createRow;
  #translateParams;
  #native;

  constructor(addon, db, query, { persistent, pluck, bigint } = {}) {
    this.#needsTranslation = persistent === true && !pluck;
    const paramNames = [];
    this.#native = addon.statementNew(
      db,
      query,
      persistent === true,
      pluck === true,
      bigint === true,
      paramNames
    );
    const isArrayParams = paramNames.every((name) => name === null);
    const isObjectParams =
      !isArrayParams && paramNames.every((name) => typeof name === 'string');
    if (!isArrayParams && !isObjectParams) {
      throw new TypeError('Cannot mix named and anonymous params in query');
    }
    if (isArrayParams) {
      this.#translateParams = (params) => {
        if (!Array.isArray(params)) {
          throw new TypeError('Query requires an array of anonymous params');
        }
        return params;
      };
    } else {
      this.#translateParams = vm.runInThisContext(`
        (function translateParams(params) {
          if (Array.isArray(params)) {
            throw new TypeError('Query requires an object of named params');
          }
          return [
            ${paramNames
              .map((name) => `params[${JSON.stringify(name)}]`)
              .join(',\n')}
          ];
        })
      `);
    }
  }

  #updateCache(result) {
    if (this.#cache === result) {
      assert(this.#createRow !== void 0);
      return this.#createRow;
    }
    const half = result.length >>> 1;
    const lines = [];
    for (let i = 0; i < half; i += 1) {
      lines.push(`${JSON.stringify(result[i])}: value[${half} + ${i}],`);
    }
    this.#cache = result;
    const createRow = vm.runInThisContext(`(function createRow(value) {
      return {
        ${lines.join('\n')}
      };
    })`);
    this.#createRow = createRow;
    return createRow;
  }

  #checkParams(params) {
    if (params === void 0) {
      return void 0;
    }
    if (typeof params !== 'object' || params === null) {
      throw new TypeError('Params must be either object or array');
    }
    return this.#translateParams(params);
  }

  get(params) {
    const result = this.#addon.statementStep(
      this.#native,
      this.#checkParams(params),
      this.#cache,
      true
    );
    if (result === void 0) {
      return void 0;
    }
    if (!this.#needsTranslation) {
      return result;
    }
    return this.#updateCache(result)(result);
  }

  all(params) {
    const result = [];
    let singleUseParams = this.#checkParams(params);
    while (true) {
      const row = this.#addon.statementStep(
        this.#native,
        singleUseParams,
        this.#cache,
        false
      );
      singleUseParams = null;
      if (row === void 0) {
        break;
      }
      if (!this.#needsTranslation) {
        result.push(row);
        continue;
      }
      result.push(this.#updateCache(row)(row));
    }
    return result;
  }

  get #addon() {
    return global.__signalSqlAddon;
  }
}

class Database {
  #native;
  #addon;

  constructor(addon, path) {
    this.#addon = addon;
    this.#native = addon.databaseOpen(path);
  }

  pragma(source, { simple } = {}) {
    const stmt = new Statement(this.#addon, this.#native, `PRAGMA ${source}`, {
      persistent: true,
      pluck: simple === true,
    });
    return simple ? stmt.get() : stmt.all();
  }

  prepare(query, options = {}) {
    return new Statement(this.#addon, this.#native, query, {
      persistent: true,
      ...options,
    });
  }

  initTokenizer() {
    this.#addon.databaseInitTokenizer(this.#native);
  }

  close() {
    this.#addon.databaseClose(this.#native);
  }
}

function main() {
  const [dbPath, sqlKey, addonPath, sinceArg] = process.argv.slice(2);
  if (!dbPath || !sqlKey || !addonPath) {
    throw new Error('usage: signal_desktop_export.cjs <db-path> <sql-key> <addon-path> [since-ms]');
  }
  const addon = loadAddon(addonPath);
  global.__signalSqlAddon = addon;

  const sinceMS = Number.parseInt(sinceArg || '0', 10) || 0;
  const db = new Database(addon, dbPath);
  try {
    db.pragma(`key = "x'${sqlKey}'"`);
    db.pragma('journal_mode = WAL');
    db.pragma('synchronous = FULL');
    db.pragma('foreign_keys = ON');
    db.initTokenizer();

    const conversations = db.prepare(`
      SELECT
        c.id AS id,
        c.type AS type,
        COALESCE(c.name, '') AS name,
        COALESCE(c.profileName, '') AS profile_name,
        COALESCE(c.profileFamilyName, '') AS profile_family_name,
        COALESCE(c.profileFullName, '') AS profile_full_name,
        COALESCE(c.e164, '') AS e164,
        COALESCE(c.serviceId, '') AS service_id,
        COALESCE(c.groupId, '') AS group_id,
        COALESCE(c.members, '') AS members,
        COALESCE(c.active_at, 0) AS active_at
      FROM conversations c
      WHERE c.type IN ('private', 'group')
      ORDER BY c.active_at DESC, c.id ASC
    `).all();

    const messagesSQL = `
      SELECT
        m.id AS id,
        m.conversationId AS conversation_id,
        m.type AS type,
        COALESCE(m.body, '') AS body,
        COALESCE(m.sent_at, 0) AS sent_at,
        COALESCE(m.received_at, 0) AS received_at,
        COALESCE(m.source, '') AS source,
        COALESCE(m.sourceServiceId, '') AS source_service_id,
        COALESCE(json_extract(m.json, '$.quote'), '') AS quote_json,
        COALESCE(json_extract(m.json, '$.reactions'), '') AS reactions_json,
        COALESCE(a.contentType, '') AS content_type,
        COALESCE(a.path, '') AS attachment_path,
        COALESCE(a.fileName, '') AS file_name,
        COALESCE(a.caption, '') AS caption
      FROM messages m
      LEFT JOIN (
        SELECT
          ma.messageId,
          ma.contentType,
          ma.path,
          ma.fileName,
          ma.caption
        FROM message_attachments ma
        JOIN (
          SELECT messageId, MIN(rowid) AS first_rowid
          FROM message_attachments
          GROUP BY messageId
        ) first
          ON first.messageId = ma.messageId
         AND first.first_rowid = ma.rowid
      ) a ON a.messageId = m.id
      WHERE m.type IN ('incoming', 'outgoing')
    `;

    const messages = sinceMS > 0
      ? db.prepare(
          messagesSQL +
            `
              AND (CASE WHEN COALESCE(m.sent_at, 0) > 0 THEN m.sent_at ELSE m.received_at END) >= ?
              ORDER BY (CASE WHEN COALESCE(m.sent_at, 0) > 0 THEN m.sent_at ELSE m.received_at END) ASC, m.id ASC
            `
        ).all([sinceMS])
      : db.prepare(
          messagesSQL +
            `
              ORDER BY (CASE WHEN COALESCE(m.sent_at, 0) > 0 THEN m.sent_at ELSE m.received_at END) ASC, m.id ASC
            `
        ).all();

    process.stdout.write(JSON.stringify({ conversations, messages }));
  } finally {
    db.close();
  }
}

main();
