package web

import (
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
	// Refresh is set while the run is not terminal so the page reloads itself
	// without any client-side script.
	Refresh bool
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

// NeedsRefresh makes the run page poll itself while the run is still executing.
func (v runView) NeedsRefresh() bool { return v.Refresh }
