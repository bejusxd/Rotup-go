package web

import (
	"context"
	"net/http"
	"time"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

// Backups is the repository and backup use-case seam. Every mutating method is
// an explicit administrator action: nothing here initializes a repository,
// starts a backup, or recovers anything on its own.
type Backups interface {
	ListRepositories(ctx context.Context) ([]backup.RepositoryView, error)
	RepositoryDetail(ctx context.Context, repositoryID string) (backup.RepositoryView, error)
	BackupHistory(ctx context.Context, repositoryID string, limit int) ([]backup.BackupHistoryEntry, error)
	EligibleSources(ctx context.Context) ([]backup.EligibleSource, error)
	StartRepositorySetup(ctx context.Context, input backup.SetupInput) (string, error)
	StartBackup(ctx context.Context, repositoryID string, sourceIDs []string) (string, error)
	StartRepositoryRecovery(ctx context.Context, repositoryID string) (string, error)
}

// repositoriesView is the repository list page model.
type repositoriesView struct {
	Repositories []backup.RepositoryView
	// Candidates are registered disks that do not have a repository yet, so the
	// page can offer establishing one where it is actually possible.
	Candidates []removable.MediaView
	Engine     EngineStatus
	Events     []domain.Event
	Refresh    bool
}

// NeedsRefresh keeps the page current while a repository operation runs.
func (v repositoriesView) NeedsRefresh() bool { return v.Refresh }

// repositoryView is the single repository page model.
type repositoryView struct {
	Repository backup.RepositoryView
	Sources    []backup.EligibleSource
	History    []backup.BackupHistoryEntry
	Engine     EngineStatus
	Events     []domain.Event
	Refresh    bool
	// AnyEligible reports whether the backup form has anything to offer.
	AnyEligible bool
	// MinimumPasswordLength is shown next to the password field so the rule is
	// stated where it applies rather than only in an error.
	MinimumPasswordLength int
}

// NeedsRefresh keeps the page current while a repository operation runs.
func (v repositoryView) NeedsRefresh() bool { return v.Refresh }

func (h *handler) listRepositories(response http.ResponseWriter, request *http.Request) {
	h.renderRepositories(response, request, http.StatusOK, "", "")
}

func (h *handler) renderRepositories(response http.ResponseWriter, request *http.Request,
	status int, notice, failure string) {

	ctx := request.Context()
	view := repositoriesView{}
	if h.deps.BackupEngine != nil {
		view.Engine = h.deps.BackupEngine()
	}

	repositories, err := h.deps.Backups.ListRepositories(ctx)
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "repositories",
			view, "", safeError(err))
		return
	}
	view.Repositories = repositories
	for _, item := range repositories {
		if item.Busy {
			view.Refresh = true
		}
	}

	// Registered disks without a repository are the only place establishing one
	// is offered. Unregistered media never appears here: a repository is only
	// ever created on a disk an administrator deliberately registered.
	if h.deps.Media != nil {
		if snapshot, err := h.deps.Media.Snapshot(ctx); err == nil {
			claimed := make(map[string]bool, len(repositories))
			for _, item := range repositories {
				claimed[item.Repository.DiskID] = item.Repository.Established()
			}
			for _, media := range snapshot.Views {
				if media.Registered() && !claimed[media.Disk.ID] {
					view.Candidates = append(view.Candidates, media)
				}
			}
		}
	}
	view.Events = h.repositoryEvents(ctx, 25)

	h.renderWithMessages(response, request, status, "repositories", view, notice, failure)
}

func (h *handler) showRepository(response http.ResponseWriter, request *http.Request) {
	h.renderRepository(response, request, request.PathValue("id"), http.StatusOK, "", "")
}

