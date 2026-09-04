package backup

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/staging"
)

// maxRevisionBatchesPerSource bounds how many revision batches one source may
// contribute to a single read-set. It is a very high ceiling that only a
// pathological history reaches; hitting it is reported rather than silently
// truncating the read-set, because a truncated read-set would produce coverage
// records that claim less than was actually backed up, or more.
const maxRevisionBatchesPerSource = 1000

// readSet is exactly what one backup run will read.
//
// It is built once, under the staging barrier, immediately before restic
// starts, and is then persisted. Every later statement ROTUP makes about that
// backup — which staging versions it contains, which revision batches it
// covers — is read back from these rows rather than recomputed, because the
// staging tree will have moved on by then.
type readSet struct {
	States  []domain.BackupSourceState
	Batches []domain.BackupRevisionBatch
	// Paths are the absolute directories restic is given, in a stable order.
	Paths []string
}

// captureReadSet resolves the run's immutable source selection into the exact
// state that is about to be read.
//
// Eligibility is checked here, not at request time, and that is deliberate: the
// question is not whether a source looked usable when the administrator clicked
// the button, but whether it is a state ROTUP can stand behind at the instant
// the barrier closed around it.
func (s *Service) captureReadSet(ctx context.Context, runID string, selectedSourceIDs []string,
	at time.Time) (readSet, error) {

	var captured readSet
	if len(selectedSourceIDs) == 0 {
		return readSet{}, domain.NewError(domain.ErrClassInvariant, "backup_sources_empty",
			"a backup run must name at least one source")
	}

	for _, sourceID := range selectedSourceIDs {
		source, err := s.store.Source(ctx, sourceID)
		if err != nil {
			return readSet{}, err
		}
		if source == nil {
			return readSet{}, domain.NewError(domain.ErrClassConfiguration, "source_missing",
				"a source this backup was asked to include no longer exists")
		}
		if err := eligibleForBackup(*source); err != nil {
			return readSet{}, err
		}

		currentDir, err := s.layout.CurrentDir(*source)
		if err != nil {
			return readSet{}, err
		}
		if err := s.layout.AssertWithinRoot(currentDir); err != nil {
			return readSet{}, err
		}
		if _, statErr := os.Stat(currentDir); statErr != nil {
			// A source that claims a published stable state but whose tree is
			// not there is not something to back up around. It is a
			// contradiction, and the honest response is to refuse.
			return readSet{}, domain.NewError(domain.ErrClassIntegrity, "staging_current_missing",
				fmt.Sprintf("source %q claims a stable staging state but its current tree is not present", source.Name))
		}
		relative, err := staging.CurrentRelativePath(*source)
		if err != nil {
			return readSet{}, err
		}

		captured.States = append(captured.States, domain.BackupSourceState{
			BackupRunID:            runID,
			SourceID:               source.ID,
			CurrentCollectionRunID: source.LastCollectionRunID,
			StagingStateVersion:    source.StagingStateVersion,
			PublishedAt:            publishedAt(*source),
			CurrentRelativePath:    relative,
			CapturedAt:             at,
		})
		captured.Paths = append(captured.Paths, currentDir)

		// The source's staging metadata travels with the data. It is small,
		// ROTUP-owned, holds no secret, and is what lets someone reading the
		// media later see which published state the bytes belong to without
		// this daemon's database.
		metadataDir, err := s.layout.MetadataDir(source.ID)
		if err != nil {
			return readSet{}, err
		}
		if _, statErr := os.Stat(metadataDir); statErr == nil {
			captured.Paths = append(captured.Paths, metadataDir)
		}

		batches, err := s.captureRevisionBatches(ctx, runID, *source, at)
		if err != nil {
			return readSet{}, err
		}
		for _, batch := range batches {
			captured.Batches = append(captured.Batches, batch.record)
			captured.Paths = append(captured.Paths, batch.directory)
		}
	}
	return captured, nil
}

// capturedBatch pairs a persisted read-set row with the directory restic reads.
type capturedBatch struct {
	record    domain.BackupRevisionBatch
	directory string
}

