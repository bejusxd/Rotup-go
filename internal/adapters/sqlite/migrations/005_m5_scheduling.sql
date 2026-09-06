-- M5 scheduling and orchestration, schema only.
--
-- Migrations 001 to 004 are immutable and are not touched.
--
-- This migration adds the durable scheduling tables and widens runs so a run can
-- carry the job, schedule, retry and coalescing state the orchestrator will need.
-- It adds no behaviour: nothing in this slice writes a job, a schedule, or any of
-- the new run statuses. A database that migrates through this file starts no
-- automatic work, because with no rows in jobs and schedules there is nothing to
-- evaluate.
--
-- Widening runs needs a table rebuild for the same reason migration 004 did: the
-- status and trigger values are enforced by CHECK constraints, and SQLite cannot
-- alter one. run_source_results, revision_batches, backup_runs,
-- backup_source_states and events all carry foreign keys into runs, so dropping
-- the old table with foreign keys enabled would delete or orphan the history this
-- migration exists to carry forward.
--
-- rotup:table-rebuild
--
--
-- Two deliberate divergences from the architecture's proposed schema in section
-- 18, recorded here rather than made silently:
--
-- 1. runs keeps `source_id` and `summary`. Section 18 lists neither. The running
--    implementation stores a collection run's source on the run and renders
--    `summary` in history and the API, and job_sources plus run_source_results do
--    not replace either for the manual, job-less runs that every existing
--    database is full of. Dropping them here would break M2 and M4 behaviour to
--    match a schema whose scheduling half is not implemented yet. They stay, and
--    a later slice may retire them once jobs actually own source selection.
--
-- 2. runs.kind keeps 'DEMO'. Section 18 omits it; migration 001 created those
--    rows and the daemon still records one at startup, so removing the value
--    would make existing databases unmigratable.
--
-- Everything else follows section 18 exactly, including column names that are
-- easy to get wrong: `retry_not_before`, not next_attempt_at.

-- One configured unit of automatic work. VERIFY and RETENTION are accepted by
-- the constraint because section 18 defines them, but M5 implements neither;
-- they belong to M8.
CREATE TABLE jobs (
    id                         TEXT PRIMARY KEY,
    name                       TEXT NOT NULL UNIQUE,
    kind                       TEXT NOT NULL CHECK (kind IN (
                                          'COLLECTION', 'BACKUP_LATEST',
                                          'VERIFY', 'RETENTION')),
    enabled                    INTEGER NOT NULL DEFAULT 1
                                      CHECK (enabled IN (0, 1)),
    backup_after_collection    INTEGER NOT NULL DEFAULT 0
                                      CHECK (backup_after_collection IN (0, 1)),
    partial_triggers_backup    INTEGER NOT NULL DEFAULT 1
                                      CHECK (partial_triggers_backup IN (0, 1)),
    timeout_seconds            INTEGER NOT NULL CHECK (timeout_seconds > 0),
    retry_interval_seconds     INTEGER NOT NULL DEFAULT 300
                                      CHECK (retry_interval_seconds >= 0),
    retry_deadline_seconds     INTEGER CHECK (retry_deadline_seconds > 0),
    options_json               TEXT NOT NULL DEFAULT '{}',
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL
);

-- Selected weekdays and one local execution time in a named timezone. There is
-- no cron expression and no interval: section 13 fixes this contract for v1.
--
-- days_of_week_mask is a 7-bit set and must select at least one day, so a
-- schedule can never be enabled yet unable to fire.
CREATE TABLE schedules (
    id                         TEXT PRIMARY KEY,
    job_id                     TEXT NOT NULL REFERENCES jobs(id)
                                      ON DELETE CASCADE,
    enabled                    INTEGER NOT NULL DEFAULT 1
                                      CHECK (enabled IN (0, 1)),
    days_of_week_mask          INTEGER NOT NULL
                                      CHECK (days_of_week_mask BETWEEN 1 AND 127),
    local_time                 TEXT NOT NULL,
    timezone                   TEXT NOT NULL,
    last_nominal_run_at        TEXT,
    next_run_at                TEXT,
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL
);

CREATE INDEX idx_schedules_due
    ON schedules(enabled, next_run_at);

CREATE INDEX idx_schedules_job ON schedules(job_id);

-- Which sources a job collects, in a stable order. ON DELETE RESTRICT on the
-- source is deliberate: a source that a job still references must be removed
-- from that job explicitly, not silently dropped out of a schedule.
CREATE TABLE job_sources (
    job_id                     TEXT NOT NULL REFERENCES jobs(id)
                                      ON DELETE CASCADE,
    source_id                  TEXT NOT NULL REFERENCES sources(id)
                                      ON DELETE RESTRICT,
    position                   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, source_id)
);

