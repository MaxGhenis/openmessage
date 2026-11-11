import fc from "fast-check";
import { describe, expect, test } from "vitest";

import {
  commitsAheadOf,
  formatDate,
  formatMegabytes,
  normalizeRelease,
  parseCommitTotal
} from "../lib/release.js";
import { clockTime, parseTranscript } from "../lib/transcript.js";

// Invariants, stated for every input rather than for examples:
//   parseTranscript  — lossless (its lines rejoin into the input), total (never
//                      throws), and faithful (a rendered conversation parses
//                      back to the same messages, in order).
//   normalizeRelease — total; returns null or a release whose version matches
//                      its tag and whose size is a positive integer or null.
//   parseCommitTotal — total; returns null or a positive safe integer, and
//                      recovers the page number it was given.
//   commitsAheadOf   — null or a non-negative integer equal to the difference.
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

  test("parseCommitTotal is total and returns null or a positive safe integer", () => {
    fc.assert(
      fc.property(fc.anything(), (header) => {
        const total = parseCommitTotal(header);
        expect(total === null || (Number.isSafeInteger(total) && total > 0)).toBe(true);
      })
    );
  });

  test("parseCommitTotal recovers the last page it is given", () => {
    fc.assert(
      fc.property(fc.integer({ min: 1, max: 10_000_000 }), (count) => {
        const header = `<https://api.github.com/repositories/1/commits?sha=main&per_page=1&page=2>; rel="next", <https://api.github.com/repositories/1/commits?sha=main&per_page=1&page=${count}>; rel="last"`;
        expect(parseCommitTotal(header)).toBe(count);
      })
    );
  });

  test("commitsAheadOf is the non-negative difference or null", () => {
    fc.assert(
      fc.property(fc.integer({ min: 0, max: 1_000_000 }), fc.integer({ min: 0, max: 1_000_000 }), (main, release) => {
        const ahead = commitsAheadOf(main, release);
        if (main >= release) {
          expect(ahead).toBe(main - release);
        } else {
          expect(ahead).toBeNull();
        }
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
      fc.property(fc.string(), (text) => {
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
