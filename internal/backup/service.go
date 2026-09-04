// Package backup owns the M4 repository and backup use cases: establishing a
// restic repository on a registered disk, running one manual BACKUP_LATEST, and
// recovering a repository an interrupted operation left unproven.
//
// Its organising rule is that a backup is only a success when every step of it
// has been proven, in order, in one operation: the read-set was captured before
// anything was read, the disk's identity was verified after mounting, the
// repository's identity was verified after opening, restic named a snapshot,
// that snapshot was found by a separate read, the filesystem was flushed, and
// the unmount was confirmed against the kernel's own mount table. A run that
// stops short of that is recorded as what it is, and the repository it touched
// becomes suspect until an explicit recovery proves otherwise.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/staging"
)

const component = "backup"

// Store is the repository seam the backup use cases need. It is deliberately
// narrow and use-case shaped rather than a general database interface.
type Store interface {
	Source(ctx context.Context, id string) (*domain.Source, error)
	ListSources(ctx context.Context) ([]domain.Source, error)
	ListRevisionBatches(ctx context.Context, sourceID string, limit int) ([]domain.RevisionBatch, error)

	Disk(ctx context.Context, id string) (*domain.Disk, error)
	ListDisks(ctx context.Context) ([]domain.Disk, error)

	CreateRepository(ctx context.Context, repository domain.Repository) error
	Repository(ctx context.Context, id string) (*domain.Repository, error)
	RepositoryForDisk(ctx context.Context, diskID string) (*domain.Repository, error)
	ListRepositories(ctx context.Context) ([]domain.Repository, error)
	AttachRepositoryCredential(ctx context.Context, repositoryID string, ref domain.CredentialRef) (string, error)
	RepositoryCredentialRef(ctx context.Context, repositoryID string) (domain.CredentialRef, error)
	AcknowledgeRepositoryEscrow(ctx context.Context, repositoryID string, at time.Time) error
	SetRepositoryEstablished(ctx context.Context, repositoryID, resticRepositoryID string,
		health domain.RepositoryHealth, establishedAt time.Time) error
	SetRepositoryHealth(ctx context.Context, repositoryID string, health domain.RepositoryHealth, reason string) error
	TouchRepositoryOpened(ctx context.Context, repositoryID string, at time.Time) error

	RecordRun(ctx context.Context, run domain.Run) error
	UpdateRun(ctx context.Context, run domain.Run) error
	Run(ctx context.Context, id string) (*domain.Run, error)
	ActiveRepositoryRuns(ctx context.Context) ([]domain.Run, error)

	CreateBackupRun(ctx context.Context, backup domain.BackupRun) error
	BackupRun(ctx context.Context, runID string) (*domain.BackupRun, error)
	ListBackupRunsForRepository(ctx context.Context, repositoryID string, limit int) ([]domain.BackupRun, error)
	BackupRunsAwaitingRecovery(ctx context.Context, repositoryID string) ([]domain.BackupRun, error)
	BackupSourceStates(ctx context.Context, runID string) ([]domain.BackupSourceState, error)
	BackupRevisionBatches(ctx context.Context, runID string) ([]domain.BackupRevisionBatch, error)
	SaveReadSet(ctx context.Context, runID string, states []domain.BackupSourceState,
		batches []domain.BackupRevisionBatch) error
	SetBackupStage(ctx context.Context, runID string, stage domain.DurabilityStage, at time.Time) error
	SetBackupBarrierAcquired(ctx context.Context, runID string, at time.Time) error
	RecordCandidateSnapshot(ctx context.Context, runID, snapshotID string, stats domain.BackupRun) error
	RecordVerifiedSnapshot(ctx context.Context, runID, snapshotID string, at time.Time) error
	CompleteBackupRun(ctx context.Context, run domain.Run, backup domain.BackupRun,
		coverage []domain.RevisionCoverage) error
	FailBackupRun(ctx context.Context, runID string, stage domain.DurabilityStage) error

	AppendEvent(ctx context.Context, event domain.Event) error
}

// Service implements the M4 repository and backup use cases.
type Service struct {
	store   Store
	engine  ports.BackupEngine
	media   ports.DiskLeaser
	layout  *staging.Layout
	barrier *staging.Barrier
	secrets ports.SecretStore
	logger  *slog.Logger

	// base is the daemon lifetime context. Background work is bound to it, not
	// to the HTTP request that started it, so a browser going away can never
	// abandon a half-written repository.
	base   context.Context
	wg     sync.WaitGroup
	now    func() time.Time
	inline bool
}

// Options configures the service.
type Options struct {
	Store   Store
	Engine  ports.BackupEngine
	Media   ports.DiskLeaser
	Layout  *staging.Layout
	Barrier *staging.Barrier
	Secrets ports.SecretStore
	Logger  *slog.Logger
	// Now is injectable for deterministic tests.
	Now func() time.Time
	// RunInline executes work synchronously instead of in a goroutine. Tests
	// use it; the daemon does not.
	RunInline bool
}

