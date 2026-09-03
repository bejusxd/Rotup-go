package web

import (
	"context"
	"net/http"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

// Media is the removable-media use-case seam. Every mutating method is an
// explicit administrator action; nothing here mounts or registers on its own.
type Media interface {
	Snapshot(ctx context.Context) (removable.Snapshot, error)
	Register(ctx context.Context, filesystemUUID, name string) (string, error)
	UpdateSettings(ctx context.Context, diskID, name string, enabled bool) error
	Mount(ctx context.Context, diskID string) error
	SyncAndUnmount(ctx context.Context, diskID string) error
	PlatformAvailable() bool
	Busy(diskID string) bool
}

// disksView is the disks page model.
type disksView struct {
	Snapshot removable.Snapshot
	Events   []domain.Event
	// Refresh keeps the page current while any disk has an operation running.
	Refresh bool
}

// NeedsRefresh makes the page poll itself while a media operation runs.
func (v disksView) NeedsRefresh() bool { return v.Refresh }

// BackgroundContext returns the daemon-lifetime context, falling back to a
// background context when none was wired in.
func (h *handler) BackgroundContext() context.Context {
	if h.deps.Background != nil {
		return h.deps.Background()
	}
	return context.Background()
}

func (h *handler) listDisks(response http.ResponseWriter, request *http.Request) {
	h.renderDisks(response, request, http.StatusOK, "", "")
}

func (h *handler) renderDisks(response http.ResponseWriter, request *http.Request,
	status int, notice, failure string) {

	snapshot, err := h.deps.Media.Snapshot(request.Context())
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "disks",
			disksView{}, "", safeError(err))
		return
	}
	view := disksView{Snapshot: snapshot}
	for _, item := range snapshot.Views {
		if item.Busy {
			view.Refresh = true
		}
	}
	if events, err := h.deps.Repo.ListEvents(request.Context(), 25); err == nil {
		for _, event := range events {
			if event.Component == "removable" {
				view.Events = append(view.Events, event)
			}
		}
	}
	h.renderWithMessages(response, request, status, "disks", view, notice, failure)
}

func (h *handler) registerDisk(response http.ResponseWriter, request *http.Request) {
	if failure := h.checkMutation(request); failure != "" {
		h.renderDisks(response, request, http.StatusBadRequest, "", failure)
		return
	}
	filesystemUUID := request.PostFormValue("filesystem_uuid")
	name := request.PostFormValue("name")

	if _, err := h.deps.Media.Register(request.Context(), filesystemUUID, name); err != nil {
		h.renderDisks(response, request, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/disks", http.StatusSeeOther)
}

func (h *handler) updateDisk(response http.ResponseWriter, request *http.Request) {
	if failure := h.checkMutation(request); failure != "" {
		h.renderDisks(response, request, http.StatusBadRequest, "", failure)
		return
	}
	diskID := request.PathValue("id")
	name := request.PostFormValue("name")
	enabled := request.PostFormValue("enabled") != ""

	if err := h.deps.Media.UpdateSettings(request.Context(), diskID, name, enabled); err != nil {
		h.renderDisks(response, request, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/disks", http.StatusSeeOther)
}

func (h *handler) mountDisk(response http.ResponseWriter, request *http.Request) {
	h.mediaOperation(response, request, h.deps.Media.Mount)
}

func (h *handler) unmountDisk(response http.ResponseWriter, request *http.Request) {
	h.mediaOperation(response, request, h.deps.Media.SyncAndUnmount)
}

// mediaOperation runs one explicit media action.
//
// It deliberately uses the daemon's background context rather than the
// request's: a browser that navigates away or disconnects must not cancel a
// mount, a flush, or an unmount partway through and leave the media in a state
// nobody recorded.
func (h *handler) mediaOperation(response http.ResponseWriter, request *http.Request,
	operate func(context.Context, string) error) {

	if failure := h.checkMutation(request); failure != "" {
		h.renderDisks(response, request, http.StatusBadRequest, "", failure)
		return
	}
	diskID := request.PathValue("id")
	if err := operate(h.BackgroundContext(), diskID); err != nil {
		h.renderDisks(response, request, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/disks", http.StatusSeeOther)
}
