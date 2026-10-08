import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, test } from "vitest";

import { BLOG_POSTS, RECEIPTS, SENDING_TOOLS, TOOL_GROUPS } from "../data/facts.js";

const siteRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

function sources(dir) {
  return readdirSync(path.join(siteRoot, dir)).flatMap((name) => {
    const relative = path.join(dir, name);
    if (statSync(path.join(siteRoot, relative)).isDirectory()) {
      return sources(relative);
    }
    return /\.jsx?$/.test(name) ? [relative] : [];
  });
}

const pages = [...sources("app"), ...sources("components")].map((file) => ({
  file,
  text: readFileSync(path.join(siteRoot, file), "utf8")
}));
const copy = pages.map((page) => page.text).join("\n");
const tools = TOOL_GROUPS.flatMap((group) => group.tools);

describe("receipts in use", () => {
  const used = new Set();
  for (const match of copy.matchAll(/<Receipts\s+ids=\{\[([^\]]*)\]\}/g)) {
    for (const id of match[1].matchAll(/"([A-Za-z]+)"/g)) {
      used.add(id[1]);
    }
  }

  test("every receipt a page shows is defined", () => {
    expect(used.size).toBeGreaterThan(0);
    for (const id of used) {
      expect(RECEIPTS, `receipt "${id}" is used but not defined`).toHaveProperty(id);
    }
  });

  test("every defined receipt is shown somewhere", () => {
    expect(Object.keys(RECEIPTS).filter((id) => !used.has(id))).toEqual([]);
  });
});

describe("tool names in copy", () => {
  // snake_case words in <code> that are not tools.
  const notTools = new Set(["source_platform"]);

  test("every tool the pages name exists in the release", () => {
    const named = [...copy.matchAll(/<code>([a-z]+(?:_[a-z]+)+)<\/code>/g)].map((match) => match[1]);
    expect(named.length).toBeGreaterThan(0);
    for (const name of named) {
      if (!notTools.has(name)) {
        expect(tools, `"${name}" is named on the site but is not a tool in the release`).toContain(name);
      }
    }
  });

  test("the tools flagged as sending to other people are real tools, and none is missed", () => {
    for (const name of SENDING_TOOLS) {
      expect(tools).toContain(name);
    }
    // Any tool whose name says it sends or reacts must be in the list.
    const shouldBeListed = tools.filter((name) => /^(send|react)_/.test(name));
    expect([...SENDING_TOOLS].sort()).toEqual(shouldBeListed.sort());
  });
});

describe("pages", () => {
  test("each engineering note has a page at its slug", () => {
    for (const post of BLOG_POSTS) {
      expect(pages.map((page) => page.file)).toContain(path.join("app", "blog", post.slug, "page.jsx"));
      expect(Date.parse(post.revised)).toBeGreaterThanOrEqual(Date.parse(post.published));
    }
  });

  test("the site loads nothing from an analytics or font host", () => {
    for (const host of ["googletagmanager.com", "google-analytics.com", "fonts.googleapis.com/css", "gtag("]) {
      const offenders = pages.filter((page) => page.text.includes(host) && !page.file.includes("privacy"));
      // data/facts.js cites the app's own font request; no site source may load one.
      expect(offenders.map((page) => page.file), host).toEqual([]);
    }
  });
});
