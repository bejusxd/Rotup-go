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

const repositoryColumns = `id, disk_id, filesystem_uuid, relative_path,
	credential_ref_id, restic_repository_id, origin, health, suspect_reason,
	escrow_acknowledged_at, established_at, last_opened_at, last_snapshot_id,
	last_successful_backup_at, created_at, updated_at`

// CreateRepository records a repository an administrator explicitly asked
// ROTUP to establish on a registered disk.
func (s *Store) CreateRepository(ctx context.Context, repository domain.Repository) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO repositories (`+repositoryColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			repository.ID,
			repository.DiskID,
			domain.NormalizeFilesystemUUID(repository.FilesystemUUID),
			repository.RelativePath,
			nullableText(repository.CredentialRefID),
			nullableText(repository.ResticRepositoryID),
			string(repository.Origin),
			string(repository.Health),
			nullableText(repository.SuspectReason),
			nullableTime(timeOrZero(repository.EscrowAcknowledgedAt)),
			nullableTime(timeOrZero(repository.EstablishedAt)),
			nullableTime(timeOrZero(repository.LastOpenedAt)),
			nullableText(repository.LastSnapshotID),
			nullableTime(timeOrZero(repository.LastSuccessfulBackupAt)),
			formatTime(repository.CreatedAt),
			formatTime(repository.UpdatedAt),
		)
		if err != nil {
			return translateRepositoryConstraint(err)
		}
		return nil
	})
}

// AttachRepositoryCredential stores the credential metadata for a repository
// password and points the repository at it, in one transaction. The password
// itself is already in the secret store; only its locator reaches SQLite.
func (s *Store) AttachRepositoryCredential(ctx context.Context, repositoryID string,
	ref domain.CredentialRef) (string, error) {

	var displaced string
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var previousID, previousLocator sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT c.id, c.provider_locator
			FROM repositories r
			LEFT JOIN credential_refs c ON c.id = r.credential_ref_id
			WHERE r.id = ?`, repositoryID).Scan(&previousID, &previousLocator)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NewError(domain.ErrClassConfiguration, "repository_missing",
				"that repository no longer exists")
		}
		if err != nil {
			return fmt.Errorf("read current repository credential: %w", err)
		}

		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credential_refs (id, display_name, provider, provider_locator, purpose, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ref.ID, ref.DisplayName, ref.Provider, ref.ProviderLocator, string(ref.Purpose), now, now,
		); err != nil {
			return fmt.Errorf("insert repository credential reference: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE repositories SET credential_ref_id = ?, updated_at = ? WHERE id = ?`,
			ref.ID, now, repositoryID,
		); err != nil {
			return fmt.Errorf("attach repository credential: %w", err)
		}
		if previousID.Valid && previousID.String != "" {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM credential_refs WHERE id = ?`, previousID.String); err != nil {
				return fmt.Errorf("remove displaced repository credential reference: %w", err)
			}
			displaced = previousLocator.String
		}
		return nil
	})
	return displaced, err
}

// RepositoryCredentialRef loads the metadata of a repository's password. It
// never returns secret material; only the locator that addresses it.
func (s *Store) RepositoryCredentialRef(ctx context.Context, repositoryID string) (domain.CredentialRef, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT c.id, c.display_name, c.provider, c.provider_locator, c.purpose,
		       c.created_at, c.updated_at
		FROM repositories r
		JOIN credential_refs c ON c.id = r.credential_ref_id
		WHERE r.id = ?`, repositoryID)

	var ref domain.CredentialRef
	var purpose string
	err := row.Scan(&ref.ID, &ref.DisplayName, &ref.Provider, &ref.ProviderLocator,
		&purpose, &ref.CreatedAt, &ref.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CredentialRef{}, nil
	}
	if err != nil {
		return domain.CredentialRef{}, fmt.Errorf("read repository credential reference: %w", err)
	}
	ref.Purpose = domain.SecretPurpose(purpose)
	return ref, nil
}

// AcknowledgeRepositoryEscrow records that the administrator confirmed the
// repository password is held outside this host.
func (s *Store) AcknowledgeRepositoryEscrow(ctx context.Context, repositoryID string, at time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE repositories
			SET escrow_acknowledged_at = COALESCE(escrow_acknowledged_at, ?), updated_at = ?
			WHERE id = ?`, formatTime(at), formatTime(time.Now().UTC()), repositoryID)
		if err != nil {
			return fmt.Errorf("record repository escrow acknowledgement: %w", err)
		}
		return requireRepositoryRow(result)
	})
}

// SetRepositoryEstablished records the identity restic reported and the
// repository's proven health, in one statement.
//
// Identity and health are written together on purpose: a repository that is
// healthy but has no recorded identity could not be verified on the next mount,
// and a repository with an identity but an unproven health would invite exactly
// the assumption this milestone exists to prevent.
func (s *Store) SetRepositoryEstablished(ctx context.Context, repositoryID, resticRepositoryID string,
	health domain.RepositoryHealth, establishedAt time.Time) error {

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE repositories
			SET restic_repository_id = ?, health = ?, suspect_reason = NULL,
			    established_at = COALESCE(established_at, ?),
			    last_opened_at = ?, updated_at = ?
			WHERE id = ?`,
			resticRepositoryID,
			string(health),
			formatTime(establishedAt),
			formatTime(establishedAt),
			formatTime(time.Now().UTC()),
			repositoryID,
		)
		if err != nil {
			return fmt.Errorf("record repository identity: %w", err)
		}
		return requireRepositoryRow(result)
	})
}

