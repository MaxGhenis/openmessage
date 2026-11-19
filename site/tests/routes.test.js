// The server routes as Next.js serves them, with Vercel Blob replaced by a
// mock. tests/public-writes.test.js covers the bounds in lib/public-writes.mjs
// directly; this file checks that the routes wire them up and that what the
// waitlist stores, scripts/read-waitlist.mjs reads back.
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";

import fc from "fast-check";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { MAIN_COMMIT, RELEASE, REPO_URL } from "../data/facts.js";
import { HEARTBEAT, WAITLIST, WAITLIST_THANKS, isJSONContentType } from "../lib/public-writes.mjs";
import { fileAt, git, listAt } from "./git.js";

const put = vi.fn();
const list = vi.fn();
vi.mock("@vercel/blob", () => ({ put: (...args) => put(...args), list: (...args) => list(...args) }));

const { POST: waitlistPost } = await import("../app/api/waitlist/route.js");
const { GET: heartbeatGet, POST: heartbeatPost } = await import("../app/api/heartbeat/route.js");
const { GET: downloadGet } = await import("../app/download/route.js");
const { decryptRecord, latestPerAddress, printable } = await import("../scripts/read-waitlist.mjs");

const KEY = randomBytes(32).toString("hex");
const STORED_FIELDS = ["arch", "install_id", "os", "platforms", "received_at", "version"];

// Each route keeps a per-address rate limit for the life of the module, so
// every request gets its own client address unless a test pins one.
let requestCount = 0;
function post(body, { address, contentType = "application/json" } = {}) {
  requestCount += 1;
  return new Request("https://openmessage.ai/api/x", {
    method: "POST",
    headers: {
      "Content-Type": contentType,
      "x-vercel-forwarded-for": address ?? `198.18.${(requestCount >> 8) & 255}.${requestCount & 255}`
    },
    body: typeof body === "string" ? body : JSON.stringify(body)
  });
}

beforeEach(() => {
  put.mockReset();
  put.mockResolvedValue({ url: "https://blob.example/x" });
  list.mockReset();
  list.mockResolvedValue({ blobs: [], hasMore: false });
  process.env.BLOB_READ_WRITE_TOKEN = "test-token";
  process.env.VERCEL_ENV = "production";
  process.env.WAITLIST_ENCRYPTION_KEY = KEY;
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  delete process.env.BLOB_READ_WRITE_TOKEN;
  delete process.env.WAITLIST_ENCRYPTION_KEY;
  delete process.env.VERCEL_ENV;
  vi.restoreAllMocks();
});

