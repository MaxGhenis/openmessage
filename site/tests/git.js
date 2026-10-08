import { execFileSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { MAIN_COMMIT, RELEASE } from "../data/facts.js";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

export function git(args) {
  return execFileSync("git", args, { cwd: repoRoot, encoding: "utf8", maxBuffer: 256 * 1024 * 1024 });
}

export function refFor(receipt) {
  return receipt.ref === "release" ? RELEASE.tag : MAIN_COMMIT;
}

// Reads a file as it was at a git ref. CI checks out with full history and tags
// (see the Site Build job) so both refs resolve.
export function fileAt(ref, filePath) {
  return git(["show", `${ref}:${filePath}`]);
}

export function listAt(ref, dir) {
  return git(["ls-tree", "-r", "--name-only", ref, "--", dir]).split("\n").filter(Boolean);
}
