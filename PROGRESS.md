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
- Tightened the reconciliation preflight and projections: it now requires the existing Signal account, never creates transport state, treats sender names only as display metadata, derives group behavior from the remote ID, avoids self participants, and preserves accurate partial counts.
- Replaced the direct alias-helper assertion with an end-to-end Signal replay through the real sink, decoder, and worker, proving one durable row survives under the local alias.
- Finished the command safety contract: the macOS app directory is the default, legacy is always read-only, dry-run opens v2 read-only, wrapped causes remain inspectable, and partial failures do not emit a contradictory completion message.
- Added coverage for the migration-compatible fallback key and explicit skip-reason accounting.
- Closed the final integrity audit findings: unusable sender metadata now degrades to a null sender instead of dropping valid history, and existing conversation kinds are corrected from the authoritative Signal remote prefix.

## Next

- Run the required build and full focused test suite.
- Write the final report output file and mark this document complete.
