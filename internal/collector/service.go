// Package collector owns the manual M2 collection use cases: a read-only
// connection test and a full source collection into staging. It coordinates the
// transfer engine, the staging layout, and durable run history, and it is the
// package that decides what ROTUP is allowed to claim about a source's staging
// state after every outcome.
package collector

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

// Store is the repository seam the collector needs. It is deliberately narrow.
type Store interface {
	Source(ctx context.Context, id string) (*domain.Source, error)
	ListSources(ctx context.Context) ([]domain.Source, error)
	CredentialRef(ctx context.Context, sourceID string) (domain.CredentialRef, error)

	RecordRun(ctx context.Context, run domain.Run) error
	UpdateRun(ctx context.Context, run domain.Run) error
	ActiveRuns(ctx context.Context) ([]domain.Run, error)

	SetSourceStagingStatus(ctx context.Context, sourceID string, status domain.StagingStatus) error
	SaveSourceResult(ctx context.Context, result domain.SourceResult, source *domain.Source) error
	RecordRevisionBatch(ctx context.Context, batch domain.RevisionBatch) error
	AppendEvent(ctx context.Context, event domain.Event) error
}

const component = "collector"

// Service executes manual collection work in the background so that an HTTP
// request never has to stay open and a browser disconnect cannot affect a run.
type Service struct {
	store  Store
	engine ports.TransferEngine
	layout *staging.Layout
	logger *slog.Logger

	// base is the daemon lifetime context. Background work is bound to it, not
	// to the request that started it.
	base   context.Context
	wg     sync.WaitGroup
	locks  *keyedLocks
	now    func() time.Time
	inline bool

	// barrier is the daemon-wide staging mutation barrier. A collection may
	// only reconcile a source's staging tree when no backup is reading it, and
	// a backup may only read a tree no collection is reconciling. The two
	// services share one barrier; two separate ones would each be individually
	// consistent and jointly useless.
	barrier *staging.Barrier
}

// Options configures the service.
type Options struct {
	Store  Store
	Engine ports.TransferEngine
	Layout *staging.Layout
	Logger *slog.Logger
	// Barrier is the shared staging mutation barrier. A nil value builds a
	// private one, which is only correct in a test that has no backup service.
	Barrier *staging.Barrier
	// Now is injectable for deterministic tests.
	Now func() time.Time
	// RunInline executes work synchronously instead of in a goroutine. Tests
	// use it; the daemon does not.
	RunInline bool
}

// New builds the collection service. base bounds every background run.
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
		layout:  options.Layout,
		logger:  logger.With("component", component),
		base:    base,
		locks:   newKeyedLocks(),
		now:     now,
		inline:  options.RunInline,
		barrier: barrier,
	}
}

// Wait blocks until every background run has finished.
func (s *Service) Wait() { s.wg.Wait() }

// Busy reports whether a source currently has work in flight, either its own
// operation or a backup reading its staging tree.
func (s *Service) Busy(sourceID string) bool {
	return s.locks.held(sourceID) || s.barrier.BeingRead(sourceID)
}

// StartConnectionTest creates a durable connection-test run and executes it in
// the background. It returns the run identifier immediately.
func (s *Service) StartConnectionTest(ctx context.Context, sourceID string) (string, error) {
	return s.start(ctx, sourceID, domain.RunKindConnectionTest, s.executeConnectionTest)
}

// StartCollection creates a durable collection run and executes it in the
// background. It returns the run identifier immediately.
func (s *Service) StartCollection(ctx context.Context, sourceID string) (string, error) {
	return s.start(ctx, sourceID, domain.RunKindCollection, s.executeCollection)
}

