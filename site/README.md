# openmessage.ai

The marketing site: a Next.js app deployed to Vercel from this directory.

```bash
bun install
bun run dev      # http://localhost:3000
bun run test     # checks every claim against the code (needs git history and tags)
bun run build
```

## How the site stays true to the code

Site copy states facts about a specific build. Those facts live in `data/facts.js`, each next to the file and line that proves it:

```js
untrustedPreamble: { ref: "release", path: "internal/tools/tools.go", line: 63, expect: "const messagePreamble" }
```

On the page a fact shows as a read receipt: a double check mark linking to that line on GitHub, pinned to the release tag (`ref: "release"`) or to a commit on main (`ref: "main"`).

`bun run test` reads each cited file at its git ref and fails when:

- a cited line no longer contains its `expect` text;
- the tool list differs from the `mcp.NewTool(...)` names registered at the release tag;
- the connect command's host, port or path differs from the release's defaults;
- a commit count or co-author count differs from git;
- a message in the hero transcript is not in the release's demo seed data;
- a page uses a receipt that is not defined, or a tool name the release does not have.

The hero transcript in `data/demo.js` is real output. It was captured from the release build in demo mode; the file's header says how to capture it again.

## When a release ships

The Download button follows GitHub's latest release, and the homepage asks GitHub which release that is. If it is newer than `RELEASE.tag` in `data/facts.js`, the homepage says it still describes the older version. To catch up:

1. Set `RELEASE` to the new tag and move `MAIN_COMMIT` to current main.
2. Run `bun run test`. Fix each receipt it flags by re-reading the code at the new ref. If the claim is no longer true, change the copy.
3. Re-capture `data/demo.js` from the new build.
4. Check the agent setup command against the new build (main serves MCP over stdio from the app, where 0.2.9 used SSE).
5. Deploy.

## Deploying

Nothing deploys on a push. Use the Vercel CLI from this directory, from a clean checkout of the commit to ship:

```bash
vercel link --yes --project openmessage --scope max-ghenis-projects   # once per checkout
vercel deploy --scope max-ghenis-projects                             # preview
vercel deploy --prod --scope max-ghenis-projects                      # production
```

Previews share production's Blob store, so never submit the email form or POST a heartbeat on a preview.

## Server routes

| Route | Used by | Behavior |
|---|---|---|
| `POST /api/heartbeat` | App processes started with `OPENMESSAGE_TELEMETRY=1` | Stores one record per install per UTC day. `GET` returns `{"ok":true,"schema_version":1}`. |
| `GET /download` | Download buttons and shared links | 307 to the disk image on the latest GitHub release. |
| `POST /api/waitlist` | The release-email form | Stores the address encrypted with AES-256-GCM. |

Environment variables (set in Vercel): `BLOB_READ_WRITE_TOKEN` for both API routes, `WAITLIST_ENCRYPTION_KEY` (64 hex characters) for the waitlist.

To read the release-email list:

```bash
vercel env pull .env.local --environment production --scope max-ghenis-projects
bun --env-file=.env.local scripts/read-waitlist.mjs
```

The site sets no cookies and loads no analytics. Fonts are downloaded at build time and served from this origin.
