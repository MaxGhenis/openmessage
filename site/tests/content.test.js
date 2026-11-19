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

  // A repeated key in an object literal silently replaces the earlier entry,
  // so read the keys from the source rather than from the imported object.
  test("defines no receipt key twice", () => {
    const source = readFileSync(path.join(siteRoot, "data/facts.js"), "utf8");
    const start = source.indexOf("export const RECEIPTS = {");
    expect(start).toBeGreaterThan(-1);
    const block = source.slice(start, source.indexOf("\n};", start));
    const keys = [...block.matchAll(/^ {2}(\w+): \{/gm)].map((match) => match[1]);
    expect(keys.length).toBe(Object.keys(RECEIPTS).length);
    expect(keys.filter((key, index) => keys.indexOf(key) !== index)).toEqual([]);
  });

  test("the README's example receipt matches its entry in data/facts.js", () => {
    const readme = readFileSync(path.join(siteRoot, "README.md"), "utf8");
    const example = readme.match(/^(\w+): \{ ref: .* \}$/m);
    expect(example).not.toBeNull();
    const receipt = RECEIPTS[example[1]];
    expect(receipt, `README cites "${example[1]}", which is not a receipt`).toBeDefined();
    const quote = (value) => (value.includes('"') ? `'${value}'` : `"${value}"`);
    expect(example[0]).toBe(
      `${example[1]}: { ref: ${quote(receipt.ref)}, path: ${quote(receipt.path)}, line: ${receipt.line}, expect: ${quote(receipt.expect)} }`
    );
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
  test("each blog post has a page at its slug", () => {
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