describe("POST /api/waitlist", () => {
  test("what the route stores, the reader script decrypts back to the same signup", async () => {
    const response = await waitlistPost(post({ email: "  Reader@Example.com ", interest: "general", website: "" }));
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ ok: true, message: WAITLIST_THANKS });

    expect(put).toHaveBeenCalledTimes(1);
    const [pathname, stored, options] = put.mock.calls[0];
    // The path is a keyed hash of the address: stable for that address and
    // day, and it doesn't reveal the address.
    expect(pathname).toMatch(/^waitlist\/\d{4}-\d{2}-\d{2}\/[0-9a-f]{32}\.json$/);
    expect(options).toMatchObject({ access: "public", addRandomSuffix: false, allowOverwrite: true });
    expect(stored).not.toContain("reader@example.com");
    expect(pathname).not.toContain("reader");

    const signup = decryptRecord(JSON.parse(stored), KEY);
    expect(signup).toMatchObject({ email: "reader@example.com", interest: "general", source: "openmessage.ai" });
    expect(Number.isNaN(Date.parse(signup.submittedAt))).toBe(false);
  });

  test("a repeat sign-up the same day replaces the first instead of adding one", async () => {
    await waitlistPost(post({ email: "repeat@example.com", interest: "" }));
    await waitlistPost(post({ email: " REPEAT@example.com", interest: "" }));
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[1][0]).toBe(put.mock.calls[0][0]);
    expect(put.mock.calls[1][2].allowOverwrite).toBe(true);
  });

  test("round-trips any valid address", async () => {
    const local = fc.stringMatching(/^[a-z0-9][a-z0-9._+-]{0,20}$/);
    const domain = fc.stringMatching(/^[a-z0-9-]{1,15}\.[a-z]{2,6}$/);
    await fc.assert(
      fc.asyncProperty(local, domain, async (name, host) => {
        put.mockClear();
        const email = `${name}@${host}`;
        const response = await waitlistPost(post({ email, interest: "", website: "" }));
        expect(response.status).toBe(200);
        const [pathname, stored] = put.mock.calls[0];
        expect(stored).not.toContain(email);
        // The name is 32 hex characters, so it cannot spell the address (no "@").
        expect(pathname).toMatch(/^waitlist\/\d{4}-\d{2}-\d{2}\/[0-9a-f]{32}\.json$/);
        expect(decryptRecord(JSON.parse(stored), KEY).email).toBe(email);
      }),
      { numRuns: 40 }
    );
  });

  test("a record cannot be read with the wrong key or after tampering", async () => {
    await waitlistPost(post({ email: "a@b.co", interest: "", website: "" }));
    const record = JSON.parse(put.mock.calls[0][1]);
    expect(() => decryptRecord(record, randomBytes(32).toString("hex"))).toThrow();
    const flipped = Buffer.from(record.ciphertext, "base64");
    flipped[0] ^= 1;
    expect(() => decryptRecord({ ...record, ciphertext: flipped.toString("base64") }, KEY)).toThrow();
    expect(() => decryptRecord({ ...record, version: 2 }, KEY)).toThrow("Unrecognized");
    expect(() => decryptRecord(record, "abcd")).toThrow("32 bytes");
  });

  test("the honeypot answers 200 and stores nothing", async () => {
    const response = await waitlistPost(post({ email: "bot@example.com", interest: "", website: "http://spam" }));
    expect(response.status).toBe(200);
    expect(put).not.toHaveBeenCalled();
  });

  test.each([
    ["an invalid address", { email: "nope", interest: "" }, 400, "Enter a valid email address."],
    ["an address over 254 characters", { email: `${"a".repeat(250)}@b.co`, interest: "" }, 400, "Enter a valid email address."],
    ["an address with a terminal escape", { email: "a\u001b[8m@b.co", interest: "" }, 400, "Enter a valid email address."],
    ["an address with a bidi override", { email: "a\u202eoc.b@c.co", interest: "" }, 400, "Enter a valid email address."],
    ["an unknown interest", { email: "a@b.co", interest: "crypto" }, 400, "Invalid interest selection."],
    ["a body that is not JSON", "{", 400, "Invalid request body."],
    ["a body over 2 KiB", { email: "a@b.co", interest: "", padding: "x".repeat(WAITLIST.maxBodyBytes) }, 413, "The site can't take an address that long. Use a shorter one."]
  ])("rejects %s with the error the form shows", async (_label, body, status, error) => {
    const response = await waitlistPost(post(body));
    expect(response.status).toBe(status);
    const payload = await response.json();
    expect(payload.error).toBe(error);
    // The form marks the email field invalid for this one error.
    expect(payload.field).toBe(error === "Enter a valid email address." ? "email" : undefined);
    expect(put).not.toHaveBeenCalled();
  });

  // Browsers send these cross-site without a CORS preflight.
  test.each(["text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"])(
    "refuses a %s request with 415 and stores nothing",
    async (contentType) => {
      const response = await waitlistPost(post({ email: "victim@example.com", interest: "", website: "" }, { contentType }));
      expect(response.status).toBe(415);
      expect((await response.json()).error).toBe("Set the Content-Type header to application/json.");
      expect(put).not.toHaveBeenCalled();
    }
  );

  test("accepts JSON with a charset parameter", async () => {
    const response = await waitlistPost(
      post({ email: "charset@example.com", interest: "", website: "" }, { contentType: "application/json; charset=utf-8" })
    );
    expect(response.status).toBe(200);
    expect(put).toHaveBeenCalledTimes(1);
  });

  test("an address past its limit gets 429 and stores nothing", async () => {
    const address = "192.0.2.77";
    for (let i = 0; i < WAITLIST.perAddress.limit; i++) {
      expect((await waitlistPost(post({ email: `rate${i}@b.co`, interest: "" }, { address }))).status).toBe(200);
    }
    put.mockClear();
    const limited = await waitlistPost(post({ email: "late@b.co", interest: "" }, { address }));
    expect(limited.status).toBe(429);
    expect((await limited.json()).error).toBe("Your IP address has reached the sign-up limit. Try again in 10 minutes.");
    expect(put).not.toHaveBeenCalled();
    // Another address is unaffected.
    expect((await waitlistPost(post({ email: "late@b.co", interest: "" }))).status).toBe(200);
  });

  test("at the day's cap, sign-ups pause and nothing is stored", async () => {
    list.mockResolvedValue({ blobs: [], hasMore: true });
    const response = await waitlistPost(post({ email: "a@b.co", interest: "" }));
    expect(response.status).toBe(503);
    expect((await response.json()).error).toMatch(/^The site has stopped taking sign-ups for today/);
    expect(put).not.toHaveBeenCalled();
    expect(list).toHaveBeenCalledWith({
      prefix: expect.stringMatching(/^waitlist\/\d{4}-\d{2}-\d{2}\/$/),
      limit: WAITLIST.dailyCap
    });
  });

  test("outside production (a preview, vercel dev, a next dev on a default pull) refuses sign-ups and never touches Blob", async () => {
    for (const vercelEnv of ["preview", "development", null]) {
      if (vercelEnv) process.env.VERCEL_ENV = vercelEnv;
      else delete process.env.VERCEL_ENV;
      const response = await waitlistPost(post({ email: "a@b.co", interest: "" }));
      expect(response.status).toBe(503);
    }
    expect(put).not.toHaveBeenCalled();
    expect(list).not.toHaveBeenCalled();
    process.env.VERCEL_ENV = "production";
    expect((await waitlistPost(post({ email: "a@b.co", interest: "" }))).status).toBe(200);
    expect(put).toHaveBeenCalledTimes(1);
  });

  test("answers 503 when storage is not configured, and 502 when the write fails", async () => {
    delete process.env.WAITLIST_ENCRYPTION_KEY;
    expect((await waitlistPost(post({ email: "a@b.co", interest: "" }))).status).toBe(503);
    process.env.WAITLIST_ENCRYPTION_KEY = KEY;
    put.mockRejectedValueOnce(new Error("blob down"));
    expect((await waitlistPost(post({ email: "a@b.co", interest: "" }))).status).toBe(502);
  });
});

