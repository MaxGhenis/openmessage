import { describe, expect, test } from "vitest";

import { RELEASE } from "../data/facts.js";
import {
  FALLBACK_RELEASE,
  formatDate,
  commitsAheadOf,
  formatMegabytes,
  normalizeRelease,
  parseCommitTotal
} from "../lib/release.js";

const payload = {
  tag_name: "v0.2.9",
  published_at: "2026-04-21T18:56:16Z",
  draft: false,
  prerelease: false,
  assets: [
    { name: "openmessage-darwin-arm64.tar.gz", size: 8646227 },
    { name: "OpenMessage.dmg", size: 21661450 }
  ]
};

describe("normalizeRelease", () => {
  test("reads tag, date and DMG size from a GitHub release", () => {
    expect(normalizeRelease(payload)).toEqual({
      tag: "v0.2.9",
      version: "0.2.9",
      publishedAt: "2026-04-21T18:56:16.000Z",
      dmgBytes: 21661450,
      live: true
    });
  });

  test("a release with no DMG still normalizes, with an unknown size", () => {
    expect(normalizeRelease({ ...payload, assets: [] }).dmgBytes).toBeNull();
    expect(normalizeRelease({ ...payload, assets: undefined }).dmgBytes).toBeNull();
  });

  test.each([
    ["null", null],
    ["a string", "v0.2.9"],
    ["an error body", { message: "API rate limit exceeded" }],
    ["a draft", { ...payload, draft: true }],
    ["a prerelease", { ...payload, prerelease: true }],
    ["a non-semver tag", { ...payload, tag_name: "nightly" }],
    ["a suffixed tag", { ...payload, tag_name: "v0.3.0-rc1" }],
    ["a missing date", { ...payload, published_at: null }],
    ["a garbage date", { ...payload, published_at: "soon" }]
  ])("rejects %s", (_label, input) => {
    expect(normalizeRelease(input)).toBeNull();
  });

  test("never returns a version that disagrees with its tag", () => {
    for (let major = 0; major < 3; major += 1) {
      for (let minor = 0; minor < 12; minor += 1) {
        for (let patch = 0; patch < 12; patch += 1) {
          const tag = `v${major}.${minor}.${patch}`;
          const release = normalizeRelease({ ...payload, tag_name: tag });
          expect(`v${release.version}`).toBe(release.tag);
        }
      }
    }
  });

  test("size is a positive integer or null, whatever GitHub sends", () => {
    for (const size of [0, -1, 1.5, "21661450", null, undefined, NaN, Infinity]) {
      const release = normalizeRelease({ ...payload, assets: [{ name: "OpenMessage.dmg", size }] });
      expect(release.dmgBytes).toBeNull();
    }
  });
});

describe("fallback", () => {
  test("is the release the facts were checked against, marked as not live", () => {
    expect(FALLBACK_RELEASE).toMatchObject({ tag: RELEASE.tag, version: RELEASE.version, live: false });
    expect(Number.isNaN(Date.parse(FALLBACK_RELEASE.publishedAt))).toBe(false);
  });
});

describe("parseCommitTotal", () => {
  const link = (last) =>
    `<https://api.github.com/repositories/1/commits?sha=main&per_page=1&page=2>; rel="next", <https://api.github.com/repositories/1/commits?sha=main&per_page=1&page=${last}>; rel="last"`;

  test("reads the last page number as the commit total", () => {
    expect(parseCommitTotal(link(254))).toBe(254);
  });

  test.each([
    ["no header", null],
    ["an empty header", ""],
    ["a header with only prev/first", '<https://api.github.com/x?page=1>; rel="first"'],
    ["a zero page", link(0)],
    ["a non-numeric page", link("abc")]
  ])("returns null for %s", (_label, input) => {
    expect(parseCommitTotal(input)).toBeNull();
  });

  test("does not confuse per_page with page", () => {
    expect(parseCommitTotal('<https://api.github.com/x?per_page=1>; rel="last"')).toBeNull();
    expect(parseCommitTotal('<https://api.github.com/x?page=7&per_page=1>; rel="last"')).toBeNull();
    expect(parseCommitTotal('<https://api.github.com/x?per_page=1&page=7>; rel="last"')).toBe(7);
  });
});

describe("commitsAheadOf", () => {
  test.each([
    [254, 138, 116],
    [138, 138, 0],
    [137, 138, null],
    [null, 138, null],
    [254, null, null],
    [254.5, 138, null]
  ])("main %s, release %s -> %s", (mainTotal, releaseTotal, expected) => {
    expect(commitsAheadOf(mainTotal, releaseTotal)).toBe(expected);
  });
});

describe("formatDate", () => {
  test("formats in UTC, so the date does not shift with the reader's zone", () => {
    expect(formatDate("2026-04-21T18:56:16Z")).toBe("April 21, 2026");
    expect(formatDate("2026-04-21T23:59:59Z")).toBe("April 21, 2026");
    expect(formatDate("2026-04-22T00:00:00Z")).toBe("April 22, 2026");
    // 23:30 on the 21st in New York is already the 22nd in UTC.
    expect(formatDate("2026-04-21T23:30:00-04:00")).toBe("April 22, 2026");
  });

  test("returns an empty string for an unreadable date", () => {
    expect(formatDate("")).toBe("");
    expect(formatDate("soon")).toBe("");
  });

  test("every day of 2026 formats to its own month and day", () => {
    const months = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];
    const start = Date.UTC(2026, 0, 1);
    for (let day = 0; day < 365; day += 1) {
      const date = new Date(start + day * 86_400_000);
      expect(formatDate(date.toISOString())).toBe(
        `${months[date.getUTCMonth()]} ${date.getUTCDate()}, 2026`
      );
    }
  });
});

describe("formatMegabytes", () => {
  test.each([
    [21661450, "22 MB"],
    [9_500_000, "9.5 MB"],
    [10_000_000, "10 MB"],
    [1, "0.0 MB"],
    [0, ""],
    [-5, ""],
    [null, ""],
    [NaN, ""]
  ])("%s -> %s", (input, expected) => {
    expect(formatMegabytes(input)).toBe(expected);
  });

  test("is monotonic: a bigger file never shows a smaller number", () => {
    let previous = 0;
    for (let bytes = 100_000; bytes <= 60_000_000; bytes += 137_531) {
      const value = Number(formatMegabytes(bytes).replace(" MB", ""));
      expect(value).toBeGreaterThanOrEqual(previous);
      previous = value;
    }
  });
});
