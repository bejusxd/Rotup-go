# M5 implementation plan — scheduler and orchestration

Design only. **No M5 code has been written.** This maps the normative
requirements in `docs/ARCHITECTURE.md` (unchanged) onto the code that exists at
`e9af2e6`, and proposes the smallest conformant path to a working slice.

Scope boundaries taken from the milestone text: M5 is schedules,
backup-after-collection, `BACKUP_LATEST` coalescing, retry/deadline, timeouts,
missing-media waiting and the in-memory resource coordinator. Rotation (M6),
reporting (M7) and retention/verification (M8) are out, including their job
kinds.

## 1. What M5 must satisfy

| Requirement | Source |
|---|---|
| Weekday checkboxes, one local time, timezone, enabled/disabled; no cron | §13 *Initial scheduler and UI contract* (REQUIRED_V1) |
| Job form: backup-after-collection, retry interval, retry deadline, timeout; manual-run action per job | same |
| Scheduler persists `next_run_at`, creates durable runs, bounded catch-up, wakes on resource events | AD-9 |
| Runs are durable intents; aggregate attempt count, retry timing, last safe error on the run — no attempt-history table | §13 |
| Backup with no disk stays `WAITING_FOR_RESOURCE`, not repeated failure | §13, §15 |
| Automatic `BACKUP_LATEST` may coalesce; superseded runs stay auditable; **manual runs are never silently coalesced** | §13 |
| Duplicate firings prevented by an idempotency key over job + nominal due time + trigger kind | §13 |
| In-memory `ResourceCoordinator` with named locks, globally sorted acquisition | §17 |
| Retry classes, fixed interval, explicit deadline, absent policy = one attempt | §15 |
| Timeout cancels context, then graceful, then bounded forced termination | §13 |
| Startup sweeps stale active runs to `INTERRUPTED`, recomputes due times, enqueues **at most one** missed automatic run per job | §16 |
| No database transaction spans filesystem, network or subprocess work | §17 |

## 2. Current code, and the delta

### Already present and reusable unchanged

| Concern | Where it lives now |
|---|---|
| Staging mutation barrier (exclusive read for backup) | `internal/staging.Barrier`, used at `backup.go:129` |
| Per-source exclusivity | `keyedLocks` in `internal/collector/service.go` |
| Disk exclusivity + mount lifecycle | `removable.Lease` via `ports.MediaService.Acquire` |
| Run persistence, typed errors, safe messages | `internal/adapters/sqlite`, `internal/domain` |
| Crash sweep of active repository runs | `backup.Service.Recover` (`ActiveRepositoryRuns`) |
| Manual APIs | `POST /api/v1/sources/{id}/collect`, `/repositories/{id}/backup`, `/recover` |
| Inline-vs-goroutine execution seam | `Options.RunInline` in both services |

### Gaps M5 must close

| Gap | Change |
|---|---|
| No `jobs`, `schedules`, `job_sources` tables | migration `005` |
| `runs` lacks `job_id`, `schedule_id`, `parent_run_id`, `nominal_due_at`, `idempotency_key`, `waiting_reason`, `priority`, `attempt_count`, `retry_not_before`, `retry_deadline_at`, `superseded_by_run_id`, and `idx_runs_dispatch` | migration `005` (table rebuild, as `002`/`004` did) |
| `runs.status` CHECK lacks `WAITING_FOR_RESOURCE`, `QUEUED`, `PREFLIGHT`, `FINALIZING`, `RETRY_DELAY`, `TIMED_OUT`, `CANCELED`, `SKIPPED`, `SUPERSEDED` | migration `005`; `domain.RunStatus` constants and `Terminal()`/`Successful()` |
| `runs.trigger_kind` CHECK allows only `MANUAL`, `RECOVERY` | add `SCHEDULED`, `COLLECTION_SUCCESS`, `DEVICE_AVAILABLE`, `RETRY` |
| **`ActiveRepositoryRuns` and `ActiveRuns` hardcode `status IN ('PENDING','RUNNING')`** | must be derived from the enum before any new status is written — see §3.7 |
| No schedule arithmetic | new `internal/schedule` package |
| No scheduler loop, no queue, no retry timer | new `internal/orchestrator` package |
| Three independent lock mechanisms, no global ordering | `internal/coordinator` wrapping the existing three |
| `Recover` only sweeps repository runs | extend to every active status, all kinds |

Nothing above changes M4 completion semantics. The orchestrator creates and
schedules **intents**; deciding whether a run succeeded stays entirely inside
`collector.Service` and `backup.Service`, which is what keeps M5 from opening a
new false-success path.

