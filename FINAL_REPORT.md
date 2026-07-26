# Final Report: `openmessage v2 reconcile-signal`

## Outcome

Implemented the defensive, local-only Signal history reconciliation command on
`fix/signal-thread-durable-v2-reads`. The repair imports legacy Signal messages
through the v2 historical-import seam, uses the live natural keys, remains
idempotent, reports deferred media explicitly, and never touches transport,
credential, or network state.

## Delivered

- Added `internal/reconcile.Signal` with JSON-serializable report and skip-reason
  accounting, `--since` filtering, dry-run planning, identity and participant
  projection, conversation recency updates, and explicit media deferral.
- Preserved the byte-identical legacy Signal source IDs, including outgoing
  `local:` aliases, and recognized the older bare-timestamp outgoing projection
  only when the canonical legacy key is the exact local alias.
- Required the existing `signal-primary` / `signal_cli` account instead of
  creating or enabling transport state.
- Corrected existing conversation kinds from the authoritative Signal remote
  prefix, avoided group and self peer links, and retained valid messages with
  missing or malformed sender metadata by using a null sender identity.
- Added repair-free read-only open paths for the legacy and v2 stores. The
  command always reads legacy through the read-only path and opens v2 read-only
  during dry-run.
- Added `openmessage v2 reconcile-signal [--from <dir>] [--since YYYY-MM-DD]
  [--dry-run] [--json]` with the macOS app data directory default, instance-lock
  and backend-probe refusal guards, distinct exit codes, one JSON stdout value,
  and human guidance on stderr.
- Added tests for key parity, import and second-run idempotence, preexisting
  twins, fallback keys, skip reasons, deferred media, `--since`, dry-run
  immutability, account and participant safety, backend refusal, read-only
  routing, partial reports, and real sink/decoder/worker outgoing replay.

## Safety

- The maintenance command was never run.
- The real OpenMessage data directory was never opened.
- The application/backend was not started, stopped, or restarted.
- All database execution used temporary test stores.
- No forbidden transport, API, v2-read, or participant-in-flight files were
  modified.
- Nothing was pushed and no pull request was opened.

## Verification

Passed:

```text
GOWORK=off go build ./...
GOWORK=off go test ./internal/db/ ./internal/reconcile/ ./internal/storage/sqlite/ ./internal/ingest/
GOWORK=off go test ./internal/reconcile/ ./cmd/ -run '(?i)signal|reconcile' -count=1
```

The required combined package command was also run. The reconcile, sqlite, and
ingest packages passed, but the `cmd` package process was stopped by the
pre-existing `TestRunSendRealTransportSequence`: this managed sandbox forbids
`httptest.NewServer` from binding `[::1]:0` (`operation not permitted`). To
separate that environment restriction from code failures, the complete `cmd`
suite passed with only the seven pre-existing tests that bind local TCP
listeners skipped. The exact combined command should be rerun once outside the
bind-restricted sandbox.

## Commits

- `6721938` docs: restore signal reconcile progress ledger
- `0543da3` reapply the Signal reconciliation library
- `45525a3` add the reconcile-signal CLI
- `078d41d` wire command dispatch and tests
- `127fd3c` add read-only store open paths
- `c6d07d3` harden Signal reconciliation integrity
- `502402a` enforce reconcile command safety seams
- `b0a4089` cover fallback keys and skip reasons
- `14ba67e` make live replay timing deterministic
- `93c129a` retain malformed-sender messages and correct conversation kinds

## Next

No implementation work remains. Rerun the exact combined test command in an
environment that permits loopback listeners before release.
