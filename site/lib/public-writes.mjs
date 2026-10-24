// Server-side bounds for the two public routes that write to Vercel Blob:
// /api/waitlist and /api/heartbeat. Anyone can call them, so without bounds
// each request could store a new object (the waitlist kept a fresh UUID path
// per submission, and the heartbeat took any caller-chosen install id).
//
// Three layers:
//  - request shape: a small body cap and strict field formats;
//  - a per-instance rate limit by client address, which throttles a burst on
//    the warm serverless instances it lands on;
//  - a daily object budget per route, read from Blob itself, which bounds
//    storage across all instances: writes stop once the day's folder holds
//    the cap. Concurrent requests can overshoot it by at most their number.
//
// The handlers take their storage calls as arguments so they can be tested
// without Next.js or Vercel.

import { createCipheriv, createHmac, randomBytes } from "node:crypto";

export const WAITLIST = {
  maxBodyBytes: 2048,
  maxEmailLength: 254,
  perAddress: { limit: 5, windowMs: 10 * 60 * 1000 },
  dailyCap: 1000,
};

export const HEARTBEAT = {
  maxBodyBytes: 2048,
  perAddress: { limit: 30, windowMs: 60 * 60 * 1000 },
  dailyCap: 1000,
  schemaVersion: 1,
};

const allowedInterests = new Set(["", "mac-app", "whatsapp", "signal", "mcp", "general"]);
const UNCONFIGURED = "Updates are not configured right now. Email max@maxghenis.com instead.";
const PAUSED = "Sign-ups are paused for today. Email max@maxghenis.com instead.";

export function isValidEmail(value) {
  return value.length <= WAITLIST.maxEmailLength && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

// The app's install id: 16 random bytes, hex (internal/telemetry installID).
export function isInstallID(value) {
  return typeof value === "string" && /^[0-9a-f]{32}$/.test(value);
}

// Reads a JSON body of at most maxBytes. Returns { value } or { status }.
export async function readJSONBody(request, maxBytes) {
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > maxBytes) return { status: 413 };
  let text;
  try {
    text = await request.text();
  } catch {
    return { status: 400 };
  }
  if (Buffer.byteLength(text, "utf8") > maxBytes) return { status: 413 };
  try {
    return { value: JSON.parse(text) };
  } catch {
    return { status: 400 };
  }
}

// Vercel's edge sets these headers to the connecting client's address.
export function clientAddress(request) {
  const forwarded = request.headers.get("x-vercel-forwarded-for") || request.headers.get("x-forwarded-for") || "";
  return forwarded.split(",")[0].trim() || request.headers.get("x-real-ip") || "unknown";
}

// A fixed-window counter per key, held in this instance's memory. It forgets
// keys whose window has passed and never tracks more than maxKeys at once.
export function createRateLimiter({ limit, windowMs, maxKeys = 10000, now = Date.now }) {
  const windows = new Map();
  return function allow(key) {
    const t = now();
    let entry = windows.get(key);
    if (entry && t - entry.start >= windowMs) {
      windows.delete(key);
      entry = undefined;
    }
    if (!entry) {
      if (windows.size >= maxKeys) {
        for (const [k, v] of windows) {
          if (t - v.start >= windowMs) windows.delete(k);
        }
        while (windows.size >= maxKeys) windows.delete(windows.keys().next().value);
      }
      entry = { start: t, count: 0 };
      windows.set(key, entry);
    }
    entry.count += 1;
    return entry.count <= limit;
  };
}

// True while the folder holds fewer than cap objects. list() returns at most
// `limit` entries, so asking for cap is enough to tell.
export async function underDailyBudget(list, prefix, cap) {
  const { blobs, hasMore } = await list({ prefix, limit: cap });
  return !hasMore && blobs.length < cap;
}

function day(now) {
  return new Date(now()).toISOString().slice(0, 10);
}

// The pathname is a keyed hash of the address, so a repeat sign-up replaces
// the earlier one that day instead of adding an object, and the path does
// not reveal the address.
export function waitlistPathname(email, key, date) {
  const digest = createHmac("sha256", key).update(`openmessage-waitlist\0${email}`).digest("hex").slice(0, 32);
  return `waitlist/${date}/${digest}.json`;
}

function encryptWaitlistRecord(record, key) {
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", key, iv);
  const ciphertext = Buffer.concat([cipher.update(JSON.stringify(record), "utf8"), cipher.final()]);
  return {
    version: 1,
    algorithm: "aes-256-gcm",
    submittedAt: record.submittedAt,
    iv: iv.toString("base64"),
    tag: cipher.getAuthTag().toString("base64"),
    ciphertext: ciphertext.toString("base64"),
  };
}

