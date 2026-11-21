package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/coordinator"
	"github.com/bejusxd/rotup/internal/domain"
)

// PrepareJob acquires the existing resources without creating another run.
// The caller must durably claim this intent before invoking work and must call
// release even when that claim fails. Work is synchronous and creates no worker.
func (s *Service) PrepareJob(ctx context.Context, run domain.Run, sourceIDs []string) (work func(context.Context) error, release func(), err error) {
	if run.Kind != domain.RunKindCollection || len(sourceIDs) == 0 {
		return nil, nil, domain.NewError(domain.ErrClassConfiguration, "job_sources_empty", "a collection job needs selected sources")
	}
	ids := append([]string(nil), sourceIDs...)
	sort.Strings(ids)
	c := coordinator.New(s.barrier, s, nil)
	var releases []func()
	var once sync.Once
	release = func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}
	var sources []domain.Source
	for i, id := range ids {
		if i > 0 && id == ids[i-1] {
			continue
		}
		r, acquireErr := c.TryCollection(ctx, id)
		if acquireErr != nil {
			release()
			return nil, nil, acquireErr
		}
		releases = append(releases, r)
		source, readErr := s.store.Source(ctx, id)
		if readErr != nil {
			release()
			return nil, nil, readErr
		}
		if source == nil || !source.Enabled {
			release()
			return nil, nil, domain.NewError(domain.ErrClassConfiguration, "source_disabled_or_missing", "a selected source is disabled or missing")
		}
		if source.StagingStatus == domain.StagingRecoveryRequired || source.StagingStatus == domain.StagingUpdating {
			release()
			return nil, nil, domain.NewError(domain.ErrClassIntegrity, "staging_recovery_required", "a selected source needs recovery before automatic collection")
		}
		sources = append(sources, *source)
	}
	work = func(ctx context.Context) error {
		run.ErrorClass, run.ErrorCode, run.SafeError = "", "", ""
		run.FinishedAt = time.Time{}
		run.Status, run.StartedAt, run.Summary = domain.RunStatusRunning, s.now(), "collecting job sources"
		if err := s.saveRun(ctx, run); err != nil {
			return err
		}
		successes, warnings := 0, false
		var firstErr error
		for _, source := range sources {
			if err := ctx.Err(); err != nil {
				firstErr = err
				break
			}
			outcome := s.collect(ctx, run, source, s.now())
			// Shutdown/timeout may cancel transfer, but final accounting must
			// still run with a bounded cleanup context before releasing staging.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			outcome = s.persistCollectionResult(cleanup, run, source, outcome)
			cancel()
			if outcome.Status.Successful() {
				successes++
			}
			warnings = warnings || outcome.Status == domain.RunStatusSucceededWithWarning
			if outcome.Err != nil && firstErr == nil {
				firstErr = outcome.Err
			}
		}
		run.Status = domain.RunStatusSucceeded
		if warnings {
			run.Status = domain.RunStatusSucceededWithWarning
		}
		if successes != len(sources) {
			run.Status = domain.RunStatusFailed
			if successes > 0 {
				run.Status = domain.RunStatusPartial
			}
		}
		run.Summary = fmt.Sprintf("collected %d of %d selected sources", successes, len(sources))
		run.FinishedAt = s.now()
		if firstErr != nil {
			run.ErrorClass, run.ErrorCode, run.SafeError = domain.ErrClassInternal, "collection_failed", "one or more sources could not be collected"
			var typed *domain.Error
			if domain.AsError(firstErr, &typed) {
				run.ErrorClass, run.ErrorCode, run.SafeError = typed.Class, typed.Code, typed.SafeMessage()
			}
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return s.saveRun(cleanup, run)
	}
	return work, release, nil
}
