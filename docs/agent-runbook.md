# Agent & operator runbook

Hard-won operational knowledge for working on a **live** OpenMessage install
(supporting a real user, debugging sends, re-pairing). If you are an automated
agent doing a support task, read this first — most of it cost hours to learn
the hard way.

## Data layout — the #1 gotcha

There are **two separate data directories**, and they are **not the same store**:

| Used by | Path | Notes |
|---|---|---|
| **macOS app** (live) | `~/Library/Application Support/OpenMessage/` | The real `messages.db` + `session.json`. `BackendManager` launches the backend with `OPENMESSAGES_DATA_DIR` set to this. |
| **CLI default** | `~/.local/share/openmessage/` | What `openmessage read/status/pair/serve` use when run with **no** env var. Frequently **stale** relative to the app. |

Consequences:

- To read or modify the **app's live data** from the CLI, set
  `OPENMESSAGES_DATA_DIR="$HOME/Library/Application Support/OpenMessage"`.
  Querying `~/.local/share/openmessage/messages.db` shows a different
  (usually older) message history — do not trust it for "what did the user
  just receive/send."
- `BackendManager.migrateOldDataIfNeeded()` copies `session.json` (+ db files)
  from `~/.local/share/openmessage` → App Support **only when App Support has
  no `session.json`**. So to force the app unpaired you must clear the session
  in **both** dirs (see re-pairing below), or the migration restores it.

## Reading the user's live messages

The running app holds `messages.db` open in WAL mode, so a second SQLite reader
often fails with `unable to open database file (14)`, and `?immutable=1` opens
but misses WAL-only (recent) writes. **Prefer the running app's HTTP API**
(loopback-guarded; `curl` from localhost passes the origin check):

```
GET /api/status
GET /api/conversations?limit=500
GET /api/conversations/<conversation_id>/messages?limit=N
GET /api/search?q=<term>
```

Outgoing message rows carry a `Status`: `OUTGOING_SENDING` → `OUTGOING_SENT`/
`OUTGOING_DELIVERED`, or `OUTGOING_FAILED:<STATUS>` when a send is rejected.

## MCP serving — exactly one process may own live transports

**The failure mode (empirically confirmed 2026-07-20):** `openmessage serve
--mcp-stdio` used to start the **full transport stack** — the Google,
WhatsApp, and Signal supervisors auto-started in every serve mode. MCP hosts
(Claude Code via `~/.mcp.json`, Claude Desktop) spawn one such process **per
session**, each connecting with the **same WhatsApp device credentials and
signal-cli account as the running app**. WhatsApp treats that as a second
device login and kills the session — a fresh pairing at 20:40:41 was dead with
`401: logged out from another device` by 20:41:07, seconds after two Claude
MCP processes spawned. Concurrent signal-cli pollers likewise corrupt/deauth
Signal (the 2026-07-13 WhatsApp logout and Signal's `needs_reauth` death were
this same fratricide). `instance.lock` never protected against this — only
`backup` and `migrate` honor it.

**The fix: MCP client mode.** `serve --mcp-stdio` with no other transport
(the exact shape MCP hosts spawn) is now a **transportless client** of the
running app:

- **Zero transport supervisors, zero dispatchers, zero sync loops, zero
  schedulers, zero telemetry.** The app daemon owns all of those. Regression
  tests: `TestRunServeMCPStdioStartsZeroTransportSupervisors` (cmd) and
  `TestBuiltBinaryMCPStdioClientShapeStartsNoTransports` (binary-level).
- **Reads stay local** (store attach, WAL-safe). At startup the client probes
  the daemon (`/api/status`); if the daemon serves the same data dir and
  reports v2-primary, the client reads the v2 store. With the daemon down it
  falls back to `OPENMESSAGES_V2_*` env exactly like `openmessage read`.
  If `OPENMESSAGES_DATA_DIR` is unset, the client adopts the data dir the
  daemon reports — set it explicitly in the MCP config anyway (see below).
