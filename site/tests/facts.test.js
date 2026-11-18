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
  UNWARNED_QUOTING_TOOLS,
  WARNED_TOOLS,
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

  // The release's internal/tools package as top-level function bodies keyed by
  // name. gofmt puts each top-level func's closing brace alone at column 0.
  function packageFunctions(ref) {
    const bodies = new Map();
    for (const file of listAt(ref, "internal/tools")) {
      if (!file.endsWith(".go") || file.endsWith("_test.go")) {
        continue;
      }
      for (const chunk of fileAt(ref, file).split(/^(?=func )/m)) {
        const name = chunk.match(/^func (?:\([^)]*\)\s*)?(\w+)/);
        if (name) {
          const end = chunk.search(/^}$/m);
          bodies.set(name[1], end === -1 ? chunk : chunk.slice(0, end + 1));
        }
      }
    }
    return bodies;
  }

  // True if the function, or any package function it calls, mentions needle.
  function reaches(bodies, start, needle) {
    const seen = new Set();
    const stack = [start];
    while (stack.length > 0) {
      const fn = stack.pop();
      if (seen.has(fn) || !bodies.has(fn)) {
        continue;
      }
      seen.add(fn);
      const body = bodies.get(fn);
      if (body.includes(needle)) {
        return true;
      }
      for (const [, called] of body.matchAll(/\b(\w+)\(/g)) {
        stack.push(called);
      }
    }
    return false;
  }

  test("the tools the site says warn about message text are exactly the ones that do", () => {
    const bodies = packageFunctions(RELEASE.tag);
    expect(fileAt(RELEASE.tag, "internal/tools/tools.go")).toContain("const messagePreamble");
    const handlers = new Map();
    for (const body of bodies.values()) {
      for (const [, toolFn, handlerFn] of body.matchAll(/s\.AddTool\((\w+)\(\),\s*(\w+)\(a\)\)/g)) {
        const name = bodies.get(toolFn).match(/mcp\.NewTool\(\s*"([a-z_]+)"/)[1];
        handlers.set(name, handlerFn);
      }
    }
    expect([...handlers.keys()].sort()).toEqual([...siteTools].sort());

    const warned = [...handlers]
      .filter(([, handlerFn]) => reaches(bodies, handlerFn, "messagePreamble"))
      .map(([name]) => name);
    expect(warned.sort()).toEqual([...WARNED_TOOLS].sort());
    for (const name of UNWARNED_QUOTING_TOOLS) {
      expect(handlers.has(name), `${name} is not a tool in the release`).toBe(true);
      expect(warned).not.toContain(name);
    }
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
  test("commit count", () => {
    expect(Number(git(["rev-list", "--count", MAIN_COMMIT]).trim())).toBe(MAIN_STATS.commits);
  });

  test("the release is an ancestor of main, so main-only changes landed after it", () => {
    expect(git(["rev-list", "--count", `${MAIN_COMMIT}..${RELEASE.tag}`]).trim()).toBe("0");
  });

  test("commits carrying a Claude co-author trailer", () => {
    const bodies = git(["log", MAIN_COMMIT, "--format=%B%x00"]).split("\0");
    const withTrailer = bodies.filter((body) => /^co-authored-by:\s*claude/im.test(body)).length;
    expect(withTrailer).toBe(MAIN_STATS.commitsWithClaudeTrailer);
    expect(withTrailer).toBeLessThanOrEqual(MAIN_STATS.commits);
  });
});
