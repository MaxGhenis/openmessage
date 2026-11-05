// Run from site/: node --test lib/public-writes.test.mjs
import assert from "node:assert/strict";
import { createDecipheriv, createHash } from "node:crypto";
import { test } from "node:test";

import {
  HEARTBEAT,
  WAITLIST,
  createRateLimiter,
  handleHeartbeat,
  handleWaitlist,
  isInstallID,
  isValidEmail,
  readJSONBody,
  underDailyBudget,
} from "./public-writes.mjs";

const KEY_HEX = "11".repeat(32);
const ENV = { BLOB_READ_WRITE_TOKEN: "test-token", WAITLIST_ENCRYPTION_KEY: KEY_HEX };
const T0 = Date.parse("2026-10-23T12:00:00Z");
const INSTALL = "0123456789abcdef0123456789abcdef";

// An in-memory Vercel Blob: put refuses an existing pathname unless
// allowOverwrite, and list returns at most `limit` entries with hasMore.
function fakeBlob() {
  const objects = new Map();
  return {
    objects,
    async put(pathname, body, options) {
      if (objects.has(pathname) && !options.allowOverwrite) {
        throw new Error(`blob already exists: ${pathname}`);
      }
      objects.set(pathname, { body, options });
      return { pathname };
    },
    async list({ prefix, limit }) {
      const matching = [...objects.keys()].filter((p) => p.startsWith(prefix)).sort();
      return { blobs: matching.slice(0, limit).map((pathname) => ({ pathname })), hasMore: matching.length > limit };
    },
    count(prefix) {
      return [...objects.keys()].filter((p) => p.startsWith(prefix)).length;
    },
  };
}

function post(body, { ip = "203.0.113.7", headers = {} } = {}) {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  return new Request("https://openmessage.ai/api/test", {
    method: "POST",
    body: text,
    headers: { "content-type": "application/json", "x-vercel-forwarded-for": ip, ...headers },
  });
}

function deps(blob, overrides = {}) {
  return { put: blob.put, list: blob.list, env: ENV, allow: () => true, now: () => T0, log: () => {}, ...overrides };
}

function decrypt(stored) {
  const record = JSON.parse(stored.body);
  const decipher = createDecipheriv("aes-256-gcm", Buffer.from(KEY_HEX, "hex"), Buffer.from(record.iv, "base64"));
  decipher.setAuthTag(Buffer.from(record.tag, "base64"));
  return JSON.parse(Buffer.concat([decipher.update(Buffer.from(record.ciphertext, "base64")), decipher.final()]).toString("utf8"));
}

test("readJSONBody caps the body by header and by actual size", async () => {
  assert.deepEqual(await readJSONBody(post({ a: 1 }), 100), { value: { a: 1 } });
  assert.deepEqual(await readJSONBody(post("x".repeat(101)), 100), { status: 413 });
  assert.deepEqual(await readJSONBody(post({ a: 1 }, { headers: { "content-length": "5000" } }), 100), { status: 413 });
  assert.deepEqual(await readJSONBody(post("{not json"), 100), { status: 400 });
  // Multibyte text counts bytes, not characters.
  assert.deepEqual(await readJSONBody(post(JSON.stringify("é".repeat(60))), 100), { status: 413 });
});

test("formats: emails are bounded and install ids match the app's", () => {
  assert.equal(isValidEmail("a@b.co"), true);
  assert.equal(isValidEmail(`${"a".repeat(WAITLIST.maxEmailLength - 5)}@b.co`), true);
  assert.equal(isValidEmail(`${"a".repeat(WAITLIST.maxEmailLength - 4)}@b.co`), false);
  assert.equal(isInstallID(INSTALL), true);
  for (const bad of [INSTALL.toUpperCase(), INSTALL.slice(1), `${INSTALL}0`, "attacker-install-000001", 42, null]) {
    assert.equal(isInstallID(bad), false, String(bad));
  }
});

