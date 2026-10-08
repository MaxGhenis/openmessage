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
GET /api/search?q=<term>            # conversation summaries (one row per thread)
GET /api/search/messages?q=<term>   # raw message rows (MessageID/Body/TimestampMS…)
```

**`/api/search` returns conversation-level results** (`ConversationID`, `Name`,
`Participants`, `preview`) for the web UI's search box — parsing message fields
out of it silently yields nothing. For message hits use `/api/search/messages`,
which returns the same DTO as `/api/conversations/<id>/messages` and accepts
`phone`, `conversation_id`, `since`/`until` (YYYY-MM-DD, local; `until`
inclusive to end of day), and `limit` (default 50, max 500).

Outgoing message rows carry a `Status`: `OUTGOING_SENDING` → `OUTGOING_SENT`/
`OUTGOING_DELIVERED`, or `OUTGOING_FAILED:<STATUS>` when a send is rejected.

### Message JSON field names — the epoch-0 trap

The legacy message DTO marshals **Go field names**, not snake_case, for its core
fields, and snake_case only where a tag says so:

```
MessageID  ConversationID  SenderName  SenderNumber  Body  TimestampMS
Status  IsFromMe  MediaID  MimeType  Reactions  ReplyToID
source_platform  source_id  mentions_me  transcript
```

**`TimestampMS` — capital M, capital S, no underscore.** A reader that looks for
`timestamp_ms` or `TimestampMs` silently gets `0`, and formatting epoch 0
renders every message as **1969-12-31 19:00 ET ("Wed 19:00")**. That is not a
data bug and not a Signal bug: an entire thread showing one identical
placeholder timestamp means the reader used the wrong key. Check the raw JSON
before chasing the bridge:

```bash
curl -s "http://127.0.0.1:7007/api/conversations/<id>/messages?limit=1" | jq '.[0]|keys'
```

The web UI reads `TimestampMS` throughout, so these names are a stable contract
— don't "fix" them by renaming.

## v2 cutover: conversation IDs re-key (issue #155)

On a **v2-primary** install the API serves the v2 store, where a conversation's
primary key is a derived 32-hex hash (`v2keys.DeriveID("conversation", account,
remoteID)`) — **not** the legacy id. The legacy form is preserved as
`conversations.remote_conversation_id` (`signal:+1555…`, `signal-group:<b64>=`,
a WhatsApp JID, a Google thread id).

Consequence, and the shape of the 7/26 incident: an agent stores
`signal:+1555…` on Friday while the app reads legacy-primary, the app restarts
into v2-primary over the weekend, and every read of that stored id returns `[]`
— the thread looks deleted while its rows sit safely under the hash key. The
fix (this PR) makes v2 reads accept **either** key: unknown ids fall back to a
`remote_conversation_id` lookup across accounts, and the returned DTO always
carries the canonical v2 id. Prefer storing the **v2 id** for anything durable;
the alias exists so old references keep working.

Two more cutover artifacts worth knowing:

- **Only the Google decoder emits `ConversationEvent` frames.** Signal and
  WhatsApp conversations are minted from message frames, which carry no kind and
  no roster. Before this PR that meant post-cutover Signal/WhatsApp **groups
  were stored as `direct`**, and 1:1 threads had **no participants and no
  title** — they listed as blank rows with `Participants: "[]"`. The projector
  now infers kind from the remote ID (authoritative for Signal/WhatsApp,
  never guessed for opaque Google thread ids) and links the direct peer; a
  daemon-startup sweep repairs rows that predate the fix.
- **`/api/status` freshness only measures the ACTIVE read source.** Running
  v2-primary, a stalled v2 ingest projection is invisible there — the legacy
  path can keep ingesting for days while readers see nothing new, and every
  per-platform row still reports `behind_days: 0`. This is not hypothetical: the
  Signal projection stalled from Thu 7/23 to Sat 7/25 while legacy kept writing.
  Each platform entry now also carries `legacy_latest_ms`, `projection_lag_ms`,
  and `projection_stalled` (plus a top-level `projection_stalled`), so read
  those before trusting freshness on a migrated install. A transport that stops
  delivering altogether shows in `silence` instead (see "Google Messages silent
  while connected" below):

```bash
curl -s http://127.0.0.1:7007/api/status | jq '.freshness'
```

## Google thread ids are device-local: phone swaps re-key everything

Google Messages conversation and message ids are the phone's own row ids, not
account-level identifiers. A new phone, factory reset, or backup restore starts
a fresh id space: the same person's thread arrives under a new numeric id, and
that id can equal the id an unrelated thread had on the old phone. The
signature of a restore is message ids that *increase as the message gets
older* (history was inserted newest-first) with live messages continuing the
counter from wherever the restore stopped.

What that did on 2026-09-03 (phone replaced ~Aug 19; re-pair connected the new
device at 16:03): every new-space frame whose id collided with an old-space
row was appended into that unrelated thread (a campaign text from a new number
landed in a named contact's thread; shortcode promos landed in "Alex Barnett"),
re-served history duplicated because the remote message id changed too, and
once ConversationEvents arrived they overwrote the colliding row's title and
roster ("United Airlines" became "Miro"). The legacy `messages.db` took the
same writes through its own path.

The projector now treats participant identity as the authority for Google
frames: an inbound message whose sender is not the bound direct thread's peer
moves the id to the sender's thread (minting one if needed); a
ConversationEvent whose roster contradicts the bound row re-binds by roster;
re-delivered content (same direction, sender, millisecond, body) is skipped.
Displaced rows keep their history under `displaced:<id>:<conversation_id>` and
are re-linked by peer when their thread shows up under its new id. Watch
`v2_ingest.per_account.<account>.remote_rebinds` and `content_dupes_skipped`
in `/api/status` — a burst right after a re-pair is the fix working, not a
fault.

**Repairing history after a reset** (daemon must be down — the command takes
the instance lock and refuses while `/api/status` answers; park the watchdog
first):

```bash
OPENMESSAGES_DATA_DIR="$HOME/Library/Application Support/OpenMessage" \
  openmessage repair google-idspace --since <RFC3339 or unix-ms of the re-pair> \
  --reference /path/to/pre-incident/store.sqlite3 --report repair.json          # dry run
