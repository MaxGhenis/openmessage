import { list, put } from "@vercel/blob";
import { NextResponse } from "next/server";

import { WAITLIST, createRateLimiter, handleWaitlist } from "../../../lib/public-writes.mjs";

// Bounds (body size, address format, per-address rate, daily object budget)
// live in lib/public-writes.mjs.
const allow = createRateLimiter(WAITLIST.perAddress);

export async function POST(request) {
  const { status, body } = await handleWaitlist(request, { put, list, env: process.env, allow });
  return NextResponse.json(body, { status });
}
