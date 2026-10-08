import { RELEASE, REPO } from "../data/facts.js";

// The Download button redirects to GitHub's "latest release", so the page asks
// GitHub which release that is instead of trusting a constant. If GitHub is
// unreachable or rate-limited the page falls back to the release these facts
// were checked against, and says nothing it cannot back up.

const API = `https://api.github.com/repos/${REPO}`;
const HEADERS = { Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" };
const REVALIDATE_SECONDS = 3600;

export const FALLBACK_RELEASE = {
  tag: RELEASE.tag,
  version: RELEASE.version,
  publishedAt: RELEASE.publishedAt,
  dmgBytes: null,
  live: false
};

// Pure: turns a GitHub release payload into the few fields the page shows.
// Returns null for anything that is not a published, tagged release.
export function normalizeRelease(payload) {
  if (!payload || typeof payload !== "object" || payload.draft || payload.prerelease) {
    return null;
  }
  const tag = typeof payload.tag_name === "string" ? payload.tag_name.trim() : "";
  if (!/^v\d+\.\d+\.\d+$/.test(tag)) {
    return null;
  }
  const published = typeof payload.published_at === "string" ? Date.parse(payload.published_at) : NaN;
  if (Number.isNaN(published)) {
    return null;
  }
  // Re-serialized, so the page only ever prints a date in one known shape.
  const publishedAt = new Date(published).toISOString();
  const assets = Array.isArray(payload.assets) ? payload.assets : [];
  const dmg = assets.find((asset) => asset?.name === RELEASE.dmgName);
  const dmgBytes = Number.isInteger(dmg?.size) && dmg.size > 0 ? dmg.size : null;
  return { tag, version: tag.slice(1), publishedAt, dmgBytes, live: true };
}

// Pure: reads the commit total out of the Link header GitHub sends for
// `commits?per_page=1`, whose last page number is the number of commits.
// Null when the header is missing or has no last page.
export function parseCommitTotal(linkHeader) {
  if (typeof linkHeader !== "string") {
    return null;
  }
  const match = linkHeader.match(/[?&]page=(\d+)>;\s*rel="last"/);
  if (!match) {
    return null;
  }
  const total = Number(match[1]);
  return Number.isSafeInteger(total) && total > 0 ? total : null;
}

// Pure: commits on main beyond the release, given main's total. Null if the
// total is unknown or smaller than the release's own count (which would mean
// the inputs disagree, so no number is better than a wrong one).
export function commitsAheadOf(mainTotal, releaseTotal) {
  if (!Number.isSafeInteger(mainTotal) || !Number.isSafeInteger(releaseTotal)) {
    return null;
  }
  const ahead = mainTotal - releaseTotal;
  return ahead >= 0 ? ahead : null;
}

async function getJson(url) {
  const response = await fetch(url, { headers: HEADERS, next: { revalidate: REVALIDATE_SECONDS } });
  if (!response.ok) {
    throw new Error(`GitHub answered ${response.status}`);
  }
  return response.json();
}

export async function getLatestRelease() {
  try {
    return normalizeRelease(await getJson(`${API}/releases/latest`)) ?? FALLBACK_RELEASE;
  } catch {
    return FALLBACK_RELEASE;
  }
}

// How many commits main has, or null when GitHub can't say. One small request:
// the count comes from the pagination header, so no commit data is downloaded.
export async function getMainCommitTotal() {
  try {
    const response = await fetch(`${API}/commits?sha=main&per_page=1`, {
      headers: HEADERS,
      next: { revalidate: REVALIDATE_SECONDS }
    });
    if (!response.ok) {
      return null;
    }
    return parseCommitTotal(response.headers.get("link"));
  } catch {
    return null;
  }
}

// "2026-04-21T18:56:16Z" -> "April 21, 2026", in UTC so every visitor and
// every build reads the same date.
export function formatDate(iso) {
  const time = Date.parse(iso);
  if (Number.isNaN(time)) {
    return "";
  }
  return new Intl.DateTimeFormat("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
    timeZone: "UTC"
  }).format(new Date(time));
}

// Decimal megabytes, the unit Finder shows. Null in, empty out.
export function formatMegabytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return "";
  }
  const megabytes = bytes / 1_000_000;
  return `${megabytes >= 10 ? Math.round(megabytes) : megabytes.toFixed(1)} MB`;
}
