// Bounds on the two public routes that write to Vercel Blob (lib/public-writes.mjs).
// Run from site/: bun run test
import assert from "node:assert/strict";
import { createDecipheriv, createHash } from "node:crypto";

import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import fc from "fast-check";
import { test } from "vitest";

import {
  HEARTBEAT,
  WAITLIST,
  canStore,
  createRateLimiter,
  handleHeartbeat,
  handleWaitlist,
  isInstallID,
  isValidEmail,
  readJSONBody,
  underDailyBudget,
} from "../lib/public-writes.mjs";

const KEY_HEX = "11".repeat(32);
const ENV = { BLOB_READ_WRITE_TOKEN: "test-token", WAITLIST_ENCRYPTION_KEY: KEY_HEX, VERCEL_ENV: "production" };
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
  assert.match(paused.body.error, /stopped taking sign-ups/);
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

// A Blob stand-in that fails the test if a route reads or writes it at all.
function forbiddenBlob() {
  const calls = [];
  const record = (name) => async (...args) => {
    calls.push([name, ...args]);
    throw new Error(`${name} called on a deployment that must not store`);
  };
  return { calls, put: record("put"), list: record("list") };
}

test("canStore: only a production deployment with a token", () => {
  assert.equal(canStore({ BLOB_READ_WRITE_TOKEN: "t", VERCEL_ENV: "production" }), true);
  assert.equal(canStore({ BLOB_READ_WRITE_TOKEN: "t" }), false, "a development pull sets no VERCEL_ENV");
  for (const vercelEnv of ["", "preview", "development", "Production", "staging"]) {
    assert.equal(canStore({ BLOB_READ_WRITE_TOKEN: "t", VERCEL_ENV: vercelEnv }), false, vercelEnv);
  }
  assert.equal(canStore({ VERCEL_ENV: "production" }), false, "no token, no storage");
  assert.equal(canStore({ BLOB_READ_WRITE_TOKEN: "", VERCEL_ENV: "production" }), false);
});