// SetRepositoryHealth records a repository's health and the safe reason behind
// it. A reason is stored only for a state that needs one; clearing the state
// clears the reason so a stale explanation cannot outlive the problem.
func (s *Store) SetRepositoryHealth(ctx context.Context, repositoryID string,
	health domain.RepositoryHealth, reason string) error {

	if health.WritableForBackup() {
		reason = ""
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE repositories SET health = ?, suspect_reason = ?, updated_at = ?
			WHERE id = ?`,
			string(health), nullableText(reason), formatTime(time.Now().UTC()), repositoryID)
		if err != nil {
			return fmt.Errorf("record repository health: %w", err)
		}
		return requireRepositoryRow(result)
	})
}

// TouchRepositoryOpened records that the repository was successfully opened.
func (s *Store) TouchRepositoryOpened(ctx context.Context, repositoryID string, at time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE repositories SET last_opened_at = ?, updated_at = ? WHERE id = ?`,
			formatTime(at), formatTime(time.Now().UTC()), repositoryID)
		if err != nil {
			return fmt.Errorf("record repository open: %w", err)
		}
		return requireRepositoryRow(result)
	})
}

// Repository loads one repository, or nil when it does not exist.
func (s *Store) Repository(ctx context.Context, id string) (*domain.Repository, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories WHERE id = ?`, id)
	repository, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return repository, nil
}

// RepositoryForDisk loads the repository of a registered disk, or nil.
func (s *Store) RepositoryForDisk(ctx context.Context, diskID string) (*domain.Repository, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE disk_id = ?`, diskID)
	repository, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return repository, nil
}

// ListRepositories returns every repository ordered by disk.
func (s *Store) ListRepositories(ctx context.Context) ([]domain.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+repositoryColumns+` FROM repositories ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer rows.Close()

	var result []domain.Repository
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repositories: %w", err)
	}
	return result, nil
}

func scanRepository(row interface{ Scan(...any) error }) (*domain.Repository, error) {
	var repository domain.Repository
	var credentialRefID, resticID, suspectReason, lastSnapshotID sql.NullString
	var origin, health string
	var escrowAt, establishedAt, lastOpenedAt, lastBackupAt, createdAt, updatedAt sql.NullString

	if err := row.Scan(&repository.ID, &repository.DiskID, &repository.FilesystemUUID,
		&repository.RelativePath, &credentialRefID, &resticID, &origin, &health,
		&suspectReason, &escrowAt, &establishedAt, &lastOpenedAt, &lastSnapshotID,
		&lastBackupAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan repository: %w", err)
	}

	repository.CredentialRefID = credentialRefID.String
	repository.ResticRepositoryID = resticID.String
	repository.Origin = domain.RepositoryOrigin(origin)
	repository.Health = domain.RepositoryHealth(health)
	repository.SuspectReason = suspectReason.String
	repository.LastSnapshotID = lastSnapshotID.String

	var err error
	if repository.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if repository.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	for _, field := range []struct {
		raw    sql.NullString
		target **time.Time
	}{
		{escrowAt, &repository.EscrowAcknowledgedAt},
		{establishedAt, &repository.EstablishedAt},
		{lastOpenedAt, &repository.LastOpenedAt},
		{lastBackupAt, &repository.LastSuccessfulBackupAt},
	} {
		parsed, parseErr := parseTime(field.raw)
		if parseErr != nil {
			return nil, parseErr
		}
		if !parsed.IsZero() {
			value := parsed
			*field.target = &value
		}
	}
	return &repository, nil
}

func requireRepositoryRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update repository: %w", err)
	}
	if affected == 0 {
		return domain.NewError(domain.ErrClassConfiguration, "repository_missing",
			"that repository no longer exists")
	}
	return nil
}

func translateRepositoryConstraint(err error) error {
	message := err.Error()
	if strings.Contains(message, "UNIQUE") && strings.Contains(message, "repositories.disk_id") {
		return domain.WrapError(domain.ErrClassConfiguration, "repository_already_exists",
			"this disk already has a ROTUP repository", err)
	}
	return fmt.Errorf("persist repository: %w", err)
}
