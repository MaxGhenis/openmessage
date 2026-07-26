# Reconcile Signal Progress

## State

- In progress on branch `fix/signal-thread-durable-v2-reads`.
- Preserving pre-existing worktree changes, including participant-link helpers and v2 read/ingest work.
- The real OpenMessage data directory and running application are out of scope; all execution will use temporary stores.
- The reconciliation library and its focused Signal/worker contract tests pass.

## Done

- Recorded the task constraints and required verification commands.
- Implemented `internal/reconcile.Signal` with source-ID-first message keys, dry-run planning, preexisting detection, direct peer links, recency repair, media deferral reporting, and skipped-reason counts.
- Added parity, idempotence, preexisting-twin, dry-run, and outgoing worker-alias tests.
- Confirmed focused tests with `GOWORK=off go test ./internal/reconcile/ ./internal/ingest/ -run '(?i)signal|reconcile'`.

## Next

- Implement and test `openmessage v2 reconcile-signal`.
- Run the required build and focused test suite.
- Write the final report output file and update this document with the completed state.
