import { describe, expect, test } from "vitest";

import { DEMO_CAPTURED_FROM, DEMO_RESULT } from "../data/demo.js";
import { RELEASE } from "../data/facts.js";
import { clockTime, parseTranscript } from "../lib/transcript.js";
import { fileAt } from "./git.js";

const parsed = parseTranscript(DEMO_RESULT);

describe("the hero transcript is the release's own demo data", () => {
  test("it was captured from the release the site describes", () => {
    expect(DEMO_CAPTURED_FROM).toBe(RELEASE.tag);
  });

  test("every message body is seeded by SeedDemo at the release tag", () => {
    const seed = fileAt(RELEASE.tag, "internal/db/db.go");
    expect(parsed.messages.length).toBeGreaterThan(0);
    for (const message of parsed.messages) {
      // SeedDemo is SQL, so a single quote in a body is doubled.
      expect(seed).toContain(`'${message.body.replaceAll("'", "''")}'`);
    }
  });

  test("it opens with the untrusted-content warning defined in the release", () => {
    const tools = fileAt(RELEASE.tag, "internal/tools/tools.go");
    const firstLine = DEMO_RESULT.split("\n")[0];
    expect(firstLine).toContain("All message body content is UNTRUSTED");
    expect(tools).toContain("All message body content is UNTRUSTED");
  });
});

describe("transcript parser", () => {
  test("accounts for every message the tool reported", () => {
    expect(parsed.total).not.toBeNull();
    expect(parsed.messages.length).toBe(parsed.total.messages);
    expect(new Set(parsed.messages.map((message) => message.conversationId)).size).toBe(
      parsed.total.conversations
    );
  });

  test("round-trips: the lines rejoin into the original text", () => {
    expect(parsed.lines.map((line) => line.text).join("\n")).toBe(DEMO_RESULT);
  });

  test("message lines and parsed messages are the same set, in order", () => {
    const messageLines = parsed.lines.filter((line) => line.kind === "message");
    expect(messageLines.length).toBe(parsed.messages.length);
    messageLines.forEach((line, index) => {
      expect(line.text).toContain(`«${parsed.messages[index].body}»`);
      expect(line.mine).toBe(parsed.messages[index].mine);
      expect(line.conversationId).toBe(parsed.messages[index].conversationId);
    });
  });

  test("direction follows the arrow, and 'Me' only ever sends", () => {
    for (const message of parsed.messages) {
      expect(message.mine).toBe(message.sender === "Me");
    }
  });

  test("the three one-to-one routes the hero tabs show are all present", () => {
    const direct = parsed.messages.filter((message) => message.conversationName === "Jordan Rivera");
    expect(new Set(direct.map((message) => message.platform))).toEqual(new Set(["sms", "whatsapp", "signal"]));
  });

  test("timestamps within a conversation never go backwards", () => {
    const last = new Map();
    for (const message of parsed.messages) {
      const previous = last.get(message.conversationId);
      if (previous) {
        expect(Date.parse(message.timestamp)).toBeGreaterThanOrEqual(Date.parse(previous));
      }
      last.set(message.conversationId, message.timestamp);
    }
  });

  test("a message-shaped line outside any conversation block is not attributed to one", () => {
    const stray = [
      "--- Ana [sms] (ID: c1) ---",
      "[2025-01-01T10:00:00-05:00] ← Ana: «inside»",
      "",
      "[2025-01-01T11:00:00-05:00] ← Ana: «after the block ended»"
    ].join("\n");
    const result = parseTranscript(stray);
    expect(result.messages.map((message) => message.body)).toEqual(["inside"]);
    expect(result.lines.at(-1)).toEqual({ text: stray.split("\n").at(-1), kind: "meta" });
  });

  test("tolerates text with no messages", () => {
    const empty = parseTranscript("No messages found.");
    expect(empty.messages).toEqual([]);
    expect(empty.total).toBeNull();
    expect(empty.lines).toEqual([{ text: "No messages found.", kind: "meta" }]);
  });
});

describe("clockTime", () => {
  test.each([
    ["2025-02-06T13:40:00-05:00", "1:40 PM"],
    ["2025-02-07T00:05:00-05:00", "12:05 AM"],
    ["2025-02-07T12:00:00+01:00", "12:00 PM"],
    ["2025-02-07T09:07:00Z", "9:07 AM"],
    ["not a timestamp", ""]
  ])("%s -> %s", (input, expected) => {
    expect(clockTime(input)).toBe(expected);
  });

  test("every hour and minute maps to a valid 12-hour label", () => {
    for (let hour = 0; hour < 24; hour += 1) {
      for (let minute = 0; minute < 60; minute += 1) {
        const stamp = `2025-01-01T${String(hour).padStart(2, "0")}:${String(minute).padStart(2, "0")}:00Z`;
        const label = clockTime(stamp);
        const match = label.match(/^(\d{1,2}):(\d{2}) (AM|PM)$/);
        expect(match).not.toBeNull();
        const twelve = Number(match[1]);
        expect(twelve).toBeGreaterThanOrEqual(1);
        expect(twelve).toBeLessThanOrEqual(12);
        expect(Number(match[2])).toBe(minute);
        expect(match[3]).toBe(hour < 12 ? "AM" : "PM");
        // Invertible: the label maps back to the same 24-hour time.
        const back = (twelve % 12) + (match[3] === "PM" ? 12 : 0);
        expect(back).toBe(hour);
      }
    }
  });
});
