# Reconcile Signal Progress

## State

- In progress on branch `fix/signal-thread-durable-v2-reads`.
- Preserving pre-existing worktree changes, including participant-link helpers and v2 read/ingest work.
- The real OpenMessage data directory and running application are out of scope; all execution will use temporary stores.

## Done

- Recorded the task constraints and required verification commands.

## Next

- Inspect the existing migration, storage, ingest, and command patterns.
- Implement and test the Signal reconciliation library.
- Implement and test `openmessage v2 reconcile-signal`.
- Run the required build and focused test suite.
- Write the final report output file and update this document with the completed state.
