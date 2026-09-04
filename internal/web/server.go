// Package web serves the local administration UI. Handlers call application use
// cases and render validated view models; they never call adapters directly and
// they never render secret material.
package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/collector"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/staging"
)

// StatusSource is the application seam the dashboard and health endpoint need.
type StatusSource interface {
	ApplicationStatus() string
	DatabaseHealth(context.Context) error
	LatestRun(context.Context) (*domain.Run, error)
}

// Repository is the read seam for configuration and history.
type Repository interface {
	ListSources(ctx context.Context) ([]domain.Source, error)
	Source(ctx context.Context, id string) (*domain.Source, error)
	CredentialRef(ctx context.Context, sourceID string) (domain.CredentialRef, error)
	Run(ctx context.Context, id string) (*domain.Run, error)
	ListRuns(ctx context.Context, limit int) ([]domain.Run, error)
	ListRunsForSource(ctx context.Context, sourceID string, limit int) ([]domain.Run, error)
	LatestCollectionForSource(ctx context.Context, sourceID string) (*domain.Run, error)
	SourceResult(ctx context.Context, runID, sourceID string) (*domain.SourceResult, error)
	ListRevisionBatches(ctx context.Context, sourceID string, limit int) ([]domain.RevisionBatch, error)
	RevisionBatchForRun(ctx context.Context, runID string) (*domain.RevisionBatch, error)
	ListEvents(ctx context.Context, limit int) ([]domain.Event, error)
}

// BackupRepository is the read seam for the durable backup detail a run page
// shows. It is separate from Repository so a deployment without the backup
// milestone wired in still renders every other page.
type BackupRepository interface {
	BackupRun(ctx context.Context, runID string) (*domain.BackupRun, error)
	BackupSourceStates(ctx context.Context, runID string) ([]domain.BackupSourceState, error)
	BackupRevisionBatches(ctx context.Context, runID string) ([]domain.BackupRevisionBatch, error)
	Repository(ctx context.Context, id string) (*domain.Repository, error)
	RevisionCoverageForBatch(ctx context.Context, batchID string) ([]domain.RevisionCoverage, error)
}

// SourceAdmin is the configuration and credential use-case seam.
type SourceAdmin interface {
	Create(ctx context.Context, input collector.SourceInput) (string, error)
	Update(ctx context.Context, id string, input collector.SourceInput) error
	ReplaceCredential(ctx context.Context, sourceID, password string) error
	ClearCredential(ctx context.Context, sourceID string) error
}

// Collector starts manual runs. Each call returns immediately with a durable
// run identifier so the browser never has to hold a request open.
type Collector interface {
	StartConnectionTest(ctx context.Context, sourceID string) (string, error)
	StartCollection(ctx context.Context, sourceID string) (string, error)
	Busy(sourceID string) bool
}

// StagingBrowser is the read-only staging inspection seam. Only the staging
// layout may turn a source and a relative path into a real directory.
type StagingBrowser interface {
	CurrentDir(source domain.Source) (string, error)
	SourceRevisionsDir(sourceID string) (string, error)
	List(base, relative string) (staging.Listing, error)
}

// EngineStatus reports the discovered transfer engine for the dashboard.
type EngineStatus struct {
	Name             string
	Version          string
	Available        bool
	DiscoveredOnPath bool
	Detail           string
}

// Deps are everything the HTTP layer needs.
type Deps struct {
	DevMode    bool
	Version    string
	Status     StatusSource
	Repo       Repository
	BackupRepo BackupRepository
	Admin      SourceAdmin
	Runner     Collector
	Staging    StagingBrowser
	Media      Media
	Backups    Backups
	Engine     func() EngineStatus
	// BackupEngine reports the discovered snapshot engine. It is separate from
	// Engine because a daemon can have one of the two tools and not the other,
	// and the UI has to be able to say which.
	BackupEngine func() EngineStatus
	Logger       *slog.Logger
	// Background returns a context bound to the daemon's lifetime rather than
	// to an HTTP request, so an operation a browser started survives that
	// browser going away.
	Background func() context.Context
}

type handler struct {
	deps      Deps
	templates *templateSet
	csrf      *csrfGuard
	logger    *slog.Logger
}

type healthResponse struct {
	OverallStatus     string `json:"overall_status"`
	DatabaseStatus    string `json:"database_status"`
	ApplicationStatus string `json:"application_status"`
}

// NewHandler builds the complete HTTP handler.
func NewHandler(deps Deps) (http.Handler, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{
		deps:      deps,
		templates: templates,
		csrf:      newCSRFGuard(),
		logger:    logger.With("component", "web"),
	}

	mux := http.NewServeMux()
	h.apiRoutes(mux)
	mux.HandleFunc("GET /healthz", h.health)
	spa, err := newSPAHandler()
	if err != nil {
		return nil, err
	}
	mux.Handle("/legacy/", http.StripPrefix("/legacy", h.legacyRoutes()))
	mux.Handle("/", spa)
	return securityHeaders(mux), nil
}

