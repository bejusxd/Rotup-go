package collector

import (
	"context"

	"github.com/bejusxd/rotup/internal/domain"
)

// RecoveryReport summarizes what startup recovery changed.
type RecoveryReport struct {
	InterruptedRuns  int
	RecoveredSources int
}

// Recover runs the M2 crash-safety boundary before the daemon accepts new work.
//
// It is deliberately conservative rather than clever. Every run a previous
// process left active becomes INTERRUPTED, and any source still claiming
// UPDATING becomes RECOVERY_REQUIRED, because a process that died mid
// reconciliation cannot have proven that the current tree is coherent. Full
// work-area resumption and quarantine belong to the later recovery milestone;
// what matters here is that ROTUP never reports an unproven state as stable.
func (s *Service) Recover(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport

	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		return report, err
	}
	for _, run := range active {
		if run.Kind.TouchesRepository() {
			// Repository work has its own recovery, which needs the run's
			// durability stage to decide what the media may be holding. It runs
			// before this one and owns those rows.
			continue
		}
		run.Status = domain.RunStatusInterrupted
		run.FinishedAt = s.now()
		run.ErrorClass = domain.ErrClassCancellation
		run.ErrorCode = "daemon_restarted"
		run.SafeError = "the daemon stopped while this run was active"
		run.Summary = run.SafeError
		if err := s.store.UpdateRun(ctx, run); err != nil {
			return report, err
		}
		report.InterruptedRuns++
		s.appendEvent(ctx, domain.SeverityWarning, "run.interrupted_by_restart", run.SourceID, run.ID, nil)
	}

	sources, err := s.store.ListSources(ctx)
	if err != nil {
		return report, err
	}
	for _, source := range sources {
		switch source.StagingStatus {
		case domain.StagingUpdating:
			if err := s.store.SetSourceStagingStatus(ctx, source.ID, domain.StagingRecoveryRequired); err != nil {
				return report, err
			}
			report.RecoveredSources++
			s.appendEvent(ctx, domain.SeverityError, "staging.recovery_required", source.ID, "",
				map[string]any{"reason": "the daemon stopped while reconciliation could be mutating the current tree"})
			s.logger.Warn("source left in an unproven staging state was marked for recovery",
				"source_id", source.ID, "previous_status", string(source.StagingStatus))
		case domain.StagingStable:
			// A stable source whose work areas show an interrupted mutating run
			// cannot actually be trusted: the database may simply not have been
			// updated before the process died.
			if s.workSuggestsMutation(source.ID) {
				if err := s.store.SetSourceStagingStatus(ctx, source.ID, domain.StagingRecoveryRequired); err != nil {
					return report, err
				}
				report.RecoveredSources++
				s.appendEvent(ctx, domain.SeverityError, "staging.recovery_required", source.ID, "",
					map[string]any{"reason": "an interrupted work area records that the current tree may have been modified"})
			}
		}
	}
	return report, nil
}

// workSuggestsMutation reports whether any of the source's work areas record a
// run that had entered, or may have entered, the mutating phase. The staging
// package owns that judgement; an unreadable work directory is itself treated
// as a reason to be cautious.
func (s *Service) workSuggestsMutation(sourceID string) bool {
	areas, err := s.layout.ScanWork(sourceID)
	if err != nil {
		s.logger.Warn("source work areas could not be inspected", "source_id", sourceID, "error", err)
		return true
	}
	for _, area := range areas {
		if area.MutationPossible() {
			return true
		}
	}
	return false
}
