# Migration backup (Wave 4, R1)

The first cutover step is an offline, verified backup of the legacy data:

```sh
openmessage backup
```

The command uses the same data-directory resolution as the app:
`OPENMESSAGES_DATA_DIR` when set, otherwise
`~/.local/share/openmessage`. By default it writes
`<data-dir>/migration-backups/<UTC-stamp>/`; use `--to <directory>` to choose
an unused destination. `--json` emits one JSON object for automation.

Stop the OpenMessage backend before running the command. R1 checks the current
daemon endpoint at `http://127.0.0.1:7007/api/status` and holds an exclusive,
non-blocking advisory lock on `<data-dir>/instance.lock` for the full backup.
The lock file's JSON body is diagnostic and can remain after a crash; the OS
lock on the open file descriptor, not that body, is authoritative. R1 creates
this convention for later offline migration/cutover commands to reuse. The
current backend predates the convention and does not yet take the lock, so its
default HTTP health endpoint is the running-backend authority in this release;
a backend deliberately started on another port or without HTTP must be stopped
by the operator.

The command requires free space equal to at least twice the legacy
`messages.db` size. It creates `messages.db` with SQLite `VACUUM INTO`, runs
`PRAGMA quick_check` on that copy, and copies any present `session.json`,
`signal-cli/`, `whatsapp-session.db`, and `openmessage.db` state. All copied
files are recorded with source path, byte size, mode, and SHA-256.

`manifest.json` is atomically written last. Its presence marks a complete,
verified backup; a timestamped directory without it is incomplete and must not
be used as cutover evidence.

Stable exit codes are:

- `0`: backup complete
- `2`: refused because a backend may be running or the instance lock is held
- `3`: path/free-space preflight failed
- `4`: backup or copy failed
- `5`: copied database verification failed

The command never transforms or deletes the source database. Migration is a
separate later step.
