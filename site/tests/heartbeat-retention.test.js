// Retention cleanup for install reports (lib/heartbeat-retention.mjs) and the
// cron route that runs it, against an in-memory Blob store.
// Run from site/: bun run test
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import fc from "fast-check";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import {
  RETENTION,
  cutoffDay,
  expireHeartbeats,
  expiredHeartbeatFolders,
  handleExpireHeartbeats,
  isCronRequest,
} from "../lib/heartbeat-retention.mjs";

const siteRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const DAY_MS = 24 * 60 * 60 * 1000;
const NOW = Date.parse("2026-11-18T04:17:30Z");
const SECRET = "s3cret-cron-value-0123456789";
const ENV = { BLOB_READ_WRITE_TOKEN: "test-token", CRON_SECRET: SECRET, VERCEL_ENV: "production" };
const noSleep = async () => {};

// An in-memory Vercel Blob with the list() shape the SDK returns: pages of at
// most `limit` (capped at pageSize to exercise cursors), folded mode that
// rolls everything below the prefix's next "/" into folder strings with a
// trailing slash, and a del() that ignores missing URLs.
function fakeBlob({ pageSize = 1000 } = {}) {
  const objects = new Map();
  const deletes = [];
  const url = (pathname) => `https://store.example/${pathname}`;
  return {
    objects,
    deletes,
    add(pathname) {
      objects.set(pathname, url(pathname));
    },
    async list({ prefix = "", limit = 1000, cursor, mode = "expanded" }) {
      const matching = [...objects.keys()].filter((p) => p.startsWith(prefix)).sort();
      let entries;
      if (mode === "folded") {
        const seen = new Set();
        entries = [];
        for (const p of matching) {
          const rest = p.slice(prefix.length);
          const slash = rest.indexOf("/");
          if (slash === -1) entries.push({ blob: p });
          else {
            const folder = prefix + rest.slice(0, slash + 1);
            if (!seen.has(folder)) {
              seen.add(folder);
              entries.push({ folder });
            }
          }
        }
      } else {
        entries = matching.map((p) => ({ blob: p }));
      }
      const start = cursor ? Number(cursor) : 0;
      const size = Math.min(limit, pageSize);
      const page = entries.slice(start, start + size);
      const hasMore = start + size < entries.length;
      return {
        blobs: page.filter((e) => e.blob).map((e) => ({ pathname: e.blob, url: url(e.blob) })),
        folders: mode === "folded" ? page.filter((e) => e.folder).map((e) => e.folder) : undefined,
        hasMore,
        cursor: hasMore ? String(start + size) : undefined,
      };
    },
    async del(urls) {
      deletes.push(urls.length);
      for (const u of [].concat(urls)) {
        for (const [p, v] of objects) if (v === u) objects.delete(p);
      }
    },
  };
}

function day(offset) {
  return new Date(Date.parse("2026-11-18T00:00:00Z") + offset * DAY_MS).toISOString().slice(0, 10);
}

function cronRequest(authorization) {
  const headers = authorization === undefined ? {} : { authorization };
  return new Request("https://openmessage.ai/api/cron/expire-heartbeats", { headers });
}

test("cutoffDay counts whole UTC days back from today", () => {
  assert.equal(cutoffDay(NOW, 90), "2026-08-20");
  assert.equal(cutoffDay(Date.parse("2026-11-18T23:59:59Z"), 90), "2026-08-20", "the time of day doesn't matter");
  assert.equal(cutoffDay(Date.parse("2027-03-01T00:00:00Z"), 1), "2027-02-28");
  assert.equal(cutoffDay(Date.parse("2028-03-01T00:00:00Z"), 1), "2028-02-29");
  assert.equal(cutoffDay(Date.parse("2027-01-01T12:00:00Z"), 365), "2026-01-01");
});

test("deletes day folders on or before the cutoff and nothing else", async () => {
  const blob = fakeBlob({ pageSize: 3 });
  const keep = [
    `heartbeats/${day(-89)}/a.json`,
    `heartbeats/${day(0)}/b.json`,
    "heartbeats/latest/c.json",
    "heartbeats/2026-02-30/d.json",
    "heartbeats/loose.json",
    `waitlist/${day(-300)}/e.json`,
    `heartbeatsx/${day(-300)}/f.json`,
  ];
  const expire = [`heartbeats/${day(-90)}/g.json`, `heartbeats/${day(-91)}/h.json`, `heartbeats/${day(-400)}/i.json`];
  for (let i = 0; i < 7; i++) expire.push(`heartbeats/${day(-120)}/${i}.json`);
  for (const p of [...keep, ...expire]) blob.add(p);

  const result = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90 });
  assert.deepEqual(result, { cutoff: day(-90), folders: 4, deleted: expire.length, complete: true });
  assert.deepEqual([...blob.objects.keys()].sort(), [...keep].sort());

  const again = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90 });
  assert.deepEqual(again, { cutoff: day(-90), folders: 0, deleted: 0, complete: true }, "a second run finds nothing");
});