# review, then add --apply (copies v2/store.sqlite3* to v2/repair-backups/<stamp>/ first)
```

`--since` is the row *creation* time from which rows are suspect. `--reference`
is any copy of `v2/store.sqlite3` taken before the reset (the app-support dir
snapshots agents take before support work are exactly this); without it the
repair still re-files messages but cannot restore overwritten titles/rosters.
Outgoing-only windows are reported as `ambiguous` and left in place: a
message frame carries no recipient, so nothing proves where an outbound text
belongs until the thread's ConversationEvent re-binds the id.

## Google Messages silent while "connected": the phone stopped relaying

**Symptom:** no new SMS/RCS for hours while `/api/status` shows
`google.connected: true`, `phone_responding: true`, no `needs_repair`, and
`freshness.google.behind_days: 0`. `phone_responding` only turns false when
libgm's ditto pings time out; it says nothing about whether the phone relays
messages. And Google is usually the newest platform, so the relative
`behind_days` rule can never flag it.

**What 2026-10-06 showed.** The last Google frame reached the v2 inbox at
01:11 EDT; nothing more arrived for 38 hours. Apart from brief reconnects and
three app relaunches on 10/6, the long-poll stayed up: pings were answered
(`Phone responding again` after brief timeouts), extra `GET_UPDATES` calls
were answered, and `Listen recovered` followed each network change. The phone
was the part that stopped: after a full restart at 15:29 on 10/7 it pushed
messages sent from the phone on 10/6 at 07:12 and 08:55. Google message ids
are the phone's row ids (previous section). The phone created ids 87881–88004
(124 ids) between the last relayed message (22:40 on 10/5) and 15:18 on 10/7,
about what a day and a half of ordinary traffic produces, and OpenMessage
received 4 of them. Pulling didn't help either: three app relaunches on 10/6
(each starts a shallow backfill) and six `Reconciling recent conversations`
runs on 10/7 put none of the missing rows into `messages.db`. On this install
the request/response calls themselves came back empty: after a relaunch at
15:56 on 10/7, with push working again, the startup backfill logged
`Fetched conversations count=0` and a deep backfill scanned 3 folders and
found 0 conversations, with no errors. That is a separate defect (libgm
accepts a correctly typed but empty response; the cause is not established),
and until it is fixed no pull can confirm or repair delivery. A phone restart
ended the push stall; a re-pair was not tried and is not the first thing to
try. (Google Messages auto-updated on the phone at 02:07 on 10/6, an hour
after the last frame; nothing on hand says whether that caused it. The same
restart also cleared a separate IMS-stack SMS fault on that phone, which on
its own had not stopped RCS relaying from 10/3 to 10/5.)

**Detection.** Each platform in `freshness` now carries a `silence` block that
judges how long the transport has delivered nothing against that platform's own
baseline (`internal/freshness`): the hour-of-day profile of the 14 whole local
days before the silence began. On a v2-primary daemon it reads v2 inbox
receipts (each distinct message or conversation event a bridge hands to
ingest, before decoding); when readers use the legacy store, it reads that
store's incoming-message timestamps (outgoing rows don't count, so a failed
send can't reset it). A silence is a stall when:

- the profile expected activity in at least 6 of the silent hours, judged
  only when the baseline has at least 7 active days and a median of at least
  20 events per active day;
- or 16 hours have passed on a platform with that median over at least 3
  active days (so it also covers the days after an outage, while one burst on
  a pairing day doesn't count);
- or 72 hours have passed, whatever the baseline, so no platform stays fresh
  forever.

A stall sets `stale: true` with `stale_reason: "silent"` ("behind", the
relative rule, takes precedence). The top-level `silence_stalled` is true when
some platform's `stale_reason` is `"silent"`, so a platform dead and "behind"
for weeks doesn't hold it. The macOS app says to check or restart the phone
instead of "needs re-pairing", and posts one notification per silence episode;
a reconnect or an app relaunch during the same silence does not repeat it. If
an activity query fails during a silence, its last verdict is kept, marked
`carried_over`, with its silence length brought up to now and the length rules
applied again. A silence whose baseline can't be read is judged without one on
every refresh (`baseline_unavailable`), so only the 72-hour floor can fire.

```bash
curl -s http://127.0.0.1:7007/api/status | jq '.freshness.google | {stale, stale_reason, silence}'
```

On the 80-day Google history of the install that hit this, the rule flags
every silence longer than 16 hours once a baseline exists (the July and
August outages among them) and no ordinary night or weekend since 9/4, when
that history became continuous (the worst ordinary quiet stretch scored 5.0
of the 6 needed). It would have flagged this stall at about 13:45 on 10/6.
Silences that start in the day are flagged after about 7–8 hours; ones that
start in the evening after about 13–15 hours, with the 16-hour cap as the
bound. A day that an outage covers only in part still counts in a later
baseline and can delay a repeat detection by an hour or two.

**What a stall leaves behind.** On a v2-primary install nothing re-fetches the
messages a stall skipped: the startup backfill and recent reconcile write only
the legacy `messages.db`, and v2 reads see only what the live long-poll
delivers. Find the hole by the phone's row ids, which advance with every
message on the phone:

```bash
sqlite3 -readonly "$HOME/Library/Application Support/OpenMessage/v2/store.sqlite3" \
  "select cast(remote_message_id as integer) id, datetime(occurred_at_ms/1000,'unixepoch','localtime')
   from messages where account_id='google-primary' and remote_message_id glob '[0-9]*'
   order by occurred_at_ms desc limit 40"
