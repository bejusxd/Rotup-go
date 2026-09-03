package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// StatusSource is the small application seam needed by the M1 HTTP handlers.
type StatusSource interface {
	ApplicationStatus() string
	DatabaseHealth(context.Context) error
	LatestRun(context.Context) (*domain.Run, error)
}

type handler struct {
	source  StatusSource
	version string
	page    *template.Template
}

type dashboardData struct {
	Name              string
	Version           string
	ApplicationStatus string
	DatabaseStatus    string
	LatestRun         *domain.Run
	Error             string
}

type healthResponse struct {
	OverallStatus     string `json:"overall_status"`
	DatabaseStatus    string `json:"database_status"`
	ApplicationStatus string `json:"application_status"`
}

const dashboardTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>ROTUP</title>
</head>
<body>
  <main>
    <h1>{{.Name}}</h1>
    <p>Version: {{.Version}}</p>
    <p>Application status: {{.ApplicationStatus}}</p>
    <p>Database status: {{.DatabaseStatus}}</p>
    {{if .Error}}<p>Dashboard error: {{.Error}}</p>{{end}}
    <section>
      <h2>Latest run</h2>
      {{with .LatestRun}}
        <p><strong>{{.Kind}}</strong> — {{.Status}}</p>
        <p>{{.Summary}}</p>
        <p>Finished: {{.FinishedAt.Format "2006-01-02T15:04:05Z07:00"}}</p>
      {{else}}
        <p>No runs recorded.</p>
      {{end}}
    </section>
  </main>
</body>
</html>`

// NewHandler returns the complete M1 HTTP handler.
func NewHandler(source StatusSource, version string) (http.Handler, error) {
	page, err := template.New("dashboard").Parse(dashboardTemplate)
	if err != nil {
		return nil, err
	}
	h := &handler{source: source, version: version, page: page}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.health)
	mux.HandleFunc("/", h.dashboard)
	return mux, nil
}

func (h *handler) dashboard(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(response, request)
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	data := dashboardData{
		Name:              "ROTUP",
		Version:           h.version,
		ApplicationStatus: h.source.ApplicationStatus(),
		DatabaseStatus:    "ok",
	}
	statusCode := http.StatusOK
	if err := h.source.DatabaseHealth(request.Context()); err != nil {
		data.DatabaseStatus = "unavailable"
		data.Error = "database health check failed"
		statusCode = http.StatusServiceUnavailable
	} else {
		latest, err := h.source.LatestRun(request.Context())
		if err != nil {
			data.DatabaseStatus = "unavailable"
			data.Error = "run history could not be read"
			statusCode = http.StatusServiceUnavailable
		} else {
			data.LatestRun = latest
		}
	}

	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(statusCode)
	_ = h.page.Execute(response, data)
}

func (h *handler) health(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	result := healthResponse{
		OverallStatus:     "ok",
		DatabaseStatus:    "ok",
		ApplicationStatus: h.source.ApplicationStatus(),
	}
	statusCode := http.StatusOK
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := h.source.DatabaseHealth(ctx); err != nil {
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
