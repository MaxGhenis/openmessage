# OpenMessage Release Checklist

Use this before shipping a TestFlight/App Store build, public website update, or signed macOS release.

## Preparing the v0.3.0 GitHub release

The release-preparation PR proposes version 0.3.0 / macOS build 19. Review
[the release notes](releases/v0.3.0.md) with the code and keep the PR in draft
until the remaining platform checks below are complete. Opening or merging the
PR does not publish a release.

- Base the release on `MaxGhenis/openmessage` main. Do not merge a local
  integration branch or rely on an unmerged contributor branch to build it.
- Keep the checked-in dependency graph. The existing Google Messages replacement
  is the public `MaxGhenis/gmessages` fork at
  `0e43542dfa0e0b97e410f185a5842e8740106099`; this proposal does not require
  PR #179 or a contributor's replacement fork.
- The release workflow disables Go workspaces and private-module overrides,
  resolves dependencies through the public Go proxy and checksum database, and
  verifies/tests them using an empty module cache. Review the uploaded
  `release-public-modules` artifact for the exact dependency versions.
- Check the proposed `CFBundleShortVersionString` and `CFBundleVersion` in the
  macOS source plist. All release artifacts use the same resolved tag/commit;
  the workflow rejects a missing tag, an unmerged commit, or a tag that disagrees
  with the app version.
- After the PR is merged and all release gates pass, a maintainer can push the
  `v0.3.0` tag at the approved commit. **Pushing the tag triggers publication.**
  Manual runs also require an existing tag merged into main and build that tag,
  regardless of the branch chosen in the workflow UI. Neither path creates a
  tag automatically.
- Keep the release tag immutable. The workflow checks it again before uploading
  artifacts. Release notes include the resolved source commit.

For an isolated container check, use a clean Git archive, a unique image name,
and a disposable container with no host data or credential mounts. Use
`serve --no-transports` for an unpaired startup check; browser tests use the
synthetic e2e server. Avoid the default Compose project and port when another
OpenMessage instance is installed. Do not connect test containers to a real
message store or reuse a live pairing session.

Release preparation still needs macOS packaging/signing/notarization checks and
authorized live messaging checks before publication. Linux container results
do not satisfy those gates. In particular, verify Google account pairing and
dual-SIM behavior separately; issue #158 is not fully resolved by this release.

## 1. Worktree And Privacy Preflight

- Run `git status --short` and confirm every changed file is intentional.
- Confirm private recovery files, chat exports, screenshots, and local databases are not tracked. In particular, `.tmp-openmessage-recover-filtered.sql` must stay untracked and ignored locally.
- Check new screenshots and demo assets for real names, private chats, phone numbers, maps, and account data.
- Confirm app version/build numbers are updated where the release channel requires them.

## 2. Automated Checks

- Run `go test ./internal/web ./internal/client ./internal/db ./internal/app`.
- Run `npm run test:e2e`.
- Run the macOS app from a clean start and confirm the backend stays alive for at least a basic send/receive session.
- If publishing the website, run the site build and verify the production route before promotion.

## 3. Messaging Dogfood Matrix

- Google Messages: pairing, reconnect, SMS send/receive, RCS attribution, image receive, notifications, and read receipts.
- WhatsApp: pairing, reconnect, text send, image plus caption send, reaction send/receive, group leave, group names, and avatar loading.
- Signal: pairing, history/backfill, group names, image receive, reactions, and stale connection recovery.
- Long threads: opening, sending, receiving, older-message scrollback, media hydration, and bottom pin behavior.
- Multi-route contacts: contact list chips, main-pane platform tabs, send target switching, and unread state.

## 4. Diagnostics

- Open Settings and export/copy diagnostics after a clean launch.
- Confirm `/api/diagnostics` includes `schema_version`, `generated_at_iso`, `backend`, `memory`, `capabilities`, platform counts, and bridge status snapshots.
- Attach diagnostics to issues when investigating crashes, backend exits, dropped connections, stale media, or missing notifications.
- Do not paste message bodies, contact exports, or database dumps into public issues.

## 5. Website And Storefront

- Update website copy when launch-quality platform support changes.
- Regenerate screenshots from sanitized demo data only.
- Verify `openmessage.ai` and any deep links return 200 before announcing.
- Confirm the App Store/TestFlight notes match the shipped platform support.

## 6. Release Gate

- No open P1/P2 review findings or known privacy leaks.
- No failing required CI checks.
- Release artifacts are generated from the intended commit.
- Rollback path is known before promoting a website deployment or public build.
