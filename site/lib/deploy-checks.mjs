// Checks that stop a production build which would deploy a broken promise.
//
// next.config.mjs runs productionBuildProblem during `next build`. Vercel sets
// VERCEL_ENV at build time as well as at runtime, so the check fires only for
// a production deployment; CI, local builds and previews skip it.

// An error message, or null when the build may go ahead.
export function productionBuildProblem(env, vercelConfig) {
  if (env.VERCEL_ENV !== "production") return null;
  const crons = Array.isArray(vercelConfig?.crons) ? vercelConfig.crons : [];
  if (crons.length > 0 && !env.CRON_SECRET) {
    const paths = crons.map((cron) => cron.path).join(", ");
    return (
      `CRON_SECRET is not set for Production. Vercel Cron calls ${paths} with it as a bearer token, ` +
      "and the route refuses every run without it, so heartbeat records would never expire although " +
      "/privacy says they do. Add it (vercel env add CRON_SECRET production) and deploy again."
    );
  }
  return null;
}
