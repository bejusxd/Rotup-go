package web

import (
	"context"
	"net/http"
	"time"

	"github.com/bejusxd/rotup/internal/orchestrator"
)

// Jobs is the job configuration seam. Runs it queues are executed by the
// daemon's dispatcher, never inside the HTTP request.
type Jobs interface {
	Create(ctx context.Context, input orchestrator.JobInput) (string, error)
	List(ctx context.Context) ([]orchestrator.JobDefinition, error)
	Enable(ctx context.Context, id string, enabled bool) error
	Run(ctx context.Context, id string) (string, error)
}

type scheduleDTO struct {
	ID               string  `json:"id"`
	Enabled          bool    `json:"enabled"`
	DaysOfWeekMask   uint8   `json:"daysOfWeekMask"`
	LocalTime        string  `json:"localTime"`
	Timezone         string  `json:"timezone"`
	LastNominalRunAt string  `json:"lastNominalRunAt,omitempty"`
	NextRunAt        *string `json:"nextRunAt"`
}

type jobDTO struct {
	ID                    string        `json:"id"`
	Name                  string        `json:"name"`
	Kind                  string        `json:"kind"`
	Enabled               bool          `json:"enabled"`
	SourceIDs             []string      `json:"sourceIds"`
	BackupAfterCollection bool          `json:"backupAfterCollection"`
	PartialTriggersBackup bool          `json:"partialTriggersBackup"`
	TimeoutSeconds        int64         `json:"timeoutSeconds"`
	RetryIntervalSeconds  int64         `json:"retryIntervalSeconds"`
	RetryDeadlineSeconds  *int64        `json:"retryDeadlineSeconds"`
	Schedules             []scheduleDTO `json:"schedules"`
}

func toJobDTO(d orchestrator.JobDefinition) jobDTO {
	out := jobDTO{ID: d.Job.ID, Name: d.Job.Name, Kind: string(d.Job.Kind), Enabled: d.Job.Enabled,
		SourceIDs: d.SourceIDs, BackupAfterCollection: d.Job.BackupAfterCollection,
		PartialTriggersBackup: d.Job.PartialTriggersBackup, TimeoutSeconds: d.Job.TimeoutSeconds,
		RetryIntervalSeconds: d.Job.RetryIntervalSeconds, RetryDeadlineSeconds: d.Job.RetryDeadlineSeconds,
		Schedules: []scheduleDTO{}}
	if out.SourceIDs == nil {
		out.SourceIDs = []string{}
	}
	for _, s := range d.Schedules {
		item := scheduleDTO{ID: s.ID, Enabled: s.Enabled, DaysOfWeekMask: s.DaysOfWeekMask,
			LocalTime: s.LocalTime, Timezone: s.Timezone, LastNominalRunAt: s.LastNominalRunAt}
		if s.NextRunAt != nil {
			v := s.NextRunAt.UTC().Format(time.RFC3339)
			item.NextRunAt = &v
		}
		out.Schedules = append(out.Schedules, item)
	}
	return out
}

func (h *handler) apiJobs(w http.ResponseWriter, r *http.Request) {
	defs, err := h.deps.Jobs.List(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	items := make([]jobDTO, 0, len(defs))
	for _, d := range defs {
		items = append(items, toJobDTO(d))
	}
	h.apiJSON(w, 200, struct {
		Items []jobDTO `json:"items"`
	}{items})
}

func (h *handler) apiCreateJob(w http.ResponseWriter, r *http.Request) {
	var body orchestrator.JobInput
	if !h.apiBody(w, r, &body) {
		return
	}
	id, err := h.deps.Jobs.Create(r.Context(), body)
	if err != nil {
		h.apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+id)
	h.apiJSON(w, 201, struct {
		ID string `json:"id"`
	}{id})
}

func (h *handler) apiEnableJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		h.apiFailure(w, 400, "enabled_required", "CONFIGURATION", "Send enabled true or false.")
		return
	}
	if err := h.deps.Jobs.Enable(r.Context(), r.PathValue("id"), *body.Enabled); err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		ID string `json:"id"`
	}{r.PathValue("id")})
}

func (h *handler) apiRunJob(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !h.apiBody(w, r, &body) {
		return
	}
	runID, err := h.deps.Jobs.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		h.apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+runID)
	h.apiJSON(w, 202, struct {
		RunID string `json:"runId"`
	}{runID})
}
