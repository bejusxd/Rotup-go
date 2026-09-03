-- M3 removable media vertical slice.
--
-- Migrations 001 and 002 are immutable and are not touched. This one adds the
-- registered-disk table and extends the event log so an event can point at a
-- disk.
--
-- What is deliberately absent is as important as what is here. There is no
-- column for a device node: /dev/sdX is an observation that is only true until
-- the next enumeration, and storing it would invite exactly the mistake of
-- writing to whatever now answers to that name. The durable identity is the
-- filesystem UUID, and everything else about a device is re-derived from a
-- fresh enumeration every time.
--
-- The repository columns the architecture defines for this table belong to the
-- backup milestone and are not created here; adding them later is an additive
-- forward migration.

CREATE TABLE disks (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    filesystem_uuid       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    filesystem_type       TEXT NOT NULL,
    enabled               INTEGER NOT NULL DEFAULT 1
                              CHECK (enabled IN (0, 1)),
    first_registered_at   TEXT NOT NULL,
    last_connected_at     TEXT,
    last_disconnected_at  TEXT,
    current_status        TEXT NOT NULL DEFAULT 'DISCONNECTED'
                              CHECK (current_status IN (
                                  'DISCONNECTED', 'PHYSICALLY_CONNECTED',
                                  'DETECTED', 'AVAILABLE_UNMOUNTED',
                                  'VALIDATING', 'MOUNTING', 'READY',
                                  'BACKUP_RUNNING', 'VERIFYING',
                                  'MAINTENANCE_RUNNING', 'SYNCING',
                                  'UNMOUNTING', 'SAFE_TO_REMOVE', 'ERROR')),
    last_error_code       TEXT,
    updated_at            TEXT NOT NULL
);

CREATE INDEX idx_disks_status ON disks(current_status);

-- Events can already point at a source and a run; media events point at a disk.
ALTER TABLE events ADD COLUMN disk_id TEXT REFERENCES disks(id) ON DELETE SET NULL;
