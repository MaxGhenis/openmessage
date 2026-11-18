// Bounds on the two public routes that write to Vercel Blob (lib/public-writes.mjs).
// Run from site/: bun run test
import assert from "node:assert/strict";
import { createDecipheriv, createHash } from "node:crypto";

import fc from "fast-check";
import { test } from "vitest";

import {
  HEARTBEAT,
  WAITLIST,
  WAITLIST_THANKS,
  createRateLimiter,
  handleHeartbeat,
  handleWaitlist,
  isInstallID,
  isValidEmail,
  readJSONBody,
  underDailyBudget,
} from "../lib/public-writes.mjs";

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

// A cross-site page can send these Content-Types (or none) without a CORS
// preflight; application/json needs one, and the routes never grant it.
test("readJSONBody reads only a JSON Content-Type, whatever its case or parameters", async () => {
  for (const type of ["application/json", "Application/JSON", "application/json; charset=utf-8"]) {
    assert.deepEqual(await readJSONBody(post({ a: 1 }, { headers: { "content-type": type } }), 100), { value: { a: 1 } }, type);
  }
  for (const type of ["text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/json"]) {
    assert.deepEqual(await readJSONBody(post({ a: 1 }, { headers: { "content-type": type } }), 100), { status: 415 }, type);
  }
  // A Blob body with no type leaves the request with no Content-Type at all.
  const untyped = new Request("https://openmessage.ai/api/test", { method: "POST", body: new Blob(['{"a":1}']) });
  assert.equal(untyped.headers.get("content-type"), null);
  assert.deepEqual(await readJSONBody(untyped, 100), { status: 415 });
});

test("formats: emails are bounded and install ids match the app's", () => {
  assert.equal(isValidEmail("a@b.co"), true);
  assert.equal(isValidEmail(`${"a".repeat(WAITLIST.maxEmailLength - 5)}@b.co`), true);
  assert.equal(isValidEmail(`${"a".repeat(WAITLIST.maxEmailLength - 4)}@b.co`), false);
  assert.equal(isValidEmail("jöhn@exämple.com"), true, "an internationalized address");
  for (const bad of ["a\u001b[8m@b.co", "\u001b]0;title\u0007@b.co", "a\u0000b@c.co", "a\u202eb@c.co", "a\u200b@b.co", "a@b.co\u009b", "a\u00ad@b.co"]) {
    assert.equal(isValidEmail(bad), false, JSON.stringify(bad));
  }
  assert.equal(isInstallID(INSTALL), true);
  for (const bad of [INSTALL.toUpperCase(), INSTALL.slice(1), `${INSTALL}0`, "attacker-install-000001", 42, null]) {
    assert.equal(isInstallID(bad), false, String(bad));
  }
});

// scripts/read-waitlist.mjs prints addresses to a terminal, so one control or
// format character anywhere makes an otherwise valid address invalid.
const controlOrFormat = fc
  .oneof(
    fc.integer({ min: 0x00, max: 0x1f }),
    fc.integer({ min: 0x7f, max: 0x9f }),
    fc.constantFrom(0xad, 0x200b, 0x200e, 0x202e, 0x2066, 0xfeff, 0xe0001)
  )
  .map((codePoint) => String.fromCodePoint(codePoint));

test("an address with a control or format character anywhere is invalid (property)", () => {
  const local = fc.stringMatching(/^[a-z0-9][a-z0-9._+-]{0,20}$/);
  const domain = fc.stringMatching(/^[a-z0-9-]{1,15}\.[a-z]{2,6}$/);
  fc.assert(
    fc.property(local, domain, controlOrFormat, fc.nat(), (name, host, char, at) => {
      assert.match(char, /^[\p{Cc}\p{Cf}]$/u);
      const address = `${name}@${host}`;
      assert.equal(isValidEmail(address), true, address);
      const i = at % (address.length + 1);
      assert.equal(isValidEmail(address.slice(0, i) + char + address.slice(i)), false);
    }),
    { numRuns: 300 }
  );
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
  assert.deepEqual(first, { status: 200, body: { ok: true, message: WAITLIST_THANKS } });
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
    // What <form enctype="text/plain"> on another site sends.
    [post({ email: "a@b.co", interest: "", x: "=" }, { headers: { "content-type": "text/plain" } }), 415],
    // SGR 8 (conceal) and an OSC 8 link, which would hide or disguise later output in a terminal.
    [post({ email: "x\u001b[8m\u001b]8;;https://evil.example/\u0007ok\u001b]8;;\u0007@b.co" }), 400],
  ];
  for (const [request, status] of cases) {
    assert.equal((await handleWaitlist(request, deps(blob))).status, status);
  }
  // Only the invalid-address error names the field, so the form marks it alone.
  assert.deepEqual(await handleWaitlist(post({ email: "a\u001b@b.co" }), deps(blob)), {
    status: 400,
    body: { error: "Enter a valid email address.", field: "email" },
  });
  assert.equal((await handleWaitlist(post({ email: "a@b.co", interest: "other" }), deps(blob))).body.field, undefined);
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
  assert.deepEqual(await handleHeartbeat(post(beat, { headers: { "content-type": "text/plain;charset=UTF-8" } }), deps(blob)), { status: 415, body: { ok: false } });
  // A charset parameter still reads as JSON; the repeat replaces the same object.
  assert.deepEqual(await handleHeartbeat(post(beat, { headers: { "content-type": "application/json; charset=utf-8" } }), deps(blob)), { status: 200, body: { ok: true } });
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