```

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

## The watchdog + staleness sentinel (Max's install)

The app can die silently and take every platform's sync with it. On
2026-07-29 it died at about 19:56 (probably jetsam during a day of
memory-pressure kills; no crash report survives). Nothing relaunched it, and
the v2 inbox received no frame from any platform from 19:50 that evening until
23:46 on 7/31. Since then a launchd agent,
`com.maxghenis.openmessage-watchdog`, has run
`~/dotfiles/bin/openmessage-watchdog` every 5 minutes. It relaunches a dead or
hung daemon, and it alerts when the daemon is up but a platform has gone
quiet.

This section follows the script's code on Max's local dotfiles `master` at
`c30e106` (2026-10-08). The script's header comment summarizes the checks but
is incomplete: it leaves out the inbox-read alert, the parse-error path and
the `STATE`/`LOG` overrides, and it lists a top-level `projection_stalled`
check that never fires (the daemon publishes that flag inside `freshness`).
launchd runs the working-tree file, so editing it, or checking out another
branch in `~/dotfiles`, changes live behavior within 5 minutes. Try changes on
a copy ([testing a change](#testing-a-change)).

- Log: `~/Library/Logs/openmessage-watchdog.log`.
- Script stderr, including Python failures:
  `~/Library/Logs/openmessage-watchdog-launchd.log`. Look there when the main
  log shows probes and relaunches but no staleness lines. From 2026-09-15 to
  09-20, `/usr/bin/python3` refused to run (an unaccepted Xcode license), every
  staleness check was off, and the main log said nothing about it.
- State: `~/.local/state/openmessage-watchdog/` holds `consecutive_fails`,
  `last_action_epoch` (the relaunch throttle), the episode counters
  `disc_<platform>` and `repair_google`, and one `alert_<key>` cooldown stamp
  per alert (epoch seconds; deleting one re-arms that alert).
- Loaded? `launchctl list | grep openmessage-watchdog`.

### Relaunching a dead or hung daemon

A run first skips, logging why, if the `watchdog-disabled` flag exists
([parking it](#parking-the-launchd-watchdog)), if any process's command line
contains `openmessage pair`, or if `/Applications/OpenMessage.app` is missing.
Skipped runs neither count nor reset anything. Otherwise it fetches
`http://127.0.0.1:7007/api/status` with a 5 s timeout. The probe fails when the
reply doesn't contain the string `"connected"`: connection refused, no answer
within 5 s, or an error body. Any real status payload passes, even with every
platform down. The probe tests that the daemon answers, not that platforms are
up.

