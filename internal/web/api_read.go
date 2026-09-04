package web

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func (h *handler) apiSourceList(ctx context.Context) ([]sourceDTO, error) {
	items := []sourceDTO{}
	if h.deps.Repo == nil {
		return items, nil
	}
	sources, err := h.deps.Repo.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range sources {
		busy := h.deps.Runner != nil && h.deps.Runner.Busy(s.ID)
		items = append(items, sourceInfo(s, busy))
	}
	return items, nil
}
func (h *handler) apiSources(w http.ResponseWriter, r *http.Request) {
	items, err := h.apiSourceList(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		Sources []sourceDTO `json:"sources"`
	}{items})
}
func (h *handler) apiSource(w http.ResponseWriter, r *http.Request) {
	s, err := h.deps.Repo.Source(r.Context(), r.PathValue("id"))
	if err != nil {
		h.apiError(w, err)
		return
	}
	if s == nil {
		h.apiFailure(w, 404, "source_missing", "CONFIGURATION", "This source does not exist.")
		return
	}
	runs, err := h.deps.Repo.ListRunsForSource(r.Context(), s.ID, 100)
	if err != nil {
		h.apiError(w, err)
		return
	}
	batches, err := h.deps.Repo.ListRevisionBatches(r.Context(), s.ID, 100)
	if err != nil {
		h.apiError(w, err)
		return
	}
	items := []revisionDTO{}
	for _, b := range batches {
		items = append(items, revisionInfo(b))
	}
	h.apiJSON(w, 200, struct {
		Source          sourceDTO     `json:"source"`
		Runs            []runDTO      `json:"runs"`
		RevisionBatches []revisionDTO `json:"revisionBatches"`
		HistoryLimit    int           `json:"historyLimit"`
	}{sourceInfo(*s, h.deps.Runner != nil && h.deps.Runner.Busy(s.ID)), runList(runs), items, 100})
}

type disksDTO struct {
	Disks             []diskDTO  `json:"disks"`
	GeneratedAt       *time.Time `json:"generatedAt"`
	PlatformAvailable bool       `json:"platformAvailable"`
	PlatformReason    string     `json:"platformReason"`
	EnumerationError  string     `json:"enumerationError"`
}

func (h *handler) apiDiskList(ctx context.Context) (disksDTO, error) {
	v := disksDTO{Disks: []diskDTO{}}
	if h.deps.Media == nil {
		return v, nil
	}
	s, err := h.deps.Media.Snapshot(ctx)
	if err != nil {
		return v, err
	}
	v.GeneratedAt, v.PlatformAvailable, v.PlatformReason, v.EnumerationError = instant(s.GeneratedAt), s.PlatformAvailable, s.PlatformReason, s.EnumerationError
	for _, d := range s.Views {
		v.Disks = append(v.Disks, diskInfo(d, h.deps.DevMode))
	}
	return v, nil
}
func (h *handler) apiDisks(w http.ResponseWriter, r *http.Request) {
	v, err := h.apiDiskList(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, v)
}
func (h *handler) apiDisk(w http.ResponseWriter, r *http.Request) {
	v, err := h.apiDiskList(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	for _, d := range v.Disks {
		if d.ID == r.PathValue("id") {
			h.apiJSON(w, 200, struct {
				Disk             diskDTO `json:"disk"`
				EnumerationError string  `json:"enumerationError"`
			}{d, v.EnumerationError})
			return
		}
	}
	h.apiFailure(w, 404, "disk_missing", "CONFIGURATION", "This registered disk does not exist.")
}

func (h *handler) apiRepositoryList(ctx context.Context) ([]repositoryDTO, error) {
	items := []repositoryDTO{}
	if h.deps.Backups == nil {
		return items, nil
	}
	repos, err := h.deps.Backups.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range repos {
		items = append(items, repositoryInfo(v))
	}
	return items, nil
}
func (h *handler) apiRepositories(w http.ResponseWriter, r *http.Request) {
	items, err := h.apiRepositoryList(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		Repositories []repositoryDTO `json:"repositories"`
	}{items})
}