func (s *Service) start(ctx context.Context, sourceID string, kind domain.RunKind,
	execute func(context.Context, domain.Run, domain.Source, func())) (string, error) {

	source, err := s.store.Source(ctx, sourceID)
	if err != nil {
		return "", err
	}
	if source == nil {
		return "", domain.NewError(domain.ErrClassConfiguration, "source_missing",
			"the source no longer exists")
	}
	if !source.Enabled {
		return "", domain.NewError(domain.ErrClassConfiguration, "source_disabled",
			"the source is disabled; enable it before running this operation")
	}

	// The exclusive per-source lock is taken before the run row exists so that
	// a rejected request never leaves an orphaned PENDING run behind.
	release, ok := s.locks.tryAcquire(sourceID)
	if !ok {
		return "", domain.NewError(domain.ErrClassUnavailable, "source_busy",
			"another operation is already running for this source")
	}

	// A collection reconciles the source's current tree and its revision area,
	// so it additionally takes the staging mutation barrier, which fails when a
	// backup is reading that source. The locks are always taken in this order,
	// and a backup never takes the per-source operation lock, so the two paths
	// cannot deadlock. A connection test mutates nothing and is deliberately
	// left outside the barrier: it must not be able to stall a backup.
	releaseBarrier := func() {}
	if kind == domain.RunKindCollection {
		barrierRelease, barrierErr := s.barrier.AcquireMutation(sourceID)
		if barrierErr != nil {
			release()
			return "", barrierErr
		}
		releaseBarrier = barrierRelease
	}
	releaseAll := func() {
		releaseBarrier()
		release()
	}

	runID, err := domain.NewID()
	if err != nil {
		releaseAll()
		return "", err
	}
	now := s.now()
	run := domain.Run{
		ID:          runID,
		Kind:        kind,
		TriggerKind: domain.RunTriggerManual,
		SourceID:    sourceID,
		Status:      domain.RunStatusPending,
		Summary:     "requested",
		RequestedAt: now,
		UpdatedAt:   now,
	}
	if err := s.store.RecordRun(ctx, run); err != nil {
		releaseAll()
		return "", err
	}

	if s.inline {
		execute(s.base, run, *source, releaseAll)
		return runID, nil
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		execute(s.base, run, *source, releaseAll)
	}()
	return runID, nil
}

// executeConnectionTest performs a read-only probe. It never touches staging.
func (s *Service) executeConnectionTest(ctx context.Context, run domain.Run, source domain.Source, release func()) {
	defer release()

	run.Status = domain.RunStatusRunning
	run.StartedAt = s.now()
	run.Summary = "testing the connection"
	s.saveRun(ctx, run)

	credential, err := s.credentialFor(ctx, source)
	if err != nil {
		s.finishRunWithError(ctx, run, err, "the connection test could not start")
		return
	}

	result, err := s.engine.TestConnection(ctx, specFor(source), credential)
	if err != nil {
		s.finishRunWithError(ctx, run, err, "the connection test failed")
		return
	}

	summary := connectionSummary{
		Entries:    result.Entries,
		DurationMS: result.Duration.Milliseconds(),
	}
	run.Status = domain.RunStatusSucceeded
	run.Summary = describeConnection(source, result)
	run.SummaryJSON = encodeSummary(summary)
	run.FinishedAt = s.now()
	s.saveRun(ctx, run)
}

// executeCollection runs the seven collection phases. Its central rule is that
// nothing may mutate the current tree until connectivity, credentials, staging
// safety, the work area, and the unexpected-empty guard have all passed, and
// that a state ROTUP cannot prove is never reported as a successful update.
func (s *Service) executeCollection(ctx context.Context, run domain.Run, source domain.Source, release func()) {
	defer release()

	started := s.now()
	run.Status = domain.RunStatusRunning
	run.StartedAt = started
	run.Summary = "collecting"
	s.saveRun(ctx, run)

	outcome := s.collect(ctx, run, source, started)
	s.finishCollection(ctx, run, source, outcome)
}

