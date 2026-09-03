CREATE TABLE runs (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind = 'DEMO'),
    status       TEXT NOT NULL CHECK (status IN ('SUCCEEDED', 'FAILED')),
    summary      TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    finished_at  TEXT NOT NULL
);

CREATE INDEX idx_runs_latest ON runs(finished_at DESC, id DESC);

