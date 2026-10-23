// Anonymous heartbeat receiver.
//
// The OpenMessage app sends one POST per install per 24h with:
//   { install_id, version, os, arch, platforms, sent_at, schema_version }
//
// We only persist counts. Install IDs are written to Vercel Blob bucketed by
// day so we can compute DAU without retaining individual identifiers long-term.
//
// Required env vars:
//   BLOB_READ_WRITE_TOKEN   — Vercel Blob token (already set for waitlist)
//
// Telemetry is opt-in client-side. The server bounds what anyone can store:
// install ids must have the app's format, each address is rate-limited, and
// at most HEARTBEAT.dailyCap objects are written per day
// (lib/public-writes.mjs).

import { list, put } from "@vercel/blob";
import { NextResponse } from "next/server";

import { HEARTBEAT, createRateLimiter, handleHeartbeat } from "../../../lib/public-writes.mjs";

const allow = createRateLimiter(HEARTBEAT.perAddress);

export async function POST(request) {
  const { status, body } = await handleHeartbeat(request, { put, list, env: process.env, allow });
  return NextResponse.json(body, { status });
}

export async function GET() {
  return NextResponse.json({ ok: true, schema_version: HEARTBEAT.schemaVersion });
}
