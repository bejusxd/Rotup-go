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
| M5 scheduler | Slices 1-3 of 6 implemented: migration `005` with the run status/trigger enums, `internal/schedule` (occurrence arithmetic and idempotency keys), and `internal/orchestrator` (durable intent creation and startup reconciliation). No dispatcher, worker pool, resource coordinator or UI yet — nothing executes a run. Automation is inert: with no `jobs` and no `schedules` rows nothing evaluates. Plan and remaining slices in `docs/operations/m5-plan.md`. |
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
11. Wrote and self-reviewed the M5 implementation plan
    (`docs/operations/m5-plan.md`). Design only; no M5 code exists. The review
    corrected three errors in the first draft and surfaced one latent hazard in
    existing code (below).
12. Implemented M5 slice 1: migration `005` (`jobs`, `schedules`, `job_sources`,
    `runs` rebuilt with the full §18 columns, CHECKs and indexes) and the
    `RunStatus`/`RunTrigger` enum widening. Schema only; nothing writes a new
    status. Two §18 divergences kept deliberately and documented in the migration
    header (`runs.source_id`/`summary` retained, `DEMO` kind retained).
13. Implemented M5 slice 2: `internal/schedule`, pure occurrence arithmetic and
    idempotency key derivation. Both daylight-saving edges, non-hour shifts,
    week wrap, boundary and validation cases are tested. No I/O, no loop.
14. Implemented M5 slice 3: `internal/orchestrator` (durable intent creation and
    startup reconciliation), the `jobs`/`schedules` store, and the §3.7 widening
    of both active-run queries from `domain.ActiveRunStatuses()`. Intent creation
    and schedule bookkeeping commit together; a duplicate claim is not an error
    and still advances the schedule; a failed advance rolls the run back. Nothing
    executes a run yet.
15. **Open — next is M5 slice 4**, the resource coordinator: named locks over the
    existing staging barrier, per-source locks and disk leases, acquired in the
    globally sorted order. Do not treat local/fake-media tests as USB acceptance.

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

The current position: **M4 software is complete and reviewed but not accepted;
M5 has begun, three slices of six.** The plan in `docs/operations/m5-plan.md` is
reviewed and accepted. The next task is slice 4 and nothing beyond it.

Nothing in M5 executes a run yet. Slices 1 to 3 build the durable side only:
schema, occurrence arithmetic, and the planner that records that work is due. The
dispatcher, the worker pools and the resource coordinator are still to come, so a
configured schedule currently produces a `PENDING` row and nothing else.

Two product decisions are **pending user input** and must not be implemented as
though they were settled. Neither blocks slice 4:

- Which disk a scheduled `BACKUP_LATEST` should prefer when several authorized
  disks are connected (ARCHITECTURE open question 3). §14 already settles that
  backup is *permitted* to any enabled registered disk; only the preference is
  open.
- Whether a schedule may fire while an earlier run of the same job is still
  running, or should be skipped with a recorded reason.

**The §3.7 hazard is closed.** `ActiveRepositoryRuns` and `ActiveRuns` no longer
spell out `PENDING` and `RUNNING`; both build their `IN` clause from
`domain.ActiveRunStatuses()`, which is derived from `Terminal()`. A test seeds a
run in every active status and asserts both queries return it, so a status added
later cannot be forgotten in a `WHERE` clause. The unchanged M4 crash spike was
re-run over the rewritten queries, because that evidence rested on them.

Slice 3 itself writes only `PENDING`, so the widening was precautionary rather
than load-bearing on the day it landed. The behaviour was proven directly
instead: a crashed backup seeded into each active status is swept to
`INTERRUPTED`, marks the repository `SUSPECT` when restic had started, and leaves
it `HEALTHY` when it had not — so a run that was only ever waiting for a disk
casts no doubt on the media.

One thing slice 2 settled that is worth not re-deciding: the idempotency key is
derived from job, nominal due time and trigger kind, exactly as §13 says, and so
does **not** include the schedule. Two schedules on one job that fall on the same
nominal minute therefore deduplicate to a single run. A test asserts it so the
behaviour cannot drift silently.

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

### Historical — coordinator checkpoint, 2026-09-06 10:03 Europe/Warsaw

Recorded when task10 (slice 2) stopped at the Claude subscription limit before
producing tracked changes, with reset reported for 14:00 Europe/Warsaw. **That
pause is over and slice 2 is implemented** (`7645085`), so this note is history
and is not an instruction to wait or resume. Prompt and result remain in
`.cache/operator/claude-task-10.txt` and `.jsonl`.

### Slice 2 review correction — 2026-09-06

Coordinator review of `7645085` found a real validation defect, fixed here.
`parseLocalTime` used `strconv.Atoi` on the two-character hour and minute
fields, and `Atoi` accepts a leading sign, so `+1:30`, `01:+2` and `-0:00` were
accepted despite the exact HH:MM contract.

This was not cosmetic. The configured text is copied verbatim into the nominal
due identity and from there into the idempotency key, so `-0:00` and `00:00`
would have produced two distinct keys for one due time — and that key is the
only thing preventing a duplicate run. The mutation check makes it concrete: with
the old parser restored, the regression fails with a genuinely produced key
`rotup/v1|job=job-1|due=2026-06-10T-0%3A00|trigger=SCHEDULED`.

