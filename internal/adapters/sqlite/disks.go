package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const diskColumns = `id, name, filesystem_uuid, filesystem_type, enabled,
	first_registered_at, last_connected_at, last_disconnected_at,
	current_status, last_error_code, updated_at`

// CreateDisk records an explicitly registered disk.
func (s *Store) CreateDisk(ctx context.Context, disk domain.Disk) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO disks (`+diskColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			disk.ID,
			disk.Name,
			domain.NormalizeFilesystemUUID(disk.FilesystemUUID),
			domain.NormalizeFilesystemType(disk.FilesystemType),
			boolToInt(disk.Enabled),
			formatTime(disk.FirstRegisteredAt),
			nullableTime(timeOrZero(disk.LastConnectedAt)),
			nullableTime(timeOrZero(disk.LastDisconnectedAt)),
			string(disk.CurrentStatus),
			nullableText(disk.LastErrorCode),
			formatTime(disk.UpdatedAt),
		)
		if err != nil {
			return translateDiskConstraint(err)
		}
		return nil
	})
}

// UpdateDiskSettings rewrites only the administrator-owned metadata. The
// filesystem UUID is never editable: it is the disk's identity, and changing it
// would silently repoint a registration at different hardware.
func (s *Store) UpdateDiskSettings(ctx context.Context, id, name string, enabled bool) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE disks SET name = ?, enabled = ?, updated_at = ? WHERE id = ?`,
			name, boolToInt(enabled), formatTime(time.Now().UTC()), id)
		if err != nil {
			return translateDiskConstraint(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update disk: %w", err)
		}
		if affected == 0 {
			return domain.NewError(domain.ErrClassConfiguration, "disk_missing",
				"that disk is not registered")
		}
		return nil
	})
}

// SaveDiskState records an observed state transition. The connection
// timestamps are only written when the caller supplies them, so a transition
// that says nothing about connectivity leaves the stored history alone.
func (s *Store) SaveDiskState(ctx context.Context, state domain.DiskState) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE disks
			SET current_status = ?,
			    last_error_code = ?,
			    last_connected_at = COALESCE(?, last_connected_at),
			    last_disconnected_at = COALESCE(?, last_disconnected_at),
			    updated_at = ?
			WHERE id = ?`,
			string(state.Status),
			nullableText(state.ErrorCode),
			nullableTime(timeOrZero(state.LastConnectedAt)),
			nullableTime(timeOrZero(state.LastDisconnectedAt)),
			formatTime(time.Now().UTC()),
			state.ID,
		)
		if err != nil {
			return fmt.Errorf("save disk state: %w", err)
		}
		return nil
	})
}

// Disk loads one registered disk, or nil when it does not exist.
func (s *Store) Disk(ctx context.Context, id string) (*domain.Disk, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+diskColumns+` FROM disks WHERE id = ?`, id)
	disk, err := scanDisk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return disk, nil
}

// DiskByFilesystemUUID looks a disk up by its durable identity.
func (s *Store) DiskByFilesystemUUID(ctx context.Context, filesystemUUID string) (*domain.Disk, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+diskColumns+` FROM disks WHERE filesystem_uuid = ? COLLATE NOCASE`,
		domain.NormalizeFilesystemUUID(filesystemUUID))
	disk, err := scanDisk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return disk, nil
}

// ListDisks returns every registered disk ordered by name.
func (s *Store) ListDisks(ctx context.Context) ([]domain.Disk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+diskColumns+` FROM disks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list disks: %w", err)
	}
	defer rows.Close()

	var result []domain.Disk
	for rows.Next() {
		disk, err := scanDisk(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *disk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate disks: %w", err)
	}
	return result, nil
}

func scanDisk(row interface{ Scan(...any) error }) (*domain.Disk, error) {
	var disk domain.Disk
	var enabled int
	var status string
	var lastErrorCode sql.NullString
	var firstRegisteredAt, lastConnectedAt, lastDisconnectedAt, updatedAt sql.NullString

	if err := row.Scan(&disk.ID, &disk.Name, &disk.FilesystemUUID, &disk.FilesystemType,
		&enabled, &firstRegisteredAt, &lastConnectedAt, &lastDisconnectedAt,
		&status, &lastErrorCode, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan disk: %w", err)
	}

	disk.Enabled = enabled != 0
	disk.CurrentStatus = domain.DiskStatus(status)
	disk.LastErrorCode = lastErrorCode.String

	var err error
	if disk.FirstRegisteredAt, err = parseTime(firstRegisteredAt); err != nil {
		return nil, err
	}
	if disk.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	connected, err := parseTime(lastConnectedAt)
	if err != nil {
		return nil, err
	}
	if !connected.IsZero() {
		disk.LastConnectedAt = &connected
	}
	disconnected, err := parseTime(lastDisconnectedAt)
	if err != nil {
		return nil, err
	}
	if !disconnected.IsZero() {
		disk.LastDisconnectedAt = &disconnected
	}
	return &disk, nil
}

func translateDiskConstraint(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "UNIQUE") && strings.Contains(message, "disks.filesystem_uuid"):
		return domain.WrapError(domain.ErrClassConfiguration, "disk_already_registered",
			"a disk with that filesystem UUID is already registered", err)
	case strings.Contains(message, "UNIQUE") && strings.Contains(message, "disks.name"):
		return domain.WrapError(domain.ErrClassConfiguration, "disk_name_taken",
			"another disk already uses that name", err)
	default:
		return fmt.Errorf("persist disk: %w", err)
	}
}
