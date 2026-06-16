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
3. The embedded Google sign-in inside `PairingView` is **blocked by Google**
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
   - **Cookies rotate roughly every 30 minutes** → extract fresh **immediately**
     before pairing, or `pair --google` returns HTTP 401.
4. `pair --google` prints `EMOJI: <emoji>`. The user taps that emoji in Google
   Messages **on the phone** (notification shade, or profile → Device pairing)
   to confirm. The Gaia client init can time out once — just retry.
5. On confirmation the session saves to the app dir; relaunch the app and sends
   work. Wipe the cookie file afterwards.

### Don't over-reconnect

Connecting/disconnecting the Google web session many times in a short window
(repeated restarts, `reconnect` calls, multiple `pair` runs) gets the account
**throttled** — the long-poll drops and `/api/status` shows
`"Google Messages connection lost; reconnecting…"` in a loop with a perfectly
valid session. The fix is to **stop and let it cool down** (minutes up to ~1h),
not to hammer reconnect. Sends may land in brief connected windows meanwhile.

## signal-cli

Require **signal-cli ≥ 0.14.5**. 0.14.1 throws
`NullPointerException: …getSender() … content is null` on certain inbound
envelopes, exits non-zero, never ACKs, and re-hits the same poison message every
poll — a crash loop that flaps the Signal `connected` flag every few seconds and
makes the whole UI flicker. `brew upgrade signal-cli` fixes it. (PR #41 also
hardened the UI to ignore redundant status pushes.)

## Deploying a new build to a live install

```
DEVELOPER_ID="Developer ID Application: Max Ghenis (8VB5UKQZC6)" ./macos/build.sh
osascript -e 'quit app "OpenMessage"'      # fully quit; `open -a` on a running app won't relaunch it
rm -rf /Applications/OpenMessage.app && cp -R macos/build/OpenMessage.app /Applications/
xattr -cr /Applications/OpenMessage.app
open -a OpenMessage
```

The user's data and pairing **persist** — they live in the data dir, not in the
`.app` bundle. A fresh restart re-establishes the Google long-poll, which can
briefly show "reconnecting" before it settles (see throttling note above).

## Verifying after support work

- `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7007/` → 200
- `/api/status` → `google/whatsapp/signal` connection + `google.needs_repair`
- A real send shows `OUTGOING_DELIVERED` in
  `/api/conversations/<id>/messages`. Don't re-send a user's real message as a
  "test" (duplicate risk on `UNKNOWN`, which is ambiguous about whether it sent);
  if you must test connectivity, get explicit per-send permission.