// captureRevisionBatches selects the revision batches of one source that may be
// backed up.
//
// Only a batch whose producing collection actually finished reconciling is
// included. An interrupted collection's archive directory may still be being
// written into, and a batch built from one is not a complete group of displaced
// objects — backing it up would record coverage for data that is not all there.
func (s *Service) captureRevisionBatches(ctx context.Context, runID string, source domain.Source,
	at time.Time) ([]capturedBatch, error) {

	batches, err := s.store.ListRevisionBatches(ctx, source.ID, maxRevisionBatchesPerSource+1)
	if err != nil {
		return nil, err
	}
	if len(batches) > maxRevisionBatchesPerSource {
		return nil, domain.NewError(domain.ErrClassCapacity, "revision_batches_excessive",
			fmt.Sprintf("source %q has more revision batches than one backup read-set may describe", source.Name))
	}

	result := make([]capturedBatch, 0, len(batches))
	for _, batch := range batches {
		if !batch.ReconciliationCompleted {
			continue
		}
		directory, err := s.layout.RevisionDir(batch.SourceID, batch.CollectionRunID)
		if err != nil {
			return nil, err
		}
		// The path recorded on the batch and the path the layout derives must
		// agree. If they do not, ROTUP would be backing up one directory and
		// recording coverage for another.
		if expected := staging.RevisionRelativePath(batch.SourceID, batch.CollectionRunID); expected != batch.RelativePath {
			return nil, domain.NewError(domain.ErrClassIntegrity, "revision_batch_path_mismatch",
				"a revision batch's recorded path does not match the path ROTUP would read")
		}
		if err := s.layout.AssertWithinRoot(directory); err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(directory); statErr != nil {
			// A recorded batch whose directory is gone cannot be backed up, and
			// must not be silently dropped from a read-set that will later be
			// used to decide what is safe to delete.
			return nil, domain.NewError(domain.ErrClassIntegrity, "revision_batch_missing",
				"a recorded revision batch's directory is not present in staging")
		}
		result = append(result, capturedBatch{
			record: domain.BackupRevisionBatch{
				BackupRunID:     runID,
				RevisionBatchID: batch.ID,
				SourceID:        batch.SourceID,
				RelativePath:    batch.RelativePath,
				BatchCreatedAt:  batch.CreatedAt,
				CapturedAt:      at,
			},
			directory: directory,
		})
	}
	return result, nil
}

// eligibleForBackup decides whether a source's staging state may be read.
//
// Only STABLE qualifies. UPDATING means reconciliation could be mutating the
// tree; RECOVERY_REQUIRED means ROTUP cannot prove the tree is coherent. EMPTY
// is excluded too, and deliberately so: a source that has never published a
// state has no state version to record and no bytes anyone asked for, so
// including it would put an empty tree in the snapshot and an audit row that
// claims a version that was never published.
func eligibleForBackup(source domain.Source) error {
	switch source.StagingStatus {
	case domain.StagingStable:
	case domain.StagingUpdating:
		return domain.NewError(domain.ErrClassUnavailable, "source_updating",
			fmt.Sprintf("source %q is being collected right now, so its staging state is not stable", source.Name))
	case domain.StagingRecoveryRequired:
		return domain.NewError(domain.ErrClassIntegrity, "source_recovery_required",
			fmt.Sprintf("source %q needs recovery: ROTUP cannot prove its staged copy is coherent, "+
				"so it must not be backed up as if it were", source.Name))
	case domain.StagingEmpty:
		return domain.NewError(domain.ErrClassConfiguration, "source_never_collected",
			fmt.Sprintf("source %q has never published a staging state; collect it before backing it up", source.Name))
	default:
		return domain.NewError(domain.ErrClassIntegrity, "source_state_unknown",
			fmt.Sprintf("source %q has a staging state ROTUP does not recognize", source.Name))
	}
	if source.StagingStateVersion < 1 {
		return domain.NewError(domain.ErrClassIntegrity, "source_state_version_missing",
			fmt.Sprintf("source %q is marked stable but has no published state version", source.Name))
	}
	return nil
}

func publishedAt(source domain.Source) time.Time {
	if source.LastPublishedAt == nil {
		return time.Time{}
	}
	return *source.LastPublishedAt
}

// EligibleSource is one source the administrator may include in a backup.
type EligibleSource struct {
	Source domain.Source
	// Eligible reports whether this source may be backed up right now.
	Eligible bool
	// Reason explains an ineligible source, so the page says why rather than
	// silently omitting it.
	Reason string
}

// EligibleSources lists every enabled source with its current backup
// eligibility. It is a read model for the manual backup form and performs no
// state transition, so opening the page changes nothing.
func (s *Service) EligibleSources(ctx context.Context) ([]EligibleSource, error) {
	sources, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]EligibleSource, 0, len(sources))
	for _, source := range sources {
		if !source.Enabled {
			continue
		}
		entry := EligibleSource{Source: source, Eligible: true}
		if err := eligibleForBackup(source); err != nil {
			entry.Eligible = false
			entry.Reason = safeMessage(err)
		} else if s.barrier.Mutating(source.ID) {
			entry.Eligible = false
			entry.Reason = "a collection is currently changing this source's staging tree"
		}
		result = append(result, entry)
	}
	return result, nil
}
