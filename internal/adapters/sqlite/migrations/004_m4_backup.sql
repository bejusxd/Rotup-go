-- M4 backup vertical slice.
--
-- Migrations 001, 002 and 003 are immutable and are not touched.
--
-- This migration does two things. It widens the runs table so a run can be a
-- repository setup, a BACKUP_LATEST, or a verification, and it adds the
-- repository and backup tables.
--
-- Widening runs needs a table rebuild, because the kind and status values are
-- enforced by CHECK constraints and SQLite cannot alter one. The rebuild uses
-- the create-copy-verify-swap pattern the architecture's migration strategy
-- calls for.
--
-- revision_batches, run_source_results and events already carry foreign keys
-- into runs, so this cannot be done inside an ordinary migration transaction.
-- With foreign keys enabled, dropping the old table performs an implicit delete
-- of every row in it first, and each child's referential action then fires:
-- run_source_results would cascade away, events would have their run_id set to
-- NULL, and revision_batches would refuse outright. A migration that destroys
-- the collection history it exists to carry forward is worse than no migration.
--
-- The directive below therefore asks the migration runner for SQLite's
-- documented rebuild procedure: foreign keys off, rebuild, foreign_key_check,
-- commit, foreign keys on. The check inside the transaction is what proves the
-- swapped-in table is still referentially whole before anything is committed.
--
-- rotup:table-rebuild
--
-- What is deliberately absent matters as much as what is here. There is no
-- device-node column anywhere: the durable identity of a disk is its filesystem
-- UUID and the durable identity of a repository is the pair of that UUID and
-- restic's own repository ID. There is no snapshot table, because a v1 backup
-- run produces exactly one snapshot and it is recorded on the run. There is no
-- retention, forget, or prune state: those belong to M8.

CREATE TABLE runs_m4 (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL CHECK (kind IN (
                          'DEMO', 'CONNECTION_TEST', 'COLLECTION',
                          'DISK_SETUP', 'BACKUP_LATEST', 'VERIFY')),
    trigger_kind      TEXT NOT NULL DEFAULT 'MANUAL'
                          CHECK (trigger_kind IN ('MANUAL', 'RECOVERY')),
    source_id         TEXT REFERENCES sources(id) ON DELETE SET NULL,
    status            TEXT NOT NULL CHECK (status IN (
                          'PENDING', 'RUNNING', 'SUCCEEDED',
                          'SUCCEEDED_WITH_WARNING', 'PARTIAL', 'FAILED',
                          'INTERRUPTED')),
    summary           TEXT NOT NULL DEFAULT '',
    summary_json      TEXT NOT NULL DEFAULT '{}',
    last_error_class  TEXT,
    last_error_code   TEXT,
    last_safe_error   TEXT,
    requested_at      TEXT NOT NULL,
    started_at        TEXT,
    finished_at       TEXT,
    updated_at        TEXT NOT NULL
);

INSERT INTO runs_m4 (
    id, kind, trigger_kind, source_id, status, summary, summary_json,
    last_error_class, last_error_code, last_safe_error,
    requested_at, started_at, finished_at, updated_at
)
SELECT id, kind, trigger_kind, source_id, status, summary, summary_json,
       last_error_class, last_error_code, last_safe_error,
       requested_at, started_at, finished_at, updated_at
FROM runs;

DROP TABLE runs;

ALTER TABLE runs_m4 RENAME TO runs;

CREATE INDEX idx_runs_latest ON runs(finished_at DESC, id DESC);
CREATE INDEX idx_runs_recent ON runs(requested_at DESC, id DESC);
CREATE INDEX idx_runs_source ON runs(source_id, requested_at DESC);
CREATE INDEX idx_runs_active ON runs(status) WHERE status IN ('PENDING', 'RUNNING');
CREATE INDEX idx_runs_kind ON runs(kind, requested_at DESC);

