# ROTUP operator checkpoint

Updated: 2026-09-05. Initial baseline: `f7c6de8`; first operator fix: `d4ff71b`.

## Operating mandate

The user delegated routine project operation: select bounded tasks, coordinate
coding agents, inspect commits, run terminal/WSL/tests, verify local UI, and keep
this checkpoint current. Escalate passwords/sudo interaction, destructive work
on real media, and genuine product or architecture decisions. Never print secrets.
Use one writer per file area; require evidence before accepting agent reports.
Preserve `docs/ARCHITECTURE.md` and existing media identity/recovery invariants.

## Milestones

| Milestone | Evidence and current status |
|---|---|
| M1 skeleton | Implemented; current Go suite and live health endpoint pass. |
| M2 collection | Implemented; the live daemon collected the real SMB share three more times, including an `archive_then_sync` run that displaced a replaced and a removed file into a recorded revision batch. One reporting defect found and fixed (see below). |
| M3 media | Implemented; prior loop-device evidence exists, and mount/sync/unmount ran cleanly through the daemon during the loop-image backup run. Physical removal acceptance remains separate. |
| M4 backup | Implemented, acceptance OPEN. On the real daemon against a loop image: first manual backup, a full changed/deleted round trip, and explicit repository verification (real `restic check`) returning HEALTHY with all four snapshots retained; every snapshot restored independently. See m4-loop-acceptance.md. USB acceptance still requires real removable media, interrupted-run adoption and physical removal evidence per m4-backup.md. |
| M5 scheduler | Not started; held until M4 acceptance and explicit scope review. |
| M6 rotation | Planned. |
| M7 reporting | Planned. |
| M8 retention | Planned. |
| M9 hardening | Planned; bootstrap work is supporting infrastructure, not milestone completion. |

## Current validation

- `gofmt -l`, `go vet ./...`, `go test ./...` and both Linux builds passed under
  Ubuntu WSL, most recently after the collector displaced-object fix.
- Frontend: 28 tests in 4 files passed; TypeScript check passed.
- The live WSL test daemon now runs the committed HEAD build: version `6670d28`,
  pid 57320, user bejo (uid 1000), bound to `127.0.0.1:8080`, executable
  `.cache/operator/head/rotupd`. It was restarted at an idle, unmounted boundary
  with SIGTERM only and reuses the previous process's `ROTUP_*` configuration
  verbatim (same database, staging, secret, rclone, restic and helper paths);
  the startup log records `applied=0 available=4`, so no migration ran and no
  empty database was created.
- `/api/v1/setup`: all 8 application steps done, `complete: true`. System setup
  still reports broad DrvFS data-directory modes and no installed systemd unit;
  that is a property of this working-tree fixture, not the approved layout.
- No live installation, data migration or real-media mutation performed during
  operator takeover. The stored repository password was used once, locally, only
  as a `--password-file` argument pointing at the existing protected secret file
  for the independent restore; its value was never read, printed or copied.

## Work queue

1. Implemented installer fix: reject unsafe directory ancestors and stop dependent
   runtime/data/config mutations after parent refusal. Isolated regression tests
   cover redirected data, runtime, configuration and `/var/lib` directories.
2. Implemented custom-path correction: selected paths drive provisioning,
   generated environment, mount dependencies and inspection. Unsafe/overlapping
   paths fail before writes; existing shared database parent permissions remain.
3. Implemented account-aware active helper verification: root invokes the probe
   as the selected account; a different unprivileged account cannot claim success.
4. Implemented and recorded the first manual `BACKUP_LATEST` plus an independent
   restic restore against the loop-image fixture. Evidence lives in
   `docs/operations/m4-loop-acceptance.md`.
5. Implemented and recorded the `archive_then_sync` changed/deleted round trip
   against the real SMB share and the loop repository, and fixed the displaced
   object under-count it exposed.
6. Brought the live test daemon to HEAD, confirmed the displaced-object fix on
   the running build (a collection now reports 2 displaced objects against its
   2-item revision batch), and exercised the supported verification action with
   a real `restic check`.
7. Reconcile the live WSL runtime with the approved installation layout, then
   resume M4 acceptance only with positively identified test media. Do not treat
   local/fake-media tests as USB acceptance.

## Follow-through evidence and actual blocker

- Both real-restic suites passed (7 adapter and 9 service scenarios), including
  cancellation, lost completion commit, recovery, identity mismatch and simulated
  disappearance. These use temporary repositories, not physical USB.
- Full Go suite and vet passed after the two additional fixes; Linux daemon and
  helper built in `.cache/operator/`.
- Real setup executed twice in a private user/mount namespace with custom paths:
  first run 14 changes, second run zero changes. Only expected helper invocation
  failure remained (sudo cannot operate normally inside this sandbox).
- Live WSL has SMB and the existing unprivileged daemon. There is still no USB
  disk; `/home/bejo/rotup-test-disks/disk-a.img` is ext4 with UUID
  `063c94b9-bd1e-4496-b70c-85fadc635531`, matching the registered test disk, and
  is now attached as a loop device.
- `sudo -n true` reports interactive authentication required. Do not bypass this
  through WSL root login. User must enter the password in their terminal.
