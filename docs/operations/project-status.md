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
| M4 backup | Implemented, acceptance OPEN. See the evidence matrix below for what is proven and on what media. Live loop-image work covers manual backup, the changed/deleted round trip, explicit verification and a graceful interruption. The process-crash sweep-and-adopt path is proven across a real process boundary but on fake media; lock refusal is fault-injected because it is not demonstrable live. Physical USB acceptance is separate and still deferred. |
| M5 scheduler | Not started. The M4 readiness review recommends development may begin; awaiting coordinator decision. Note the milestone gate is M4 *software* readiness, not M4 acceptance, which needs hardware. |
| M6 rotation | Planned. |
| M7 reporting | Planned. |
| M8 retention | Planned. |
| M9 hardening | Planned; bootstrap work is supporting infrastructure, not milestone completion. |

## Current validation

- `gofmt -l`, `go vet ./...`, `go test ./...` and both Linux builds passed under
  Ubuntu WSL, most recently after the collector displaced-object fix.
- Frontend: 28 tests in 4 files passed; TypeScript check passed.
- The live WSL test daemon runs the committed build `6670d28` (code-identical to
  HEAD; every commit since is documentation). Binary
  `.cache/operator/head/rotupd`, sha256 `96cfc9bc…`, user bejo (uid 1000), bound
  to `127.0.0.1:8080`. Each restart reuses the captured `ROTUP_*` configuration
  verbatim and logs `applied=0 available=4`, so no migration runs and no empty
  database is created.
- The loop image is attached again: the coordinator reran the reviewed
  `scripts/prepare-wsl-acceptance.sh`. Reverified rather than assumed — `lsblk`
  and the daemon's own disk view both report UUID
  `063c94b9-bd1e-4496-b70c-85fadc635531`, and the disk reads
  `AVAILABLE_UNMOUNTED`. The loop node itself is never treated as identity.
