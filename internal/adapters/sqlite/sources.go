package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const sourceColumns = `id, name, host, share_name, subdirectory, username,
	credential_ref_id, staging_relative_path, enabled,
	include_rules_json, exclude_rules_json, reconciliation_policy,
	staging_status, staging_state_version, last_published_at,
	last_collection_run_id, created_at, updated_at`

// CreateSource inserts a validated source.
func (s *Store) CreateSource(ctx context.Context, source domain.Source) error {
	includes, excludes, err := encodeRules(source)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sources (`+sourceColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			source.ID, source.Name, source.Host, source.ShareName,
			nullableText(source.Subdirectory), nullableText(source.Username),
			nullableText(source.CredentialRefID), source.StagingRelativePath,
			boolToInt(source.Enabled), includes, excludes,
			string(source.ReconciliationPolicy), string(domain.StagingEmpty), 0,
			nil, nil, formatTime(now), formatTime(now),
		)
		if err != nil {
			return translateConstraint(err, "source")
		}
		return nil
	})
}

// UpdateSourceSettings rewrites only the administrator-editable, non-secret
// fields. Staging state and the credential reference are deliberately not
// touched here so that saving a form can never disturb them.
func (s *Store) UpdateSourceSettings(ctx context.Context, source domain.Source) error {
	includes, excludes, err := encodeRules(source)
	if err != nil {
		return err
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE sources
			SET name = ?, host = ?, share_name = ?, subdirectory = ?, username = ?,
			    staging_relative_path = ?, enabled = ?,
			    include_rules_json = ?, exclude_rules_json = ?,
			    reconciliation_policy = ?, updated_at = ?
			WHERE id = ?`,
			source.Name, source.Host, source.ShareName,
			nullableText(source.Subdirectory), nullableText(source.Username),
			source.StagingRelativePath, boolToInt(source.Enabled),
			includes, excludes, string(source.ReconciliationPolicy),
			formatTime(time.Now().UTC()), source.ID,
		)
		if err != nil {
			return translateConstraint(err, "source")
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update source: %w", err)
		}
		if affected == 0 {
			return domain.NewError(domain.ErrClassConfiguration, "source_missing",
				"the source no longer exists")
		}
		return nil
	})
}

// SetSourceStagingStatus records a staging state transition without advancing
// the state version. It is used to mark UPDATING before reconciliation and
// RECOVERY_REQUIRED when coherence cannot be proven.
func (s *Store) SetSourceStagingStatus(ctx context.Context, sourceID string, status domain.StagingStatus) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sources SET staging_status = ?, updated_at = ? WHERE id = ?`,
			string(status), formatTime(time.Now().UTC()), sourceID)
		if err != nil {
			return fmt.Errorf("set staging status: %w", err)
		}
		return nil
	})
}

// Source loads one source by identifier, or nil when it does not exist.
func (s *Store) Source(ctx context.Context, id string) (*domain.Source, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id)
	source, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return source, nil
}

// ListSources returns every configured source ordered by name.
func (s *Store) ListSources(ctx context.Context) ([]domain.Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var result []domain.Source
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return result, nil
}

// StagingPaths returns every source's staging destination keyed by path, which
// is what source validation needs to reject overlapping destinations.
func (s *Store) StagingPaths(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT staging_relative_path, id FROM sources`)
	if err != nil {
		return nil, fmt.Errorf("read staging destinations: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var path, id string
		if err := rows.Scan(&path, &id); err != nil {
			return nil, fmt.Errorf("scan staging destination: %w", err)
		}
		result[path] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate staging destinations: %w", err)
	}
	return result, nil
}

// AttachCredential replaces the source's credential reference and, in the same
// transaction, removes the reference it displaced. The caller deletes the
// displaced provider material afterwards.
func (s *Store) AttachCredential(ctx context.Context, sourceID string, ref domain.CredentialRef) (string, error) {
	var displaced string
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		previous, err := currentCredentialRef(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credential_refs (id, display_name, provider, provider_locator, purpose, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ref.ID, ref.DisplayName, ref.Provider, ref.ProviderLocator, string(ref.Purpose), now, now,
		); err != nil {
			return fmt.Errorf("insert credential reference: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sources SET credential_ref_id = ?, updated_at = ? WHERE id = ?`,
			ref.ID, now, sourceID,
		); err != nil {
			return fmt.Errorf("attach credential: %w", err)
		}
		if previous.ID != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM credential_refs WHERE id = ?`, previous.ID); err != nil {
				return fmt.Errorf("remove displaced credential reference: %w", err)
			}
			displaced = previous.ProviderLocator
		}
		return nil
	})
	return displaced, err
}

// ClearCredential detaches and removes the source's credential reference. It
// returns the provider locator whose material the caller must delete.
func (s *Store) ClearCredential(ctx context.Context, sourceID string) (string, error) {
	var locator string
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		previous, err := currentCredentialRef(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if previous.ID == "" {
			return nil
		}
		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `
			UPDATE sources SET credential_ref_id = NULL, updated_at = ? WHERE id = ?`,
			now, sourceID,
		); err != nil {
			return fmt.Errorf("detach credential: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM credential_refs WHERE id = ?`, previous.ID); err != nil {
			return fmt.Errorf("remove credential reference: %w", err)
		}
		locator = previous.ProviderLocator
		return nil
	})
	return locator, err
}