CREATE INDEX idx_job_sources_source ON job_sources(source_id);

-- The runs rebuild.
--
-- idempotency_key is what stops a schedule firing twice for one nominal due
-- time, including across the repeated local hour when daylight saving ends. It
-- is UNIQUE and nullable: manual runs carry no key, and SQLite treats each NULL
-- as distinct, so any number of manual runs coexist.
--
-- attempt_count, retry_not_before and retry_deadline_at hold the aggregate retry
-- state section 13 asks for. There is no per-attempt history table by design.
CREATE TABLE runs_m5 (
    id                    TEXT PRIMARY KEY,
    job_id                TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    schedule_id           TEXT REFERENCES schedules(id) ON DELETE SET NULL,
    parent_run_id         TEXT REFERENCES runs_m5(id) ON DELETE SET NULL,
    kind                  TEXT NOT NULL CHECK (kind IN (
                              'DEMO', 'CONNECTION_TEST', 'COLLECTION',
                              'DISK_SETUP', 'BACKUP_LATEST', 'VERIFY',
                              'RETENTION')),
    trigger_kind          TEXT NOT NULL DEFAULT 'MANUAL'
                              CHECK (trigger_kind IN (
                                  'SCHEDULED', 'MANUAL', 'COLLECTION_SUCCESS',
                                  'DEVICE_AVAILABLE', 'RECOVERY', 'RETRY')),
    source_id             TEXT REFERENCES sources(id) ON DELETE SET NULL,
    nominal_due_at        TEXT,
    idempotency_key       TEXT UNIQUE,
    status                TEXT NOT NULL CHECK (status IN (
                              'PENDING', 'WAITING_FOR_RESOURCE', 'QUEUED',
                              'PREFLIGHT', 'RUNNING', 'FINALIZING',
                              'RETRY_DELAY', 'SUCCEEDED',
                              'SUCCEEDED_WITH_WARNING', 'PARTIAL',
                              'FAILED', 'TIMED_OUT', 'INTERRUPTED',
                              'CANCELED', 'SKIPPED', 'SUPERSEDED')),
    waiting_reason        TEXT,
    priority              INTEGER NOT NULL DEFAULT 0,
    attempt_count         INTEGER NOT NULL DEFAULT 0,
    retry_not_before      TEXT,
    retry_deadline_at     TEXT,
    summary               TEXT NOT NULL DEFAULT '',
    summary_json          TEXT NOT NULL DEFAULT '{}',
    superseded_by_run_id  TEXT REFERENCES runs_m5(id) ON DELETE SET NULL,
    last_error_class      TEXT,
    last_error_code       TEXT,
    last_safe_error       TEXT,
    requested_at          TEXT NOT NULL,
    started_at            TEXT,
    finished_at           TEXT,
    updated_at            TEXT NOT NULL
);

-- Existing rows carry across unchanged. Every new column takes its default:
-- no job, no schedule, no parent, no idempotency key, zero attempts. A manual
-- run recorded before this migration is indistinguishable afterwards from one
-- recorded after it, which is what keeps the manual workflow untouched.
INSERT INTO runs_m5 (
    id, kind, trigger_kind, source_id, status, summary, summary_json,
    last_error_class, last_error_code, last_safe_error,
    requested_at, started_at, finished_at, updated_at
)
SELECT id, kind, trigger_kind, source_id, status, summary, summary_json,
       last_error_class, last_error_code, last_safe_error,
       requested_at, started_at, finished_at, updated_at
FROM runs;

DROP TABLE runs;

ALTER TABLE runs_m5 RENAME TO runs;

CREATE INDEX idx_runs_latest ON runs(finished_at DESC, id DESC);
CREATE INDEX idx_runs_recent ON runs(requested_at DESC, id DESC);
CREATE INDEX idx_runs_source ON runs(source_id, requested_at DESC);
CREATE INDEX idx_runs_kind ON runs(kind, requested_at DESC);
CREATE INDEX idx_runs_job_time ON runs(job_id, requested_at);
CREATE INDEX idx_runs_dispatch
    ON runs(status, retry_not_before, priority, requested_at);

-- The active-run partial index now covers every non-terminal status, not just
-- the two an M4 run could occupy. No new status is written by this slice, so
-- this changes nothing today; it exists so that widening the startup sweep in a
-- later slice does not quietly lose its index.
CREATE INDEX idx_runs_active ON runs(status) WHERE status IN (
    'PENDING', 'WAITING_FOR_RESOURCE', 'QUEUED', 'PREFLIGHT',
    'RUNNING', 'FINALIZING', 'RETRY_DELAY');

CREATE INDEX idx_runs_parent ON runs(parent_run_id);