// collectOutcome is everything the collection phases learned. It is assembled
// even for failures so that history records what actually happened.
type collectOutcome struct {
	Status domain.RunStatus
	// Published is true only when the current tree is a complete reconciled
	// view that may be advertised as STABLE.
	Published bool
	// StagingStatus is what the source's durable state must become.
	StagingStatus domain.StagingStatus
	// MutationPossible records whether current may have been changed.
	MutationPossible bool

	Result   domain.SourceResult
	Err      error
	Summary  string
	Warnings []string
	Phase    staging.Phase
	Metrics  collectionSummary
}

func (s *Service) collect(ctx context.Context, run domain.Run, source domain.Source, started time.Time) collectOutcome {
	outcome := collectOutcome{
		Status:        domain.RunStatusFailed,
		StagingStatus: source.StagingStatus,
		Phase:         staging.PhaseFailedBeforeMutation,
		Result: domain.SourceResult{
			RunID:     run.ID,
			SourceID:  source.ID,
			Status:    domain.RunStatusFailed,
			StartedAt: started,
		},
	}

	// Phase 1: resolve and validate. A policy ROTUP cannot execute is refused
	// explicitly rather than being silently treated as another policy.
	if !source.ReconciliationPolicy.Supported() {
		outcome.Err = domain.NewError(domain.ErrClassConfiguration, "policy_unsupported",
			fmt.Sprintf("reconciliation policy %q is not implemented; choose archive_then_sync or copy_only",
				string(source.ReconciliationPolicy)))
		return outcome
	}
	archives := source.ReconciliationPolicy.ArchivesDisplaced()

	// Phase 2: preflight.
	paths, err := s.layout.PreparePaths(source, run.ID, archives)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	marker := staging.RunMarker{
		RunID:      run.ID,
		SourceID:   source.ID,
		Policy:     string(source.ReconciliationPolicy),
		Phase:      staging.PhasePreflight,
		CurrentDir: paths.Current,
		ArchiveDir: paths.Archive,
		StartedAt:  started.Format(time.RFC3339Nano),
	}
	if err := staging.WriteMarker(paths.Work, marker); err != nil {
		outcome.Err = err
		return outcome
	}

	before, err := staging.Measure(paths.Current)
	if err != nil {
		outcome.Err = err
		return outcome
	}

	credential, err := s.credentialFor(ctx, source)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	spec := specFor(source)

	// Connectivity and credentials must be proven before anything is allowed to
	// write into the current tree.
	if _, err := s.engine.TestConnection(ctx, spec, credential); err != nil {
		outcome.Err = err
		return outcome
	}

	// Phase 2 continued: enumerate the source. A size ROTUP cannot read is an
	// error, never an assumed zero.
	size, err := s.engine.Measure(ctx, spec, credential)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	outcome.Result.BytesScanned = size.Bytes
	outcome.Result.FilesScanned = size.Count

	// The unexpected-empty guard. A source that has become unreachable, been
	// unshared, or lost its contents must never be read as authorization to
	// empty the staging tree that still holds the last good copy.
	if size.Count == 0 && before.Files > 0 {
		outcome.Err = domain.NewError(domain.ErrClassIntegrity, "source_unexpectedly_empty",
			fmt.Sprintf("the source reported no files while staging still holds %d; collection was blocked so the staged copy is not destroyed",
				before.Files))
		s.appendEvent(ctx, domain.SeverityWarning, "collection.blocked_empty_source", source.ID, run.ID,
			map[string]any{"staged_files": before.Files})
		return outcome
	}
	if guard := deletionGuardWarning(size.Count, before.Files, archives); guard != "" {
		outcome.Warnings = append(outcome.Warnings, guard)
	}

	// Phase 3 to 5: transfer and reconcile. From here on the current tree may
	// change, so the durable state says so before the engine is started.
	if err := s.store.SetSourceStagingStatus(ctx, source.ID, domain.StagingUpdating); err != nil {
		outcome.Err = err
		return outcome
	}
	marker.Phase = staging.PhaseMutating
	if err := staging.WriteMarker(paths.Work, marker); err != nil {
		outcome.Err = err
		outcome.StagingStatus = domain.StagingRecoveryRequired
		outcome.MutationPossible = true
		outcome.Phase = staging.PhaseFailedAfterMutation
		return outcome
	}

	request := ports.TransferRequest{
		Source:         spec,
		Credential:     credential,
		Mode:           ports.TransferCopy,
		DestinationDir: paths.Current,
		TempDir:        paths.Work,
	}
	if archives {
		request.Mode = ports.TransferSyncWithArchive
		request.ArchiveDir = paths.Archive
	}
	transfer, transferErr := s.engine.Collect(ctx, request)

	outcome.Warnings = append(outcome.Warnings, transfer.Warnings...)
	outcome.Result.BytesCopied = transfer.Stats.Bytes
	outcome.Result.FilesCopied = transfer.Stats.Transfers
	outcome.Result.FilesDeletedOrArchived = displacedObjects(transfer.Stats, archives)
	outcome.Result.EngineChecks = transfer.Stats.Checks
	outcome.Result.EngineErrors = transfer.Stats.Errors
	if transfer.Stats.Available && transfer.Stats.TotalBytes > 0 {
		outcome.Result.BytesScanned = transfer.Stats.TotalBytes
	}

	// An engine that returned no error but whose own counters say the transfer
	// did not complete has not produced a publishable tree. The adapter already
	// refuses this for rclone; repeating the check here keeps the invariant
	// independent of which engine is wired in, and keeps a future adapter's
	// oversight from turning into a false STABLE.
	if transferErr == nil && transfer.Stats.Available {
		if signal := transfer.Stats.FailureSignal(); signal != "" {
			transferErr = domain.NewError(domain.ErrClassExternalTool, "engine_reported_incomplete",
				"the transfer engine reported success but its own counters say the collection did not complete: "+signal)
		}
	}

	// The archive directory is inspected regardless of the engine's outcome,
	// because objects it moved there are exactly the objects no longer in
	// current and must be recorded before anything else is decided.
	if archives {
		if err := s.recordArchive(ctx, source, run, paths.Archive, transferErr == nil); err != nil {
			outcome.Warnings = append(outcome.Warnings,
				"the revision batch could not be recorded: "+safeMessage(err))
		}
	}

	if transferErr != nil {
		outcome.Err = transferErr
		outcome.Status = statusForFailure(transferErr)
		// A failure can only be declared harmless when the engine's own
		// counters prove it changed nothing. Missing statistics, an
		// interrupted process, or any observed mutation all mean the current
		// tree cannot be vouched for.
		provenUntouched := transfer.Stats.Available && !transfer.Stats.MutationObserved()
		if provenUntouched {
			outcome.StagingStatus = source.StagingStatus
			outcome.Phase = staging.PhaseFailedBeforeMutation
			return outcome
		}
		outcome.MutationPossible = true
		outcome.StagingStatus = domain.StagingRecoveryRequired
		outcome.Phase = staging.PhaseFailedAfterMutation
		s.appendEvent(ctx, domain.SeverityError, "collection.recovery_required", source.ID, run.ID,
			map[string]any{"reason": "the collection failed after staging may have been modified"})
		return outcome
	}

	// Phase 6: publish. Only a clean engine exit reaches this point.
	after, err := staging.Measure(paths.Current)
	if err != nil {
		outcome.Err = err
		outcome.MutationPossible = true
		outcome.StagingStatus = domain.StagingRecoveryRequired
		outcome.Phase = staging.PhaseFailedAfterMutation
		return outcome
	}

	published := s.now()
	state := staging.PublishedState{
		SourceID:     source.ID,
		RunID:        run.ID,
		StateVersion: source.StagingStateVersion + 1,
		PublishedAt:  published.Format(time.RFC3339Nano),
		Files:        after.Files,
		Bytes:        after.Bytes,
	}
	if err := staging.WritePublishedState(paths.Metadata, state); err != nil {
		// The bytes are reconciled but the published metadata is not durable,
		// so the state cannot be advertised as stable.
		outcome.Err = err
		outcome.MutationPossible = true
		outcome.StagingStatus = domain.StagingRecoveryRequired
		outcome.Phase = staging.PhaseFailedAfterMutation
		return outcome
	}

	outcome.Published = true
	outcome.StagingStatus = domain.StagingStable
	outcome.Phase = staging.PhasePublished
	outcome.Result.StagingStateVersion = state.StateVersion
	outcome.Result.PublishedAt = published
	// Reaching here means the engine exited cleanly and every counter it
	// reported agrees the transfer completed, so the only thing left that can
	// downgrade the run is advisory: engine warnings about individual objects,
	// or statistics ROTUP could not read at all.
	outcome.Status = domain.RunStatusSucceeded
	if len(outcome.Warnings) > 0 {
		outcome.Status = domain.RunStatusSucceededWithWarning
	}
	if !transfer.Stats.Available {
		// A clean exit is the engine's own statement that it finished, so this
		// still publishes. What cannot be verified is the detail behind it, and
		// the run says so rather than presenting unchecked metrics as fact.
		outcome.Warnings = append(outcome.Warnings,
			"the transfer engine produced no machine-readable statistics, so its completion counters could not be verified and the reported metrics are incomplete")
		outcome.Status = domain.RunStatusSucceededWithWarning
	}
	outcome.Metrics = collectionSummary{
		Policy:              string(source.ReconciliationPolicy),
		FilesCopied:         outcome.Result.FilesCopied,
		BytesCopied:         outcome.Result.BytesCopied,
		FilesArchived:       outcome.Result.FilesDeletedOrArchived,
		EngineChecks:        outcome.Result.EngineChecks,
		EngineErrors:        outcome.Result.EngineErrors,
		CurrentFiles:        after.Files,
		CurrentBytes:        after.Bytes,
		StagingStateVersion: state.StateVersion,
	}
	outcome.Summary = fmt.Sprintf("collected %d files (%d bytes) into staging state version %d",
		outcome.Result.FilesCopied, outcome.Result.BytesCopied, state.StateVersion)

	// Phase 7: record the terminal marker. The work area itself is retained;
	// M2 does not delete work or revision data.
	marker.Phase = staging.PhasePublished
	if err := staging.WriteMarker(paths.Work, marker); err != nil {
		outcome.Warnings = append(outcome.Warnings, "the run marker could not be finalized: "+safeMessage(err))
	}
	return outcome
}