- **No app process** (`pgrep -x OpenMessage`, the Swift wrapper): on the 2nd
  consecutive failure it runs `open -ga OpenMessage`. That launches by name,
  so if the wrong build comes up, run the audit in [bundle-id
  shadowing](#bundle-id-shadowing--only-one-app-may-claim-comopenmessageapp).
- **App process running** (a hung backend, or a backend that died and that the
  app did not restart: it restarts a backend it launched at most 3 times in a
  row, and never one it reused): on the 3rd consecutive failure it asks the app
  to quit, sends `pkill -x OpenMessage` (SIGTERM) if the app is still running
  10 s later, waits 3 s, and relaunches. It never signals the
  `openmessage serve` backend itself, and the relaunched app adopts its own
  bundle's `openmessage serve` if one is still listening on 7007 instead of
  starting another. If a relaunch didn't help, check
  `lsof -nP -iTCP:7007 -sTCP:LISTEN`. This path first ran on 2026-10-08 at
  12:22, after a reused backend died under a running app.

It relaunches at most once per 30 minutes, counted from its own last relaunch
(manual restarts don't count), to stay clear of Google's reconnect throttling
([don't over-reconnect](#dont-over-reconnect)). Every relaunch posts "Daemon
was down - relaunched the app", in both cases; `quitting hung app` in the log
tells them apart. The probe sends no control token. That works only while
`/api/` auth is accept-and-log: if enforcement ships, every probe will fail.

### Staleness alerts

While the daemon answers, the script reads `/api/status` for the "app up,
platform silently dead" class. It only alerts; platform recovery stays with the
in-app supervisors. It alerts on:

- a paired platform (`paired` true, `connected` false) on 3 consecutive runs
  where the daemon answered. That is 10–15 minutes while the Mac is awake, and
  longer across sleep, because launchd skips the runs that fall while it is
  asleep;
- `google.needs_repair` on 3 consecutive answered runs;
- `google.repairs_paced >= 3` (key `repairs_paced`): at least three cookie
  repairs since the daemon started had to wait out the daemon's own minimum
  repair interval (90 s by default), so something is revoking the cookies
  within minutes. The counter never resets while the daemon runs, so the alert
  repeats until the daemon restarts, even after the churn stops;
- `freshness.<platform>.projection_stalled` (key `proj_<platform>`; v2-primary
  daemons only): the platform's newest message in the v2 read store is more
  than 5 minutes older than its newest in the legacy store, or the read store
  has no rows for a platform the legacy store has. It compares newest
  timestamps, not ingest delay, so a gap left in the past keeps it true
  indefinitely;
- a platform's newest received message
  (`freshness.<platform>.latest_received_ms`, or `latest_ms` if it has
  received nothing) more than 48 h older than
  `freshness.newest_ms` (key `behind_<platform>`). `newest_ms` is the newest
  message of any kind on any stored platform, including the platform's own
  sends. The premise is that traffic elsewhere proves the pipe works, but the
  alert always says "while other platforms flow", even when the newer message
  is the platform's own send. A platform with no rows in the read store never
  trips this check; only `proj_<platform>` catches it;
- no message, sent or received, on any platform for more than 24 h (key
  `all_quiet`). With WhatsApp and Signal unlinked this works as a Google
  silence alarm: it fired at 24, 30 and 36 h during the 2026-10-06 stall;
- v2 ingest `quarantined` above 0, summed over accounts (key `quarantine`);
- Signal `receive_recovery.pending_count >= 5` (key `signal_recovery`);
- a paired, connected platform whose silence outlasts its own baseline, by
  the rule in
  [Google Messages silent while "connected"](#google-messages-silent-while-connected-the-phone-stopped-relaying).
  How it is reported depends on the daemon:
  - A daemon with PR #190 publishes `freshness.<platform>.silence`. The app
    posts one `<platform> has gone quiet` notification per silence episode (if
    its desktop notifications are on), and the chief-of-staff watcher
    (`com.maxghenis.cos.openmessage-health-watch`) relays Google's verdict to
    Max on Telegram. The watchdog only logs it, as a `note:` line on every run.
  - On an older daemon with v2 ingest enabled, the watchdog computes Google's
    verdict itself from `v2/store.sqlite3` (constants mirrored from
    `internal/freshness`), marks the alert "(watchdog estimate)" (key
    `silent_google`), and lists it first so that it leads the notification. If
    it can't read the inbox, it raises `silence_check` instead, at normal
    priority. It skips this whenever any platform publishes a `silence` block.

  To see which applies, run
  `curl -s http://127.0.0.1:7007/api/status | jq '.freshness.google.silence'`.
  With the daemon answering, `null` almost always means it predates #190 (a
  #190 daemon also omits the block when Google has no recorded activity or its
  first activity query fails), and then the watchdog's estimate and
  `all_quiet` are the only silence alarms.

### How alerts repeat

Each run posts at most one macOS notification: its first fresh alert, plus
"(+N more - see log)" when there are others. The log has each alert as
`ALERT: …` and the notification as `NOTIFY: …`. A `NOTIFY:` line records the
attempt, not that macOS showed it.

- The disconnect and `needs_repair` alerts count consecutive answered runs and
  notify once, when the count reaches 3, so one notification covers the whole
  episode. The count resets on any answered run without the condition,
  including one where the platform became unpaired or the status didn't parse,
  so `reconnected: <platform>` and `google repair cleared` in the log don't
  prove a recovery. Runs where the daemon is down, or where the watchdog
  skips, leave the count where it was.
- Every other alert has a 6-hour cooldown per key. While the condition holds,
  the first answered run at least 6 hours after the key's last stamp alerts
  again; runs in between log `suppressed (cooldown): <key>`. Every alert in a
  run is stamped, not only the one the notification shows. Alerts that start
  together therefore stay in lockstep, and one that is never first only ever
  shows up as "+N more". Stamps aren't cleared when a condition clears, so a
  recurrence within 6 hours stays silent.
- If `/api/status` contains `"connected"` but isn't valid JSON, the run logs
  `status parse error: …` and checks nothing else.

Consequences worth knowing:

- Only the disconnect and silence checks look at pairing. The trailing,
  projection and Signal-recovery checks don't, so a platform left unlinked
  keeps alerting. On 2026-10-08, with WhatsApp and Signal unlinked, the
  notification every 6 hours read "signal projection stalled (+2 more - see
  log)" (the other two are `behind_signal` and `behind_whatsapp`), and it can
  hide a new alert raised in the same run.
- `quarantined` is an in-memory counter that starts at zero whenever the
  backend starts. Any backend restart (an app or watchdog relaunch, or the app
  restarting `openmessage serve`) therefore stops the alert. The quarantined
  frames stay in the v2 `inbox` table, marked processed like frames that
  projected fine, and the cause is not stored (issue #161). Their inbox ids
  are only in the backend's `Quarantined ingest frame` log lines.
- Quarantine alerts recur: they appeared on 33 days between 2026-08-01 and
  10-08, many of them 6-hourly repeats of one unchanged count. The first
  surfaced three Google conversation snapshots quarantined over
  duplicated self-participants (fixed by deduping in `refreshConversation`,
  PR #160). The recent ones have no diagnosed cause.

### Testing a change

`OPENMESSAGE_WATCHDOG_DRYRUN=1` logs decisions without relaunching or
notifying, but it still writes state: `consecutive_fails`, the episode
counters and the `alert_<key>` stamps (not the relaunch stamp). Run against
the real state dir, it can swallow the next real alert: a stamped key stays
quiet for 6 hours, and an episode counter pushed past 3 never alerts. Point
`OPENMESSAGE_WATCHDOG_STATE` and `OPENMESSAGE_WATCHDOG_LOG` at a scratch
directory and run a copy of the script:

```bash
d=$(mktemp -d)
cp ~/dotfiles/bin/openmessage-watchdog "$d/wd"   # edit "$d/wd" to try a change
OPENMESSAGE_WATCHDOG_DRYRUN=1 OPENMESSAGE_WATCHDOG_STATE="$d" \
  OPENMESSAGE_WATCHDOG_LOG="$d/log" bash "$d/wd"
cat "$d/log"
```

A dry run still obeys the real `watchdog-disabled` flag (its path is fixed)
and probes the live daemon. To exercise the staleness checks against a crafted
payload, set `OPENMESSAGE_WATCHDOG_PORT` to a stub server that serves it at
`/api/status`; the body must contain `"connected"`, or the run takes the
relaunch path. Repeated dry runs of the relaunch path count past the threshold
(`4/3`, `5/3`, …), because only a real relaunch or an answered probe resets
the counter.

### Reading the backend's os_log

**`log` is a zsh builtin** (this cost an hour). In non-interactive zsh
(scripts, `zsh -c`, an agent's shell tool), `log show …` and `log stream …`
hit the builtin and fail with `log:1: too many arguments` instead of reading
the unified log. Interactive zsh on macOS disables the builtin in
`/etc/zshrc`, which is why the same command works in Terminal. Call
`/usr/bin/log` explicitly.

The app pipes the backend's stdout and stderr (zerolog writes to stderr) into
os_log under subsystem `com.openmessage.app`, category `Backend`, every line at
Info level whatever its zerolog level. `log` shows only default-level entries
unless asked (`log show` needs `--info`), and macOS keeps Info entries only in
memory, so a quarantine cause from hours ago is gone. Capture live:

```bash
/usr/bin/log stream --level info \
  --predicate 'subsystem == "com.openmessage.app" AND category == "Backend"'
```

To reproduce a quarantine offline, copy `v2/store.sqlite3` with its `-wal` and
`-shm` files, take the account's frames from the window before the alert, and
feed them through a worker built with that codec's real decoder. Model it on
`newGoogleEchoHarness` in `internal/ingest/google_echo_e2e_test.go`; the
harness in `worker_paths_test.go` uses a fake decoder. The frames that fail
are the quarantined ones. No ready-made replay test exists.

### Parking the launchd watchdog

**Before intentionally keeping the app down for more than a few minutes**
(re-pairing, a slow deploy, long debugging), park the launchd watchdog:

```bash
touch "$HOME/Library/Application Support/OpenMessage/watchdog-disabled"
```

While the flag exists, every run logs `skip: disable flag present` and exits:
no probe, no relaunch, no watchdog alert. (The app's own silence notification
and the chief-of-staff watcher don't read the flag.) Remove it when you're
done, whatever the outcome. Nothing ages it out. A flag forgotten from
2026-08-29 17:44 to 09-03 16:00 (1,278 consecutive skipped runs) silenced the
6-hourly `all_quiet` alerts in the middle of a two-week outage: the v2 inbox
has no frames from any platform between 08-20 18:55 and 09-03 16:03. If alerts
seem to have stopped, `tail` the log first; a parked watchdog says so on every
run. The watchdog also skips while an `openmessage pair` process is running,
but that check matches any command line containing the string, so don't rely
on it for multi-step procedures. The quick deploy recipe below doesn't need
parking if the app is back within a few minutes: with no app process, a
relaunch takes two failed probes 5 minutes apart, and the watchdog skips while
`/Applications/OpenMessage.app` is missing.

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

1. Park the launchd watchdog
   ([parking it](#parking-the-launchd-watchdog)); the flag stops its
   relaunches and its alerts alike. Then
   `osascript -e 'quit app "OpenMessage"'`.
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
5. `pair --google` prints `EMOJI: <emoji>`. The user taps that emoji in Google
   Messages **on the phone** (notification shade, or profile → Device pairing)
   to confirm. The Gaia client init can time out once — just retry.
6. On confirmation the session saves to the app dir; relaunch the app and sends
   work. Wipe the cookie file afterwards. Whatever the outcome, remove the
   launchd watchdog's `watchdog-disabled` flag.

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
[`1dc753f2084e`](https://github.com/MaxGhenis/gmessages/commit/1dc753f2084eedf8db3efb63a2ac3fecb1deecf1).
It is upstream `mautrix/gmessages` base
[`3433cc07d5ea`](https://github.com/mautrix/gmessages/commit/3433cc07d5ea9522309adad3a8c92ed5b08dc11d),
which contains the auth-refresh retry, plus two carried patches, oldest first:

1. `Add ListConversationsWithCursor for paginated conversation listing`. That
   method is required by OpenMessage's backfill and reconciliation paths.
2. `libgm: don't complete data requests with payload-less frames`. Read
   actions (list/get conversations, messages, contacts, thumbnails) whose
   answer arrives without the encrypted payload now wait up to 10 s for the
   real response and then fail with `libgm.ErrNoResponsePayload` instead of
   returning an empty success. See "Pulls that return nothing" below.
   Upstream still has the old behaviour.

**Keep the fork rebased on upstream.** The weekly
`gmessages-fork-drift.yml` workflow records the base and patch set (count and
subjects) and fails as soon as upstream `main` advances. When rebasing, replay
both carried patches, verify the auth-refresh retry is still present, and
update the fork pin, `EXPECTED_PATCHES`, and recorded SHAs together. The pinned
commit must be on the fork's `main`. `TestGMessagesForkRejectsPayloadlessResponses`
runs the pinned fork's own response-acceptance tests, so it fails if a rebase
drops the second patch or keeps its exported names without the rejection. The
durable architectural fix (move SMS/RCS onto an Android companion) is issue #75.

### Pulls that return nothing (phone switched to Google-account pairing)

**Symptom (2026-10-06 to 10-08):** new messages still arrive
(`freshness.google.latest_received_ms` keeps advancing) and
`phone_responding` is `true`, but every pull comes back empty. Startup backfill
logs `Fetched conversations count=0`, deep backfill reports
`conversations_found=0, errors=0`, and `GetOrCreateConversation` /
`GetConversation` return no conversation, so sends fail with "transport
returned no conversation". The account had ~1,040 Google conversations.

**Cause, observed on the wire:** the phone had switched to Google-account
(Gaia) pairing while OpenMessage's session is a QR (Bugle) pairing. The phone
answers each data request (and each liveness ping) with exactly one frame of
message type `GAIA_1` that carries only the field-11 account container (the
Google account address) and no field-8 response payload. No real response
follows. libgm used to hand that frame to the waiting request as a
pre-allocated, empty response with a nil error. The same container arrives as
`GET_UPDATES` frames, which libgm logs as `Got unknown event type` with
`decrypted_data=EhMKE…` and turns into a synthetic `events.AccountChange`
(upstream mautrix-gmessages reports this state as "You switched to Google
account pairing, please log in to continue using SMS/RCS").

**What you see now:**

- libgm logs `Phone answered a pending request without a response payload`
  and, 10 s later, `Phone never sent a response payload; failing the request`.
  They carry the envelope shape and no content, as `frame` on the first and
  `last_frame` on the second (`message_type`, `f5`/`f8`/`f11` presence and
  lengths, `decoded_size`, `unknown_len`, `account_switch`). Answers that arrive after their request
  already finished are logged as `Received response with no pending request`.
- Pulls fail with an error that matches `libgm.ErrNoResponsePayload`; deep
  backfill counts them in `errors`.
- `/api/status` → `google.pull_health` records the last pull (`last_trigger`,
  `last_outcome` = `ok|empty|no_payload|error`, `last_error`) and raises
  `empty_with_local_history: true` when the latest INBOX listing or targeted
  lookup returned no data while the store held at least `threshold` (10)
  Google conversations. `account_switch: true` means the phone sent the
  account-switch notice. The next INBOX listing or targeted lookup that
  returns data clears it; other folders, later pages and transport errors
  leave it alone. The threshold is a heuristic: an account whose INBOX is
  legitimately empty while archived threads remain would also raise it.

```bash
curl -s http://127.0.0.1:7007/api/status | jq '.google.pull_health'
```

**Fix:** not a reconnect, a restart, or a cookie refresh (none change the
phone's pairing mode). Either re-link OpenMessage with Google-account pairing
(cookie method, re-pair recipe above), or switch the phone back to QR/device
pairing in Google Messages → Device pairing. Both are the user's call.

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

### Signal `needs_reauth` — read the fingerprint before believing the park

`needs_reauth: true` is the bridge's **interpretation** of a signal-cli error,
not necessarily server truth. Three live episodes (2026-07-20, 2026-07-24,
2026-08-06) parked a **valid** link for 12–22h because one boot-time
`listAccounts` came up empty and the bridge latched a permanent park; a single
`POST /api/signal/connect` reconnected in ~5s each time.

**Why an empty `listAccounts` is ambiguous** (mechanism verified live
2026-08-23 on signal-cli 0.14.5 with `--verbose --verbose`): `listAccounts` is
not a pure local read. signal-cli loads every account in `accounts.json`, and
the load runs its own account check (`AccountHelper.checkAccountState`) — a
**server round-trip** while the stored account is marked registered. Any
failure there makes signal-cli print one `WARN … Ignoring <number>: …` /
`Failed to load <number>: …` line and report **zero accounts with exit 0**:

- **Transient check failure** (network not up at login-time autostart, server
  hiccup): the account file keeps `"registered": true`, so the next probe can
  succeed — this is the false-park shape the episodes above match.
- **Genuine deregistration**: the server answers the check with
  `DeviceDeregisteredException: device was deregistered` → `[403]
  Authorization failed! (AccountCheckException)`, and signal-cli **persists
  `"registered": false`** into `data/<account>`. From then on every load
  throws `NotRegisteredException` *before* any network call — the park is
  real and only a re-link fixes it (observed live 2026-08-23; flipping the
  flag back to true just reproduced the 403 and re-persisted false).

The bridge classifies with corroboration instead of trusting one probe:

- The receive-start probe **retries in-generation** (3 attempts, paced),
  then checks `data/accounts.json`. Probe empty but accounts.json still lists
  the account → **transient** exit (`signal_account_probe_empty`), retried on
  supervisor backoff — no park, no `needs_reauth`.
- Only 3 **consecutive generations** of that disagreement park, under
  `signal_account_unreadable` — and that park **self-retests every 15 min**
  (one `RetryBlocked`; log line "Signal reauth park retest"), so a lingering
  transient park heals without manual intervention. While signal-cli still
  considers the account registered each retest costs one light server check;
  after a persisted `"registered": false` the retests fail locally with zero
  network traffic, so a genuinely deregistered account is never hammered. The
  streak count in `last_error` grows by one per retest — a large number means
  the park has been standing for hours, not that anything is thrashing.
- Server-confirmed evidence still parks fast and stays parked:
  a receive-loop "not registered" / "authorization failed" needs 2
  consecutive confirmations (seconds), then parks under
  `signal_account_invalid` with **no** automatic retest. Probe empty with
  accounts.json **also** empty parks immediately (`signal_account_invalid`).

Debugging a parked Signal — check, in order:

1. `/api/status` fingerprint/error. `signal_account_invalid` from `receive` →
   server-confirmed unlink; re-pair is real.
2. Contention: `pgrep -fl signal-cli`, `lsof` on the config dir (see the MCP
   fratricide section above).
3. The persisted server verdict:
   `jq .registered "$DATADIR/signal-cli/data/<account-file>"` — **`false`
   means the server rejected the device** (genuine; only a re-link fixes it);
   `true` with an `signal_account_unreadable` park means transient check
   failures — wait for the retest or fire `POST /api/signal/connect`.
4. To see the verdict live (only while the app's Signal supervisor is parked
   and no signal-cli process is running):
   `signal-cli --verbose --verbose --config "$DATADIR/signal-cli" listAccounts`
   — genuine shows `DeviceDeregisteredException` + `[403] Authorization
   failed!`; transient shows an IO-flavored "Error while checking account"
   with `registered` still true afterwards.

**Never unpair to "fix" a park**: unpair `os.RemoveAll`s the signal-cli dir
including CDN-expired media — permanent loss. Before a *genuine* re-link, back
up the whole `signal-cli/` dir first and restore the media subdirs
(`attachments`, `avatars`, `stickers`, `outgoing-attachments`) — not `data/` —
after the new link (recipe proven 2026-07-20; backups live under
`<datadir>/app-backups/`).

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

- **Build outputs:** unless `RELEASE=1`, `build.sh` stamps
  `com.openmessage.app.dev` **and** names the bundle `OpenMessage (dev)`
  (`CFBundleName` + `CFBundleDisplayName`). Both matter: id-based launches
  (notification clicks, `open -b`) resolve by `CFBundleIdentifier`, but
  name-based launches (`open -a OpenMessage`, Spotlight) resolve by the
  registered *name*, which comes from the plist — **not** the `.app`
  filename (a bundle renamed on disk still registered as "OpenMessage" from
  its plist). With both stamped, neither launch path can land on a dev build.
- **Backups/archives kept on disk:** rename `Contents/Info.plist` →
  `Contents/Info.plist.disabled`. With no `Info.plist` LaunchServices can't read
  a bundle id. Lossless and reversible; see `~/openmessage-ROLLBACK-README.md`
  for the restore recipe.

**Sharp edge — don't run a dev GUI on the live machine.** Because the dev id
differs, macOS no longer dedupes it against the installed app: launching a dev
build alongside it starts a real second GUI. That GUI *adopts* the daemon
already listening on port 7007 (`BackendManager.reuseExistingBackendIfNeeded`
— transport-safe, it won't spawn a competing stack), but its stop path
SIGTERMs the adopted PID — **quitting the dev GUI kills the live backend out
from under the installed app.** If that happens, relaunch the installed app.
Tracked with the other dev-id-scoped traps in issue #165.

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