test("rate limiter: limit per window, a fresh window after it, bounded keys", () => {
  let t = 0;
  const allow = createRateLimiter({ limit: 2, windowMs: 1000, maxKeys: 2, now: () => t });
  assert.deepEqual([allow("a"), allow("a"), allow("a")], [true, true, false]);
  t = 1000;
  assert.equal(allow("a"), true);
  allow("b");
  allow("c"); // evicts the oldest key ("a") instead of growing past maxKeys
  assert.deepEqual([allow("a"), allow("a"), allow("a")], [true, true, false]);
});

test("underDailyBudget stops at the cap", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < 3; i++) await blob.put(`w/2026-10-23/${i}`, "", {});
  assert.equal(await underDailyBudget(blob.list, "w/2026-10-23/", 4), true);
  assert.equal(await underDailyBudget(blob.list, "w/2026-10-23/", 3), false);
  assert.equal(await underDailyBudget(blob.list, "w/2026-10-24/", 1), true);
});

test("waitlist: one encrypted object per address per day, and the path hides the address", async () => {
  const blob = fakeBlob();
  const first = await handleWaitlist(post({ email: "Ann@Example.com", interest: "mcp" }), deps(blob));
  assert.equal(first.status, 200);
  const again = await handleWaitlist(post({ email: "ann@example.com ", interest: "signal" }), deps(blob));
  assert.equal(again.status, 200);
  assert.equal(blob.objects.size, 1, "a repeat sign-up replaces the day's object");
  const [[pathname, stored]] = blob.objects;
  assert.match(pathname, /^waitlist\/2026-10-23\/[0-9a-f]{32}\.json$/);
  assert.ok(!pathname.includes(createHash("sha256").update("ann@example.com").digest("hex").slice(0, 32)), "unkeyed hash of the address");
  assert.ok(!stored.body.includes("ann@example.com"));
  assert.deepEqual(decrypt(stored), { email: "ann@example.com", interest: "signal", submittedAt: "2026-10-23T12:00:00.000Z", source: "openmessage.ai" });

  await handleWaitlist(post({ email: "ann@example.com" }), deps(blob, { now: () => T0 + 86_400_000 }));
  assert.equal(blob.objects.size, 2, "the next day is a new object");
});

test("waitlist: rejects without writing", async () => {
  const blob = fakeBlob();
  const cases = [
    [post({ email: "not-an-email" }), 400],
    [post({ email: `${"a".repeat(300)}@b.co` }), 400],
    [post({ email: "a@b.co", interest: "other" }), 400],
    [post("x".repeat(WAITLIST.maxBodyBytes + 1)), 413],
    [post("{"), 400],
  ];
  for (const [request, status] of cases) {
    assert.equal((await handleWaitlist(request, deps(blob))).status, status);
  }
  assert.deepEqual(await handleWaitlist(post({ email: "a@b.co", website: "spam" }), deps(blob)), { status: 200, body: { ok: true, message: "Thanks." } });
  assert.equal((await handleWaitlist(post({ email: "a@b.co" }), deps(blob, { env: { BLOB_READ_WRITE_TOKEN: "t" } }))).status, 503);
  assert.equal((await handleWaitlist(post({ email: "a@b.co" }), deps(blob, { env: { ...ENV, WAITLIST_ENCRYPTION_KEY: "abcd" } }))).status, 503);
  assert.equal(blob.objects.size, 0);
});

test("waitlist: an address past its rate gets 429 and writes nothing", async () => {
  const blob = fakeBlob();
  const allow = createRateLimiter({ ...WAITLIST.perAddress, now: () => T0 });
  for (let i = 0; i < WAITLIST.perAddress.limit; i++) {
    assert.equal((await handleWaitlist(post({ email: `u${i}@b.co` }), deps(blob, { allow }))).status, 200);
  }
  assert.equal((await handleWaitlist(post({ email: "late@b.co" }), deps(blob, { allow }))).status, 429);
  assert.equal((await handleWaitlist(post({ email: "late@b.co" }, { ip: "198.51.100.9" }), deps(blob, { allow }))).status, 200);
  assert.equal(blob.objects.size, WAITLIST.perAddress.limit + 1);
});

