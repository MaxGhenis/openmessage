# Reconcile Signal Progress

## State

- In progress on branch `fix/signal-thread-durable-v2-reads`.
- Preserving the participant-link and v2-read commits already added to this branch.
- The reconciler work was isolated temporarily on `wip/signal-reconcile` by a concurrent repository process and is being reapplied without rewriting history.
- The real OpenMessage data directory and running application remain untouched; all execution uses temporary stores.

## Done

- Recorded and committed the task constraints at the start.
- Implemented and focused-tested the Signal reconciliation library, including key parity, idempotence, preexisting twins, dry-run behavior, and the worker alias contract.
- Implemented and focused-tested the guarded `v2 reconcile-signal` command on the isolation branch.
- Reapplied the preserved library and command commits to the requested branch.
- Added focused-tested read-only open paths for both stores so dry-runs cannot migrate or otherwise modify database files.

## Next

- Tighten account, identity, participant, and live-replay integrity behavior from review.
- Finish the command's default-path, read-only, error-wrapping, and partial-report behavior.
- Run the required build and full focused test suite.
- Write the final report output file and mark this document complete.