// Each handler returns { status, body } for the route to send as JSON.

export async function handleWaitlist(request, { put, list, env, allow, now = Date.now, log = console.error, dailyCap = WAITLIST.dailyCap }) {
  const body = await readJSONBody(request, WAITLIST.maxBodyBytes);
  if (body.status === 413) return { status: 413, body: { error: "Request is too large." } };
  if (body.status) return { status: 400, body: { error: "Invalid request body." } };
  const payload = body.value;

  const email = `${payload?.email || ""}`.trim().toLowerCase();
  const interest = `${payload?.interest || ""}`.trim();
  const website = `${payload?.website || ""}`.trim();

  if (website) {
    return { status: 200, body: { ok: true, message: "Thanks." } };
  }
  if (!isValidEmail(email)) {
    return { status: 400, body: { error: "Enter a valid email address." } };
  }
  if (!allowedInterests.has(interest)) {
    return { status: 400, body: { error: "Invalid interest selection." } };
  }
  if (!allow(clientAddress(request))) {
    return { status: 429, body: { error: "Too many sign-ups from this address. Try again later." } };
  }

  const key = Buffer.from(env.WAITLIST_ENCRYPTION_KEY || "", "hex");
  if (!env.BLOB_READ_WRITE_TOKEN || key.length !== 32) {
    log("Waitlist storage is not configured.");
    return { status: 503, body: { error: UNCONFIGURED } };
  }

  try {
    const date = day(now);
    if (!(await underDailyBudget(list, `waitlist/${date}/`, dailyCap))) {
      log("Waitlist daily budget reached.");
      return { status: 503, body: { error: PAUSED } };
    }
    const submittedAt = new Date(now()).toISOString();
    const record = encryptWaitlistRecord({ email, interest: interest || "general", submittedAt, source: "openmessage.ai" }, key);
    await put(waitlistPathname(email, key, date), JSON.stringify(record, null, 2), {
      access: "public",
      addRandomSuffix: false,
      allowOverwrite: true,
      contentType: "application/json",
    });
  } catch (error) {
    log("Waitlist submit failed:", error);
    return { status: 502, body: { error: "Could not save your email right now. Try again shortly." } };
  }

  return { status: 200, body: { ok: true, message: "Thanks. I’ll send product updates when there’s something real to share." } };
}

function safeString(value, max) {
  return `${value || ""}`.replace(/[^A-Za-z0-9._-]/g, "").slice(0, max);
}

export async function handleHeartbeat(request, { put, list, env, allow, now = Date.now, log = console.error, dailyCap = HEARTBEAT.dailyCap }) {
  const body = await readJSONBody(request, HEARTBEAT.maxBodyBytes);
  if (body.status) return { status: body.status, body: { ok: false } };
  const payload = body.value;
  if (!payload || payload.schema_version !== HEARTBEAT.schemaVersion || !isInstallID(payload.install_id)) {
    return { status: 400, body: { ok: false } };
  }
  if (!allow(clientAddress(request))) {
    return { status: 429, body: { ok: false } };
  }
  if (!env.BLOB_READ_WRITE_TOKEN) {
    // No-op if not configured; respond OK so the app doesn't retry.
    return { status: 200, body: { ok: true, persisted: false } };
  }

  const date = day(now);
  const record = {
    install_id: payload.install_id,
    version: safeString(payload.version, 32),
    os: safeString(payload.os, 16),
    arch: safeString(payload.arch, 16),
    platforms: {
      google_messages: !!payload?.platforms?.google_messages,
      whatsapp: !!payload?.platforms?.whatsapp,
      signal: !!payload?.platforms?.signal,
    },
    received_at: new Date(now()).toISOString(),
  };
  try {
    if (!(await underDailyBudget(list, `heartbeats/${date}/`, dailyCap))) {
      // Answer OK so the app doesn't retry; the day's count is already capped.
      return { status: 200, body: { ok: true, persisted: false } };
    }
    // One object per install per day: a repeat replaces it.
    await put(`heartbeats/${date}/${payload.install_id}.json`, JSON.stringify(record), {
      access: "public",
      addRandomSuffix: false,
      allowOverwrite: true,
      contentType: "application/json",
    });
  } catch (error) {
    log("heartbeat persist failed:", error);
    return { status: 502, body: { ok: false } };
  }
  return { status: 200, body: { ok: true } };
}