func (h *handler) renderRepository(response http.ResponseWriter, request *http.Request,
	repositoryID string, status int, notice, failure string) {

	ctx := request.Context()
	detail, err := h.deps.Backups.RepositoryDetail(ctx, repositoryID)
	if err != nil {
		h.renderWithMessages(response, request, http.StatusNotFound, "repositories",
			repositoriesView{}, "", safeError(err))
		return
	}

	view := repositoryView{
		Repository:            detail,
		Refresh:               detail.Busy,
		MinimumPasswordLength: domain.MinimumRepositoryPasswordLength,
	}
	if h.deps.BackupEngine != nil {
		view.Engine = h.deps.BackupEngine()
	}
	if sources, err := h.deps.Backups.EligibleSources(ctx); err == nil {
		view.Sources = sources
		for _, source := range sources {
			if source.Eligible {
				view.AnyEligible = true
			}
		}
	}
	if history, err := h.deps.Backups.BackupHistory(ctx, repositoryID, 25); err == nil {
		view.History = history
		for _, entry := range history {
			if !entry.Run.Status.Terminal() {
				view.Refresh = true
			}
		}
	}
	view.Events = h.repositoryEvents(ctx, 25)

	h.renderWithMessages(response, request, status, "repository", view, notice, failure)
}

// repositoryEvents returns the recent events the repository pages show. It
// filters on the component rather than reading a second table, so the audit
// trail shown here is the same append-only log everything else uses.
func (h *handler) repositoryEvents(ctx context.Context, limit int) []domain.Event {
	if h.deps.Repo == nil {
		return nil
	}
	events, err := h.deps.Repo.ListEvents(ctx, 100)
	if err != nil {
		return nil
	}
	var result []domain.Event
	for _, event := range events {
		if event.Component != "backup" {
			continue
		}
		result = append(result, event)
		if len(result) >= limit {
			break
		}
	}
	return result
}

func (h *handler) initializeRepository(response http.ResponseWriter, request *http.Request) {
	h.repositorySetup(response, request, backup.SetupInitialize)
}

func (h *handler) adoptRepository(response http.ResponseWriter, request *http.Request) {
	h.repositorySetup(response, request, backup.SetupAdopt)
}

// repositorySetup handles both establishment modes. The password is read from
// the form, passed straight into the use case, and never stored in a view
// model, a log line, or a redirect.
func (h *handler) repositorySetup(response http.ResponseWriter, request *http.Request, mode backup.SetupMode) {
	if failure := h.checkMutation(request); failure != "" {
		h.renderRepositories(response, request, http.StatusBadRequest, "", failure)
		return
	}
	input := backup.SetupInput{
		DiskID:             request.PathValue("id"),
		Mode:               mode,
		Password:           request.PostFormValue("repository_password"),
		EscrowAcknowledged: request.PostFormValue("escrow_acknowledged") != "",
	}
	runID, err := h.deps.Backups.StartRepositorySetup(h.BackgroundContext(), input)
	if err != nil {
		h.renderRepositories(response, request, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/runs/"+runID, http.StatusSeeOther)
}

func (h *handler) startBackup(response http.ResponseWriter, request *http.Request) {
	repositoryID := request.PathValue("id")
	if failure := h.checkMutation(request); failure != "" {
		h.renderRepository(response, request, repositoryID, http.StatusBadRequest, "", failure)
		return
	}
	sourceIDs := request.PostForm["source_id"]
	runID, err := h.deps.Backups.StartBackup(h.BackgroundContext(), repositoryID, sourceIDs)
	if err != nil {
		h.renderRepository(response, request, repositoryID, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/runs/"+runID, http.StatusSeeOther)
}

func (h *handler) recoverRepository(response http.ResponseWriter, request *http.Request) {
	repositoryID := request.PathValue("id")
	if failure := h.checkMutation(request); failure != "" {
		h.renderRepository(response, request, repositoryID, http.StatusBadRequest, "", failure)
		return
	}
	runID, err := h.deps.Backups.StartRepositoryRecovery(h.BackgroundContext(), repositoryID)
	if err != nil {
		h.renderRepository(response, request, repositoryID, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/runs/"+runID, http.StatusSeeOther)
}

// repositoryAge renders how long ago a repository was last backed up.
func repositoryAge(at *time.Time, now time.Time) time.Duration {
	if at == nil || at.IsZero() {
		return 0
	}
	return now.Sub(*at)
}
