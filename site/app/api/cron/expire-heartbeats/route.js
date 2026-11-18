// Daily cleanup of install reports past the retention period. Vercel Cron
// calls this route on production with `Authorization: Bearer <CRON_SECRET>`
// (the schedule lives in site/vercel.json); any other caller gets 401.
// lib/heartbeat-retention.mjs does the work and says what it may delete.
//
// Required env vars on production:
//   BLOB_READ_WRITE_TOKEN   — Vercel Blob token
//   CRON_SECRET             — random string; Vercel sends it as the bearer token

import { del, list } from "@vercel/blob";
import { NextResponse } from "next/server";

import { handleExpireHeartbeats } from "../../../../lib/heartbeat-retention.mjs";

// The run paces its deletes and takes about 100 s at most; allow 300 s.
export const maxDuration = 300;

export async function GET(request) {
  const { status, body } = await handleExpireHeartbeats(request, { list, del, env: process.env });
  return NextResponse.json(body, { status, headers: { "Cache-Control": "no-store" } });
}