// finishCollection writes the terminal marker, the per-source result, and the
// run row. The source's durable staging state is advanced in the same
// transaction as the result so the two can never disagree.
func (s *Service) finishCollection(ctx context.Context, run domain.Run, source domain.Source, outcome collectOutcome) {
	finished := s.now()
	outcome.Result.FinishedAt = finished
	outcome.Result.Status = outcome.Status
	outcome.Result.Warnings = outcome.Warnings

	if outcome.Phase != staging.PhasePublished {
		if workDir, err := s.layout.WorkDir(source.ID, run.ID); err == nil {
			if marker, ok, readErr := staging.ReadMarker(workDir); readErr == nil && ok {
				marker.Phase = outcome.Phase
				marker.Note = safeMessage(outcome.Err)
				_ = staging.WriteMarker(workDir, marker)
			}
		}
	}

	updated := source
	updated.StagingStatus = outcome.StagingStatus
	updated.LastCollectionRunID = run.ID
	if outcome.Published {
		updated.StagingStateVersion = outcome.Result.StagingStateVersion
		published := outcome.Result.PublishedAt
		updated.LastPublishedAt = &published
	}
	if err := s.store.SaveSourceResult(ctx, outcome.Result, &updated); err != nil {
		s.logger.Error("collection result could not be persisted", "run_id", run.ID, "error", err)
	}

	run.Status = outcome.Status
	run.FinishedAt = finished
	run.SummaryJSON = encodeSummary(outcome.Metrics)
	if outcome.Err != nil {
		var typed *domain.Error
		if domain.AsError(outcome.Err, &typed) {
			run.ErrorClass = typed.Class
			run.ErrorCode = typed.Code
			run.SafeError = typed.SafeMessage()
		} else {
			run.ErrorClass = domain.ErrClassInternal
			run.ErrorCode = "internal_error"
			run.SafeError = "the collection failed for an internal reason"
		}
		run.Summary = run.SafeError
		if outcome.MutationPossible {
			run.Summary += " — staging needs recovery and is not reported as up to date"
		}
	} else {
		run.Summary = outcome.Summary
	}
	s.saveRun(ctx, run)

	s.logger.Info("collection finished",
		"run_id", run.ID,
		"source_id", source.ID,
		"status", string(outcome.Status),
		"published", outcome.Published,
		"staging_status", string(outcome.StagingStatus))
}