## 3. Design decisions

### Schedules, timezone and DST

`schedules` stores `days_of_week_mask`, `local_time` and an IANA `timezone`
(§18). `next_run_at` is stored as an instant; all comparisons are instant
comparisons, so DST never affects "is it due".

Computing the next instant from a local wall time needs two explicit rules,
because a local time can be missing or doubled:

- **Spring forward (local time does not exist).** Fire at the first instant that
  does exist on that local date — in practice the moment the offset shifts. The
  alternative, skipping the day, silently loses a backup.
- **Fall back (local time occurs twice).** Fire at the **first** occurrence.

The idempotency key is derived from job + **nominal local due date/time** +
trigger kind, not the instant. This is what makes the fall-back hour safe: both
occurrences map to one key, and the second insert is rejected rather than
running twice.

Timezone defaults to the host zone when a schedule is created. That is a
displayed field, not a new configuration knob.

### Durable intents versus in-memory coordination

Strictly per AD-13 and §17: **SQLite holds run state; it never holds locks or
leases.** The orchestrator persists a run row before doing anything, then asks
the in-memory coordinator for resources. A crash therefore loses all locks and
leaves only intents, which is exactly what the startup sweep is written against.

Lock ordering, globally sorted to avoid deadlock (§17):

```
1. staging-mutator   (shared for collect, exclusive for backup)
2. source:<id>       (ascending id)
3. disk:<id>         (ascending id)
```

The coordinator is a thin wrapper: it does not replace `staging.Barrier`,
`keyedLocks` or the media lease, it acquires them in this order and releases in
reverse. Keeping the existing primitives means M4's proven behaviour is not
re-implemented.

### `BACKUP_LATEST` coalescing

Automatic backup demand coalesces; manual demand never does (§13).

Proposed rule: when an automatic `BACKUP_LATEST` intent is created for a target
that already has a non-terminal automatic `BACKUP_LATEST` intent, the **older**
run is marked `SUPERSEDED` and stays in history with its reason. Superseding the
older one, rather than dropping the new one, keeps the most recent demand — which
matches `BACKUP_LATEST` meaning "back up whatever is current now".

A manual request never supersedes and is never superseded; it queues behind the
running operation through the coordinator.

### Missing media

A backup intent with no eligible disk becomes `WAITING_FOR_RESOURCE`. It is woken
by device-available events and by the existing low-frequency media reconciliation
loop. **It does not increment `attempt_count`** (§15) — waiting is not failing,
and letting it consume the retry budget would turn "the disk is not plugged in
yet" into a permanent failure by morning.

### Retry and failure classification

Reusing `domain.ErrorClass` exactly as M4 already produces it:

| Class | Automatic retry |
|---|---|
| `UNAVAILABLE`, `TIMEOUT`, `EXTERNAL_TOOL` (selected exits), SQLite busy | yes, at the job's fixed interval |
| `AUTHENTICATION`, `CONFIGURATION`, `INTEGRITY`, `INVARIANT`, `UNSAFE_DEVICE` | no — needs a state change or an operator |
| `CAPACITY` | no — pauses until space changes (§15) |
| `CANCELLATION` | no |

`retry_deadline_seconds` absent ⇒ **one attempt**, per §15's "zero/absent retry
policy means one attempt". Retrying without a deadline is not offered.

### Timeouts

`timeout_seconds` bounds the run's context. On expiry: cancel the context, let
the existing adapters' graceful path run, then bounded forced termination through
`internal/platform/process`. The run becomes `TIMED_OUT`. For a backup this
enters the same durability path M4 already proves — an unconfirmed snapshot is
left for explicit recovery, never adopted automatically.

### 3.7 The active-status queries are a correctness hazard

`ActiveRepositoryRuns` (`sqlite/backups.go:514`) and `ActiveRuns`
(`sqlite/runs.go:180`) both select `status IN ('PENDING','RUNNING')` with the two
values written out literally. Those queries are what the startup sweep is built
on, and M4's crash evidence rests on them.

The moment a run can sit in `QUEUED`, `PREFLIGHT`, `WAITING_FOR_RESOURCE`,
`FINALIZING` or `RETRY_DELAY`, a crash in one of those states becomes **invisible
to the sweep**: the run stays non-terminal forever and, for a backup, the
repository is never marked `SUSPECT`. That is precisely the false-success class
M4 spent four tasks closing.

Two consequences, both binding on the implementation:

- Add `domain.ActiveRunStatuses()` derived from `RunStatus.Terminal()` and build
  both `IN` clauses from it, so the enum is the single source of truth and a
  future status cannot be forgotten.
