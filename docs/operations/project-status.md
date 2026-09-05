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
| M2 collection | Implemented; live UI records successful SMB test and published collection. Not rerun against SMB in this checkpoint. |
| M3 media | Implemented; prior loop-device evidence exists, and mount/sync/unmount ran cleanly through the daemon during the loop-image backup run. Physical removal acceptance remains separate. |
| M4 backup | Implemented, acceptance OPEN. First manual backup and an independent restore now succeeded on the real daemon against a loop image; see m4-loop-acceptance.md. USB acceptance still requires real removable media, the changed/deleted-file round trip and interruption evidence per m4-backup.md. |
| M5 scheduler | Not started; held until M4 acceptance and explicit scope review. |
| M6 rotation | Planned. |
| M7 reporting | Planned. |
| M8 retention | Planned. |
| M9 hardening | Planned; bootstrap work is supporting infrastructure, not milestone completion. |

## Current validation

- `gofmt -l`, `go vet ./...`, `go test ./...` and both Linux builds passed under
  Ubuntu WSL at `dd4d8f1` (re-run 2026-09-05, no code changes in this task).
- Frontend: 28 tests in 4 files passed; TypeScript check passed.
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
5. Reconcile the live WSL runtime with the approved installation layout, then
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
- Noticed while validating: the compiled `rotup-mounthelper` binary in the
  repository root is **tracked**, so `go build ./cmd/rotup-mounthelper` dirties
  the working tree. `.gitignore` anchors `/rotupd` and `/rotupd.exe` but not this
  one. The accidental rebuild was reverted rather than committed. Untracking the
  binary and extending `.gitignore` is a separate decision for the coordinator,
  not part of this task.

## Continuation

Read this checkpoint, actual HEAD/status and installation/m4 operations docs.
Check for active writers before dispatch. Finish the first unresolved queue item,
review its diff and tests, commit a coherent checkpoint, then update this file.
Do not start future milestones or infer acceptance from a green unit test suite.