- **The store opens repair-free** (`app.NewClient`): the startup repair
  sweeps (legacy artifacts, contentless recency, tapbacks, empty stubs,
  WhatsApp media placeholders) run only in store-owning entrypoints
  (`app.New` — the daemon and write-capable CLI commands). One client spawns
  per Claude session, so dozens of concurrent sessions must not each burst
  repair writes into the live `messages.db`. The read-only CLI
  (`read`/`status`) opens the legacy store the same way. Regression tests:
  `TestNewClientPerformsNoStoreWrites` (internal/app),
  `TestRunServeMCPClientDoesNotRepairStore` and
  `TestOpenCommandReadSourceLegacyDoesNotRepairStore` (cmd).
- **Sends/reactions route through the daemon** (`/api/v1/outbox` on v2,
  `/api/send`+`/api/react` on legacy), like the CLI has done since PR #140,
  with the same do-not-resend idempotency contract. With the app closed,
  send tools return an actionable "start the OpenMessage app" error — they
  never fall back to opening their own connections.
- Escape hatches: `--transports` forces the old standalone full-stack stdio
  behavior (only for machines where the MCP process is the *only* OpenMessage
  process, ever); `--no-transports` strips transports from a **legacy-mode**
  web/SSE shape (degraded debug instance: local reads work, sends fail with
  "not connected"). On a **v2-primary** install a `--web --no-transports`
  process refuses to start — the v2 read path there needs the dispatcher
  stack — so use the MCP client shape or `openmessage read` for store access
  instead.

**MCP config (`~/.mcp.json`) for a macOS app install:**

```json
"openmessage": {
  "command": "/usr/local/bin/openmessage",
  "args": ["serve", "--mcp-stdio"],
  "env": {
    "OPENMESSAGES_DATA_DIR": "/Users/<user>/Library/Application Support/OpenMessage",
    "OPENMESSAGES_V2_PRIMARY": "1"
  }
}
```

Pin `OPENMESSAGES_DATA_DIR` to the app's dir so reads, the control token, and
daemon-truth detection all line up (two-data-dirs trap above). On a migrated
(v2-primary) install, also set `OPENMESSAGES_V2_PRIMARY=1` — the legacy
`messages.db` froze at cutover, and this keeps MCP reads on the v2 store even
when the app is closed or predates the `auth.data_dir` status field (drop the
line on a non-migrated install). Keep the PATH binary in lockstep with the
installed app — both open the same SQLite stores and a version-skewed binary
can migrate the schema under the older one.

**Never** configure MCP to run `serve --web`, `serve --mcp-sse`, or
`serve ... --transports` alongside the app: those are daemon shapes and will
fight the app for the WhatsApp/Signal sessions exactly as described above.

## Pairing & the "zombie session"

**Symptom:** sends fail with `OUTGOING_FAILED:UNKNOWN`; `/api/status` shows
`google.connected=true`; reconnect and app restarts don't help. The Google
Messages **linked-device session has lapsed** — the phone silently unlinked the
device (common after travel / network changes). The connection flag lies; the
session is dead for sends.

Key facts:

- The native macOS **Platforms** view (`OpenMessageApp.swift`) only offers a
  re-pair control when the session is **absent** (`!google.paired` →
  `ContentView` shows `PairingView`). While it believes it's connected it shows
  "Open inbox / Sync history" with **no re-pair button**. That "Open inbox"
  string is **native Swift, not a stale webview cache** — don't go chasing
  WKWebView caches (a red herring that cost real time). As of PR #42 the **web
  UI** surfaces a "Google Messages isn't sending — Re-pair" banner when
  `google.needs_repair` is set (3 consecutive Google send failures while
  connected). Issue #43 tracks adding the same affordance to the native view.
- **QR pairing is dead** — Google disabled device-pairing QR for many accounts.
  Use **Google Account pairing**.

### Re-pair recipe (the one that works)