// recordArchive measures the run's archive directory and, when it holds
// anything, records a revision batch. completed marks whether the run that
// produced it actually finished reconciling, so a partial archive can never be
// presented as a finished batch.
func (s *Service) recordArchive(ctx context.Context, source domain.Source, run domain.Run,
	archiveDir string, completed bool) error {

	stats, err := staging.Measure(archiveDir)
	if err != nil {
		return err
	}
	if stats.Files == 0 {
		return nil
	}
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	batch := domain.RevisionBatch{
		ID:                      id,
		SourceID:                source.ID,
		CollectionRunID:         run.ID,
		RelativePath:            staging.RevisionRelativePath(source.ID, run.ID),
		CreatedAt:               s.now(),
		ItemCount:               stats.Files,
		ByteCount:               stats.Bytes,
		ReconciliationCompleted: completed,
	}
	return s.store.RecordRevisionBatch(ctx, batch)
}

func (s *Service) credentialFor(ctx context.Context, source domain.Source) (ports.CredentialHandle, error) {
	if source.CredentialRefID == "" {
		return ports.CredentialHandle{}, nil
	}
	ref, err := s.store.CredentialRef(ctx, source.ID)
	if err != nil {
		return ports.CredentialHandle{}, err
	}
	if ref.ProviderLocator == "" {
		return ports.CredentialHandle{}, domain.NewError(domain.ErrClassConfiguration, "credential_missing",
			"the source references a credential that no longer exists; replace it")
	}
	return ports.CredentialHandle{Locator: ref.ProviderLocator}, nil
}