func (h *handler) legacyRoutes() http.Handler {
	deps := h.deps
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.dashboard)

	if deps.Repo != nil {
		mux.HandleFunc("GET /sources", h.listSources)
		mux.HandleFunc("GET /sources/new", h.newSourceForm)
		mux.HandleFunc("POST /sources", h.createSource)
		mux.HandleFunc("GET /sources/{id}", h.showSource)
		mux.HandleFunc("POST /sources/{id}", h.updateSource)
		mux.HandleFunc("POST /sources/{id}/credential", h.replaceCredential)
		mux.HandleFunc("POST /sources/{id}/credential/clear", h.clearCredential)
		mux.HandleFunc("POST /sources/{id}/test", h.startConnectionTest)
		mux.HandleFunc("POST /sources/{id}/collect", h.startCollection)
		mux.HandleFunc("GET /sources/{id}/staging", h.browseStaging)
		mux.HandleFunc("GET /runs", h.listRuns)
		mux.HandleFunc("GET /runs/{id}", h.showRun)
	}
	if deps.Backups != nil {
		mux.HandleFunc("GET /repositories", h.listRepositories)
		mux.HandleFunc("GET /repositories/{id}", h.showRepository)
		mux.HandleFunc("POST /disks/{id}/repository/init", h.initializeRepository)
		mux.HandleFunc("POST /disks/{id}/repository/adopt", h.adoptRepository)
		mux.HandleFunc("POST /repositories/{id}/backup", h.startBackup)
		mux.HandleFunc("POST /repositories/{id}/recover", h.recoverRepository)
	}
	if deps.Media != nil {
		mux.HandleFunc("GET /disks", h.listDisks)
		mux.HandleFunc("POST /disks/register", h.registerDisk)
		mux.HandleFunc("POST /disks/{id}", h.updateDisk)
		mux.HandleFunc("POST /disks/{id}/mount", h.mountDisk)
		mux.HandleFunc("POST /disks/{id}/unmount", h.unmountDisk)
	}

	return mux
}

// securityHeaders applies the response hardening the local UI needs. There is
// no inline script anywhere in the templates, so the policy can forbid it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		header := response.Header()
		header.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(response, request)
	})
}

type dashboardData struct {
	Name              string
	Version           string
	ApplicationStatus string
	DatabaseStatus    string
	LatestRun         *domain.Run
	Engine            EngineStatus
	BackupEngine      EngineStatus
	Repositories      []backup.RepositoryView
	Sources           []sourceRow
	Error             string
}

func (h *handler) dashboard(response http.ResponseWriter, request *http.Request) {
	data := dashboardData{
		Name:              "ROTUP",
		Version:           h.deps.Version,
		ApplicationStatus: h.deps.Status.ApplicationStatus(),
		DatabaseStatus:    "ok",
	}
	if h.deps.Engine != nil {
		data.Engine = h.deps.Engine()
	}
	if h.deps.BackupEngine != nil {
		data.BackupEngine = h.deps.BackupEngine()
	}

	statusCode := http.StatusOK
	if err := h.deps.Status.DatabaseHealth(request.Context()); err != nil {
		data.DatabaseStatus = "unavailable"
		data.Error = "database health check failed"
		statusCode = http.StatusServiceUnavailable
	} else {
		latest, err := h.deps.Status.LatestRun(request.Context())
		if err != nil {
			data.DatabaseStatus = "unavailable"
			data.Error = "run history could not be read"
			statusCode = http.StatusServiceUnavailable
		} else {
			data.LatestRun = latest
		}
		if h.deps.Repo != nil && statusCode == http.StatusOK {
			rows, err := h.sourceRows(request.Context())
			if err != nil {
				data.Error = "sources could not be read"
			} else {
				data.Sources = rows
			}
		}
		if h.deps.Backups != nil && statusCode == http.StatusOK {
			if repositories, err := h.deps.Backups.ListRepositories(request.Context()); err == nil {
				data.Repositories = repositories
			}
		}
	}

	h.render(response, request, statusCode, "dashboard", data)
}

func (h *handler) health(response http.ResponseWriter, request *http.Request) {
	result := healthResponse{
		OverallStatus:     "ok",
		DatabaseStatus:    "ok",
		ApplicationStatus: h.deps.Status.ApplicationStatus(),
	}
	statusCode := http.StatusOK
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := h.deps.Status.DatabaseHealth(ctx); err != nil {
		result.OverallStatus = "degraded"
		result.DatabaseStatus = "unavailable"
		statusCode = http.StatusServiceUnavailable
	}
	if result.ApplicationStatus != "running" {
		result.OverallStatus = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(statusCode)
	_ = json.NewEncoder(response).Encode(result)
}
