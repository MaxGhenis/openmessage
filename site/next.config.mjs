import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { PHASE_PRODUCTION_BUILD } from "next/constants.js";

import { productionBuildProblem } from "./lib/deploy-checks.mjs";

const dirname = path.dirname(fileURLToPath(import.meta.url));

const securityHeaders = [
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" }
];

const nextConfig = {
  turbopack: {
    root: dirname
  },
  async headers() {
    return [{ source: "/:path*", headers: securityHeaders }];
  },
  async redirects() {
    return [
      // The investor memo that used to live here was marked "not for public
      // distribution"; send old links to the homepage instead of a 404.
      { source: "/thesis", destination: "/", permanent: false }
    ];
  }
};

// A production build stops if it would ship a promise the deployment can't
// keep (lib/deploy-checks.mjs).
export default function config(phase) {
  if (phase === PHASE_PRODUCTION_BUILD) {
    const vercelConfig = JSON.parse(readFileSync(path.join(dirname, "vercel.json"), "utf8"));
    const problem = productionBuildProblem(process.env, vercelConfig);
    if (problem) throw new Error(problem);
  }
  return nextConfig;
}