type eligibleSourceDTO struct {
	Source   sourceDTO `json:"source"`
	Eligible bool      `json:"eligible"`
	Reason   string    `json:"reason"`
}
type historyDTO struct {
	Run    runDTO    `json:"run"`
	Backup backupDTO `json:"backup"`
}

func (h *handler) apiRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	v, err := h.deps.Backups.RepositoryDetail(ctx, id)
	if err != nil {
		h.apiError(w, err)
		return
	}
	sources, err := h.deps.Backups.EligibleSources(ctx)
	if err != nil {
		h.apiError(w, err)
		return
	}
	history, err := h.deps.Backups.BackupHistory(ctx, id, 100)
	if err != nil {
		h.apiError(w, err)
		return
	}
	eligible := []eligibleSourceDTO{}
	for _, s := range sources {
		eligible = append(eligible, eligibleSourceDTO{sourceInfo(s.Source, false), s.Eligible, s.Reason})
	}
	runs := []historyDTO{}
	for _, r := range history {
		runs = append(runs, historyDTO{runInfo(r.Run), backupInfo(r.Backup)})
	}
	h.apiJSON(w, 200, struct {
		Repository   repositoryDTO       `json:"repository"`
		Sources      []eligibleSourceDTO `json:"sources"`
		History      []historyDTO        `json:"history"`
		HistoryLimit int                 `json:"historyLimit"`
	}{repositoryInfo(v), eligible, runs, 100})
}

func runList(runs []domain.Run) []runDTO {
	items := []runDTO{}
	for _, r := range runs {
		items = append(items, runInfo(r))
	}
	return items
}
func (h *handler) apiRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := h.deps.Repo.ListRuns(r.Context(), 100)
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		Runs  []runDTO `json:"runs"`
		Limit int      `json:"limit"`
	}{runList(runs), 100})
}
func (h *handler) apiRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := h.deps.Repo.Run(ctx, r.PathValue("id"))
	if err != nil {
		h.apiError(w, err)
		return
	}
	if run == nil {
		h.apiFailure(w, 404, "run_missing", "CONFIGURATION", "This run does not exist.")
		return
	}
	v := struct {
		Run             runDTO           `json:"run"`
		Source          *sourceDTO       `json:"source"`
		Collection      *sourceResultDTO `json:"collection"`
		RevisionBatches []revisionDTO    `json:"revisionBatches"`
		Backup          *backupDTO       `json:"backup"`
	}{Run: runInfo(*run), RevisionBatches: []revisionDTO{}}
	if run.SourceID != "" {
		s, err := h.deps.Repo.Source(ctx, run.SourceID)
		if err != nil {
			h.apiError(w, err)
			return
		}
		if s != nil {
			dto := sourceInfo(*s, false)
			v.Source = &dto
		}
		if run.Kind == domain.RunKindCollection {
			result, err := h.deps.Repo.SourceResult(ctx, run.ID, run.SourceID)
			if err != nil {
				h.apiError(w, err)
				return
			}
			if result != nil {
				v.Collection = &sourceResultDTO{result.StagingStateVersion, instant(result.PublishedAt), result.BytesScanned, result.BytesCopied, result.FilesScanned, result.FilesCopied, result.FilesDeletedOrArchived, result.EngineErrors, nonNil(result.Warnings)}
			}
			batches, err := h.deps.Repo.ListRevisionBatches(ctx, run.SourceID, 100)
			if err != nil {
				h.apiError(w, err)
				return
			}
			for _, b := range batches {
				if b.CollectionRunID == run.ID {
					v.RevisionBatches = append(v.RevisionBatches, revisionInfo(b))
				}
			}
		}
	}
	if h.deps.BackupRepo != nil && run.Kind == domain.RunKindBackupLatest {
		b, err := h.deps.BackupRepo.BackupRun(ctx, run.ID)
		if err != nil {
			h.apiError(w, err)
			return
		}
		if b != nil {
			dto := backupInfo(*b)
			states, err := h.deps.BackupRepo.BackupSourceStates(ctx, run.ID)
			if err != nil {
				h.apiError(w, err)
				return
			}
			for _, s := range states {
				dto.ReadSet = append(dto.ReadSet, readStateDTO{s.SourceID, s.CurrentCollectionRunID, s.StagingStateVersion, s.CurrentRelativePath, instant(s.PublishedAt), instant(s.CapturedAt)})
			}
			batches, err := h.deps.BackupRepo.BackupRevisionBatches(ctx, run.ID)
			if err != nil {
				h.apiError(w, err)
				return
			}
			for _, b := range batches {
				dto.RevisionBatches = append(dto.RevisionBatches, readBatchDTO{b.RevisionBatchID, b.SourceID, b.RelativePath, instant(b.BatchCreatedAt), instant(b.CapturedAt)})
			}
			v.Backup = &dto
		}
	}
	h.apiJSON(w, 200, v)
}