-- One restic repository per registered disk.
--
-- filesystem_uuid duplicates the disk's UUID on purpose. Every repository
-- operation re-reads the disk record and refuses to proceed if the two disagree,
-- so a repository record can never be carried onto different hardware by an
-- identifier alone. restic_repository_id is restic's own identity, read back
-- from the repository; until it is present the repository is not proven.
CREATE TABLE repositories (
    id                         TEXT PRIMARY KEY,
    disk_id                    TEXT NOT NULL UNIQUE REFERENCES disks(id)
                                   ON DELETE RESTRICT,
    filesystem_uuid            TEXT NOT NULL COLLATE NOCASE,
    relative_path              TEXT NOT NULL,
    credential_ref_id          TEXT REFERENCES credential_refs(id)
                                   ON DELETE RESTRICT,
    restic_repository_id       TEXT,
    origin                     TEXT NOT NULL CHECK (origin IN
                                   ('INITIALIZED', 'ADOPTED')),
    health                     TEXT NOT NULL CHECK (health IN (
                                   'UNINITIALIZED', 'INITIALIZING', 'HEALTHY',
                                   'SUSPECT', 'RECOVERY_REQUIRED', 'UNAVAILABLE')),
    suspect_reason             TEXT,
    escrow_acknowledged_at     TEXT,
    established_at             TEXT,
    last_opened_at             TEXT,
    last_snapshot_id           TEXT,
    last_successful_backup_at  TEXT,
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL
);

CREATE INDEX idx_repositories_health ON repositories(health);

-- The durable detail of one BACKUP_LATEST run.
--
-- durability_stage is the whole point of this table. A run status says how the
-- run ended; the stage says how far it got along the path from "restic has been
-- started" to "the bytes are on media that has been flushed and unmounted".
-- After a crash those are different questions, and only the stage can tell
-- recovery whether a snapshot may exist that it should look for.
CREATE TABLE backup_runs (
    run_id                     TEXT PRIMARY KEY REFERENCES runs(id)
                                   ON DELETE CASCADE,
    repository_id              TEXT NOT NULL REFERENCES repositories(id)
                                   ON DELETE RESTRICT,
    disk_id                    TEXT NOT NULL REFERENCES disks(id)
                                   ON DELETE RESTRICT,
    semantics                  TEXT NOT NULL DEFAULT 'BACKUP_LATEST'
                                   CHECK (semantics = 'BACKUP_LATEST'),
    durability_stage           TEXT NOT NULL CHECK (durability_stage IN (
                                   'REQUESTED', 'READ_SET_CAPTURED', 'MOUNTED',
                                   'RESTIC_RUNNING', 'SNAPSHOT_CREATED',
                                   'SNAPSHOT_VERIFIED', 'SYNCED',
                                   'UNMOUNT_VERIFIED', 'DURABLE_SUCCESS',
                                   'FAILED')),
    durability                 TEXT NOT NULL DEFAULT 'UNPROVEN'
                                   CHECK (durability IN (
                                       'UNPROVEN', 'NORMAL',
                                       'ADOPTED_AFTER_INTERRUPTION')),
    selected_source_ids_json   TEXT NOT NULL DEFAULT '[]',
    read_set_captured_at       TEXT,
    barrier_acquired_at        TEXT,
    restic_started_at          TEXT,
    candidate_snapshot_id      TEXT,
    verified_snapshot_id       TEXT,
    snapshot_verified_at       TEXT,
    synced_at                  TEXT,
    unmount_verified_at        TEXT,
    durable_success_at         TEXT,
    files_processed            INTEGER NOT NULL DEFAULT 0,
    files_new                  INTEGER NOT NULL DEFAULT 0,
    files_changed              INTEGER NOT NULL DEFAULT 0,
    files_unmodified           INTEGER NOT NULL DEFAULT 0,
    bytes_processed            INTEGER NOT NULL DEFAULT 0,
    bytes_added                INTEGER NOT NULL DEFAULT 0,
    engine_errors              INTEGER NOT NULL DEFAULT 0,
    -- A durable success must name the snapshot it proved. Making that a
    -- constraint means the database itself refuses to hold a success that
    -- cannot say what was written.
    CHECK (durability_stage <> 'DURABLE_SUCCESS'
        OR (verified_snapshot_id IS NOT NULL AND durable_success_at IS NOT NULL))
);

