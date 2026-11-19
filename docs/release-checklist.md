# OpenMessage Release Checklist

Use this before shipping a TestFlight/App Store build, public website update, or signed macOS release.

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

## 7. Homebrew Cask

Do this after the release workflow has published the release, so the cask pins the bytes users actually download.

```bash
tag=vX.Y.Z
checksums_dir="$(mktemp -d)"
gh release download "$tag" --repo MaxGhenis/openmessage --pattern SHA256SUMS --dir "$checksums_dir"
grep ' OpenMessage.dmg$' "$checksums_dir/SHA256SUMS"
gh release view "$tag" --repo MaxGhenis/openmessage --json assets \
  --jq '.assets[] | select(.name == "OpenMessage.dmg") | .digest | ltrimstr("sha256:")'
```

- The two hashes must match. The cask's `sha256` takes exactly the 64 hex digits; GitHub's digest starts with `sha256:`, which the `ltrimstr` drops. A release cut by hand may have no SHA256SUMS; use the digest then.
- In `Casks/openmessage.rb`, set `version` to the tag without its `v` and `sha256` to that hash, then open a PR. Until it merges, the cask keeps installing whatever version `main` pins.
- In the same PR, keep `depends_on macos:` matching the release's `LSMinimumSystemVersion` in `macos/OpenMessage/Sources/Info.plist` (14.0 is `:sonoma`).
- Don't replace a published DMG with different bytes; cut a new tag instead. Once the cask pins a DMG, a replacement fails every fresh `brew install` with a checksum error until a PR with the new hash reaches `main`, and `gh release upload --clobber` deletes the old asset before the new one lands. If a replacement can't be avoided, re-run all jobs of the tag's original release run (GitHub allows re-runs for 30 days), which rebuilds the DMG and regenerates SHA256SUMS. A manual `gh release upload ... --clobber` of the DMG alone leaves SHA256SUMS stale, so take the hash from the digest then. Either way, bump the cask right after.
- This repo, which serves as its own tap, holds the only official copy of the cask: `MaxGhenis/homebrew-tap` and `homebrew/cask` don't carry it. If either ever does, bump that copy too.