- **Widening the sweep must land in the same commit that first writes a new
  status — never a commit later.** Slice 1 is safe only because it widens the
  CHECK constraint without writing any new value.

### Restart reconciliation

Extends the existing `Recover` rather than replacing it:

1. Every non-terminal run of every kind → `INTERRUPTED` with a recovery event.
   (Today only repository runs are swept.)
2. Repository-specific recovery continues exactly as M4 implements it.
3. Recompute `next_run_at` for every enabled schedule.
4. Enqueue **at most one** missed automatic run per job, regardless of how many
   were missed while the daemon was down (§16.7).
5. Requeue retryable `RETRY_DELAY` runs whose interval elapsed; wake resource
   waiters.

### Compatibility with manual operation

Manual workflows are unchanged. `StartCollection`, `StartBackup` and
`StartRepositorySetup` keep their signatures and their behaviour; the
orchestrator calls the same entry points. Manual runs get `job_id = NULL`, so
every existing API response and page keeps working.

**Automation is opt-in and inert by default.** With no `jobs` and no `schedules`
rows — the state of every existing installation after migration `005` — the
scheduler loop has nothing to evaluate and starts no runs. A disabled schedule or
a disabled job is equally inert. Nothing begins running automatically because a
user upgraded.

## 4. Unresolved product decisions, with proposed defaults

| # | Question | Proposed safe default | Grounding |
|---|---|---|---|
| 1 | Which disk does a scheduled `BACKUP_LATEST` target before rotation exists (M6)? | Any registered, enabled disk that is connected and whose repository is established and healthy. Never block. Among several, prefer the one with the oldest `last_successful_backup_at` (never-backed-up first). | §14 is explicit that an unexpected but enabled registered disk "continues safety validation and **permits backup**" and is recorded against the actual disk. Permission is not a rotation question. Open question 3 asks only which to *prefer*, so the tie-break is provisional and M6 supersedes it |
| 2 | Does a `PARTIAL` collection trigger the follow-on backup? | Yes — schema default `partial_triggers_backup = 1`. Backup eligibility already excludes non-`STABLE` sources, so a partial collection backs up only what genuinely published. | §18 default; `eligibleForBackup` already enforces the safety part |
| 3 | Default `timeout_seconds` | Seed each job from the daemon's **existing** configured values: `ROTUP_COLLECTION_TIMEOUT` (default 2 h) for collection jobs, `ROTUP_BACKUP_TIMEOUT` (default 6 h) for backup jobs. Invent no new number. | `config.DefaultCollectionTimeout`/`DefaultBackupTimeout` already exist and already bound manual runs. Reusing them means an automated run behaves exactly like the manual run an operator has already seen |
| 4 | Default retry interval / deadline | 300 s interval (schema default); **no deadline**, i.e. one attempt, until an operator sets one | §15 requires an explicit deadline for retrying; defaulting to "retry forever" would be the unsafe reading |
| 5 | May a schedule fire while an earlier run of the same job is still running? | No. Skip with a recorded reason rather than queueing a second one. | Prevents unbounded pile-up; §13 coalescing covers the backup case |

None of these needs an answer before slice 1.

### Timeout layering, which decision 3 alone does not settle

There are already two timeout layers, and M5 adds a third:

| Layer | Bounds | Current value |
|---|---|---|
| Engine subprocess | one `restic backup` / `rclone` invocation | restic backup 6 h, check 2 h, inspect 10 min; rclone measure 30 min |
| Daemon config | one collection or backup operation | 2 h / 6 h |
| **Job (new)** | the whole run, including waiting for locks and media | proposed: seeded from the daemon config values |

The job timeout must be **greater than or equal to** the operation it contains,
or it preempts the engine's own graceful termination and turns an orderly
subprocess timeout into an outer cancellation. With the proposed seeding they are
equal for backup (6 h ≈ 6 h), and in practice the engine's timer starts later so
the engine still fires first — but relying on that ordering by accident is
fragile. The implementation should either make the job timeout strictly the outer
bound (engine timeout plus a small margin) or document that the engine layer is
authoritative and the job timeout is a backstop. This is an implementation
choice, not a user decision, but it must be made deliberately in slice 5.

## 5. Implementation slices

Each slice is one commit, independently reviewable, and leaves the tree green.