test("outside production nothing reads or writes Blob, even with production's token", async () => {
  const beat = { schema_version: 1, install_id: INSTALL };
  for (const VERCEL_ENV of ["preview", "development", undefined]) {
    const blob = forbiddenBlob();
    const env = { ...ENV, VERCEL_ENV };
    assert.deepEqual(await handleHeartbeat(post(beat), deps(blob, { env })), { status: 200, body: { ok: true, persisted: false } });
    const signup = await handleWaitlist(post({ email: "a@b.co" }), deps(blob, { env }));
    assert.equal(signup.status, 503);
    assert.match(signup.body.error, /can't take sign-ups/);
    assert.deepEqual(blob.calls, [], VERCEL_ENV);
  }
  const production = fakeBlob();
  const env = { ...ENV, VERCEL_ENV: "production" };
  assert.deepEqual(await handleHeartbeat(post(beat), deps(production, { env })), { status: 200, body: { ok: true } });
  assert.equal((await handleWaitlist(post({ email: "a@b.co" }), deps(production, { env }))).status, 200);
  assert.equal(production.objects.size, 2);
});

// Invariant: on any deployment other than production, no request of any
// shape reaches Blob, and every heartbeat that would have been stored gets
// the same "don't retry" answer the app gets when storage is off.
test("no request reaches Blob outside production (property)", async () => {
  const vercelEnv = fc.oneof(fc.constant(undefined), fc.string({ maxLength: 12 }).filter((value) => value !== "production"));
  const heartbeat = fc.record({
    schema_version: fc.constantFrom(1, 2),
    install_id: fc.oneof(fc.stringMatching(/^[0-9a-f]{32}$/), fc.string({ maxLength: 40 })),
    version: fc.string({ maxLength: 40 }),
  });
  const signup = fc.record({
    email: fc.oneof(fc.emailAddress(), fc.string({ maxLength: 40 })),
    interest: fc.constantFrom("", "general", "mcp", "other"),
  });
  await fc.assert(
    fc.asyncProperty(vercelEnv, fc.array(fc.oneof(heartbeat.map((h) => ["heartbeat", h]), signup.map((s) => ["waitlist", s])), { maxLength: 12 }), async (VERCEL_ENV, requests) => {
      const blob = forbiddenBlob();
      const env = { ...ENV, VERCEL_ENV };
      for (const [route, body] of requests) {
        const result = route === "heartbeat" ? await handleHeartbeat(post(body), deps(blob, { env })) : await handleWaitlist(post(body), deps(blob, { env }));
        assert.notEqual(result.status, 502, "a 502 means a storage call was made");
        if (route === "heartbeat" && result.status === 200) assert.equal(result.body.persisted, false);
        if (route === "waitlist" && result.status === 200) assert.equal(result.body.message, "Thanks.", "only the honeypot answers 200");
      }
      assert.deepEqual(blob.calls, []);
    }),
    { numRuns: 200 }
  );
});

// Invariant: whatever sequence of requests arrives, a day's folder never
// holds more than the cap, it holds exactly one object per distinct address
// accepted that day, and a refusal at the cap writes nothing.
const signupSequence = fc.array(
  fc.record({ day: fc.integer({ min: 0, max: 2 }), user: fc.integer({ min: 0, max: 8 }) }),
  { maxLength: 40 }
);
const dayOf = (index) => `2026-10-2${3 + index}`;

test("waitlist storage never exceeds the cap or the distinct sign-ups (property)", async () => {
  let pausedSeen = 0;
  await fc.assert(
    fc.asyncProperty(fc.integer({ min: 1, max: 6 }), signupSequence, async (dailyCap, requests) => {
      const blob = fakeBlob();
      const accepted = new Map();
      for (const { day, user } of requests) {
        const date = dayOf(day);
        const email = `u${user}@b.co`;
        const before = blob.objects.size;
        const result = await handleWaitlist(
          post({ email }),
          deps(blob, { now: () => Date.parse(`${date}T12:00:00Z`), dailyCap })
        );
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
    }),
    { numRuns: 120 }
  );
  assert.ok(pausedSeen > 0, "the generated sequences reached the cap");
});

// The same invariant for heartbeats: at most the cap per day, one object per
// install that was stored, and a heartbeat at the cap writes nothing.
test("heartbeat storage never exceeds the cap or the distinct installs (property)", async () => {
  let cappedSeen = 0;
  await fc.assert(
    fc.asyncProperty(fc.integer({ min: 1, max: 6 }), signupSequence, async (dailyCap, requests) => {
      const blob = fakeBlob();
      const persisted = new Map();
      for (const { day, user } of requests) {
        const date = dayOf(day);
        const installId = user.toString(16).padStart(32, "0");
        const before = blob.objects.size;
        const result = await handleHeartbeat(
          post({ schema_version: 1, install_id: installId }),
          deps(blob, { now: () => Date.parse(`${date}T12:00:00Z`), dailyCap })
        );
        assert.equal(result.status, 200);
        if (result.body.persisted === false) {
          assert.equal(blob.objects.size, before, "a capped heartbeat writes nothing");
          cappedSeen++;
        } else {
          if (!persisted.has(date)) persisted.set(date, new Set());
          persisted.get(date).add(installId);
        }
        for (const [d, installs] of persisted) {
          const stored = blob.count(`heartbeats/${d}/`);
          assert.ok(stored <= dailyCap, `${d}: ${stored} objects over a cap of ${dailyCap}`);
          assert.equal(stored, installs.size, `${d}: one object per stored install`);
        }
      }
    }),
    { numRuns: 120 }
  );
  assert.ok(cappedSeen > 0, "the generated sequences reached the cap");
});

// The file a `vercel env pull` command in `text` writes when it pulls
// production, or null. Vercel CLI 56.2.1 writes .env.local unless given a
// file, takes -e/--environment, and defaults to development.
const VALUE_FLAGS = new Set(["-e", "--environment", "-S", "--scope", "--cwd", "-t", "--token", "--git-branch", "-A", "--local-config", "-Q", "--global-config"]);
function productionPullTarget(text) {
  const words = text.split(/\s+/).map((w) => w.replace(/^[`'"(]+|[`'",;)]+$/g, "")).filter(Boolean);
  const at = words.findIndex((w, i) => w === "pull" && words[i - 1] === "env" && words[i - 2] === "vercel");
  if (at < 0) return null;
  let environment = "development";
  let file = null;
  for (let i = at + 1; i < words.length; i++) {
    const w = words[i];
    if (["&&", "||", "|", "#", "//"].includes(w)) break;
    if (w.startsWith("--environment=")) environment = w.slice("--environment=".length);
    else if (w === "-e" || w === "--environment") environment = words[++i];
    else if (VALUE_FLAGS.has(w)) i += 1;
    else if (!w.startsWith("-") && file === null) file = w;
  }
  return environment === "production" ? file || ".env.local" : null;
}
const NEXT_DEV_LOADS = /^\.env(\.local|\.development(\.local)?)?$/;

test("productionPullTarget reads the file a production pull writes", () => {
  const cases = {
    "vercel env pull .env.waitlist.local --environment production --scope s": ".env.waitlist.local",
    "vercel env pull --environment production --scope s": ".env.local",
    "vercel env pull --environment=production .env.local": ".env.local",
    "vercel env pull -e production ./.env.development.local": "./.env.development.local",
    "`vercel env pull site/.env --yes --environment production`": "site/.env",
    "vercel env pull .env.local": null,
    "vercel env pull --environment preview": null,
    "vercel env ls production": null,
  };
  for (const [command, file] of Object.entries(cases)) assert.equal(productionPullTarget(command), file, command);
});

test("no doc tells anyone to pull production env into a file next dev loads", () => {
  // next dev loads .env, .env.local, .env.development and .env.development.local;
  // a production pull writes VERCEL_ENV="production" and the Blob token there.
  const site = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  for (const file of ["README.md", "../CLAUDE.md", "scripts/read-waitlist.mjs"]) {
    const text = readFileSync(path.join(site, file), "utf8").replace(/\\\n/g, " ");
    for (const line of text.split("\n")) {
      const target = productionPullTarget(line);
      if (target !== null) assert.ok(!NEXT_DEV_LOADS.test(path.posix.basename(target)), `${file}: ${line.trim()}`);
    }
  }
});