- Standing operational note: a daemon started from a `wsl.exe` invocation does
  not survive a WSL shutdown, so it has to be relaunched (or started from the
  operator's own terminal) after one. `.cache/operator/m4-loop/94-restart-after-interruption.sh`
  does this with the singleton and checksum guards.
- `/api/v1/setup` reports `complete: true`. System setup still reports broad
  DrvFS data-directory modes and no installed systemd unit; that is a property of
  this working-tree fixture, not the approved layout.
- No live installation, data migration or real-media mutation performed during
  operator takeover. The stored repository password was used once, locally, only
  as a `--password-file` argument pointing at the existing protected secret file
  for the independent restore; its value was never read, printed or copied.

## M4 evidence matrix

What is actually proven, by what, and on what. "Fake media" means the package's
stand-in disk lease: real restic, real SQLite, real orchestration, no mount.

| Behaviour | Evidence | Media |
|---|---|---|
| Manual backup to durable success | live loop-image run + `TestSpikeManualBackupReachesDurableSuccess` | loop image / fake |
| Changed + deleted round trip into a revision batch | live loop-image exercise 2 | loop image |
| Displaced-object count matches its batch | live exercise 3 + `TestArchivingRunReportsEveryDisplacedObject` | loop image / none |
| Explicit verification, real `restic check` | live runs `aaa23ae9…`, `0f0dd127…`, `992cf236…` | loop image |
| Graceful interruption → refused adoption | live exercise 4 (run `72297e78…`) | loop image |
| **Process crash → sweep → adoption** | **`TestSpikeCrashedBackupIsSweptThenAdopted` (SIGKILL across a real process boundary)** | **fake** |
| Locked repository → recovery fails closed, resumes on release | `TestRecoveryRefusesALockedRepositoryAndResumesWhenItIsReleased`, `TestLockedRepositoryIsReportedAsUnavailable` | none (fault injection; not demonstrable live, see exercise 5) |
| Cancellation after restic starts | `TestSpikeServiceCancellationAfterResticStarts` | fake |
| Lost completion commit | `TestSpikeCompletionCommitFailureIsRecoverableWithoutAnotherBackup` | fake |
| Wrong identity / damaged repository / missing roots | spike suite | fake |

Remaining items, with the milestone ARCHITECTURE assigns them to. The readiness
review concluded none of these is an M4 software blocker; see
`docs/operations/m4-readiness.md`.

- Crash-and-adopt is proven across a real process boundary but on fake media.
  Exercising the hardware matrix is **M9**.
- Verification cadence and `--read-data` policy in the product are **M8**. An
  ad-hoc read-only `restic check --read-data` over the live test repository read
  all 12 packs across all 5 snapshots with no errors, so the stored data itself
  is known good.
- Renames, directory removal and re-appearing files in round trips are **M2/M8**
  reconciliation edge cases, not the M4 manual path.
- Escrow procedure and recovery drill (ARCHITECTURE open question 7) block
  **production use** of M4, not M5 development.

Deferred, and not substitutable by any of the above: **physical USB acceptance** —
real removal, power loss, hotplug, and Linux mount permissions on real hardware.
A green spike suite and a green loop-image run are not evidence about hardware.

## Work queue

Items 1-10 are **done**; they are kept as a record of what was implemented and
why, not as outstanding work. The only open item is 11.

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
7. Interrupted a real backup mid-restic with SIGTERM on the loop fixture and
   proved the recovery refused to adopt it: run `72297e78…` ended `INTERRUPTED`
   then `FAILED`/`snapshot_absent`, verification `0f0dd127…` reported 0 adopted
   and 1 abandoned, and all four pre-drill snapshots survived.
8. Established that lock refusal cannot be demonstrated live with restic 0.17.3
   and covered it with tests instead: the adapter now proves both real locked
   signals map to `restic_repository_locked`, and recovery is proven to refuse a
   locked repository without adopting an otherwise-adoptable backup, then resume
   once the lock clears.
9. Closed the process-crash gap with `TestSpikeCrashedBackupIsSweptThenAdopted`:
   a child process is killed with `SIGKILL` at the durability boundary, and a
   fresh service sweeps the abandoned `RUNNING` row, grants nothing on its own,
   then adopts exactly once on explicit recovery with the restored bytes checked
   by the restic binary directly. Fake media, so still not USB acceptance.
10. Reviewed M4 software readiness against the normative ARCHITECTURE text and
    recorded the outcome in `docs/operations/m4-readiness.md`: no software
    blocker found, and much of what had been carried as an "M4 gap" is assigned
    by ARCHITECTURE to M8 (`restic check`, retention, low space) or M9 (crash,
    surprise removal, hardware matrix, packaging, runbooks). A read-only
    `restic check --read-data` over the live test repository read all 12 packs
    across all 5 snapshots with no errors. Recommendation: M4 software is
    sufficient to begin M5 development; M4 must not be recorded as accepted, and
    production use stays blocked on open question 7 (escrow).
11. **Open — awaiting coordinator decision.** If the readiness recommendation is
    accepted, begin M5 with a written design, not code. Otherwise, reconcile the
    live WSL runtime with the approved installation layout. Either way, do not
    treat local/fake-media tests as USB acceptance.

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
- Interruption drill, 2026-09-05. Fixtures `m4-interrupt-20260905T171325Z`
  (48 MiB, collection `3f53c84d…`, state v7) and `m4-interrupt2-20260905T205610Z`
  (64 MiB, collection `c72a454f…`, state v8). The first attempt caught restic in
  flight but its confirmation pause outlasted the whole 48 MiB run, so no signal
  was sent and that backup completed normally as snapshot `e162e467…`; the method
  was corrected rather than retried blindly. The drill itself: backup
  `72297e78-b407-44e2-a46a-3a455526d321`, restic `backup` subprocess pid 60498
  observed with the run's own tag, SIGTERM to daemon pid 57320 at
  `20:56:30.452Z`, about 135 ms after `resticStartedAt`. Recorded durably as
  `INTERRUPTED`/`restic_canceled`, stage `RESTIC_RUNNING`, durability
  `UNPROVEN`, empty candidate and verified snapshot IDs, null
  synced/unmount/durable timestamps; repository `SUSPECT`, last proven snapshot
  untouched. Restart as pid 540 with a checksum-verified identical binary and
  configuration. Verification `0f0dd127-681d-47bc-b723-ad1057365e4c` returned
  "5 snapshot(s), 0 interrupted backup(s) adopted, 1 abandoned"; the interrupted
  run became `FAILED`/`snapshot_absent`. Independently: all four pre-drill
  snapshots present, no snapshot tagged with the interrupted run, no restic lock,
  snapshot `fcade47e…` restored with hashes matching pre-drill values, and every
  pre-existing share file byte-identical.
- Read `HEALTHY` after that recovery as "verified", not "adopted". The proof that
  nothing was adopted is the 0/1 adopted/abandoned count, the unchanged
  `lastSnapshotId`/`lastSuccessfulBackupAt`, and the interrupted run's own
  refusal — not the health field.
- Lock-refusal finding, 2026-09-05, worth not rediscovering: ROTUP's verification
  runs `restic snapshots` and `check` without `--no-lock`, so it takes ordinary
  non-exclusive locks and only refuses when restic does. Non-exclusive locks
  coexist — proven with a real `restic backup` frozen mid-run while holding a
  lock, against which both reads exited 0 — so only an exclusive lock can force
  the refusal, and every command that takes one rewrites or deletes repository
  data. There is therefore no safe live demonstration on the acceptance
  repository, and none is claimed. Covered by
  `TestLockedRepositoryIsReportedAsUnavailable` and
  `TestRecoveryRefusesALockedRepositoryAndResumesWhenItIsReleased`, the latter
  mutation-checked. Live verification `992cf236-211f-4aed-abd2-7f2aebb0ab87`
  meanwhile returned "5 snapshot(s), 0 adopted, 0 abandoned" with no lock
  present, all four pre-drill snapshots intact and hashes matching.
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

2026-09-06 coordinator checkpoint: task08 (M5 plan, docs only) hit the Claude
subscription limit; CLI reports reset 09:00 Europe/Warsaw. Resume scheduled for
09:01, with no polling before then. `docs/operations/m5-plan.md` is an uncommitted
draft, not an approved plan. No M5 implementation started. Resume task08 to finish
the plan and review its proposed product decisions before implementation; do not
treat its disk-selection or timeout proposals as accepted architecture.

All operator tasks through the M4 readiness review are finished. Nothing in the
queue above is waiting to be executed except item 11, which needs a coordinator
decision rather than work.

The current position: **M4 software is complete and reviewed; M4 is not
accepted.** The recommendation, its evidence mapping and its two conditions are
in `docs/operations/m4-readiness.md`. The next task is either an M5 design
document or installation-layout reconciliation, depending on that decision. No
M5 implementation has been started and none should be until the coordinator
responds.

Two negative results the next operator should not spend time rediscovering:

- Lock refusal cannot be demonstrated live with restic 0.17.3, because
  verification takes only non-exclusive locks and those coexist. Do not reach for
  `prune`, `forget --prune`, `repair` or `migrate` to manufacture an exclusive
  lock on a repository holding acceptance evidence. It is covered by tests.
- A daemon started from a `wsl.exe` invocation does not survive a WSL shutdown;
  relaunch it with the guarded restart script after one.

Read this checkpoint, actual HEAD/status and installation/m4 operations docs.
Check for active writers before dispatch. Finish the first unresolved queue item,
review its diff and tests, commit a coherent checkpoint, then update this file.
Do not start future milestones or infer acceptance from a green unit test suite.
