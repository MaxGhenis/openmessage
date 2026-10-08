import { describe, expect, test } from "vitest";

import {
  CLAUDE_MCP_COMMAND,
  MAIN_COMMIT,
  MAIN_STATS,
  MCP,
  MIN_MACOS,
  RECEIPTS,
  RELEASE,
  TOOL_COUNT,
  TOOL_GROUPS,
  receiptLabel,
  receiptUrl
} from "../data/facts.js";
import { fileAt, git, listAt, refFor } from "./git.js";

describe("receipts point at code that says what the site says", () => {
  test.each(Object.entries(RECEIPTS))("%s", (_id, receipt) => {
    const lines = fileAt(refFor(receipt), receipt.path).split("\n");
    expect(receipt.line).toBeGreaterThan(0);
    expect(receipt.line).toBeLessThanOrEqual(lines.length);
    expect(lines[receipt.line - 1]).toContain(receipt.expect);
  });

  test("every receipt links to an immutable ref", () => {
    for (const receipt of Object.values(RECEIPTS)) {
      expect(["release", "main"]).toContain(receipt.ref);
      expect(receiptUrl(receipt)).toMatch(
        /^https:\/\/github\.com\/MaxGhenis\/openmessage\/blob\/(v\d+\.\d+\.\d+|[0-9a-f]{40})\/.+#L\d+$/
      );
      expect(receiptLabel(receipt)).toBe(`${receipt.path}:${receipt.line}`);
    }
  });
});

describe("MCP tools", () => {
  const siteTools = TOOL_GROUPS.flatMap((group) => group.tools);

  function registeredTools(ref) {
    const names = new Set();
    for (const file of listAt(ref, "internal/tools")) {
      if (!file.endsWith(".go") || file.endsWith("_test.go")) {
        continue;
      }
      for (const match of fileAt(ref, file).matchAll(/mcp\.NewTool\(\s*"([a-z_]+)"/g)) {
        names.add(match[1]);
      }
    }
    return [...names].sort();
  }

  test("the site lists exactly the tools the release registers", () => {
    expect([...siteTools].sort()).toEqual(registeredTools(RELEASE.tag));
  });

  test("no tool is listed twice and the count matches", () => {
    expect(new Set(siteTools).size).toBe(siteTools.length);
    expect(TOOL_COUNT).toBe(siteTools.length);
  });

  test("the connect command uses the release's default host, port and mount", () => {
    expect(CLAUDE_MCP_COMMAND).toBe(
      `claude mcp add -s user --transport sse openmessage http://${MCP.host}:${MCP.port}${MCP.ssePath}`
    );
    const serve = fileAt(RELEASE.tag, "cmd/serve.go");
    expect(serve).toContain(`port = "${MCP.port}"`);
    expect(serve).toContain(`host = "${MCP.host}"`);
    expect(MCP.ssePath).toBe("/mcp/sse");
    expect(serve).toContain("MCP SSE available at");
  });
});

describe("release facts", () => {
  test("the release tag exists and matches the app version it shipped", () => {
    const plist = fileAt(RELEASE.tag, "macos/OpenMessage/Sources/Info.plist");
    expect(plist).toContain(`<string>${RELEASE.version}</string>`);
    expect(RELEASE.tag).toBe(`v${RELEASE.version}`);
  });

  test("minimum macOS matches the Swift package", () => {
    expect(fileAt(RELEASE.tag, "macos/OpenMessage/Package.swift")).toContain(`.macOS(.v${MIN_MACOS})`);
  });

  test("the release has no connection supervisors or outbox, so the site may not claim them for it", () => {
    const files = listAt(RELEASE.tag, "cmd").concat(listAt(RELEASE.tag, "internal"));
    expect(files.some((file) => file.includes("supervisor"))).toBe(false);
    expect(files.some((file) => file.includes("outbox"))).toBe(false);
  });
});

describe("numbers quoted about main", () => {
  test("commit counts", () => {
    expect(Number(git(["rev-list", "--count", MAIN_COMMIT]).trim())).toBe(MAIN_STATS.commits);
    expect(Number(git(["rev-list", "--count", `${RELEASE.tag}..${MAIN_COMMIT}`]).trim())).toBe(
      MAIN_STATS.commitsSinceRelease
    );
    expect(Number(git(["rev-list", "--count", RELEASE.tag]).trim())).toBe(MAIN_STATS.commitsAtRelease);
  });

  test("the release is an ancestor of main, so the counts add up", () => {
    expect(git(["rev-list", "--count", `${MAIN_COMMIT}..${RELEASE.tag}`]).trim()).toBe("0");
    expect(MAIN_STATS.commitsAtRelease + MAIN_STATS.commitsSinceRelease).toBe(MAIN_STATS.commits);
  });

  test("commits carrying a Claude co-author trailer", () => {
    const bodies = git(["log", MAIN_COMMIT, "--format=%B%x00"]).split("\0");
    const withTrailer = bodies.filter((body) => /^co-authored-by:\s*claude/im.test(body)).length;
    expect(withTrailer).toBe(MAIN_STATS.commitsWithClaudeTrailer);
    expect(withTrailer).toBeLessThanOrEqual(MAIN_STATS.commits);
  });
});
