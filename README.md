# ROTUP

ROTUP is at the M3 milestone. An administrator can configure an SMB source with
a protected credential, run a manual connection test, run a manual collection
through rclone into the staging layout, and inspect the resulting runs, metrics,
revision batches and staged files. They can also see the removable media the
system currently presents, register a backup disk by its filesystem UUID, and
mount, verify, flush and unmount it explicitly.

restic, backup repositories, scheduling, rotation, email and retention are later
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
| `ROTUP_LSBLK_PATH` | `/usr/bin/lsblk` | Explicit path to the block-device listing tool (Linux) |
| `ROTUP_MOUNT_HELPER` | unset | Explicit path to the privileged mount helper (Linux) |
| `ROTUP_MEDIA_POLL_INTERVAL` | `15s` | How often removable media is re-enumerated |
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
- **Disks** (`/disks`) — observed removable media, explicit registration, and
  the mount, verify, flush and unmount lifecycle. Unknown media is observe-only.
- **History** (`/runs`) — every durable run plus the operational event log.
- **Health** (`/healthz`) — JSON health for the daemon and database.

Manual runs execute in the daemon, not in the HTTP request. Starting one
redirects to its run page, which refreshes itself until the run finishes;
closing the browser cannot cancel or corrupt it.

## Removable media

Linux is the production runtime for media management. Block devices are
enumerated with `lsblk --json`, the mount table is read from
`/proc/self/mountinfo`, and the three privileged operations run through a
separate `rotup-mounthelper` binary that accepts no path, no mount option and no
device node. Only ext4 is supported in v1. A disk's durable identity is its
filesystem UUID and never a device node, a disk is only marked `READY` after the
mounted filesystem has been read back and proven to be the registered one, and a
connected disk that is not being used is kept unmounted.

On other platforms the daemon builds and runs, and the disks page says plainly
that removable media is managed on Linux only.

See `docs/operations/m3-removable-media.md` for the mechanism rationale,
installation, and the integration spike.

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
- A clean exit status is necessary but not sufficient. If rclone exits zero
  while its own counters report a fatal error, exhausted retries, per-object
  errors, or fewer completed transfers than it queued, the run fails: a tree
  the engine will not vouch for never becomes `STABLE` and never advances the
  staging state version.
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

No SMB server, no rclone binary, no root, no Linux and no removable hardware are
needed for the normal test run.

### Linux media spike

The Linux mount mechanism is verified against a real kernel by an opt-in spike
that creates its own filesystem image and loop device:

```sh
go build -o /tmp/rotup-mounthelper ./cmd/rotup-mounthelper
sudo ROTUP_LINUX_SPIKE=1 ROTUP_MOUNT_HELPER=/tmp/rotup-mounthelper   go test ./internal/platform/linux/ -run Spike -v
```

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
