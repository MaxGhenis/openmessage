// Usage heartbeat receiver.
//
// An OpenMessage process started with OPENMESSAGE_TELEMETRY=1 sends a POST
// with Content-Type: application/json (internal/telemetry/telemetry.go, every
// release from v0.2.0) and this body:
//   { install_id, version, os, arch, platforms, sent_at, schema_version }
// Any other Content-Type gets a 415.
// No shipped build sets that variable for the user, so most installs send
// nothing.
//
// What is kept: one JSON record per install per UTC day, at
// heartbeats/<day>/<install_id>.json in Vercel Blob. The record holds the random
// install ID, version, OS, CPU architecture, which networks are paired, and the
// time received. It holds no IP address, no user agent and no message data.
// Records are pseudonymous (the same install ID recurs) and nothing deletes
// them automatically. The privacy page says the same; change both together.
//
// Anyone can call this route, so lib/public-writes.mjs bounds what it can
// store: the install ID must have the app's format, each address is
// rate-limited, a repeat on the same day replaces that day's record, and at
// most HEARTBEAT.dailyCap records are written per day.
//
// Required env vars:
//   BLOB_READ_WRITE_TOKEN   — Vercel Blob token (shared with the waitlist)

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