test("a run stops at its delete budget, and the next run finishes", async () => {
  const blob = fakeBlob({ pageSize: 4 });
  for (const d of [-200, -150, -100]) for (let i = 0; i < 10; i++) blob.add(`heartbeats/${day(d)}/${i}.json`);
  blob.add(`heartbeats/${day(-1)}/new.json`);

  const first = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90, batch: 3, maxDeletes: 14 });
  assert.equal(first.deleted, 14);
  assert.equal(first.complete, false);
  assert.ok(blob.deletes.every((n) => n <= 3), "no del() call exceeds the batch");
  assert.equal(blob.objects.size, 31 - 14, "the oldest records go first");
  assert.equal([...blob.objects.keys()].filter((p) => p.includes(day(-200))).length, 0);

  const second = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90, batch: 3, maxDeletes: 14 });
  assert.equal(second.deleted, 14);
  assert.equal(second.complete, false);
  const third = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90, batch: 3, maxDeletes: 14 });
  assert.deepEqual(third, { cutoff: day(-90), folders: 1, deleted: 2, complete: true });
  assert.deepEqual([...blob.objects.keys()], [`heartbeats/${day(-1)}/new.json`]);
});

test("deletes only blobs inside the folder, even if list() returns others", async () => {
  const blob = fakeBlob();
  blob.add(`heartbeats/${day(-100)}/a.json`);
  blob.add("waitlist/2026-08-01/keep.json");
  const leaky = async (options) => {
    const page = await blob.list(options);
    if (options.mode === "folded") return page;
    return { ...page, blobs: [...page.blobs, { pathname: "waitlist/2026-08-01/keep.json", url: blob.objects.get("waitlist/2026-08-01/keep.json") }] };
  };
  const result = await expireHeartbeats({ list: leaky, del: blob.del, sleep: noSleep, now: () => NOW });
  assert.equal(result.deleted, 1);
  assert.deepEqual([...blob.objects.keys()], ["waitlist/2026-08-01/keep.json"]);
});

test("deletes at most RETENTION.deleteBatch per call, pausing between calls", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < 120; i++) blob.add(`heartbeats/${day(-100)}/${String(i).padStart(3, "0")}.json`);
  for (let i = 0; i < 10; i++) blob.add(`heartbeats/${day(-99)}/${i}.json`);
  const sleeps = [];
  const result = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: async (ms) => sleeps.push(ms), now: () => NOW });
  assert.deepEqual(result, { cutoff: day(-RETENTION.heartbeatDays), folders: 2, deleted: 130, complete: true });
  assert.equal(RETENTION.deleteBatch, 50);
  assert.deepEqual(blob.deletes, [50, 50, 20, 10]);
  assert.deepEqual(sleeps, [RETENTION.pauseMs, RETENTION.pauseMs, RETENTION.pauseMs], "a pause before every call but the first");
  // 50 per call, one call a second, stays under Blob's 75 a second.
  assert.ok((RETENTION.deleteBatch * 1000) / RETENTION.pauseMs < 75);
});

test("a rate-limited delete waits as asked and retries; other errors stop the run", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < 3; i++) blob.add(`heartbeats/${day(-100)}/${i}.json`);
  let failures = 2;
  const flaky = async (urls) => {
    if (failures-- > 0) throw Object.assign(new Error("rate limited"), { retryAfter: 2 });
    return blob.del(urls);
  };
  const sleeps = [];
  const result = await expireHeartbeats({ list: blob.list, del: flaky, sleep: async (ms) => sleeps.push(ms), now: () => NOW });
  assert.equal(result.deleted, 3);
  assert.equal(blob.objects.size, 0);
  assert.deepEqual(sleeps, [2000, 2000]);

  blob.add(`heartbeats/${day(-100)}/x.json`);
  let calls = 0;
  const limited = async () => {
    calls += 1;
    throw Object.assign(new Error("rate limited"), { retryAfter: 60 });
  };
  sleeps.length = 0;
  await assert.rejects(expireHeartbeats({ list: blob.list, del: limited, sleep: async (ms) => sleeps.push(ms), now: () => NOW }), /rate limited/);
  assert.equal(calls, RETENTION.rateLimitRetries + 1);
  assert.deepEqual(sleeps, Array(RETENTION.rateLimitRetries).fill(RETENTION.maxRetryWaitMs), "waits are capped");

  calls = 0;
  const broken = async () => {
    calls += 1;
    throw new Error("store suspended");
  };
  await assert.rejects(expireHeartbeats({ list: blob.list, del: broken, sleep: noSleep, now: () => NOW }), /store suspended/);
  assert.equal(calls, 1, "no retry for an error that isn't a rate limit");
});