- Prepared `scripts/prepare-wsl-acceptance.sh` for this exact existing WSL fixture.
  It verifies the image UUID/type, installs the built helper/wrapper and tmpfiles
  rule, restores root-owned runtime directories, verifies the existing sudo rule
  as bejo, and attaches the image only if not already associated. It does not
  format, mount, change sudo rules, migrate data or restart the daemon.
- After the user runs that script through sudo: re-enumerate media, verify UUID
  and UI state, continue existing manual backup/restore evidence. A loop-image
  run must remain explicitly separate from physical USB acceptance.
- Loop-image run completed on 2026-09-05: run `215e1052-de95-4cfd-825a-2d03e3e3d425`,
  snapshot `d9f75cf767479ed3c66dde53f24b407855ed4e755c2cf8b90ff9ac3df8eb0a8f`,
  4 files / 259 bytes, restored independently with matching checksums, staging
  unchanged, no restic lock left, disk synced and unmounted. It exercised one
  collection only, no changed/deleted-file round trip and no interruption, so it
  is explicitly not USB acceptance.
- `sudo` was not needed for this run: the daemon's existing narrow helper rule
  covered mount and unmount. No further privilege was requested.
- Round trip completed on 2026-09-05 against the real SMB share
  (`/home/bejo/rotup-smb-source`, identified from Samba's own configuration) with
  a uniquely named test-only fixture that is deliberately left in place.
  Collections `d754887a…` (state version 3) and `0aacb8c0…` (state version 4,
  revision batch `abd67870-0955-4792-be0e-32dac78b2e1b`, 2 items / 102 bytes,
  reconciliation completed). Backups `79940590…` → snapshot `3caa1587…` and
  `66a5aac3…` → snapshot `f41cbc26…`, both `DURABLE_SUCCESS`. Both snapshots were
  restored independently: the archived hashes equal the pre-change fixture, the
  current hashes equal the live share, and the baseline snapshot is unaltered.
  Every pre-existing share file hashes as it did before.
- Defect found and fixed during that run: a collection reported one displaced
  object while its own revision batch held two. `archive_then_sync` moves rather
  than deletes, and rclone counts those moves as renames — verified directly
  against rclone 1.75.0, which reported `deletes: 1, renames: 2` for one replaced
  and one removed file. `internal/collector/service.go` now derives the figure
  from renames under an archiving policy; the collector's fake engine was
  mirroring the wrong counters, which is why nothing caught it. Regression
  `TestArchivingRunReportsEveryDisplacedObject` fails against the old expression.
  No archived, restored or backed-up bytes were affected; the defect was in an
  operator-facing count only.
- On the HEAD build, second subfixture `m4-counterfix-20260905T164543Z`:
  collections `04e82d07…` (state version 5, 0 displaced) and `978acb47…` (state
  version 6, **2 displaced**, revision batch `026e9bc2-0dfe-4cc0-a899-fec66626cac6`,
  2 items / 98 bytes). Backup `64b44e52…` → snapshot `fcade47e…`, 12 files /
  643 bytes, covering both revision batches. The pre-fix under-count is fixed on
  the running daemon, not only in tests.
- Explicit verification run `aaa23ae9-1b7a-452c-a221-b4a4b63677dc`
  (`VERIFY`/`RECOVERY`) succeeded: "repository verified: 4 snapshot(s), 0
  interrupted backup(s) adopted, 0 abandoned". Health moved HEALTHY → SUSPECT
  while in flight → HEALTHY, and all four snapshots were listed independently
  afterwards with no restic lock left. This is the explicit check path only;
  interrupted-run adoption is still unexercised on live media.
- Incidental finding, not corrected here: five tracked text files carry CRLF in
  this working tree, so WSL git reports them modified while Windows git (with
  `core.autocrlf=true`) reports a clean tree. `git diff --ignore-cr-at-eol` is
  empty for all of them. Any WSL tooling that gates on a clean tree needs to
  allow CR-only differences or the line endings need normalising — a decision
  for the coordinator.
- Noticed while validating: the compiled `rotup-mounthelper` binary in the
  repository root is **tracked**, so `go build ./cmd/rotup-mounthelper` dirties
  the working tree. `.gitignore` anchors `/rotupd` and `/rotupd.exe` but not this
  one. The accidental rebuild was reverted rather than committed. Untracking the
  binary and extending `.gitignore` is a separate decision for the coordinator,
  not part of this task.

## Continuation

Claude task 04 stopped at the subscription session limit on 2026-09-05; the CLI
reports reset at 22:40 Europe/Warsaw. The coordination heartbeat is PAUSED.
Preparation reached successful collection `3f53c84d-9e59-49fb-9041-cf5a7a8dea89`
(staging version 7, about 48 MiB test data). The interruption script was written
under `.cache/operator/m4-loop/92-interrupt.sh`, but no interruption backup was
started according to the live run history. Do not count this drill as passed.
At coordinator inspection the daemon health is OK and the UUID-verified loop
disk is unmounted. Resume by reviewing task-04 prompt/log and the prepared script,
then rechecking active runs/media before any action. Last accepted task commit
is `b6fe2aa`; no M5 work started.

Read this checkpoint, actual HEAD/status and installation/m4 operations docs.
Check for active writers before dispatch. Finish the first unresolved queue item,
review its diff and tests, commit a coherent checkpoint, then update this file.
Do not start future milestones or infer acceptance from a green unit test suite.
