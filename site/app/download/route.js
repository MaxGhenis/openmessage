import { NextResponse } from "next/server";

import { RELEASE_DMG_URL } from "../../data/facts";

export const dynamic = "force-dynamic";

// Sends the visitor to the disk image on GitHub's latest release. The `source`
// and `platform` query parameters on older links are accepted and ignored: the
// site records nothing about who downloads.
export async function GET() {
  const response = NextResponse.redirect(RELEASE_DMG_URL, 307);
  response.headers.set("Cache-Control", "no-store, max-age=0");
  return response;
}