test("a page longer than the budget deletes only the budget", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < 10; i++) blob.add(`heartbeats/${day(-100)}/${i}.json`);
  for (let i = 0; i < 3; i++) blob.add(`heartbeats/${day(-95)}/${i}.json`);
  const result = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90, maxDeletes: 4 });
  assert.deepEqual(result, { cutoff: day(-90), folders: 2, deleted: 4, complete: false });
  assert.equal(blob.objects.size, 9);
  assert.equal([...blob.objects.keys()].filter((p) => p.includes(day(-95))).length, 3, "the later folder waits for the next run");
});

test("a budget that ends exactly on the last folder reports the run complete", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < 5; i++) blob.add(`heartbeats/${day(-100)}/${i}.json`);
  const result = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days: 90, maxDeletes: 5 });
  assert.deepEqual(result, { cutoff: day(-90), folders: 1, deleted: 5, complete: true });
});

test("expiredHeartbeatFolders follows the cursor across pages", async () => {
  const blob = fakeBlob({ pageSize: 2 });
  for (const d of [-95, -94, -93, -92, -91, -90, -89]) blob.add(`heartbeats/${day(d)}/x.json`);
  assert.deepEqual(await expiredHeartbeatFolders(blob.list, day(-90)), [-95, -94, -93, -92, -91, -90].map((d) => `heartbeats/${day(d)}/`));
});

// Invariant: whatever the store holds, one run with enough budget leaves
// exactly the objects outside heartbeats/, the heartbeat records dated after
// the cutoff, and anything under heartbeats/ that is not in a dated folder;
// a second run deletes nothing.
test("cleanup keeps exactly what retention allows (property)", async () => {
  const entry = fc.record({
    area: fc.constantFrom("heartbeats", "heartbeats", "heartbeats", "waitlist", "other"),
    offset: fc.integer({ min: -400, max: 2 }),
    id: fc.integer({ min: 0, max: 30 }),
    shape: fc.constantFrom("dated", "dated", "dated", "undated", "loose"),
  });
  await fc.assert(
    fc.asyncProperty(fc.array(entry, { maxLength: 60 }), fc.integer({ min: 1, max: 365 }), fc.integer({ min: 1, max: 5 }), async (entries, days, pageSize) => {
      const blob = fakeBlob({ pageSize });
      for (const { area, offset, id, shape } of entries) {
        if (shape === "loose") blob.add(`${area}/${id}.json`);
        else if (shape === "undated") blob.add(`${area}/not-a-day-${id % 3}/${id}.json`);
        else blob.add(`${area}/${day(offset)}/${id}.json`);
      }
      const before = [...blob.objects.keys()];
      const cutoff = cutoffDay(NOW, days);
      const expected = before.filter((p) => {
        const m = /^heartbeats\/(\d{4}-\d{2}-\d{2})\/[^/]+$/.exec(p);
        return !(m && m[1] <= cutoff);
      });
      const result = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days, batch: 4 });
      assert.equal(result.complete, true);
      assert.equal(result.deleted, before.length - expected.length);
      assert.deepEqual([...blob.objects.keys()].sort(), expected.sort());
      const again = await expireHeartbeats({ list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, days, batch: 4 });
      assert.equal(again.deleted, 0);
    }),
    { numRuns: 200 }
  );
});

test("isCronRequest needs CRON_SECRET and its exact bearer header", () => {
  assert.equal(isCronRequest(cronRequest(`Bearer ${SECRET}`), ENV), true);
  assert.equal(isCronRequest(cronRequest(`Bearer ${SECRET}x`), ENV), false);
  assert.equal(isCronRequest(cronRequest(SECRET), ENV), false);
  // Headers trims values, so build the raw header "Bearer " by hand: with no
  // CRON_SECRET it would equal `Bearer ${""}`.
  const rawBearer = { headers: { get: (name) => (name === "authorization" ? "Bearer " : null) } };
  for (const CRON_SECRET of [undefined, ""]) assert.equal(isCronRequest(rawBearer, { ...ENV, CRON_SECRET }), false);
  assert.equal(isCronRequest(cronRequest(undefined), ENV), false);
  assert.equal(isCronRequest(cronRequest("Bearer "), { ...ENV, CRON_SECRET: "" }), false, "an unset secret authorizes nothing");
  assert.equal(isCronRequest(cronRequest("Bearer undefined"), { BLOB_READ_WRITE_TOKEN: "t" }), false);
});

