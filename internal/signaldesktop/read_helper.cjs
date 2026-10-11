// Reads Signal Desktop's SQLCipher database for OpenMessage's v2 history feed.
//
// It never writes. The request (database path, SQLCipher key, addon path and
// queries) arrives as JSON on stdin, so the key never shows in the process
// list. After keying, the connection is switched to query_only, every query
// must be a single SELECT (or WITH ... SELECT), and all of them run inside one
// read transaction, so they see one consistent WAL snapshot while Signal
// Desktop keeps writing. No journal_mode, synchronous or foreign_keys pragma
// runs: those are the writer's settings, not a reader's.
//
// Output is one JSON object on stdout: {"results": {name: {"rows": [...]}}}.
// A query marked optional that fails (for example a column an older Signal
// Desktop lacks) reports {"error": "..."} instead of failing the whole read.
//
// The Statement and Database wrappers mirror internal/importer's
// signal_desktop_export.cjs, which has driven this addon since 2025.
const vm = require('node:vm');
const assert = require('node:assert');

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
    const result = global.__signalSqlAddon.statementStep(
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
      const row = global.__signalSqlAddon.statementStep(
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

// readOnlyQuery accepts one SELECT statement and nothing else. query_only
// already refuses writes; this keeps a malformed request from even trying.
function readOnlyQuery(sql) {
  const trimmed = String(sql || '').trim().replace(/;\s*$/, '');
  if (!/^(SELECT|WITH)\b/i.test(trimmed)) {
    throw new Error('only SELECT queries are allowed');
  }
  if (trimmed.includes(';')) {
    throw new Error('only one statement per query is allowed');
  }
  return trimmed;
}

function main() {
  const request = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
  const { db: dbPath, key, addon: addonPath, queries } = request;
  if (!dbPath || !key || !addonPath || !Array.isArray(queries)) {
    throw new Error('request needs db, key, addon and queries');
  }
  if (!/^[0-9a-fA-F]+$/.test(key)) {
    throw new Error('key must be hex');
  }
  const addon = require(addonPath);
  global.__signalSqlAddon = addon;

  const db = new Database(addon, dbPath);
  const results = {};
  try {
    db.pragma(`key = "x'${key}'"`);
    db.pragma('query_only = ON');
    if (Number(db.pragma('query_only', { simple: true })) !== 1) {
      throw new Error('could not make the connection read-only');
    }
    db.initTokenizer();
    db.prepare('BEGIN DEFERRED').all();
    try {
      for (const query of queries) {
        if (!query || typeof query.name !== 'string' || query.name === '') {
          throw new Error('every query needs a name');
        }
        try {
          const params = Array.isArray(query.params) ? query.params : [];
          results[query.name] = {
            rows: db.prepare(readOnlyQuery(query.sql)).all(params),
          };
        } catch (error) {
          if (!query.optional) {
            throw error;
          }
          results[query.name] = { error: String(error && error.message) };
        }
      }
    } finally {
      db.prepare('COMMIT').all();
    }
  } finally {
    db.close();
  }
  process.stdout.write(JSON.stringify({ results }));
}

try {
  main();
} catch (error) {
  process.stderr.write(String(error && error.message ? error.message : error));
  process.exit(1);
}
