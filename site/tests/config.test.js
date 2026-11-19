import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, test } from "vitest";

import nextConfig from "../next.config.mjs";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

describe("security headers", () => {
  async function headersFor(source) {
    const rules = await nextConfig.headers();
    const rule = rules.find((entry) => entry.source === source);
    expect(rule, `no header rule for ${source}`).toBeDefined();
    return Object.fromEntries(rule.headers.map(({ key, value }) => [key, value]));
  }

  function directives(policy) {
    return Object.fromEntries(
      policy
        .split(";")
        .map((part) => part.trim().split(/\s+/))
        .filter((words) => words[0])
        .map(([name, ...values]) => [name, values])
    );
  }

  test("every path gets a CSP that blocks plugins, <base> rewrites, outside form posts and framing", async () => {
    const policy = directives((await headersFor("/:path*"))["Content-Security-Policy"]);
    expect(policy["object-src"]).toEqual(["'none'"]);
    expect(policy["base-uri"]).toEqual(["'self'"]);
    expect(policy["form-action"]).toEqual(["'self'"]);
    expect(policy["frame-ancestors"]).toEqual(["'none'"]);
  });

  test("the CSP leaves scripts and styles alone, so inline RSC payloads and the theme script run", async () => {
    const policy = directives((await headersFor("/:path*"))["Content-Security-Policy"]);
    for (const name of ["default-src", "script-src", "script-src-elem", "style-src", "style-src-elem"]) {
      expect(policy, name).not.toHaveProperty(name);
    }
  });

  test("frame-ancestors and X-Frame-Options agree", async () => {
    const headers = await headersFor("/:path*");
    expect(headers["X-Frame-Options"]).toBe("DENY");
    expect(directives(headers["Content-Security-Policy"])["frame-ancestors"]).toEqual(["'none'"]);
  });
});

describe("repo-root deploy guard", () => {
  test("a deploy from the repo root uploads vercel.json and nothing else", () => {
    const rules = readFileSync(path.join(repoRoot, ".vercelignore"), "utf8")
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith("#"));
    expect(rules).toEqual(["*", "!vercel.json"]);
  });

  test("the root vercel.json build fails", () => {
    const config = JSON.parse(readFileSync(path.join(repoRoot, "vercel.json"), "utf8"));
    expect(config.buildCommand).toMatch(/&& exit 1$/);
  });
});