type dashboardDTO struct {
	ApplicationStatus      string          `json:"applicationStatus"`
	DatabaseStatus         string          `json:"databaseStatus"`
	Sources                []sourceDTO     `json:"sources"`
	Storage                disksDTO        `json:"storage"`
	Repositories           []repositoryDTO `json:"repositories"`
	RecentRuns             []runDTO        `json:"recentRuns"`
	RecentRunLimit         int             `json:"recentRunLimit"`
	LastSuccessfulBackupAt *time.Time      `json:"lastSuccessfulBackupAt"`
}

func (h *handler) dashboardInfo(ctx context.Context) (dashboardDTO, error) {
	v := dashboardDTO{ApplicationStatus: h.deps.Status.ApplicationStatus(), DatabaseStatus: "ok", RecentRuns: []runDTO{}, RecentRunLimit: 100}
	if err := h.deps.Status.DatabaseHealth(ctx); err != nil {
		return v, domain.NewError(domain.ErrClassUnavailable, "database_unavailable", "The database health check failed.")
	}
	var err error
	if v.Sources, err = h.apiSourceList(ctx); err != nil {
		return v, err
	}
	if v.Storage, err = h.apiDiskList(ctx); err != nil {
		return v, err
	}
	if v.Repositories, err = h.apiRepositoryList(ctx); err != nil {
		return v, err
	}
	if h.deps.Repo != nil {
		runs, err := h.deps.Repo.ListRuns(ctx, 100)
		if err != nil {
			return v, err
		}
		v.RecentRuns = runList(runs)
	}
	// Use persisted success metadata, not a bounded history window or a restic
	// snapshot candidate, to answer whether a durable backup has ever succeeded.
	for _, r := range v.Repositories {
		if r.LastSuccessfulBackupAt != nil && (v.LastSuccessfulBackupAt == nil || r.LastSuccessfulBackupAt.After(*v.LastSuccessfulBackupAt)) {
			v.LastSuccessfulBackupAt = r.LastSuccessfulBackupAt
		}
	}
	return v, nil
}
func (h *handler) apiDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	v, err := h.dashboardInfo(ctx)
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, v)
}
func (h *handler) apiDebug(w http.ResponseWriter, r *http.Request) {
	// Route registration AND the handler guard fail closed in production.
	if !h.deps.DevMode {
		h.apiFailure(w, 404, "api_not_found", "CONFIGURATION", "This API endpoint does not exist.")
		return
	}
	v, err := h.dashboardInfo(r.Context())
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		Platform       string       `json:"platform"`
		TransferEngine engineDTO    `json:"transferEngine"`
		BackupEngine   engineDTO    `json:"backupEngine"`
		Status         dashboardDTO `json:"status"`
	}{runtime.GOOS, engineInfo(h.deps.Engine), engineInfo(h.deps.BackupEngine), v})
}