func (s *Service) saveRun(ctx context.Context, run domain.Run) {
	if err := s.store.UpdateRun(ctx, run); err != nil {
		s.logger.Error("run state could not be persisted", "run_id", run.ID, "error", err)
	}
}

func (s *Service) finishRunWithError(ctx context.Context, run domain.Run, err error, fallback string) {
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
}

func (s *Service) appendEvent(ctx context.Context, severity, eventType, sourceID, runID string, payload map[string]any) {
	encoded := "{}"
	if payload != nil {
		if raw, err := json.Marshal(payload); err == nil {
			encoded = string(raw)
		}
	}
	event := domain.Event{
		OccurredAt:  s.now(),
		Severity:    severity,
		EventType:   eventType,
		Component:   component,
		SourceID:    sourceID,
		RunID:       runID,
		PayloadJSON: encoded,
	}
	if err := s.store.AppendEvent(ctx, event); err != nil {
		s.logger.Error("event could not be persisted", "event_type", eventType, "error", err)
	}
}

func specFor(source domain.Source) ports.SourceSpec {
	return ports.SourceSpec{
		Host:         source.Host,
		Share:        source.ShareName,
		Subdirectory: source.Subdirectory,
		Username:     source.Username,
		IncludeRules: source.IncludeRules,
		ExcludeRules: source.ExcludeRules,
	}
}

