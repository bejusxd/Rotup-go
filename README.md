# ROTUP

ROTUP is at the M2 milestone: the collection vertical slice. An administrator
can configure an SMB source with a protected credential, run a manual connection
test, run a manual collection through rclone into the staging layout, and
inspect the resulting runs, metrics, revision batches, and staged files in the
local web UI.

USB media, restic, backups, scheduling, rotation, email, and retention are later
milestones and are deliberately not implemented.

## Run locally

```sh
go run ./cmd/rotupd
```

The dashboard listens on `http://127.0.0.1:8080` by default.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ROTUP_HTTP_ADDR` | `127.0.0.1:8080` | Local administration listener |
| `ROTUP_DB_PATH` | `./rotup-dev.db` | SQLite control-state database |
| `ROTUP_STAGING_ROOT` | `./rotup-staging` | Root of the ROTUP-owned staging layout |
| `ROTUP_SECRET_ROOT` | `./rotup-secrets` | Service-owned directory holding secret material |
| `ROTUP_RCLONE_PATH` | discovered on `PATH` | Explicit path to the rclone executable |
| `ROTUP_COLLECTION_TIMEOUT` | `2h` | Time limit for one collection transfer |
| `ROTUP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

The staging root and the secret root must not overlap; the daemon refuses to
start if they do, so secret material can never end up inside a tree that
staging listings or a future backup would reach.

`ROTUP_RCLONE_PATH` should be set explicitly in production. If it is unset the
daemon falls back to the inherited `PATH` as a development convenience and logs
a warning. Without an rclone binary the daemon still starts and the UI still
works; connection tests and collections report that the engine is unavailable.

ROTUP never downloads or installs rclone.

## What the UI offers

- **Dashboard** (`/`) — daemon, database, and transfer-engine status, the latest
  run, and every source's staging state.
- **Sources** (`/sources`) — create and edit sources, set or clear the stored
  password, start a connection test or a collection, and open the staging
  browser. Stored passwords are never displayed again.
- **History** (`/runs`) — every durable run plus the operational event log.
- **Health** (`/healthz`) — JSON health for the daemon and database.

Manual runs execute in the daemon, not in the HTTP request. Starting one
redirects to its run page, which refreshes itself until the run finishes;
closing the browser cannot cancel or corrupt it.

## Reconciliation policies

- `archive_then_sync` (default) — the current tree is made to match the source,
  and every replaced or removed file is moved into that run's revision batch
  first. Nothing is deleted.
- `copy_only` — additions and updates only; nothing is ever removed.
- `mirror` — defined by the architecture but not implemented. It is rejected
  explicitly rather than silently treated as another policy.

Revision batches accumulate on disk in M2. Retention and cleanup arrive with M8,
which is also when a batch may first become eligible for removal.

## Safety rules a collection enforces

A collection never mutates the current tree until connectivity, the credential,
staging path safety, the work area, and the unexpected-empty guard have all
passed. In particular:

- A source that is unreachable, rejects the credential, or reports no files at
  all while staging still holds data does not run. The last good staged copy is
  never destroyed to match a source that has disappeared.
- A failure that ROTUP cannot prove was harmless leaves the source in
  `RECOVERY_REQUIRED`, and the UI says plainly that the staged view cannot be
  trusted. Running a clean collection again republishes a proven state.
- A restart while a reconciliation was in flight marks the source
  `RECOVERY_REQUIRED` rather than assuming the safe case.

## Tests

```sh
go test ./...
go vet ./...
go build ./cmd/rotupd
```

No SMB server and no rclone binary are needed for the normal test run.

### rclone safety spike

The exact rclone semantics that `archive_then_sync` depends on are verified by
an opt-in spike against a real binary:

```sh
ROTUP_RCLONE_SPIKE=1 go test ./internal/adapters/rclone/ -run Spike -v
```

It uses a local directory as the source, because the properties under test
(`copy`, `sync`, `--backup-dir`, `--delete-after`, the JSON statistics, and the
password obfuscation format) are backend-independent. SMB-specific behaviour
still needs a real share on the Linux target.