// CredentialRef loads the metadata of a source's credential. It never returns
// secret material; only the provider locator, which addresses it.
func (s *Store) CredentialRef(ctx context.Context, sourceID string) (domain.CredentialRef, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT c.id, c.display_name, c.provider, c.provider_locator, c.purpose,
		       c.created_at, c.updated_at
		FROM sources s
		JOIN credential_refs c ON c.id = s.credential_ref_id
		WHERE s.id = ?`, sourceID)

	var ref domain.CredentialRef
	var purpose string
	err := row.Scan(&ref.ID, &ref.DisplayName, &ref.Provider, &ref.ProviderLocator,
		&purpose, &ref.CreatedAt, &ref.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CredentialRef{}, nil
	}
	if err != nil {
		return domain.CredentialRef{}, fmt.Errorf("read credential reference: %w", err)
	}
	ref.Purpose = domain.SecretPurpose(purpose)
	return ref, nil
}

func currentCredentialRef(ctx context.Context, tx *sql.Tx, sourceID string) (domain.CredentialRef, error) {
	var ref domain.CredentialRef
	var id, locator sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT c.id, c.provider_locator
		FROM sources s
		LEFT JOIN credential_refs c ON c.id = s.credential_ref_id
		WHERE s.id = ?`, sourceID).Scan(&id, &locator)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CredentialRef{}, domain.NewError(domain.ErrClassConfiguration, "source_missing",
			"the source no longer exists")
	}
	if err != nil {
		return domain.CredentialRef{}, fmt.Errorf("read current credential reference: %w", err)
	}
	ref.ID = id.String
	ref.ProviderLocator = locator.String
	return ref, nil
}

func scanSource(row interface{ Scan(...any) error }) (*domain.Source, error) {
	var source domain.Source
	var subdirectory, username, credentialRefID sql.NullString
	var lastPublishedAt, lastCollectionRunID sql.NullString
	var createdAt, updatedAt sql.NullString
	var includes, excludes, policy, status string
	var enabled int

	if err := row.Scan(&source.ID, &source.Name, &source.Host, &source.ShareName,
		&subdirectory, &username, &credentialRefID, &source.StagingRelativePath,
		&enabled, &includes, &excludes, &policy, &status, &source.StagingStateVersion,
		&lastPublishedAt, &lastCollectionRunID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan source: %w", err)
	}

	source.Subdirectory = subdirectory.String
	source.Username = username.String
	source.CredentialRefID = credentialRefID.String
	source.Enabled = enabled != 0
	source.ReconciliationPolicy = domain.ReconciliationPolicy(policy)
	source.StagingStatus = domain.StagingStatus(status)
	source.LastCollectionRunID = lastCollectionRunID.String

	if err := json.Unmarshal([]byte(includes), &source.IncludeRules); err != nil {
		return nil, fmt.Errorf("decode include rules: %w", err)
	}
	if err := json.Unmarshal([]byte(excludes), &source.ExcludeRules); err != nil {
		return nil, fmt.Errorf("decode exclude rules: %w", err)
	}

	published, err := parseTime(lastPublishedAt)
	if err != nil {
		return nil, err
	}
	if !published.IsZero() {
		source.LastPublishedAt = &published
	}
	if source.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if source.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &source, nil
}

func encodeRules(source domain.Source) (string, string, error) {
	includes, err := json.Marshal(orEmpty(source.IncludeRules))
	if err != nil {
		return "", "", fmt.Errorf("encode include rules: %w", err)
	}
	excludes, err := json.Marshal(orEmpty(source.ExcludeRules))
	if err != nil {
		return "", "", fmt.Errorf("encode exclude rules: %w", err)
	}
	return string(includes), string(excludes), nil
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// translateConstraint turns SQLite uniqueness failures into administrator
// facing configuration errors instead of raw driver text.
func translateConstraint(err error, entity string) error {
	message := err.Error()
	switch {
	case containsAll(message, "UNIQUE", "sources.name"):
		return domain.WrapError(domain.ErrClassConfiguration, "source_name_taken",
			"another source already uses that name", err)
	case containsAll(message, "UNIQUE", "sources.staging_relative_path"):
		return domain.WrapError(domain.ErrClassConfiguration, "staging_path_taken",
			"another source already uses that staging destination", err)
	default:
		return fmt.Errorf("persist %s: %w", entity, err)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