| # | Slice | Tests |
|---|---|---|
| ~~1~~ | ~~Migration `005` + domain enums.~~ **Done** — see §9. | — |
| ~~2~~ | ~~`internal/schedule`: pure next-run arithmetic + idempotency key.~~ **Done** — see §9. | — |
| 3 | Durable intent creation and startup reconciliation: `domain.ActiveRunStatuses()` driving both active-run queries (§3.7), create runs idempotently from due schedules, sweep all active statuses, recompute due times, at-most-one catch-up. **This is the first slice that writes a new status, so the sweep widening lands here.** | Fake clock; duplicate firing rejected; a daemon down for a week enqueues one run, not seven; a crash in each new non-terminal status is swept; the M4 crash spike still passes unchanged |
| 4 | `internal/coordinator`: named locks over the existing barrier, source locks and disk leases, in sorted order. | Two collections overlap; backup excludes collection; acquisition order is stable; no deadlock under concurrent demand; release on every path |
| 5 | Scheduler loop, worker pools, `WAITING_FOR_RESOURCE`, coalescing, retry and timeout. | Coalescing marks the older run `SUPERSEDED` and keeps it auditable; a manual run is never coalesced; missing media waits without consuming `attempt_count`; retry deadline exhaustion; timeout produces `TIMED_OUT` with no adopted snapshot |
| 6 | Backup-after-collection chaining (`parent_run_id`, `COLLECTION_SUCCESS`) and the jobs/schedules UI + API. | Chain fires on success and, per the flag, on partial; never on failure; manual endpoints byte-identical in behaviour; page renders without a job configured |

## 6. First task — slice 1 only

**Add migration `005` and the domain enum widening. No scheduler, no loop, no
orchestration.**

Completion criteria:

1. `005_m5_scheduling.sql` creates `jobs`, `schedules`, `job_sources` exactly as
   §18 specifies, including CHECK constraints and `idx_schedules_due`.
2. `runs` is rebuilt with `job_id`, `schedule_id`, `parent_run_id`,
   `nominal_due_at`, `idempotency_key` (UNIQUE), `waiting_reason`, `priority`,
   `attempt_count`, `retry_not_before`, `retry_deadline_at`,
   `superseded_by_run_id`, the full status and trigger CHECK sets from §18, and
   the `idx_runs_dispatch` and `idx_runs_job_time` indexes.
3. Every pre-existing run, backup run, read-set and revision-batch row survives
   with identical values, proven by a test that populates an M4-shaped database
   and migrates it.
4. `domain.RunStatus` gains the new statuses with correct `Terminal()` and
   `Successful()` classification; `domain.RunTrigger` gains the new triggers.
5. No production behaviour changes: `go test ./...` passes and the live daemon
   would start against a migrated database with no new activity.
6. The migration is additive-then-rebuild in the same style as `002` and `004`,
   and is applied by the existing migration runner with no new mechanism.

Explicitly **not** in the first task: schedule arithmetic, any loop, any new
run status being *written*, and any UI. Widening the CHECK constraint while
writing no new value is what makes this slice safe to land before the sweep is
widened in slice 3 (§3.7).

## 7. Architectural risks

- **The sweep widening is the sharpest edge.** Extending `Recover` from
  repository runs to every kind touches the code path M4's crash evidence rests
  on. Slice 3 must re-run the crash spike unchanged, not just its own tests.
- **Coordinator regression risk.** Wrapping three working primitives can
  reorder acquisition subtly. Mitigated by wrapping rather than reimplementing,
  and by asserting order explicitly.
- **Automatic backup makes interruption routine.** M4 proved interruption is
  handled when an operator causes it; M5 makes it happen unattended and at
  night. The retry classification is what stops a transient failure from
  becoming a nightly loop, so slice 5's tests matter more than their size
  suggests.
- **Timeout plus adoption.** A timed-out backup can leave an unconfirmed
  snapshot. It must reach the same explicit-recovery path, never automatic
  adoption; slice 5 asserts this directly.
- **The active-status queries (§3.7) are the highest-severity item in this plan.**
  Two literal `IN ('PENDING','RUNNING')` clauses silently define what "crashed
  mid-run" means. Widening the state machine without widening them reopens the
  exact false-success hole M4 closed, and it would not fail any existing test.
- **Disk selection is a preference, not a permission.** An earlier draft of this
  plan proposed waiting when several authorized disks are connected. That
  contradicts §14, which permits backup to an unexpected but enabled registered
  disk and records it against the actual disk. Per-disk repositories (AD-6) mean
  writing to any authorized disk is well-defined, not divergence. Corrected in
  decision 1.

## 9. Progress

**Slice 1 is implemented.** Migration `005_m5_scheduling.sql` adds `jobs`,
`schedules` and `job_sources` and rebuilds `runs` with every §18 column, the full
status and trigger CHECK sets, `idx_runs_dispatch` and `idx_runs_job_time`.
`domain.RunStatus` and `RunTrigger` carry the new values.