// New builds the backup service. base bounds every background operation.
func New(base context.Context, options Options) *Service {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	barrier := options.Barrier
	if barrier == nil {
		barrier = staging.NewBarrier()
	}
	return &Service{
		store:   options.Store,
		engine:  options.Engine,
		media:   options.Media,
		layout:  options.Layout,
		barrier: barrier,
		secrets: options.Secrets,
		logger:  logger.With("component", component),
		base:    base,
		now:     now,
		inline:  options.RunInline,
	}
}

// Wait blocks until every background operation has finished.
func (s *Service) Wait() { s.wg.Wait() }

// Barrier exposes the staging mutation barrier so the collection service shares
// the same one. There is exactly one barrier per daemon: two would each be
// individually consistent and jointly useless.
func (s *Service) Barrier() *staging.Barrier { return s.barrier }

// run executes work either inline or in a bounded background goroutine.
func (s *Service) run(work func(context.Context)) {
	if s.inline {
		work(s.base)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		work(s.base)
	}()
}

// requireRepository loads a repository and the disk it belongs to, proving that
// the two still agree about the filesystem UUID.
//
// The UUID check is not redundant with the foreign key. The foreign key says the
// repository points at a disk row; the UUID says that disk row still describes
// the same physical filesystem the repository was established on. Those come
// apart if a disk record is ever re-registered, and a repository that followed
// such a change would be pointed at media that has never held it.
func (s *Service) requireRepository(ctx context.Context, repositoryID string) (*domain.Repository, *domain.Disk, error) {
	repository, err := s.store.Repository(ctx, repositoryID)
	if err != nil {
		return nil, nil, err
	}
	if repository == nil {
		return nil, nil, domain.NewError(domain.ErrClassConfiguration, "repository_missing",
			"that repository does not exist")
	}
	disk, err := s.store.Disk(ctx, repository.DiskID)
	if err != nil {
		return nil, nil, err
	}
	if disk == nil {
		return nil, nil, domain.NewError(domain.ErrClassConfiguration, "disk_missing",
			"the disk this repository belongs to is no longer registered")
	}
	if domain.NormalizeFilesystemUUID(disk.FilesystemUUID) !=
		domain.NormalizeFilesystemUUID(repository.FilesystemUUID) {
		return nil, nil, domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorIdentityMismatch,
			"the registered disk no longer carries the filesystem UUID this repository was established on")
	}
	return repository, disk, nil
}

// repositorySpec builds the engine-facing description of a repository on a
// currently mounted disk.
func (s *Service) repositorySpec(ctx context.Context, repository domain.Repository,
	lease ports.DiskLease) (ports.RepositorySpec, error) {

	mountPoint, err := lease.MountPoint()
	if err != nil {
		return ports.RepositorySpec{}, err
	}
	directory, err := repositoryDirectory(mountPoint, repository.RelativePath)
	if err != nil {
		return ports.RepositorySpec{}, err
	}
	ref, err := s.store.RepositoryCredentialRef(ctx, repository.ID)
	if err != nil {
		return ports.RepositorySpec{}, err
	}
	if ref.ProviderLocator == "" {
		return ports.RepositorySpec{}, domain.NewError(domain.ErrClassConfiguration,
			"repository_password_missing",
			"this repository has no stored password; it cannot be opened")
	}
	return ports.RepositorySpec{
		Directory:  directory,
		Credential: ports.CredentialHandle{Locator: ref.ProviderLocator},
	}, nil
}

// verifyRepositoryIdentity opens the repository and proves it is the one ROTUP
// recorded.
//
// Three things must agree before anything writes: the registered disk UUID,
// which the mount already verified; the repository path, which is a ROTUP
// constant below that verified mount point; and restic's own repository
// identity, read back from the repository itself. The third is what this adds,
// and it is the one that catches a repository that was replaced, restored from
// elsewhere, or created by something other than this ROTUP installation.
func (s *Service) verifyRepositoryIdentity(ctx context.Context, repository domain.Repository,
	spec ports.RepositorySpec) error {

	if !repository.Established() {
		return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorNotEstablished,
			"this repository has never been proven; initialize or adopt it first")
	}
	info, err := s.engine.InspectRepository(ctx, spec)
	if err != nil {
		return err
	}
	if info.ID != repository.ResticRepositoryID {
		return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorIdentityMismatch,
			"the repository on this disk reports a different identity than the one ROTUP recorded")
	}
	if err := s.store.TouchRepositoryOpened(ctx, repository.ID, s.now()); err != nil {
		s.logger.Warn("the repository open time could not be recorded",
			"repository_id", repository.ID, "error", err)
	}
	return nil
}

