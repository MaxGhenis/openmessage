import { RELEASE, REPO } from "../data/facts.js";

// The Download button redirects to GitHub's "latest release", so the page asks
// GitHub which release that is instead of trusting a constant. The copy
// describes RELEASE, and the tests check it there. When GitHub confirms that
// release with its disk image, the page names it as what the button serves;
// when GitHub reports a newer one, the page says so; and when GitHub is
// unreachable, rate-limited or reports anything else, including a release
// without the disk image, the page names no version beside the button.

const API = `https://api.github.com/repos/${REPO}`;
const HEADERS = { Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" };
// Short, so the page names the release the button serves within minutes of a
// new one; the button itself always follows GitHub's latest release.
const REVALIDATE_SECONDS = 300;

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

// Pure: compares two "x.y.z" versions numerically. Returns -1, 0 or 1, or null
// when either is not three dot-separated whole numbers.
export function compareVersions(a, b) {
  const parse = (version) => {
    if (typeof version !== "string" || !/^\d+\.\d+\.\d+$/.test(version)) {
      return null;
    }
    const parts = version.split(".").map(Number);
    return parts.every(Number.isSafeInteger) ? parts : null;
  };
  const left = parse(a);
  const right = parse(b);
  if (!left || !right) {
    return null;
  }
  for (let index = 0; index < 3; index += 1) {
    if (left[index] !== right[index]) {
      return left[index] < right[index] ? -1 : 1;
    }
  }
  return 0;
}

// Pure: how the release GitHub reports relates to the one the copy describes.
// "same" when GitHub answered with that release, "newer" when it answered with
// a later one, and "unknown" otherwise: no answer, or an older or odd tag.
export function releaseStatus(release, described = RELEASE.version) {
  if (!release || release.live !== true) {
    return "unknown";
  }
  const order = compareVersions(release.version, described);
  if (order === 0) {
    return "same";
  }
  return order === 1 ? "newer" : "unknown";
}

export const UNKNOWN_LEAD = "The download button serves the latest release on GitHub.";

// Pure: everything the homepage says about the release, from GitHub's answer.
// A version, date or size appears beside the button, in the status lead or in
// the JSON-LD only when GitHub confirmed a release no older than the one the
// copy describes, dated and carrying the disk image the button downloads. The
// list of changes on main shows only when GitHub confirmed that release itself,
// and the newer-release note only when it reported a later one.
export function releaseCopy(release, described = RELEASE.version) {
  const status = releaseStatus(release, described);
  const date = status === "unknown" ? "" : formatDate(release.publishedAt);
  const size = status === "unknown" ? "" : formatMegabytes(release.dmgBytes);
  if (!date || !size) {
    return { status: "unknown", hero: "", lead: UNKNOWN_LEAD, jsonLd: {}, showNewer: false, showMainList: false };
  }
  return {
    status,
    hero: `Version ${release.version} came out ${date}, as a ${size} download.`,
    lead: `The download button serves version ${release.version}, which came out ${date}.`,
    jsonLd: {
      softwareVersion: release.version,
      datePublished: new Date(Date.parse(release.publishedAt)).toISOString().slice(0, 10)
    },
    showNewer: status === "newer",
    showMainList: status === "same"
  };
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

// "2026-04-21T18:56:16Z" -> "April 21, 2026", in UTC so every visitor and
// every build reads the same date.
export function formatDate(iso) {
  const time = typeof iso === "string" ? Date.parse(iso) : NaN;
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