Two divergences from §18 were kept deliberately and are recorded in the
migration header rather than made silently:

- `runs.source_id` and `runs.summary` stay. §18 lists neither, but the running
  implementation stores a collection run's source on the run and renders
  `summary`, and `job_sources`/`run_source_results` do not replace either for the
  job-less manual runs every existing database is full of. Retiring them belongs
  to a later slice, once jobs actually own source selection.
- `runs.kind` keeps `DEMO`. §18 omits it; migration 001 created those rows and
  the daemon still records one at startup, so removing the value would make
  existing databases unmigratable.

Two forward-safety decisions were made while the enums were being added, both
because the alternative was to leave a hazard for a later slice to discover:

- `RunStatus.Terminal()` now enumerates the *active* statuses and reports an
  unrecognised value as **active**. A status wrongly called finished is a run the
  sweep never looks at; a status wrongly called active is merely swept to
  `INTERRUPTED`. Only one of those is silent.
- `RunKindRetention.TouchesRepository()` returns true, before M8 produces such a
  run, so that milestone cannot begin writing repository-mutating runs while the
  sweep ignores them.

Still true, and unchanged by this slice: `ActiveRepositoryRuns` and `ActiveRuns`
still list `PENDING` and `RUNNING` literally. That remains correct because this
slice writes no new status. Slice 3 owns the widening (§3.7).

### Slice 2 — done

`internal/schedule` computes occurrences and idempotency keys. Pure arithmetic:
no database, no clock of its own, no runs.

Both daylight-saving edges fall out of one definition — *the earliest instant on
the target local date whose wall clock has reached the configured time* — rather
than from two special cases:

- **Spring forward.** A configured 02:30 on a day that jumps 02:00 → 03:00 fires
  at 03:00, the first valid instant. Skipping the day would silently lose a
  backup, and firing at 03:30 would drift.
- **Fall back.** A configured 02:30 on a day where 02:30 happens twice fires on
  the first pass only. Asked again from between the two passes, or from the
  second one, the schedule moves to the next selected day rather than firing
  again on the repeated hour.

The scan is minute-by-minute over the local day, which is why nothing assumes an
hour: `Australia/Lord_Howe`, which shifts by thirty minutes, is tested and
behaves identically.

The nominal local due identity is carried separately from the instant. Both
passes of a repeated hour share one `Nominal`, so they cannot produce two keys.

Key encoding is `rotup/v1|job=…|due=…|trigger=…` with every value percent-encoded
to an unreserved alphabet, so no value can imitate a delimiter — a job named
`a|due=…|trigger=…|x` provably does not collide with a plain one. The version
prefix is stored because the keys live in a UNIQUE column and a future
derivation must not be able to collide with this one.

**Reported, not invented:** the key is derived from job, nominal due time and
trigger kind exactly as §13 specifies, and therefore **not** from the schedule.
Two schedules on one job landing on the same nominal minute deduplicate to a
single run. That is what the normative text says; it is asserted by a test so
that changing it has to be deliberate. Weekday bit N is `time.Weekday(N)`
(Sunday = bit 0), documented because §18 constrains the mask to 1..127 without
fixing an order.

`time/tzdata` is imported so a host without zoneinfo cannot make schedules
unevaluatable. The standard library uses it only as a fallback, so an up-to-date
system database still wins.

**Review correction.** The first version of `parseLocalTime` used
`strconv.Atoi` on the two-character fields. `Atoi` accepts a leading sign, so
`+1:30`, `01:+2` and `-0:00` passed validation. Because the configured text is
copied verbatim into the nominal identity and from there into the key, that
would have produced two distinct keys for one due time and defeated the
deduplication the key exists for. Both entry points are now strict:
`parseLocalTime` checks ASCII digits directly, and `Nominal.Validate` refuses a
non-canonical identity before it can reach `RunKey` rather than normalising it.

Next: **slice 3**, durable intent creation and startup reconciliation. This is
the slice that first writes a new status, so it owns the §3.7 widening in the
same commit.

## 8. Review note

This plan was drafted, then reviewed against the normative text a second time.
Three errors were found and corrected rather than carried forward: the disk-
selection proposal contradicted §14; the timeout default invented a number while
`config.DefaultCollectionTimeout`/`DefaultBackupTimeout` already existed; and the
column list named `next_attempt_at`, which §18 actually calls `retry_not_before`,
while omitting `waiting_reason`, `priority`, `superseded_by_run_id`, the `SKIPPED`
status and `idx_runs_dispatch`. The active-status query hazard in §3.7 was found
during that second pass and not in the first.
