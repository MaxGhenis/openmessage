import { randomBytes } from "node:crypto";

import fc from "fast-check";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { RELEASE, REPO_URL } from "../data/facts.js";

const put = vi.fn();
vi.mock("@vercel/blob", () => ({ put: (...args) => put(...args), list: vi.fn() }));

const { POST: waitlistPost } = await import("../app/api/waitlist/route.js");
const { GET: heartbeatGet, POST: heartbeatPost } = await import("../app/api/heartbeat/route.js");
const { GET: downloadGet } = await import("../app/download/route.js");
const { decryptRecord } = await import("../scripts/read-waitlist.mjs");

const KEY = randomBytes(32).toString("hex");

function post(body) {
  return new Request("https://openmessage.ai/api/x", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: typeof body === "string" ? body : JSON.stringify(body)
  });
}

beforeEach(() => {
  put.mockReset();
  put.mockResolvedValue({ url: "https://blob.example/x" });
  process.env.BLOB_READ_WRITE_TOKEN = "test-token";
  process.env.WAITLIST_ENCRYPTION_KEY = KEY;
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  delete process.env.BLOB_READ_WRITE_TOKEN;
  delete process.env.WAITLIST_ENCRYPTION_KEY;
  vi.restoreAllMocks();
});

describe("POST /api/waitlist", () => {
  test("what the route stores, the reader script decrypts back to the same signup", async () => {
    const response = await waitlistPost(post({ email: "  Reader@Example.com ", interest: "general", website: "" }));
    expect(response.status).toBe(200);
    expect((await response.json()).ok).toBe(true);

    expect(put).toHaveBeenCalledTimes(1);
    const [pathname, stored] = put.mock.calls[0];
    expect(pathname).toMatch(/^waitlist\/\d{4}-\d{2}-\d{2}\/[0-9a-f-]{36}\.json$/);
    // The address never appears in the stored record or its path.
    expect(stored).not.toContain("reader@example.com");
    expect(pathname).not.toContain("reader");

    const signup = decryptRecord(JSON.parse(stored), KEY);
    expect(signup).toMatchObject({ email: "reader@example.com", interest: "general", source: "openmessage.ai" });
    expect(Number.isNaN(Date.parse(signup.submittedAt))).toBe(false);
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
        const stored = put.mock.calls[0][1];
        expect(stored).not.toContain(email);
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
    ["an unknown interest", { email: "a@b.co", interest: "crypto" }, 400, "Invalid interest selection."],
    ["a body that is not JSON", "{", 400, "Invalid request body."]
  ])("rejects %s with the error the form shows", async (_label, body, status, error) => {
    const response = await waitlistPost(post(body));
    expect(response.status).toBe(status);
    expect((await response.json()).error).toBe(error);
    expect(put).not.toHaveBeenCalled();
  });

  test("answers 503 when storage is not configured, and 502 when the write fails", async () => {
    delete process.env.WAITLIST_ENCRYPTION_KEY;
    expect((await waitlistPost(post({ email: "a@b.co", interest: "" }))).status).toBe(503);
    process.env.WAITLIST_ENCRYPTION_KEY = KEY;
    put.mockRejectedValueOnce(new Error("blob down"));
    expect((await waitlistPost(post({ email: "a@b.co", interest: "" }))).status).toBe(502);
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

  test("GET reports the schema version the shipped client sends", async () => {
    expect(await (await heartbeatGet()).json()).toEqual({ ok: true, schema_version: 1 });
  });

  test("stores one record per install per day and replaces it on a repeat", async () => {
    const response = await heartbeatPost(post(heartbeat));
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ ok: true });

    const [pathname, stored, options] = put.mock.calls[0];
    expect(pathname).toMatch(new RegExp(`^heartbeats/\\d{4}-\\d{2}-\\d{2}/${heartbeat.install_id}\\.json$`));
    expect(options.allowOverwrite).toBe(true);
    expect(options.addRandomSuffix).toBe(false);

    const record = JSON.parse(stored);
    expect(Object.keys(record).sort()).toEqual(["arch", "install_id", "os", "platforms", "received_at", "version"]);
    expect(record.platforms).toEqual({ google_messages: true, whatsapp: false, signal: true });
  });

  test("keeps only the fields the privacy page lists, whatever else is sent", async () => {
    await fc.assert(
      fc.asyncProperty(fc.dictionary(fc.string(), fc.jsonValue()), async (extra) => {
        put.mockClear();
        const response = await heartbeatPost(post({ ...extra, ...heartbeat, ip: "203.0.113.9", message: "secret" }));
        expect(response.status).toBe(200);
        const record = JSON.parse(put.mock.calls[0][1]);
        expect(Object.keys(record).sort()).toEqual(["arch", "install_id", "os", "platforms", "received_at", "version"]);
        expect(JSON.stringify(record)).not.toContain("secret");
        expect(JSON.stringify(record)).not.toContain("203.0.113.9");
      }),
      { numRuns: 40 }
    );
  });

  test("the stored path cannot be steered outside heartbeats/", async () => {
    // Slashes are stripped from the ID, so it stays a single file name. Dots
    // survive (the sanitizer allows them) but cannot form a path segment.
    await fc.assert(
      fc.asyncProperty(fc.string({ minLength: 16, maxLength: 80 }), async (installId) => {
        put.mockClear();
        const response = await heartbeatPost(post({ ...heartbeat, install_id: `../../waitlist/${installId}` }));
        if (response.status !== 200) {
          expect(put).not.toHaveBeenCalled();
          return;
        }
        const segments = put.mock.calls[0][0].split("/");
        expect(segments.length).toBe(3);
        expect(segments[0]).toBe("heartbeats");
        expect(segments[1]).toMatch(/^\d{4}-\d{2}-\d{2}$/);
        expect(segments[2]).toMatch(/^[A-Za-z0-9._-]{16,64}\.json$/);
      }),
      { numRuns: 60 }
    );
  });

  test.each([
    ["a wrong schema version", { ...heartbeat, schema_version: 2 }],
    ["a short install ID", { ...heartbeat, install_id: "abc" }],
    ["a body that is not JSON", "{"]
  ])("rejects %s", async (_label, body) => {
    const response = await heartbeatPost(post(body));
    expect(response.status).toBe(400);
    expect(put).not.toHaveBeenCalled();
  });

  test("answers ok without storing when no Blob token is configured", async () => {
    delete process.env.BLOB_READ_WRITE_TOKEN;
    const response = await heartbeatPost(post(heartbeat));
    expect(await response.json()).toEqual({ ok: true, persisted: false });
    expect(put).not.toHaveBeenCalled();
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