1. `osascript -e 'quit app "OpenMessage"'`.
2. Force the native pairing screen by removing `session.json` from **both**
   data dirs (back them up first):
   `~/Library/Application Support/OpenMessage/session.json` **and**
   `~/.local/share/openmessage/session.json` (else migration copies the old one
   back). Other platforms' sessions (`whatsapp-session.db`, `signal-cli/`) are
   independent — leave them.
3. **Clear the stale session FIRST (don't skip).** Running `pair --google` while a dead `session.json` is still in the data dir floods the pairing with `failed to decrypt data event: HMAC mismatch` and yields a new session that 401s on token refresh **immediately** (dead on arrival). Removing both `session.json` files (step 2) before pairing is what produces a healthy session that connects *and* syncs (`/api/status` freshness `behind_days` drops to 0). Some HMAC-mismatch lines are normal noise (events from the phone's own session the pairing client can't read) — the tell for a bad pair is an immediate post-pair 401, not the noise itself.
4. The embedded Google sign-in inside `PairingView` is **blocked by Google**
   ("sign-in not allowed in this app") and dead-ends in Google's troubleshooter.
   Use the **cookie method** instead — extract Google cookies from the user's
   signed-in Chrome and run:
   ```
   OPENMESSAGES_DATA_DIR="$HOME/Library/Application Support/OpenMessage" \
     openmessage pair --google-file <cookiefile>
   ```
   Decrypting Chrome cookies on macOS:
   - key: `security find-generic-password -w -s "Chrome Safe Storage"`
   - derive: PBKDF2-HMAC-SHA1(key, salt=`saltysalt`, iterations=1003, len=16)
   - decrypt each `encrypted_value`: strip `v10` prefix, AES-128-CBC, IV = 16
     spaces, strip PKCS7 padding; recent Chrome prepends a 32-byte domain hash —
     try stripping the first 32 bytes if the result isn't clean UTF-8.
   - source: `~/Library/Application Support/Google/Chrome/Default/Cookies`
     (the signed-in profile; `Local State` maps profiles → accounts). Build a
     `name=value; name=value; …` header from `.google.com` / `messages.google.com`
     cookies and write it to a `0600` file.
   - **Extract cookies immediately before pairing** — pairing with an older
     extract has returned HTTP 401 (the staleness threshold is not
     established; don't rely on any grace window).
4. `pair --google` prints `EMOJI: <emoji>`. The user taps that emoji in Google
   Messages **on the phone** (notification shade, or profile → Device pairing)
   to confirm. The Gaia client init can time out once — just retry.
6. On confirmation the session saves to the app dir; relaunch the app and sends
   work. Wipe the cookie file afterwards.

### Self-healing (as of #74; requirements fixed 2026-07-20) — try this before any manual cookie surgery

The macOS app **refreshes expired Google cookies in-process** and reconnects
on its own. When the reconnect watchdog sees an expired session
(`auth token: HTTP 401` / `SESSION_COOKIE_INVALID`) it reads the user's
signed-in Chrome cookies, rewrites `auth_data.cookies` in `session.json`, and
reconnects — no re-pair, no script. Implemented in `internal/googlecookies`
(darwin-only; keychain → PBKDF2 → AES-128-CBC, handles the Chrome 130+
`SHA256(host)` prefix, snapshots the cookie DB + WAL for freshness).
`refreshGoogleSessionCookies` prefers an explicit
`OPENMESSAGE_COOKIE_REFRESH_SCRIPT` if set, else this native path;
`canRefreshGoogleCookies()` gates whether the watchdog refreshes or parks.

**Cookie requirements (the 2026-07-20 fix):** a Google-account libgm session
authenticates with the five `.google.com` account cookies
(SID/HSID/SSID/APISID/SAPISID) + SAPISIDHASH — proven live against both
`/web/config` and the RegisterRefresh RPC. A `messages.google.com:OSID`
service cookie exists **only** if the user has opened Messages-for-web in that
Chrome profile; it is preferred when present but **never required**. (Before
the fix, refresh hard-required it, so on profiles that never visited
messages.google.com every repair failed with `missing required cookies:
messages.google.com:OSID` and the app looped in `needs_repair` forever — a
re-pair bought minutes, then died again.)

**Expected steady-state — check WHICH BINARY first.** Before diagnosing any
latched `needs_repair`, confirm the running backend is the fixed build:

```bash
RUNBIN=$(ps -o command= -p "$(lsof -nP -iTCP:7007 -sTCP:LISTEN -t | head -1)" | awk '{print $1}')
echo "$RUNBIN"; strings "$RUNBIN" | grep -c 'persisted rotated Google cookies'   # 0 = pre-fix build
```

**This is the single highest-yield check** — it has explained both stale-build
outages so far (2026-07-22, ~11 min latched; 2026-07-25, 06:54→13:21 local,
~6h26m). Many `.app` bundles on
a dev machine share `CFBundleIdentifier com.openmessage.app` (stale worktree
builds, dated backups, the R8 rollback copy), so LaunchServices can resolve
Spotlight/Dock/`open -a OpenMessage`/notification clicks to a **pre-fix**
build, which then latches `needs_repair` exactly like the original bug. Verify
the resolution and always launch by explicit path:

```bash
osascript -e 'tell application "Finder" to get POSIX path of (application file id "com.openmessage.app" as alias)'
open /Applications/OpenMessage.app
```

Stale-listener hazard (observed 2026-07-25): after quitting the GUI and
launching `/Applications/OpenMessage.app`, port 7007 was **still served by the
old bundle's backend**. (`BackendManager` has adopt/stop logic for existing
backends — `BackendManager.swift` "Reusing existing backend pid" / "Stopping
conflicting backend pid" — but with two same-ID bundles the outcome was a stale
listener.) After any relaunch, verify the listener is the binary you intended
and that the old PID exited:

```bash
ps -o pid=,command= -p "$(lsof -nP -iTCP:7007 -sTCP:LISTEN -t | head -1)"
```

**Observed lifetimes vary by regime; there is no known fixed timer.** An
out-of-band probe replaying a *copy* of the session (2026-07-20, n=1) got
`SESSION_COOKIE_INVALID` after ~14 minutes with Chrome active; fresh-pair
sessions died in ~3-4 min (observed 4×, one account, 2026-07-19). On fixed
builds, observed `auth_expired` episodes were 7/21 08:58, 7/22 16:12, 7/23
09:56, and 7/28 21:58 — roughly 0-2/day on this one account — **each
self-healing in ≤~2.5 min** (three cleared within a 60s sample). The two long
latches (7/22 11:21, ~11 min; 7/25 06:54→13:21, ~6h26m) both occurred while
**pre-fix builds** were running and ended when a fixed binary was
deployed/launched. Why lifetimes differ is **not established** — do not treat
any interval as a law. Healthy looks like: mostly connected, with rare
`auth_expired` dips that self-heal in ~1-2.5 min via Chrome cookie import.
Minutes-scale heal churn is **not** normal — check `google.repairs_paced`
(below).
Rotated cookies are also persisted to `session.json` (throttled, ~5 min) so a
restart resumes from fresh values instead of pair-time snapshots. That write is
atomic (temp file + fsync + rename), so a crash mid-save can never truncate the
paired credentials; a failed save retries at a tenth of the interval instead of
waiting a full one.

**The repair pacing counter.** Automatic repairs are paced to at least
`OPENMESSAGE_REPAIR_MIN_INTERVAL` (default 90s). Every delayed repair logs
`Delaying Google credential repair` (with `wait` and `paced_total`) and bumps
`google.repairs_paced` in `/api/status`:

```bash
curl -s http://127.0.0.1:7007/api/status | jq '.google.repairs_paced'
```

The counter records exactly one thing: **how many repair requests were delayed
by the floor**. A climbing count proves requests arrived faster than the
configured interval — it does not identify why (could be fast revocation, a
crash/reconnect loop, or repeated manual reconnects). It cannot clear the
session healthy either: a single failed repair parks the supervisor in Blocked
with the counter still at 0. For context, observed expiries on fixed builds
were ~0-2/day (one account).

To see *why* a repair failed: the refresh error is currently **returned but
never logged** — the native path emits no log line, and the supervisor
discards the error detail (`handleRepairResult` sets Blocked without logging
it) — so the only way to observe it today is to reproduce it directly: run
`scripts/refresh-google-session-cookies-macos.py` manually and read its error
output. (A fix to log the repair failure is chipped.)

An `auth_expired` session with its device link intact revives by cookie
rewrite alone — **do not re-pair** for `needs_repair`; that resets nothing the
refresh can't fix and risks pairing throttles. So the **first** thing to try
when SMS is dead is nothing — wait ~2-3 min for the watchdog (the one observed
live heal took 2m20s; under-waiting funnels you into the re-pair this section
warns against). If it hasn't recovered, **read the actual repair and reconnect
errors before deciding to re-pair** — the causes are broader than "the cookies
are gone": Chrome/keychain/profile access, missing or undecryptable cookies, a
session-file write failure, network or server rejection, or a genuinely revoked
device link. Re-check the running binary (above) and whether Chrome still holds
the five `.google.com` account cookies first; only once those are ruled out
fall back to the manual re-pair recipe above. The app also posts a **health notification**
(once, on the rising edge) when Google flips to `needs_repair` or WhatsApp
logs out, so a dead platform can't sit silent for days.

Prereq: the app must be **non-sandboxed** (it is — `OpenMessage.entitlements`
is hardened-runtime only) so the backend can read Chrome's cookie DB and the
`Chrome Safe Storage` keychain item. First keychain read may prompt once;
Always Allow persists it.

### gmessages fork contract

**Root cause of the repeated deaths (fixed in #73):** the `MaxGhenis/gmessages`
fork was frozen at its 2026-03-02 base and missed upstream's 2026-05-05
[`libgm/longpoll: retry on network error when refreshing auth token`](https://github.com/mautrix/gmessages/commit/0b54a8fe65207f81d353ffe63f4d2549c2eb7976).
Without it, a single transient network blip during a scheduled token refresh
permanently killed the session.

The replacement in `go.mod` pins fork commit
[`0e43542dfa0e`](https://github.com/MaxGhenis/gmessages/commit/0e43542dfa0e0b97e410f185a5842e8740106099).
It is upstream `mautrix/gmessages` base
[`3433cc07d5ea`](https://github.com/mautrix/gmessages/commit/3433cc07d5ea9522309adad3a8c92ed5b08dc11d),
which contains the auth-refresh retry, plus exactly one carried patch:
`Add ListConversationsWithCursor for paginated conversation listing`. That
method is required by OpenMessage's backfill and reconciliation paths.

**Keep the fork rebased on upstream.** The weekly
`gmessages-fork-drift.yml` workflow records the base and patch set and fails as
soon as upstream `main` advances. When rebasing, replay the single carried
patch, verify the auth-refresh retry is still present, and update the fork pin
and recorded SHAs together. The durable architectural fix (move SMS/RCS onto
an Android companion) is issue #75.

### Don't over-reconnect

Connecting/disconnecting the Google web session many times in a short window
(repeated restarts, `reconnect` calls, multiple `pair` runs) gets the account
**throttled** — the long-poll drops and `/api/status` shows
`"Google Messages connection lost; reconnecting…"` in a loop with a perfectly
valid session. The fix is to **stop and let it cool down** (minutes up to ~1h),
not to hammer reconnect. Sends may land in brief connected windows meanwhile.

## WhatsApp linking (QR and phone-number code)

Hard-won facts from the 2026-07-03/04 re-pair ordeal:

- **Passkey-protected accounts can't use phone-number code linking.** If the
  account has a WhatsApp passkey, the server accepts the typed code
  (`companion_finish` returns ok) and then sends `passkey_prologue_request` —
  a WebAuthn challenge only the user's real authenticator can answer. The
  phone shows "Couldn't link device"; the desktop used to idle silently
  (whatsmeow logged "Unhandled notification"). The bridge now surfaces a
  clear "account is protected by a passkey — scan the QR code instead" error
  via `events.PairPasskeyRequest`. QR linking does **not** involve the
  passkey step (the camera scan is the verification).
- **Code + QR windows are short.** Pairing codes expire in ~2 minutes;
  QR refs rotate ~20–60s within a ~3-minute session that ends silently
  (`qr_event: "timeout"`). Generate the code / show the QR only when the
  user's phone is already on the entry/scanner screen.
- **"Couldn't link device. Try again later." on every QR scan = WhatsApp
  refusing, not our bug.** Two causes: (a) zombie companion entries from
  failed attempts eating the 4-device limit — have the user clear stale
  entries in WhatsApp → Linked Devices; (b) a temporary linking throttle
  after repeated failed attempts — stop retrying for a few hours (hammering
  extends it). If a single clean attempt after cleanup + cooldown still
  fails, remove the WhatsApp passkey (Settings → Account → Passkeys), link,
  re-add it.
- **Debugging:** whatsmeow logs used to be discarded (`waLog.Noop`); they now
  flow through the bridge logger (`component=whatsmeow`). Debug-level os_log
  lines are NOT persisted — capture live with
  `log stream --predicate 'subsystem == "com.openmessage.app"' --level debug`
  **before** the attempt; `log show` after the fact only has warn/error.
- **`go.work` gotcha:** the repo root had an untracked `go.work` whose
  `use ../tmp/whatsmeow` silently overrode go.mod's whatsmeow pin for every
  workspace-mode build (including `macos/build.sh`). If a dependency bump
  mysteriously doesn't take, check `go.work`.

## signal-cli

Require **signal-cli ≥ 0.14.5**. 0.14.1 throws
`NullPointerException: …getSender() … content is null` on certain inbound
envelopes, exits non-zero, never ACKs, and re-hits the same poison message every
poll — a crash loop that flaps the Signal `connected` flag every few seconds and
makes the whole UI flicker. `brew upgrade signal-cli` fixes it. (PR #41 also
hardened the UI to ignore redundant status pushes.)

## Deploying a new build to a live install

**`RELEASE=1` is required.** Without it `build.sh` stamps the dev bundle id
(`com.openmessage.app.dev`) on purpose — see [bundle-id
shadowing](#bundle-id-shadowing--only-one-app-may-claim-comopenmessageapp).
Copying a dev-id build into `/Applications` would silently orphan the
`defaults write com.openmessage.app V2Primary` lever and the notification grant.

```
RELEASE=1 DEVELOPER_ID="Developer ID Application: Max Ghenis (8VB5UKQZC6)" ./macos/build.sh
osascript -e 'quit app "OpenMessage"'      # fully quit; `open -a` on a running app won't relaunch it
rm -rf /Applications/OpenMessage.app && cp -R macos/build/OpenMessage.app /Applications/
xattr -cr /Applications/OpenMessage.app
open -a OpenMessage
```

Confirm the deployed bundle kept the release id:

```
/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' /Applications/OpenMessage.app/Contents/Info.plist
# -> com.openmessage.app   (NOT ...app.dev)
```

**Building from a nested `.claude/worktrees/*` checkout needs `GOWORK=off`.**
Go walks up, finds `~/openmessage/go.work`, and resolves the main module to the
parent — `go build .` then fails with "main module … does not contain package
…/.claude/worktrees/<name>". Prefix the build with `GOWORK=off`.

## Bundle-id shadowing — only one .app may claim `com.openmessage.app`

LaunchServices resolves "OpenMessage" (Spotlight, Dock, `open -a OpenMessage`,
notification clicks) to *any* registered bundle declaring
`CFBundleIdentifier = com.openmessage.app`. Every build output, backup, and
Xcode archive used to declare it, so a stale build could be launched instead of
the installed app. This caused two outages; on 2026-07-25 a build predating the
self-heal OSID fix (PR #148) latched Google Messages in `needs_repair` for
~10.5h (06:54 → ~17:20).

Two fixes that **don't** work — verified 2026-07-25:

- `lsregister -u <path>` is **not durable**. Any LaunchServices rescan
  re-registers the bundle; a forced rescan brought all 14 straight back.
- Renaming `Foo.app` → `Foo.app.disabled` does nothing. LaunchServices
  registers on bundle *structure*, not the `.app` extension — it re-registered
  every renamed bundle at its new path.

What works:

- **Build outputs:** `build.sh` stamps `com.openmessage.app.dev` unless
  `RELEASE=1`, so a dev build structurally cannot win resolution.
- **Backups/archives kept on disk:** rename `Contents/Info.plist` →
  `Contents/Info.plist.disabled`. With no `Info.plist` LaunchServices can't read
  a bundle id. Lossless and reversible; see `~/openmessage-ROLLBACK-README.md`
  for the restore recipe.

Audit (should print exactly `/Applications/OpenMessage.app`):

```
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -dump \
 | awk '/^[[:space:]]*path:[[:space:]]/ { p=$0; sub(/^[[:space:]]*path:[[:space:]]*/,"",p); sub(/ \(0x[0-9a-f]*\)$/,"",p) }
        /^[[:space:]]*identifier:[[:space:]]/ { id=$0; sub(/^[[:space:]]*identifier:[[:space:]]*/,"",id);
        if (id=="com.openmessage.app") print p; p="" }' | sort -u
```

Note `mdfind "kMDItemCFBundleIdentifier == 'com.openmessage.app'"` is **not** a
reliable audit — Spotlight keeps stale metadata for neutralized bundles and
skips dot-directories entirely (two hidden rollback bundles were found only by
a forced `lsregister -R -f`). Filter the `lsregister` dump by `identifier:` as
above.

The user's data and pairing **persist** — they live in the data dir, not in the
`.app` bundle. A fresh restart re-establishes the Google long-poll, which can
briefly show "reconnecting" before it settles (see throttling note above).

## Verifying after support work

- `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7007/` → 200
- `/api/status` → `google/whatsapp/signal` connection + `google.needs_repair`
  (and `google.repairs_paced`, which should stay 0/absent)
- A real send shows `OUTGOING_DELIVERED` in
  `/api/conversations/<id>/messages`. Don't re-send a user's real message as a
  "test" (duplicate risk on `UNKNOWN`, which is ambiguous about whether it sent);
  if you must test connectivity, get explicit per-send permission.
- For the ingest cutover checklist, run the [ingest smoke](#ingest-smoke)
  before switching readers.

### Ingest smoke

Read `curl -s http://127.0.0.1:7007/api/status | jq '.v2_ingest'`. A healthy
enabled stack reports `enabled: true`; under `per_account`, `appended` grows as
receive frames arrive, message-bearing frames advance `projected`, and
`quarantined` remains `0`. An idle WhatsApp or Signal account can legitimately
stay at zero until a new inbound/history frame arrives.

The manual receive-only check is:

```sh
LIVE_PLATFORMS=google GOWORK=off go test -tags livetransport \
  -run TestLiveIngestVerification -v -count=1 -timeout 10m \
  ./internal/livetransport/
```

It sends nothing. Add `whatsapp` or `signal` to the comma-separated
`LIVE_PLATFORMS` list only when that platform will receive a real frame within
the test deadline; use `LIVE_GOOGLE_CONV`, `LIVE_WHATSAPP_CONV`, or
`LIVE_SIGNAL_CONV` to override the expected self-thread remote ID.