CREATE INDEX idx_backup_runs_repository ON backup_runs(repository_id, run_id);
CREATE INDEX idx_backup_runs_disk ON backup_runs(disk_id, run_id);
CREATE INDEX idx_backup_runs_stage ON backup_runs(durability_stage);

-- Exactly which published staging state of each source a backup run read.
--
-- These rows are written before restic starts. A record assembled afterwards
-- could only describe what a source looks like now, which is a different
-- question from what was actually read.
CREATE TABLE backup_source_states (
    backup_run_id              TEXT NOT NULL REFERENCES backup_runs(run_id)
                                   ON DELETE CASCADE,
    source_id                  TEXT NOT NULL REFERENCES sources(id)
                                   ON DELETE RESTRICT,
    current_collection_run_id  TEXT REFERENCES runs(id) ON DELETE RESTRICT,
    staging_state_version      INTEGER NOT NULL CHECK (staging_state_version >= 1),
    published_at               TEXT,
    current_relative_path      TEXT NOT NULL,
    captured_at                TEXT NOT NULL,
    PRIMARY KEY (backup_run_id, source_id)
);

-- Exactly which revision batches a backup run read.
CREATE TABLE backup_revision_batches (
    backup_run_id              TEXT NOT NULL REFERENCES backup_runs(run_id)
                                   ON DELETE CASCADE,
    revision_batch_id          TEXT NOT NULL REFERENCES revision_batches(id)
                                   ON DELETE RESTRICT,
    source_id                  TEXT NOT NULL REFERENCES sources(id)
                                   ON DELETE RESTRICT,
    relative_path              TEXT NOT NULL,
    batch_created_at           TEXT NOT NULL,
    captured_at                TEXT NOT NULL,
    PRIMARY KEY (backup_run_id, revision_batch_id)
);

CREATE INDEX idx_backup_revision_batches_batch
    ON backup_revision_batches(revision_batch_id);

-- Proven coverage: this revision batch is inside this verified snapshot, on
-- this repository, on this registered disk.
--
-- A row here is only ever written from a read-set that reached a proven durable
-- end, or from an explicit recovery that confirmed the snapshot exists. It is
-- deliberately not derived from timestamps: "this batch was created before
-- restic started" does not establish that restic read it, and a retention
-- decision built on that inference could delete the only copy of data that was
-- never actually backed up.
--
-- Recording the disk and repository alongside the snapshot is what lets a later
-- milestone ask which retained copies contain a given revision, without that
-- policy existing yet.
CREATE TABLE revision_coverage (
    revision_batch_id          TEXT NOT NULL REFERENCES revision_batches(id)
                                   ON DELETE RESTRICT,
    backup_run_id              TEXT NOT NULL REFERENCES backup_runs(run_id)
                                   ON DELETE RESTRICT,
    repository_id              TEXT NOT NULL REFERENCES repositories(id)
                                   ON DELETE RESTRICT,
    disk_id                    TEXT NOT NULL REFERENCES disks(id)
                                   ON DELETE RESTRICT,
    snapshot_id                TEXT NOT NULL,
    covered_at                 TEXT NOT NULL,
    PRIMARY KEY (revision_batch_id, backup_run_id)
);

CREATE INDEX idx_revision_coverage_batch ON revision_coverage(revision_batch_id);
CREATE INDEX idx_revision_coverage_copy ON revision_coverage(disk_id, repository_id);

-- Events can already point at a source, a disk and a run. Repository events
-- point at a repository.
ALTER TABLE events ADD COLUMN repository_id TEXT REFERENCES repositories(id)
    ON DELETE SET NULL;