func statusForFailure(err error) domain.RunStatus {
	switch domain.ClassOf(err) {
	case domain.ErrClassCancellation, domain.ErrClassTimeout:
		return domain.RunStatusInterrupted
	default:
		return domain.RunStatusFailed
	}
}

// displacedObjects reports how many objects left the current tree during one
// transfer, which is what the run's history and the operator-facing
// "deleted or archived" figure mean.
//
// An archiving policy deletes nothing: every replaced or vanished object is
// *moved* into the run's revision batch. The engine counts each of those moves
// as a rename, and counts a delete only for the subset that disappeared from
// the source — a file replaced by a newer version is renamed into the archive
// without ever being counted as a delete. Reading the delete counter alone
// therefore under-reports the batch, so a run that archived an old version and
// a removed file would claim one displaced object while its revision batch
// durably recorded two. Renames is the count that matches the batch.
//
// A non-archiving policy never renames, so its delete counter is the answer.
func displacedObjects(stats ports.TransferStats, archives bool) int64 {
	if archives {
		return stats.Renames
	}
	return stats.Deletes
}

// deletionGuardWarning warns when a source has lost a large share of its files
// but is not empty. The data is still preserved for an archiving policy, so
// this is advisory rather than blocking; a configurable threshold policy is a
// later milestone.
func deletionGuardWarning(sourceFiles, stagedFiles int64, archives bool) string {
	const minimumStagedFiles = 10
	if stagedFiles < minimumStagedFiles || sourceFiles*2 >= stagedFiles {
		return ""
	}
	if archives {
		return fmt.Sprintf("the source now has %d files while staging held %d; the displaced files were archived into this run's revision batch, not deleted",
			sourceFiles, stagedFiles)
	}
	return fmt.Sprintf("the source now has %d files while staging held %d", sourceFiles, stagedFiles)
}

func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		return typed.SafeMessage()
	}
	return "an internal error occurred"
}

// connectionSummary and collectionSummary are the bounded structured summaries
// stored on the run. They never contain paths beyond counts or any secret.
type connectionSummary struct {
	Entries    int   `json:"entries"`
	DurationMS int64 `json:"duration_ms"`
}

type collectionSummary struct {
	Policy              string `json:"reconciliation_policy,omitempty"`
	FilesCopied         int64  `json:"files_copied"`
	BytesCopied         int64  `json:"bytes_copied"`
	FilesArchived       int64  `json:"files_archived"`
	EngineChecks        int64  `json:"engine_checks"`
	EngineErrors        int64  `json:"engine_errors"`
	CurrentFiles        int64  `json:"current_files"`
	CurrentBytes        int64  `json:"current_bytes"`
	StagingStateVersion int64  `json:"staging_state_version"`
}

func encodeSummary(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func describeConnection(source domain.Source, result ports.ConnectionResult) string {
	target := source.RemotePath()
	if result.Entries < 0 {
		return fmt.Sprintf("%s on %s is reachable and readable", target, source.Host)
	}
	return fmt.Sprintf("%s on %s is reachable and readable (%d entries at the top level)",
		target, source.Host, result.Entries)
}

// keyedLocks is the minimal in-memory exclusion the architecture calls for at
// this milestone: one operation per source at a time. It is not the general
// resource coordinator, which arrives with the scheduler.
type keyedLocks struct {
	mu   sync.Mutex
	busy map[string]struct{}
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{busy: make(map[string]struct{})}
}

func (k *keyedLocks) tryAcquire(key string) (func(), bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, taken := k.busy[key]; taken {
		return nil, false
	}
	k.busy[key] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			delete(k.busy, key)
			k.mu.Unlock()
		})
	}, true
}

func (k *keyedLocks) held(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, taken := k.busy[key]
	return taken
}