test("the handler refuses without the secret and stores nothing outside production", async () => {
  const calls = [];
  const forbidden = (name) => async () => {
    calls.push(name);
    throw new Error(`${name} must not run`);
  };
  const deps = { list: forbidden("list"), del: forbidden("del"), now: () => NOW, log: () => {} };
  assert.deepEqual(await handleExpireHeartbeats(cronRequest(undefined), { ...deps, env: ENV }), { status: 401, body: { ok: false } });
  assert.deepEqual(await handleExpireHeartbeats(cronRequest("Bearer wrong"), { ...deps, env: ENV }), { status: 401, body: { ok: false } });
  assert.deepEqual(await handleExpireHeartbeats(cronRequest("Bearer "), { ...deps, env: { BLOB_READ_WRITE_TOKEN: "t" } }), { status: 401, body: { ok: false } });
  for (const VERCEL_ENV of ["preview", "development", undefined]) {
    assert.deepEqual(await handleExpireHeartbeats(cronRequest(`Bearer ${SECRET}`), { ...deps, env: { ...ENV, VERCEL_ENV } }), {
      status: 200,
      body: { ok: true, deleted: 0, storage: false },
    });
  }
  assert.deepEqual(calls, []);

  const blob = fakeBlob();
  blob.add(`heartbeats/${day(-100)}/old.json`);
  const ran = await handleExpireHeartbeats(cronRequest(`Bearer ${SECRET}`), { list: blob.list, del: blob.del, sleep: noSleep, now: () => NOW, log: () => {}, env: ENV });
  assert.deepEqual(ran, { status: 200, body: { ok: true, cutoff: day(-RETENTION.heartbeatDays), folders: 1, deleted: 1, complete: true } });
  assert.equal((await handleExpireHeartbeats(cronRequest(`Bearer ${SECRET}`), { ...deps, env: ENV })).status, 502, "a storage error answers 502");
});

const list = vi.fn();
const del = vi.fn();
vi.mock("@vercel/blob", () => ({ list: (...a) => list(...a), del: (...a) => del(...a), put: vi.fn() }));

describe("GET /api/cron/expire-heartbeats", () => {
  beforeEach(() => {
    list.mockReset();
    del.mockReset();
    list.mockResolvedValue({ blobs: [], folders: [], hasMore: false });
    process.env.BLOB_READ_WRITE_TOKEN = "test-token";
    process.env.CRON_SECRET = SECRET;
    process.env.VERCEL_ENV = "production";
    vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    delete process.env.BLOB_READ_WRITE_TOKEN;
    delete process.env.CRON_SECRET;
    delete process.env.VERCEL_ENV;
    vi.restoreAllMocks();
  });

  test("runs only for Vercel's bearer header, and site/vercel.json schedules it daily", async () => {
    const { GET } = await import("../app/api/cron/expire-heartbeats/route.js");
    const refused = await GET(cronRequest("Bearer nope"));
    expect(refused.status).toBe(401);
    expect(list).not.toHaveBeenCalled();

    const ok = await GET(cronRequest(`Bearer ${SECRET}`));
    expect(ok.status).toBe(200);
    expect(ok.headers.get("cache-control")).toBe("no-store");
    expect(await ok.json()).toMatchObject({ ok: true, deleted: 0, complete: true });
    expect(list).toHaveBeenCalledWith(expect.objectContaining({ prefix: "heartbeats/", mode: "folded" }));
    expect(del).not.toHaveBeenCalled();

    const config = JSON.parse(readFileSync(path.join(siteRoot, "vercel.json"), "utf8"));
    expect(config.crons).toEqual([{ path: "/api/cron/expire-heartbeats", schedule: expect.stringMatching(/^\d{1,2} \d{1,2} \* \* \*$/) }]);
  });
});

test("the privacy page states the retention period the cron enforces", () => {
  const page = readFileSync(path.join(siteRoot, "app/privacy/page.jsx"), "utf8").replace(/\s+/g, " ");
  expect(page).toContain(`deletes each record once it is ${RETENTION.heartbeatDays} days old`);
  expect(page).not.toMatch(/Nothing deletes them/);
});
