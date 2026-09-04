package web

import (
	"context"
	"net/http"

	"github.com/bejusxd/rotup/internal/domain"
)

type runsView struct {
	Runs []domain.Run
	// Events are the durable administrator-facing facts behind the runs:
	// credential changes, blocked collections, and recovery decisions. They are
	// shown here so the audit trail is actually readable.
	Events []domain.Event
}

type runView struct {
	Run    domain.Run
	Source *domain.Source
	Result *domain.SourceResult
	Batch  *domain.RevisionBatch
	// Backup is the durable backup detail when this run is a BACKUP_LATEST. It
	// carries the exact read-set the run captured, which is what makes the page
	// able to say what was actually snapshotted rather than what the sources
	// look like now.
	Backup       *domain.BackupRun
	Repository   *domain.Repository
	SourceStates []backupSourceStateRow
	Batches      []domain.BackupRevisionBatch
	// Refresh is set while the run is not terminal so the page reloads itself
	// without any client-side script.
	Refresh bool
}

// backupSourceStateRow pairs an audit row with the source name, so the page can
// show a readable name without pretending the name is part of the audit.
type backupSourceStateRow struct {
	State domain.BackupSourceState
	Name  string
}

func (h *handler) listRuns(response http.ResponseWriter, request *http.Request) {
	runs, err := h.deps.Repo.ListRuns(request.Context(), 100)
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "runs", runsView{}, "", safeError(err))
		return
	}
	view := runsView{Runs: runs}
	if events, err := h.deps.Repo.ListEvents(request.Context(), 50); err == nil {
		view.Events = events
	}
	h.render(response, request, http.StatusOK, "runs", view)
}

func (h *handler) showRun(response http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	run, err := h.deps.Repo.Run(ctx, request.PathValue("id"))
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "runs", runsView{}, "", safeError(err))
		return
	}
	if run == nil {
		http.NotFound(response, request)
		return
	}

	view := runView{Run: *run, Refresh: !run.Status.Terminal()}
	if h.deps.BackupRepo != nil && run.Kind == domain.RunKindBackupLatest {
		h.loadBackupDetail(ctx, &view)
	}
	if run.SourceID != "" {
		if source, err := h.deps.Repo.Source(ctx, run.SourceID); err == nil {
			view.Source = source
		}
		if run.Kind == domain.RunKindCollection {
			if result, err := h.deps.Repo.SourceResult(ctx, run.ID, run.SourceID); err == nil {
				view.Result = result
			}
			if batches, err := h.deps.Repo.ListRevisionBatches(ctx, run.SourceID, 50); err == nil {
				for i := range batches {
					if batches[i].CollectionRunID == run.ID {
						view.Batch = &batches[i]
						break
					}
				}
			}
		}
	}
	h.render(response, request, http.StatusOK, "run", view)
}

// loadBackupDetail fills in the backup half of a run page. Every value comes
// from the rows the run itself wrote before restic started; nothing is
// recomputed from current configuration, because by now the staging tree and
// the source list have moved on.
func (h *handler) loadBackupDetail(ctx context.Context, view *runView) {
	detail, err := h.deps.BackupRepo.BackupRun(ctx, view.Run.ID)
	if err != nil || detail == nil {
		return
	}
	view.Backup = detail

	if repository, err := h.deps.BackupRepo.Repository(ctx, detail.RepositoryID); err == nil {
		view.Repository = repository
	}
	if states, err := h.deps.BackupRepo.BackupSourceStates(ctx, view.Run.ID); err == nil {
		for _, state := range states {
			row := backupSourceStateRow{State: state}
			if h.deps.Repo != nil {
				if source, err := h.deps.Repo.Source(ctx, state.SourceID); err == nil && source != nil {
					row.Name = source.Name
				}
			}
			view.SourceStates = append(view.SourceStates, row)
		}
	}
	if batches, err := h.deps.BackupRepo.BackupRevisionBatches(ctx, view.Run.ID); err == nil {
		view.Batches = batches
	}
}

// NeedsRefresh makes the run page poll itself while the run is still executing.
func (v runView) NeedsRefresh() bool { return v.Refresh }
