import fc from "fast-check";
import { describe, expect, test } from "vitest";

import {
  compareVersions,
  formatDate,
  formatMegabytes,
  normalizeRelease,
  releaseCopy,
  releaseStatus
} from "../lib/release.js";
import { clockTime, parseTranscript } from "../lib/transcript.js";

// Invariants, stated for every input rather than for examples:
//   parseTranscript  — lossless (its lines rejoin into the input), total (never
//                      throws), and faithful (a rendered conversation parses
//                      back to the same messages, in order).
//   normalizeRelease — total; returns null or a release whose version matches
//                      its tag and whose size is a positive integer or null.
//   compareVersions  — total; on well-formed versions it is reflexive,
//                      antisymmetric and agrees with numeric tuple order.
//   releaseStatus    — total; "newer" exactly when GitHub answered with a
//                      later version, "same" exactly when it answered with
//                      the described one, and "unknown" otherwise.
//   releaseCopy      — total; a version, date or size appears in the hero,
//                      the status lead or the JSON-LD only for a release
//                      GitHub confirmed, no older than the described one,
//                      dated and with a disk image; the list of changes on
//                      main shows only for "same".
//   formatMegabytes  — monotonic in the byte count.
//   clockTime        — empty or a valid 12-hour label.

const platform = fc.constantFrom("sms", "whatsapp", "signal", "imessage", "gchat");
const word = fc.stringMatching(/^[A-Za-z0-9][A-Za-z0-9 .'!?,-]{0,30}$/);
const body = fc
  .string({ maxLength: 80 })
  .map((text) => text.replace(/[\r\n]/g, " "));
const timestamp = fc
  .date({ min: new Date("2020-01-01T00:00:00Z"), max: new Date("2030-01-01T00:00:00Z"), noInvalidDate: true })
  .map((date) => date.toISOString().replace(/\.\d{3}Z$/, "-05:00"));

const message = fc.record({ timestamp, mine: fc.boolean(), sender: word, body });
const block = fc.record({
  name: word,
  platform,
  conversationId: fc.stringMatching(/^[a-z0-9:-]{1,24}$/),
  messages: fc.array(message, { minLength: 1, maxLength: 6 })
});

// Renders blocks the way internal/tools formats get_person_messages output.
function render(blocks) {
  const out = ["Messages with 'Someone' across N conversation(s):", ""];
  for (const item of blocks) {
    out.push(`--- ${item.name} [${item.platform}] (ID: ${item.conversationId}) ---`);
    for (const entry of item.messages) {
      const who = entry.mine ? "→ Me" : `← ${entry.sender}`;
      out.push(`[${entry.timestamp}] ${who}: «${entry.body}»`);
    }
    out.push("");
  }
  const count = blocks.reduce((sum, item) => sum + item.messages.length, 0);
  out.push(`Total: ${count} messages across ${blocks.length} conversation(s)`);
  return out.join("\n");
}

describe("parseTranscript properties", () => {
  test("lossless and total for any text", () => {
    fc.assert(
      fc.property(fc.string({ maxLength: 400 }), (text) => {
        const parsed = parseTranscript(text);
        expect(parsed.lines.map((line) => line.text).join("\n")).toBe(text);
        expect(parsed.messages.length).toBeLessThanOrEqual(parsed.lines.length);
      })
    );
  });

  test("a rendered conversation parses back to the same messages, in order", () => {
    fc.assert(
      fc.property(fc.array(block, { minLength: 1, maxLength: 5 }), (blocks) => {
        const parsed = parseTranscript(render(blocks));
        const expected = blocks.flatMap((item) =>
          item.messages.map((entry) => ({
            timestamp: entry.timestamp,
            mine: entry.mine,
            sender: entry.mine ? "Me" : entry.sender,
            body: entry.body,
            platform: item.platform,
            conversationId: item.conversationId,
            conversationName: item.name
          }))
        );
        expect(parsed.messages).toEqual(expected);
        expect(parsed.total).toEqual({ messages: expected.length, conversations: blocks.length });
        expect(parsed.lines.filter((line) => line.kind === "block").length).toBe(blocks.length);
      })
    );
  });

  test("every message line is attributed to the block above it", () => {
    fc.assert(
      fc.property(fc.array(block, { minLength: 1, maxLength: 5 }), (blocks) => {
        let current = null;
        for (const line of parseTranscript(render(blocks)).lines) {
          if (line.kind === "block") {
            current = line.conversationId;
          } else if (line.kind === "message") {
            expect(line.conversationId).toBe(current);
          } else {
            expect(line.conversationId).toBeUndefined();
          }
        }
      })
    );
  });
});

describe("release helper properties", () => {
  test("normalizeRelease is total and returns null or a well-formed release", () => {
    fc.assert(
      fc.property(fc.anything(), (payload) => {
        const release = normalizeRelease(payload);
        if (release !== null) {
          expect(release.tag).toMatch(/^v\d+\.\d+\.\d+$/);
          expect(`v${release.version}`).toBe(release.tag);
          // Canonical ISO form, whatever shape the payload's date had.
          expect(new Date(Date.parse(release.publishedAt)).toISOString()).toBe(release.publishedAt);
          expect(release.dmgBytes === null || (Number.isInteger(release.dmgBytes) && release.dmgBytes > 0)).toBe(true);
          expect(release.live).toBe(true);
        }
      })
    );
  });

  test("normalizeRelease accepts every published semver release", () => {
    fc.assert(
      fc.property(
        fc.tuple(fc.nat(99), fc.nat(99), fc.nat(99)),
        fc.date({ min: new Date("2026-01-01T00:00:00Z"), max: new Date("2035-01-01T00:00:00Z"), noInvalidDate: true }),
        fc.option(fc.integer({ min: 1, max: 2 ** 31 }), { nil: undefined }),
        ([major, minor, patch], published, bytes) => {
          const tag = `v${major}.${minor}.${patch}`;
          const assets = bytes === undefined ? [] : [{ name: "OpenMessage.dmg", size: bytes }];
          const release = normalizeRelease({ tag_name: tag, published_at: published.toISOString(), assets });
          expect(release).toEqual({
            tag,
            version: tag.slice(1),
            publishedAt: published.toISOString(),
            dmgBytes: bytes ?? null,
            live: true
          });
        }
      )
    );
  });

  const version = fc
    .tuple(fc.nat({ max: 999 }), fc.nat({ max: 999 }), fc.nat({ max: 999 }))
    .map((parts) => ({ parts, text: parts.join(".") }));
  const tupleOrder = (left, right) => {
    for (let index = 0; index < 3; index += 1) {
      if (left[index] !== right[index]) {
        return left[index] < right[index] ? -1 : 1;
      }
    }
    return 0;
  };

  test("compareVersions agrees with numeric tuple order and is antisymmetric", () => {
    fc.assert(
      fc.property(version, version, (a, b) => {
        const order = compareVersions(a.text, b.text);
        expect(order).toBe(tupleOrder(a.parts, b.parts));
        expect(compareVersions(b.text, a.text)).toBe(-order || 0);
        expect(compareVersions(a.text, a.text)).toBe(0);
      })
    );
  });

  test("compareVersions is total and returns null for anything but x.y.z", () => {
    fc.assert(
      fc.property(fc.anything(), fc.anything(), (a, b) => {
        const order = compareVersions(a, b);
        expect([null, -1, 0, 1]).toContain(order);
        const wellFormed = (value) => typeof value === "string" && /^\d+\.\d+\.\d+$/.test(value);
        if (!wellFormed(a) || !wellFormed(b)) {
          expect(order).toBeNull();
        }
      })
    );
  });

  test("releaseStatus says newer or same only when GitHub answered", () => {
    fc.assert(
      fc.property(version, version, fc.boolean(), (reported, described, live) => {
        const status = releaseStatus({ version: reported.text, live }, described.text);
        const order = tupleOrder(reported.parts, described.parts);
        const expected = !live ? "unknown" : order === 1 ? "newer" : order === 0 ? "same" : "unknown";
        expect(status).toBe(expected);
      })
    );
  });

  test("releaseCopy names a version only for a confirmed release no older than the described one", () => {
    const publishedAt = fc.oneof(
      fc
        .date({ min: new Date("2020-01-01T00:00:00Z"), max: new Date("2030-01-01T00:00:00Z"), noInvalidDate: true })
        .map((date) => date.toISOString()),
      fc.string()
    );
    const dmgBytes = fc.oneof(fc.constant(null), fc.integer({ min: -5, max: 2_000_000_000 }));
    fc.assert(
      fc.property(version, version, fc.boolean(), publishedAt, dmgBytes, (reported, described, live, published, size) => {
        const release = { version: reported.text, live, publishedAt: published, dmgBytes: size };
        const copy = releaseCopy(release, described.text);
        const order = tupleOrder(reported.parts, described.parts);
        const named = copy.hero !== "";
        expect(named).toBe(live && order >= 0 && !Number.isNaN(Date.parse(published)) && size > 0);
        expect(Object.keys(copy.jsonLd).length > 0).toBe(named);
        if (named) {
          expect(copy.hero).toContain(`Version ${reported.text} came out`);
          expect(copy.hero).toContain(formatMegabytes(size));
          expect(copy.lead).toContain(`version ${reported.text},`);
          expect(copy.jsonLd.softwareVersion).toBe(reported.text);
        } else {
          expect(copy.lead).not.toMatch(/\d+\.\d+\.\d+/);
        }
        expect(copy.showMainList).toBe(named && order === 0);
        expect(copy.showNewer).toBe(named && order === 1);
      })
    );
  });

  test("releaseCopy is total", () => {
    fc.assert(
      fc.property(fc.anything(), fc.anything(), (release, described) => {
        const copy = releaseCopy(release, described);
        expect(typeof copy.hero).toBe("string");
        expect(typeof copy.lead).toBe("string");
      })
    );
  });

  test("releaseStatus is total", () => {
    fc.assert(
      fc.property(fc.anything(), fc.anything(), (release, described) => {
        expect(["same", "newer", "unknown"]).toContain(releaseStatus(release, described));
      })
    );
  });

  test("formatMegabytes never shows a smaller number for a bigger file", () => {
    const value = (bytes) => Number(formatMegabytes(bytes).replace(" MB", ""));
    fc.assert(
      fc.property(fc.integer({ min: 1, max: 5_000_000_000 }), fc.integer({ min: 0, max: 5_000_000_000 }), (bytes, extra) => {
        expect(value(bytes + extra)).toBeGreaterThanOrEqual(value(bytes));
      })
    );
  });

  test("formatDate is total and deterministic", () => {
    fc.assert(
      fc.property(fc.oneof(fc.string(), fc.anything()), (text) => {
        const once = formatDate(text);
        expect(typeof once).toBe("string");
        expect(formatDate(text)).toBe(once);
      })
    );
  });
});

describe("clockTime properties", () => {
  test("rejects hours past 23 and minutes past 59 instead of inventing a time", () => {
    fc.assert(
      fc.property(fc.integer({ min: 0, max: 99 }), fc.integer({ min: 0, max: 99 }), (hour, minute) => {
        const label = clockTime(`2025-01-01T${String(hour).padStart(2, "0")}:${String(minute).padStart(2, "0")}:00Z`);
        if (hour > 23 || minute > 59) {
          expect(label).toBe("");
        } else {
          expect(label).toMatch(/^(1[0-2]|[1-9]):[0-5]\d (AM|PM)$/);
        }
      })
    );
  });

  test("returns an empty string or a valid 12-hour label for any text", () => {
    fc.assert(
      fc.property(fc.string(), (text) => {
        expect(clockTime(text)).toMatch(/^$|^(1[0-2]|[1-9]):[0-5]\d (AM|PM)$/);
      })
    );
  });
});
