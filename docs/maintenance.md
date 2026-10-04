# Backup, restore and cleanup

Three commands keep the data directory healthy. All of them require a
stopped daemon — they take the same locks the daemon holds for life, so
a running daemon is refused, never raced.

## Backup

`maestro backup <destination>` snapshots the whole data directory into
a new directory: every project with its canonical and worker
repositories, quarantines, and a consistent snapshot of the database
(taken with `VACUUM INTO`, so journals in flight are never half-copied).
A `manifest.json` records the schema version and a SHA-256 fingerprint
per file. Volatile files — locks, logs, superseded database backups —
stay out.

```console
$ maestro daemon stop
$ maestro backup ~/backups/maestro-2026-10-04
backup written to /home/me/backups/maestro-2026-10-04 (12 files, schema version 1)
```

## Restore

`maestro restore <backup> <new-data-directory>` restores into a new,
empty directory — it never replaces active data. Every file is verified
against the manifest while copying, the database must pass SQLite's
integrity check and match the manifest's schema version, and a failed
verification removes the partial copy. Point `MAESTRO_DATA_HOME` at the
new directory to use it.

## Garbage collection

`maestro gc --dry-run` prints the removable objects with their sizes
and reasons, and removes nothing. `maestro gc --apply` re-plans under
the locks, applies only what both plans agree on, journals the
deletions under `gc/` in the data directory, and skips objects already
gone — an interrupted collection can be re-applied safely. Today's only
candidates are database backups superseded by a newer one; quarantined
worktrees are never collected implicitly.