describe("scripts/read-waitlist.mjs", () => {
  // A repeat sign-up on a later day is a second record; the list prints each
  // address once, with its latest record.
  const record = fc.record({
    email: fc.constantFrom("a@b.co", "c@d.co", "e@f.co", "g@h.co"),
    interest: fc.constantFrom("general", "mcp", "signal"),
    submittedAt: fc
      .integer({ min: Date.parse("2026-01-01T00:00:00Z"), max: Date.parse("2027-01-01T00:00:00Z") })
      .map((ms) => new Date(ms).toISOString())
  });

  test("latestPerAddress keeps one record per address, the latest, oldest first", () => {
    fc.assert(
      fc.property(fc.array(record, { maxLength: 30 }), (signups) => {
        const result = latestPerAddress(signups);
        const emails = result.map((signup) => signup.email);
        expect(new Set(emails).size).toBe(emails.length);
        expect(new Set(emails)).toEqual(new Set(signups.map((signup) => signup.email)));
        for (const kept of result) {
          const latest = Math.max(
            ...signups.filter((s) => s.email === kept.email).map((s) => Date.parse(s.submittedAt))
          );
          expect(Date.parse(kept.submittedAt)).toBe(latest);
        }
        const times = result.map((signup) => signup.submittedAt);
        expect(times).toEqual([...times].sort());
      }),
      { numRuns: 200 }
    );
  });

  // The script prints addresses to a terminal, where ESC sequences could hide
  // later rows, retitle the window or plant a disguised link.
  test("printable replaces each control or format character with ? and keeps the rest", () => {
    expect(printable("x\u001b[8m\u001b]8;;https://evil.example/\u0007ok@b.co")).toBe("x?[8m?]8;;https://evil.example/?ok@b.co");
    expect(printable("a\u202eb\u200b@c.co")).toBe("a?b?@c.co");
    expect(printable("jöhn@exämple.com")).toBe("jöhn@exämple.com");
    const controlOrFormat = /[\p{Cc}\p{Cf}]/u;
    const unit = fc.oneof(
      fc.constantFrom("\u0000", "\u0007", "\u001b", "\u009b", "\u00ad", "\u200b", "\u202e", "\ufeff", "\u{e0001}"),
      fc.string({ unit: "binary", minLength: 1, maxLength: 1 })
    );
    fc.assert(
      fc.property(fc.string({ unit, maxLength: 40 }), (value) => {
        const before = Array.from(value);
        const after = Array.from(printable(value));
        expect(after).toHaveLength(before.length);
        after.forEach((char, i) => {
          expect(controlOrFormat.test(char)).toBe(false);
          expect(char).toBe(controlOrFormat.test(before[i]) ? "?" : before[i]);
        });
      }),
      { numRuns: 300 }
    );
  });
});

