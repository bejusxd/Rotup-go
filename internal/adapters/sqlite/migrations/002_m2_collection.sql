-- M2 collection vertical slice.
--
-- Migration 001 is immutable and is never edited. This migration adds the M2
-- configuration tables and then rebuilds the narrow M1 runs table with SQLite's
-- create-copy-swap pattern so that existing DEMO rows survive.
--
-- Ordering matters. SQLite resolves a foreign key when a row is written, not
-- when the table is created, so every parent table must exist before the copy
-- into the rebuilt runs table runs. sources is therefore created first, and its
-- last_collection_run_id is deliberately a plain column rather than a foreign
-- key: sources and runs would otherwise reference each other, which cannot be
-- expressed while one of the two is being rebuilt.

CREATE TABLE credential_refs (
    id                TEXT PRIMARY KEY,
    display_name      TEXT NOT NULL,
    provider          TEXT NOT NULL,
    provider_locator  TEXT NOT NULL,
    purpose           TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    last_used_at      TEXT,
    UNIQUE (provider, provider_locator)
);

CREATE TABLE sources (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL UNIQUE,
    host                     TEXT NOT NULL,
    share_name               TEXT NOT NULL,
    subdirectory             TEXT,
    username                 TEXT,
    credential_ref_id        TEXT REFERENCES credential_refs(id)
                                 ON DELETE RESTRICT,
    staging_relative_path    TEXT NOT NULL UNIQUE,
    enabled                  INTEGER NOT NULL DEFAULT 1
                                 CHECK (enabled IN (0, 1)),
    include_rules_json       TEXT NOT NULL DEFAULT '[]',
    exclude_rules_json       TEXT NOT NULL DEFAULT '[]',
    reconciliation_policy    TEXT NOT NULL DEFAULT 'archive_then_sync'
                                 CHECK (reconciliation_policy IN
                                     ('copy_only', 'archive_then_sync', 'mirror')),
    staging_status           TEXT NOT NULL DEFAULT 'EMPTY'
                                 CHECK (staging_status IN (
                                     'EMPTY', 'UPDATING', 'STABLE',
                                     'RECOVERY_REQUIRED')),
    staging_state_version    INTEGER NOT NULL DEFAULT 0
                                 CHECK (staging_state_version >= 0),
    last_published_at        TEXT,
    last_collection_run_id   TEXT,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
);

CREATE TABLE runs_m2 (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL CHECK (kind IN (
                          'DEMO', 'CONNECTION_TEST', 'COLLECTION')),
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

INSERT INTO runs_m2 (
    id, kind, trigger_kind, status, summary,
    requested_at, started_at, finished_at, updated_at
)
SELECT id, kind, 'MANUAL', status, summary,
       requested_at, started_at, finished_at, finished_at
FROM runs;

DROP TABLE runs;

ALTER TABLE runs_m2 RENAME TO runs;

CREATE INDEX idx_runs_latest ON runs(finished_at DESC, id DESC);
CREATE INDEX idx_runs_recent ON runs(requested_at DESC, id DESC);
CREATE INDEX idx_runs_source ON runs(source_id, requested_at DESC);
CREATE INDEX idx_runs_active ON runs(status) WHERE status IN ('PENDING', 'RUNNING');

-- One run/source group of replaced or deleted staging objects. This is a batch
-- record, not a per-file catalog. reconciliation_completed is what keeps an
-- interrupted run's partial archive directory from being presented as a
-- finished batch; retention and backup coverage arrive with M8.
CREATE TABLE revision_batches (
    id                        TEXT PRIMARY KEY,
    source_id                 TEXT NOT NULL REFERENCES sources(id)
                                  ON DELETE RESTRICT,
    collection_run_id         TEXT NOT NULL REFERENCES runs(id)
                                  ON DELETE RESTRICT,
    relative_path             TEXT NOT NULL UNIQUE,
    created_at                TEXT NOT NULL,
    item_count                INTEGER NOT NULL DEFAULT 0,
    byte_count                INTEGER NOT NULL DEFAULT 0,
    reconciliation_completed  INTEGER NOT NULL DEFAULT 0
                                  CHECK (reconciliation_completed IN (0, 1))
);

CREATE INDEX idx_revision_batches_source
    ON revision_batches(source_id, created_at DESC);

CREATE TABLE run_source_results (
    collection_run_id          TEXT NOT NULL REFERENCES runs(id)
                                   ON DELETE CASCADE,
    source_id                  TEXT NOT NULL REFERENCES sources(id)
                                   ON DELETE RESTRICT,
    status                     TEXT NOT NULL,
    staging_state_version      INTEGER CHECK (staging_state_version >= 1),
    published_at               TEXT,
    started_at                 TEXT,
    finished_at                TEXT,
    bytes_scanned              INTEGER NOT NULL DEFAULT 0,
    bytes_copied               INTEGER NOT NULL DEFAULT 0,
    files_scanned              INTEGER NOT NULL DEFAULT 0,
    files_copied               INTEGER NOT NULL DEFAULT 0,
    files_deleted_or_archived  INTEGER NOT NULL DEFAULT 0,
    engine_checks              INTEGER NOT NULL DEFAULT 0,
    engine_errors              INTEGER NOT NULL DEFAULT 0,
    warnings_json              TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (collection_run_id, source_id)
);

CREATE UNIQUE INDEX idx_run_source_state_version
    ON run_source_results(source_id, staging_state_version)
    WHERE staging_state_version IS NOT NULL;

CREATE TABLE events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at   TEXT NOT NULL,
    severity      TEXT NOT NULL CHECK (severity IN (
                      'DEBUG', 'INFO', 'WARNING', 'ERROR', 'CRITICAL')),
    event_type    TEXT NOT NULL,
    component     TEXT NOT NULL,
    source_id     TEXT REFERENCES sources(id) ON DELETE SET NULL,
    run_id        TEXT REFERENCES runs(id) ON DELETE SET NULL,
    payload_json  TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_events_recent ON events(occurred_at DESC, id DESC);
CREATE INDEX idx_events_source ON events(source_id, occurred_at DESC);
