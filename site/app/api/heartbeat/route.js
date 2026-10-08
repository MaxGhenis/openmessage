// Usage heartbeat receiver.
//
// An OpenMessage process started with OPENMESSAGE_TELEMETRY=1 sends a POST with:
//   { install_id, version, os, arch, platforms, sent_at, schema_version }
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
// Required env vars:
//   BLOB_READ_WRITE_TOKEN   — Vercel Blob token (shared with the waitlist)

import { put } from "@vercel/blob";
import { NextResponse } from "next/server";

const SCHEMA_VERSION = 1;

function safeString(value, max = 64) {
  return `${value || ""}`.replace(/[^A-Za-z0-9._-]/g, "").slice(0, max);
}

export async function POST(request) {
  let payload;
  try {
    payload = await request.json();
  } catch {
    return NextResponse.json({ ok: false }, { status: 400 });
  }

  if (!payload || payload.schema_version !== SCHEMA_VERSION) {
    return NextResponse.json({ ok: false }, { status: 400 });
  }

  const installId = safeString(payload.install_id, 64);
  if (installId.length < 16) {
    return NextResponse.json({ ok: false }, { status: 400 });
  }

  const day = new Date().toISOString().slice(0, 10);
  const record = {
    install_id: installId,
    version: safeString(payload.version, 32),
    os: safeString(payload.os, 16),
    arch: safeString(payload.arch, 16),
    platforms: {
      google_messages: !!payload?.platforms?.google_messages,
      whatsapp: !!payload?.platforms?.whatsapp,
      signal: !!payload?.platforms?.signal,
    },
    received_at: new Date().toISOString(),
  };

  if (!process.env.BLOB_READ_WRITE_TOKEN) {
    // No-op if not configured; respond OK so the app doesn't retry.
    return NextResponse.json({ ok: true, persisted: false });
  }

  try {
    await put(
      `heartbeats/${day}/${installId}.json`,
      JSON.stringify(record),
      {
        access: "public",
        addRandomSuffix: false,
        // A second launch on the same day replaces that day's record instead
        // of failing the write.
        allowOverwrite: true,
        contentType: "application/json",
      },
    );
  } catch (err) {
    console.error("heartbeat persist failed:", err);
    return NextResponse.json({ ok: false }, { status: 502 });
  }

  return NextResponse.json({ ok: true });
}

export async function GET() {
  return NextResponse.json({ ok: true, schema_version: SCHEMA_VERSION });
}