Both entry points are now strict. `parseLocalTime` reads ASCII digits one byte at
a time, and `Nominal.Validate` rejects any non-canonical due identity before it
can reach a key — refusing rather than normalising, since normalising would hide
the caller's bug behind a key that no longer matches what was stored.

The two product decisions listed above remain pending; they did not block slice 3.

### Slice 3 — 2026-09-06/07

Implementation and validation spanned a subscription limit (reset 2026-09-07
01:00 Europe/Warsaw). The pause is over; the work was preserved rather than
redone, and the validation below was re-run on resume rather than carried over on
the strength of the earlier session's text report.

What landed: `internal/orchestrator` (the planner: `Reconcile` and `Tick`), the
`jobs`/`schedules`/`ScheduledIntent` store surface, `domain.Job`/`domain.Schedule`,
`domain.ActiveRunStatuses()`, and the §3.7 widening of both active-run queries.

Decisions worth not re-litigating:

- **Catch-up takes the most recent missed occurrence, not the oldest.** The
  nominal due time is shown in history, and a run labelled with a week-old due
  time would misdescribe itself: it collects or backs up whatever is current when
  it executes.
- **`Reconcile` leaves an already overdue schedule alone** rather than recomputing
  it forward, which would silently discard the run that was owed.
- **A duplicate claim is not an error.** The bookkeeping still advances, to the
  values the winning caller computed, because leaving it behind would make the
  schedule re-offer a claimed due time on every tick forever.
- **Schedule bookkeeping only ever moves forward** (review correction, below).
- **`Tick` does not consult whether a previous run of the job is still active.**
  That is one of the pending product decisions, and deciding it in durable state
  before anyone chose would be worse than leaving the intent for a dispatcher to
  arbitrate.

### Slice 3 review correction — stale claims could regress a schedule

Coordinator review of `2d16653` found a concurrency gap, reproduced and fixed
here. `ClaimScheduledRun` updated `last_nominal_run_at` and `next_run_at`
unconditionally, so a claim computed for an earlier occurrence that committed
after a later one dragged the schedule backwards: `next_run_at` ended up pointing
at an occurrence already claimed, and `last_nominal_run_at` named the wrong run.

The existing concurrency test did not cover this and could not have. It raced
several callers for the *same* occurrence, which the unique key settles. Two
callers holding *different* occurrences both insert legitimately, and the only
question is whose bookkeeping survives — so the reproduction is ordered and
deterministic rather than racy.

Bookkeeping now moves forward only. A claim advances when its nominal is at or
after the stored one; a schedule that has vanished still rolls the run back,
while a schedule merely further ahead keeps the run and logs that it was left
alone. `SetScheduleNextRun` gained the same rule, guarding reconciliation against
undoing a claim it did not see.

One subtlety worth keeping: the comparison is on the nominal identity, which is
canonical fixed-width `YYYY-MM-DDTHH:MM` and therefore orders correctly as text.
Stored instants are `RFC3339Nano` with a variable-width fractional part, where
text order is *not* chronological (`…:00.5Z` sorts before `…:00Z`), so
`SetScheduleNextRun` compares parsed `time.Time` values inside the write
transaction instead of adding a `WHERE` clause that would look right and be
wrong.

Validation: focused sqlite and orchestrator tests, then `gofmt`, `go vet ./...`
and `go test ./...` in WSL. The real-restic spikes were not repeated — no
recovery code changed, and task 12's evidence stands. A Windows `go test ./...`
run fails `internal/system`'s Linux path tests and is not a valid signal; WSL is
the authoritative environment.

### Coordinator pause — 2026-09-07 01:53 Europe/Warsaw

Work is paused. User requested a hard stop by 05:00 today; Claude task14
hit its subscription limit, with reset at 06:00, after that deadline.
No automatic resume is scheduled and no executor switch or paid overage is used.
HEAD implementation is 5953025; working tree was clean at this checkpoint.
Slice 3 is implemented but NOT yet accepted by the coordinator.

Resume only on renewed user instruction. First finish task14's bounded review:
check whether delayed distinct-occurrence claims can violate one catch-up and
whether scheduled BACKUP_LATEST intents without backup_runs metadata are safely
swept on startup. Review 5953025 and run proportional independent tests before
accepting slice 3; do not start slice 4 prematurely. Task14 produced no tracked
changes. Retained prompt/result: .cache/operator/claude-task-14.txt and .jsonl.
Disk-preference and overlapping-tick product decisions remain pending.

### Return to work — user decisions, 2026-10-12

Claude is no longer the executor; continuation is through the current coding
agent. HEAD on inspection was `229ee24`, with a clean working tree. No fresh
test run or runtime deployment is claimed by this documentation checkpoint.

The user resolved overlapping schedule occurrences: queue the new occurrence
for delayed execution, preserve its nominal due time and leave future schedule
times unchanged. Do not skip/supersede these occurrences merely because an
earlier run is still active. This does not change BACKUP_LATEST into a request
for a historical staging version.

The user prefers the "newest" available disk; the exact ranking field still
needs clarification. The request not to use a password also needs clarification
against existing repository setup/encryption: stored credentials already avoid
interactive prompts during ordinary backups. No password or escrow guard has
been removed.

Hardware acceptance remains deferred. Target: deploy for full-system testing
on the coming weekend (2026-10-17/18), subject to validation, not production
acceptance. Next engineering task remains the bounded slice 3 review described
above, then the remaining M5 slices. No runtime behaviour changed here.