// markSuspect records that a repository's state could not be proven, so no
// further write may target it until an explicit recovery says otherwise.
func (s *Service) markSuspect(ctx context.Context, repositoryID, reason string) error {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := s.store.SetRepositoryHealth(ctx, repositoryID, domain.RepositorySuspect, reason); err != nil {
		s.logger.Error("the repository could not be marked suspect",
			"repository_id", repositoryID, "error", err)
		return err
	}
	s.event(ctx, domain.SeverityError, "repository.marked_suspect", eventRefs{
		RepositoryID: repositoryID,
	}, map[string]any{"reason": reason})
	return nil
}

// Failure bookkeeping and safe unmount must survive operation cancellation,
// but must not leave shutdown waiting indefinitely.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// eventRefs are the entities one event points at.
type eventRefs struct {
	RepositoryID string
	DiskID       string
	SourceID     string
	RunID        string
}

func (s *Service) event(ctx context.Context, severity, eventType string, refs eventRefs, payload map[string]any) {
	encoded := "{}"
	if payload != nil {
		if raw, err := json.Marshal(payload); err == nil {
			encoded = string(raw)
		}
	}
	event := domain.Event{
		OccurredAt:   s.now(),
		Severity:     severity,
		EventType:    eventType,
		Component:    component,
		SourceID:     refs.SourceID,
		DiskID:       refs.DiskID,
		RepositoryID: refs.RepositoryID,
		RunID:        refs.RunID,
		PayloadJSON:  encoded,
	}
	if err := s.store.AppendEvent(ctx, event); err != nil {
		s.logger.Error("event could not be persisted", "event_type", eventType, "error", err)
	}
}

func (s *Service) saveRun(ctx context.Context, run domain.Run) {
	if err := s.store.UpdateRun(ctx, run); err != nil {
		s.logger.Error("run state could not be persisted", "run_id", run.ID, "error", err)
	}
}

// finishRunWithError writes a terminal failure onto a run, mapping the error's
// class onto the honest status. Cancellation and timeouts become INTERRUPTED
// rather than FAILED, because "we stopped it" and "it did not work" are
// different facts and recovery treats them differently.
func (s *Service) finishRunWithError(ctx context.Context, run domain.Run, err error, fallback string) domain.Run {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	run.Status = statusForFailure(err)
	run.FinishedAt = s.now()
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		run.ErrorClass = typed.Class
		run.ErrorCode = typed.Code
		run.SafeError = typed.SafeMessage()
	} else {
		run.ErrorClass = domain.ErrClassInternal
		run.ErrorCode = "internal_error"
		run.SafeError = fallback
	}
	run.Summary = run.SafeError
	s.saveRun(ctx, run)
	return run
}

func statusForFailure(err error) domain.RunStatus {
	switch domain.ClassOf(err) {
	case domain.ErrClassCancellation, domain.ErrClassTimeout:
		return domain.RunStatusInterrupted
	default:
		return domain.RunStatusFailed
	}
}

func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		return typed.SafeMessage()
	}
	return "the operation failed for an internal reason"
}

// newRun creates a durable run row in PENDING.
func (s *Service) newRun(ctx context.Context, kind domain.RunKind, trigger domain.RunTrigger,
	summary string) (domain.Run, error) {

	id, err := domain.NewID()
	if err != nil {
		return domain.Run{}, err
	}
	now := s.now()
	run := domain.Run{
		ID:          id,
		Kind:        kind,
		TriggerKind: trigger,
		Status:      domain.RunStatusPending,
		Summary:     summary,
		RequestedAt: now,
		UpdatedAt:   now,
	}
	if err := s.store.RecordRun(ctx, run); err != nil {
		return domain.Run{}, err
	}
	return run, nil
}

// startRun moves a run to RUNNING.
func (s *Service) startRun(ctx context.Context, run domain.Run, summary string) domain.Run {
	run.Status = domain.RunStatusRunning
	run.StartedAt = s.now()
	run.Summary = summary
	s.saveRun(ctx, run)
	return run
}

func encodeSummary(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// backupSummary is the bounded structured summary stored on a backup run. It
// contains counters and identifiers only: never a path from the media, never
// engine output, and never anything derived from a password.
type backupSummary struct {
	RepositoryID    string `json:"repository_id"`
	DiskID          string `json:"disk_id"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	Sources         int    `json:"sources"`
	RevisionBatches int    `json:"revision_batches"`
	FilesProcessed  int64  `json:"files_processed"`
	BytesProcessed  int64  `json:"bytes_processed"`
	BytesAdded      int64  `json:"bytes_added"`
	Durability      string `json:"durability,omitempty"`
}

// describeCounts renders a short human summary of a finished backup.
func describeCounts(snapshotID string, summary backupSummary) string {
	return fmt.Sprintf("snapshot %s: %d files (%d bytes) from %d source(s) and %d revision batch(es)",
		shortID(snapshotID), summary.FilesProcessed, summary.BytesProcessed,
		summary.Sources, summary.RevisionBatches)
}

// shortID renders the conventional eight-character form of a restic identifier.
func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