test("waitlist: writes stop at the daily budget", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < WAITLIST.dailyCap; i++) await blob.put(`waitlist/2026-10-23/${i}`, "", {});
  const paused = await handleWaitlist(post({ email: "a@b.co" }), deps(blob));
  assert.equal(paused.status, 503);
  assert.match(paused.body.error, /paused/);
  assert.equal(blob.count("waitlist/2026-10-23/"), WAITLIST.dailyCap);
});

test("heartbeat: one object per install per day, only for the app's id format", async () => {
  const blob = fakeBlob();
  const beat = { schema_version: 1, install_id: INSTALL, version: "0.3.0<script>", os: "darwin", arch: "arm64", platforms: { whatsapp: 1 } };
  assert.deepEqual(await handleHeartbeat(post(beat), deps(blob)), { status: 200, body: { ok: true } });
  assert.deepEqual(await handleHeartbeat(post(beat), deps(blob)), { status: 200, body: { ok: true } }, "a repeat is not a 502");
  assert.equal(blob.objects.size, 1);
  const stored = JSON.parse(blob.objects.get(`heartbeats/2026-10-23/${INSTALL}.json`).body);
  assert.equal(stored.version, "0.3.0script");
  assert.deepEqual(stored.platforms, { google_messages: false, whatsapp: true, signal: false });

  for (const bad of [{ ...beat, install_id: "attacker-install-000001" }, { ...beat, schema_version: 2 }, null]) {
    assert.equal((await handleHeartbeat(post(bad), deps(blob))).status, 400);
  }
  assert.equal((await handleHeartbeat(post("x".repeat(HEARTBEAT.maxBodyBytes + 1)), deps(blob))).status, 413);
  assert.deepEqual(await handleHeartbeat(post(beat), deps(blob, { env: {} })), { status: 200, body: { ok: true, persisted: false } });
  assert.equal((await handleHeartbeat(post(beat), deps(blob, { allow: () => false }))).status, 429);
  assert.equal(blob.objects.size, 1);
});

test("heartbeat: at the daily budget the app is told OK and nothing is written", async () => {
  const blob = fakeBlob();
  for (let i = 0; i < HEARTBEAT.dailyCap; i++) await blob.put(`heartbeats/2026-10-23/${i}`, "", {});
  const beat = { schema_version: 1, install_id: INSTALL };
  assert.deepEqual(await handleHeartbeat(post(beat), deps(blob)), { status: 200, body: { ok: true, persisted: false } });
  assert.equal(blob.count("heartbeats/2026-10-23/"), HEARTBEAT.dailyCap);
});

// Invariant: whatever sequence of requests arrives, a day's folder never
// holds more than the cap, it holds exactly one object per distinct address
// accepted that day, and a refusal at the cap writes nothing.
test("waitlist storage never exceeds the cap or the distinct sign-ups (randomized)", async () => {
  let seed = 7;
  const random = () => ((seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31);
  let pausedSeen = 0;
  for (let round = 0; round < 60; round++) {
    const blob = fakeBlob();
    const dailyCap = 1 + Math.floor(random() * 6);
    const accepted = new Map();
    for (let i = 0; i < 40; i++) {
      const date = `2026-10-2${3 + Math.floor(random() * 3)}`;
      const email = `u${Math.floor(random() * 9)}@b.co`;
      const before = blob.objects.size;
      const result = await handleWaitlist(post({ email }), deps(blob, { now: () => Date.parse(`${date}T12:00:00Z`), dailyCap }));
      if (result.status === 200) {
        if (!accepted.has(date)) accepted.set(date, new Set());
        accepted.get(date).add(email);
      } else {
        assert.equal(result.status, 503);
        assert.equal(blob.objects.size, before, "a refused sign-up writes nothing");
        pausedSeen++;
      }
      for (const [d, emails] of accepted) {
        const stored = blob.count(`waitlist/${d}/`);
        assert.ok(stored <= dailyCap, `${d}: ${stored} objects over a cap of ${dailyCap}`);
        assert.equal(stored, emails.size, `${d}: one object per distinct accepted address`);
      }
    }
  }
  assert.ok(pausedSeen > 0, "the generated sequences reached the cap");
});