// The form is a client component, so these check its source against the
// route's contract rather than render it.
describe("components/updates-form.jsx", () => {
  const form = readFileSync(new URL("../components/updates-form.jsx", import.meta.url), "utf8");

  test("falls back to the same thanks the route sends", () => {
    expect(form).toContain(`const THANKS = ${JSON.stringify(WAITLIST_THANKS)};`);
    expect(form).toContain("payload?.message || THANKS");
  });

  test("keeps the submit button focusable while sending", () => {
    expect(form).toContain('aria-disabled={sending ? "true" : undefined}');
    expect(form).not.toMatch(/\sdisabled[=\s>]/);
  });

  test("marks the field invalid only for the route's invalid-address error", () => {
    expect(form).toContain('response.status === 400 && payload?.field === "email"');
    expect(form).toContain('aria-describedby={invalid ? "updates-status" : undefined}');
    expect(form).toContain('id="updates-status"');
  });
});

describe("/api/heartbeat", () => {
  // The payload the Go client sends (internal/telemetry/telemetry.go).
  const heartbeat = {
    install_id: "0123456789abcdef0123456789abcdef",
    version: "0.2.9",
    os: "darwin",
    arch: "arm64",
    platforms: { google_messages: true, signal: true },
    sent_at: "2026-10-08T12:00:00Z",
    schema_version: 1
  };

  test("GET reports the schema version the shipped client sends, and whether this deployment stores", async () => {
    expect(await (await heartbeatGet()).json()).toEqual({ ok: true, schema_version: 1, persists: true });
    process.env.VERCEL_ENV = "preview";
    expect(await (await heartbeatGet()).json()).toEqual({ ok: true, schema_version: 1, persists: false });
    delete process.env.VERCEL_ENV;
    expect((await (await heartbeatGet()).json()).persists).toBe(false);
    process.env.VERCEL_ENV = "production";
    delete process.env.BLOB_READ_WRITE_TOKEN;
    expect((await (await heartbeatGet()).json()).persists).toBe(false);
  });

  test("stores one record per install per day and replaces it on a repeat", async () => {
    const response = await heartbeatPost(post(heartbeat));
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ ok: true });

    const [pathname, stored, options] = put.mock.calls[0];
    expect(pathname).toMatch(new RegExp(`^heartbeats/\\d{4}-\\d{2}-\\d{2}/${heartbeat.install_id}\\.json$`));
    expect(options).toMatchObject({ addRandomSuffix: false, allowOverwrite: true });

    const record = JSON.parse(stored);
    expect(Object.keys(record).sort()).toEqual(STORED_FIELDS);
    expect(record.platforms).toEqual({ google_messages: true, whatsapp: false, signal: true });

    expect((await heartbeatPost(post(heartbeat))).status).toBe(200);
    expect(put.mock.calls[1][0]).toBe(pathname);
  });

  test("keeps only the fields the privacy page lists, whatever else is sent", async () => {
    await fc.assert(
      fc.asyncProperty(
        fc.dictionary(fc.string({ maxLength: 12 }), fc.jsonValue({ maxDepth: 3 }), { maxKeys: 8 }),
        async (extra) => {
          put.mockClear();
          const body = JSON.stringify({ ...extra, ...heartbeat, ip: "203.0.113.9", message: "secret" });
          const response = await heartbeatPost(post(body));
          if (Buffer.byteLength(body, "utf8") > HEARTBEAT.maxBodyBytes) {
            expect(response.status).toBe(413);
            expect(put).not.toHaveBeenCalled();
            return;
          }
          expect(response.status).toBe(200);
          const record = JSON.parse(put.mock.calls[0][1]);
          expect(Object.keys(record).sort()).toEqual(STORED_FIELDS);
          expect(JSON.stringify(record)).not.toContain("secret");
          expect(JSON.stringify(record)).not.toContain("203.0.113.9");
        }
      ),
      { numRuns: 60 }
    );
  });

  test("stores only an install ID in the app's format, at heartbeats/<day>/<id>.json", async () => {
    const appFormat = /^[0-9a-f]{32}$/;
    const hex32 = fc.stringMatching(appFormat);
    const candidate = fc.oneof(
      hex32,
      fc.string({ maxLength: 80 }),
      hex32.map((id) => `../../waitlist/${id}`),
      hex32.map((id) => id.toUpperCase()),
      hex32.map((id) => id.slice(1))
    );
    await fc.assert(
      fc.asyncProperty(candidate, async (installId) => {
        put.mockClear();
        const response = await heartbeatPost(post({ ...heartbeat, install_id: installId }));
        if (!appFormat.test(installId)) {
          expect(response.status).toBe(400);
          expect(put).not.toHaveBeenCalled();
          return;
        }
        expect(response.status).toBe(200);
        expect(put.mock.calls[0][0]).toMatch(new RegExp(`^heartbeats/\\d{4}-\\d{2}-\\d{2}/${installId}\\.json$`));
      }),
      { numRuns: 100 }
    );
  });

  test.each([
    ["a wrong schema version", { ...heartbeat, schema_version: 2 }, 400],
    ["a short install ID", { ...heartbeat, install_id: "abc" }, 400],
    ["a body that is not JSON", "{", 400],
    ["a body over 2 KiB", { ...heartbeat, padding: "x".repeat(HEARTBEAT.maxBodyBytes) }, 413]
  ])("rejects %s", async (_label, body, status) => {
    const response = await heartbeatPost(post(body));
    expect(response.status).toBe(status);
    expect(put).not.toHaveBeenCalled();
  });

  test("refuses a request not sent as JSON with 415, and accepts JSON with a charset", async () => {
    const plain = await heartbeatPost(post(heartbeat, { contentType: "text/plain;charset=UTF-8" }));
    expect(plain.status).toBe(415);
    expect(await plain.json()).toEqual({ ok: false });
    expect(put).not.toHaveBeenCalled();
    const charset = await heartbeatPost(post(heartbeat, { contentType: "application/json; charset=utf-8" }));
    expect(charset.status).toBe(200);
    expect(put).toHaveBeenCalledTimes(1);
  });

  // The 415 stays safe only while every app version that sends heartbeats
  // sets application/json. v0.1.0 has no telemetry package.
  test("every tagged release and main that sends heartbeats sets a JSON Content-Type", () => {
    const refs = [...git(["tag", "-l", "v*"]).split("\n").filter(Boolean), MAIN_COMMIT];
    const senders = refs.filter((ref) => listAt(ref, "internal/telemetry").includes("internal/telemetry/telemetry.go"));
    expect(senders).toContain(RELEASE.tag);
    expect(senders).toContain(MAIN_COMMIT);
    for (const ref of senders) {
      const sets = fileAt(ref, "internal/telemetry/telemetry.go").match(/req\.Header\.Set\("Content-Type", "([^"]*)"\)/);
      expect(sets?.[1], ref).toBe("application/json");
      expect(isJSONContentType(new Request("https://openmessage.ai/api/heartbeat", { headers: { "Content-Type": sets[1] } }))).toBe(true);
    }
  });

  test("an address past its limit gets 429 and stores nothing", async () => {
    const address = "192.0.2.78";
    for (let i = 0; i < HEARTBEAT.perAddress.limit; i++) {
      expect((await heartbeatPost(post(heartbeat, { address }))).status).toBe(200);
    }
    put.mockClear();
    expect((await heartbeatPost(post(heartbeat, { address }))).status).toBe(429);
    expect(put).not.toHaveBeenCalled();
  });

  test("at the day's cap, the app is told ok and nothing is stored", async () => {
    list.mockResolvedValue({ blobs: [], hasMore: true });
    const response = await heartbeatPost(post(heartbeat));
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ ok: true, persisted: false });
    expect(put).not.toHaveBeenCalled();
  });

  test("answers ok without storing when no Blob token is configured", async () => {
    delete process.env.BLOB_READ_WRITE_TOKEN;
    const response = await heartbeatPost(post(heartbeat));
    expect(await response.json()).toEqual({ ok: true, persisted: false });
    expect(put).not.toHaveBeenCalled();
  });
  test("outside production (a preview, vercel dev, a next dev on a default pull) stores nothing even with production's token", async () => {
    for (const vercelEnv of ["preview", "development", null]) {
      if (vercelEnv) process.env.VERCEL_ENV = vercelEnv;
      else delete process.env.VERCEL_ENV;
      const response = await heartbeatPost(post(heartbeat));
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({ ok: true, persisted: false });
    }
    expect(put).not.toHaveBeenCalled();
    expect(list).not.toHaveBeenCalled();
  });
});

describe("GET /download", () => {
  test("redirects to the disk image on the latest release and is never cached", async () => {
    const response = await downloadGet(new Request("https://openmessage.ai/download?platform=macos-dmg&source=old-link"));
    expect(response.status).toBe(307);
    expect(response.headers.get("location")).toBe(`${REPO_URL}/releases/latest/download/${RELEASE.dmgName}`);
    expect(response.headers.get("cache-control")).toBe("no-store, max-age=0");
  });
});
