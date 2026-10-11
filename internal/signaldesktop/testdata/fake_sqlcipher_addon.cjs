// A stand-in for Signal Desktop's @signalapp/sqlcipher addon, for tests only.
// It implements the five calls read_helper.cjs makes on top of Node's
// built-in node:sqlite, so the real helper can run against a plain fixture
// database. "PRAGMA key" is an unknown pragma to plain SQLite and does nothing.
const { DatabaseSync } = require('node:sqlite');

function databaseOpen(path) {
  return new DatabaseSync(path);
}

function databaseClose(db) {
  db.close();
}

function databaseInitTokenizer() {}

function statementNew(db, query, persistent, pluck, bigint, paramNames) {
  const statement = db.prepare(query);
  const count = (query.match(/\?/g) || []).length;
  for (let i = 0; i < count; i += 1) {
    paramNames.push(null);
  }
  return { statement, persistent, pluck, iterator: null };
}

function shape(native, row) {
  if (row === undefined) {
    return undefined;
  }
  const names = Object.keys(row);
  if (native.pluck) {
    return row[names[0]];
  }
  if (!native.persistent) {
    return row;
  }
  // The real addon returns column names, then values, in one array.
  return [...names, ...names.map((name) => row[name])];
}

function statementStep(native, params, cache, isGet) {
  if (isGet) {
    return shape(native, native.statement.get(...(params || [])));
  }
  if (params !== null) {
    native.iterator = native.statement.iterate(...(params || []));
  }
  const next = native.iterator.next();
  if (next.done) {
    native.iterator = null;
    return undefined;
  }
  return shape(native, next.value);
}

module.exports = {
  databaseOpen,
  databaseClose,
  databaseInitTokenizer,
  statementNew,
  statementStep,
};
