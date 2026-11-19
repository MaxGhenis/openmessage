// Deletes install reports once they pass the retention period /privacy states.
//
// /api/heartbeat stores one record per install per UTC day at
// heartbeats/<day>/<install_id>.json, so a whole day's folder expires at once.
// A daily Vercel cron (site/vercel.json) calls GET /api/cron/expire-heartbeats
// with CRON_SECRET as a bearer token. On UTC day T the run deletes every
// folder dated T - heartbeatDays or earlier, oldest first, up to
// maxDeletesPerRun records; the next run picks up anything left. It touches
// nothing outside heartbeats/, and it skips any folder whose name is not a
// calendar date. A missed or cut-short run leaves its records for the next
// one, a day late. Two runs at once (Vercel can deliver a cron twice) delete
// each record once, but both count it, and together they delete at twice the
// paced rate; the rate-limit backoff absorbs that.
//
// Like the write handlers in public-writes.mjs, these functions take their
// storage calls as arguments so they can be tested without Next.js or Vercel.

import { createHash, timingSafeEqual } from "node:crypto";

import { canStore } from "./public-writes.mjs";

export const RETENTION = {
  heartbeatDays: 90,
  // Blob counts each deleted blob as one operation toward the Pro limit of 75
  // advanced operations a second, which the site's own writes share, and
  // @vercel/blob doesn't retry a rate-limited call. Deleting 50 per call with
  // a second between calls keeps the cleanup near 50 a second.
  deleteBatch: 50,
  pauseMs: 1000,
  // A rate-limited call waits as long as Blob asks (at most 10 s) and tries
  // again, up to 3 times.
  rateLimitRetries: 3,
  maxRetryWaitMs: 10_000,
  // About 100 s of deleting. The heartbeat route stores at most 1,000 records
  // a day, so a run clears a day's folder with room to spare.
  maxDeletesPerRun: 5000,
};

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// BlobServiceRateLimited is the one @vercel/blob error with a numeric
// retryAfter: the header parsed with parseInt, so 0 when Blob sent none and
// NaN when it doesn't start with a digit.
function isRateLimited(error) {
  return typeof error?.retryAfter === "number";
}

function retryWaitMs(retryAfter) {
  const seconds = Number.isFinite(retryAfter) && retryAfter >= 1 ? retryAfter : 1;
  return Math.min(seconds * 1000, RETENTION.maxRetryWaitMs);
}

async function deleteChunk(del, chunk, { sleep, retries }) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await del(chunk);
    } catch (error) {
      if (!isRateLimited(error) || attempt >= retries) throw error;
      await sleep(retryWaitMs(error.retryAfter));
    }
  }
}

const PREFIX = "heartbeats/";
const DAY_FOLDER = /^heartbeats\/(\d{4}-\d{2}-\d{2})\/$/;
const DAY_MS = 24 * 60 * 60 * 1000;

function isCalendarDate(value) {
  const t = Date.parse(`${value}T00:00:00Z`);
  return Number.isFinite(t) && new Date(t).toISOString().slice(0, 10) === value;
}

// The latest day whose folder has expired at time `now`: today (UTC) minus `days`.
export function cutoffDay(now, days) {
  const today = Date.parse(`${new Date(now).toISOString().slice(0, 10)}T00:00:00Z`);
  return new Date(today - days * DAY_MS).toISOString().slice(0, 10);
}

// Day folders under heartbeats/ dated on or before `cutoff`, oldest first.
export async function expiredHeartbeatFolders(list, cutoff) {
  const expired = [];
  let cursor;
  do {
    const page = await list({ prefix: PREFIX, mode: "folded", limit: 1000, cursor });
    for (const folder of page.folders || []) {
      const match = DAY_FOLDER.exec(folder);
      if (match && isCalendarDate(match[1]) && match[1] <= cutoff) expired.push(folder);
    }
    cursor = page.hasMore ? page.cursor : undefined;
  } while (cursor);
  return expired.sort();
}

export async function expireHeartbeats({
  list,
  del,
  now = Date.now,
  days = RETENTION.heartbeatDays,
  batch = RETENTION.deleteBatch,
  maxDeletes = RETENTION.maxDeletesPerRun,
  pauseMs = RETENTION.pauseMs,
  retries = RETENTION.rateLimitRetries,
  sleep = wait,
}) {
  const cutoff = cutoffDay(now(), days);
  const folders = await expiredHeartbeatFolders(list, cutoff);
  let deleted = 0;
  let calls = 0;
  for (const [index, folder] of folders.entries()) {
    // List the folder before deleting from it, so a delete can't shift a
    // cursor; stop listing once the run's budget is covered.
    const room = maxDeletes - deleted;
    const urls = [];
    let cursor;
    do {
      const page = await list({ prefix: folder, limit: 1000, cursor });
      for (const blob of page.blobs) {
        if (blob.pathname.startsWith(folder)) urls.push(blob.url);
      }
      cursor = page.hasMore ? page.cursor : undefined;
    } while (cursor && urls.length < room);
    const take = urls.slice(0, room);
    for (let i = 0; i < take.length; i += batch) {
      if (calls > 0) await sleep(pauseMs);
      const chunk = take.slice(i, i + batch);
      await deleteChunk(del, chunk, { sleep, retries });
      calls += 1;
      deleted += chunk.length; // records sent to del(), found or not
    }
    const folderDone = !cursor && take.length === urls.length;
    const moreFolders = index < folders.length - 1;
    if (!folderDone || (deleted >= maxDeletes && moreFolders)) {
      return { cutoff, folders: folders.length, deleted, complete: false };
    }
  }
  return { cutoff, folders: folders.length, deleted, complete: true };
}

function digest(value) {
  return createHash("sha256").update(value, "utf8").digest();
}

// Vercel sends `Authorization: Bearer <CRON_SECRET>` on each cron request.
// Without CRON_SECRET nothing can authorize a run.
export function isCronRequest(request, env) {
  const secret = env.CRON_SECRET || "";
  if (!secret) return false;
  const header = request.headers.get("authorization") || "";
  return timingSafeEqual(digest(header), digest(`Bearer ${secret}`));
}

export async function handleExpireHeartbeats(request, { list, del, env, now = Date.now, log = console.error, sleep = wait }) {
  if (!isCronRequest(request, env)) {
    if (!env.CRON_SECRET) log("expire-heartbeats: CRON_SECRET is not set; refusing.");
    return { status: 401, body: { ok: false } };
  }
  if (!canStore(env)) {
    return { status: 200, body: { ok: true, deleted: 0, storage: false } };
  }
  try {
    const result = await expireHeartbeats({ list, del, now, sleep });
    return { status: 200, body: { ok: true, ...result } };
  } catch (error) {
    log("expire-heartbeats failed:", error);
    return { status: 502, body: { ok: false } };
  }
}
