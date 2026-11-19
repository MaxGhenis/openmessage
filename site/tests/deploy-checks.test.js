import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { PHASE_DEVELOPMENT_SERVER, PHASE_PRODUCTION_BUILD, PHASE_PRODUCTION_SERVER } from "next/constants.js";
import { afterEach, test } from "vitest";

import { productionBuildProblem } from "../lib/deploy-checks.mjs";
import config from "../next.config.mjs";

const site = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const vercelConfig = JSON.parse(readFileSync(path.join(site, "vercel.json"), "utf8"));
const CRONS = { crons: [{ path: "/api/cron/expire-heartbeats", schedule: "17 4 * * *" }] };

test("a production build with a scheduled cron needs CRON_SECRET", () => {
  assert.match(productionBuildProblem({ VERCEL_ENV: "production" }, CRONS), /CRON_SECRET is not set.*\/api\/cron\/expire-heartbeats/);
  assert.match(productionBuildProblem({ VERCEL_ENV: "production", CRON_SECRET: "" }, CRONS), /CRON_SECRET/);
  assert.equal(productionBuildProblem({ VERCEL_ENV: "production", CRON_SECRET: "x" }, CRONS), null);
  assert.equal(productionBuildProblem({ VERCEL_ENV: "production" }, {}), null, "no crons, nothing to authorize");
  assert.equal(productionBuildProblem({ VERCEL_ENV: "production" }, { crons: [] }), null);
  for (const VERCEL_ENV of [undefined, "", "preview", "development", "Production"]) {
    assert.equal(productionBuildProblem({ VERCEL_ENV }, CRONS), null, `CI, local and preview builds skip it: ${VERCEL_ENV}`);
  }
  assert.match(productionBuildProblem({ VERCEL_ENV: "production" }, vercelConfig), /CRON_SECRET/, "site/vercel.json schedules a cron");
});

const saved = { VERCEL_ENV: process.env.VERCEL_ENV, CRON_SECRET: process.env.CRON_SECRET };
afterEach(() => {
  for (const [key, value] of Object.entries(saved)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
});

test("next.config.mjs stops a production build without CRON_SECRET, and only a build", () => {
  process.env.VERCEL_ENV = "production";
  delete process.env.CRON_SECRET;
  assert.throws(() => config(PHASE_PRODUCTION_BUILD), /CRON_SECRET is not set/);
  for (const phase of [PHASE_DEVELOPMENT_SERVER, PHASE_PRODUCTION_SERVER]) {
    assert.equal(typeof config(phase).headers, "function", phase);
  }
  process.env.CRON_SECRET = "s3cret";
  assert.equal(typeof config(PHASE_PRODUCTION_BUILD).headers, "function");
  // tests/config.test.js checks the headers on the server phase's config.
  assert.equal(config(PHASE_PRODUCTION_BUILD), config(PHASE_PRODUCTION_SERVER), "a build gets the same config as every other phase");
  delete process.env.CRON_SECRET;
  process.env.VERCEL_ENV = "preview";
  assert.equal(typeof config(PHASE_PRODUCTION_BUILD).redirects, "function", "a preview build goes ahead");
});
