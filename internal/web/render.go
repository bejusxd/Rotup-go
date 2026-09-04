package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

//go:embed templates/*.html
var templateFiles embed.FS

type templateSet struct {
	root *template.Template
}

func loadTemplates() (*templateSet, error) {
	root, err := template.New("root").Funcs(templateFuncs()).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &templateSet{root: root}, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"timestamp": func(value time.Time) string {
			if value.IsZero() {
				return "—"
			}
			return value.UTC().Format("2006-01-02 15:04:05 MST")
		},
		"duration": func(value time.Duration) string {
			if value <= 0 {
				return "—"
			}
			return value.Round(time.Millisecond).String()
		},
		"bytes": humanBytes,
		"statusClass": func(status domain.RunStatus) string {
			switch {
			case status.Successful():
				return "ok"
			case status.Terminal():
				return "bad"
			default:
				return "busy"
			}
		},
		"supportedFilesystems": func() []string { return domain.SupportedFilesystems },
		"diskClass": func(status domain.DiskStatus) string {
			switch status {
			case domain.DiskAvailableUnmounted, domain.DiskReady, domain.DiskSafeToRemove:
				return "ok"
			case domain.DiskError:
				return "bad"
			case domain.DiskDisconnected, domain.DiskDetected:
				return "muted"
			default:
				return "busy"
			}
		},
		"repositoryClass": func(health domain.RepositoryHealth) string {
			switch health {
			case domain.RepositoryHealthy:
				return "ok"
			case domain.RepositorySuspect, domain.RepositoryRecoveryRequired:
				return "bad"
			case domain.RepositoryInitializing:
				return "busy"
			default:
				return "muted"
			}
		},
		"stageClass": func(stage domain.DurabilityStage) string {
			switch stage {
			case domain.StageDurableSuccess:
				return "ok"
			case domain.StageFailed:
				return "bad"
			default:
				// Every other stage on a finished run means the backup stopped
				// short of its durability boundary, which the page must not
				// present as a neutral fact.
				return "busy"
			}
		},
		"shortID": func(value string) string {
			if len(value) <= 8 {
				return value
			}
			return value[:8]
		},
		"minimumRepositoryPassword": func() int { return domain.MinimumRepositoryPasswordLength },
		"stagingClass": func(status domain.StagingStatus) string {
			switch status {
			case domain.StagingStable:
				return "ok"
			case domain.StagingRecoveryRequired:
				return "bad"
			case domain.StagingUpdating:
				return "busy"
			default:
				return ""
			}
		},
	}
}

func humanBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exponent := int64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exponent])
}

// pageData wraps every rendered view with the shell the layout needs.
type pageData struct {
	Title     string
	Version   string
	CSRFToken string
	Notice    string
	Error     string
	// Refresh makes the layout emit a meta refresh so a page showing a run
	// that is still executing updates itself without any client-side script.
	Refresh bool
	Body    any
}

// refresher lets a view model ask the layout to poll.
type refresher interface{ NeedsRefresh() bool }

// render executes a page template. It buffers the output so a template failure
// cannot emit a half-written page with a success status.
func (h *handler) render(response http.ResponseWriter, request *http.Request, status int, page string, body any) {
	h.renderWithMessages(response, request, status, page, body, "", "")
}

func (h *handler) renderWithMessages(response http.ResponseWriter, request *http.Request, status int,
	page string, body any, notice, failure string) {

	data := pageData{
		Title:     page,
		Version:   h.deps.Version,
		CSRFToken: h.csrf.token(response, request),
		Notice:    notice,
		Error:     failure,
		Body:      body,
	}
	if value, ok := body.(refresher); ok {
		data.Refresh = value.NeedsRefresh()
	}

	var buffer bytes.Buffer
	if err := h.templates.root.ExecuteTemplate(&buffer, page+".html", data); err != nil {
		h.logger.Error("page could not be rendered", "page", page, "error", err)
		http.Error(response, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(status)
	_, _ = buffer.WriteTo(response)
}

// safeError renders an administrator-facing message for any error, never the
// underlying driver or engine text.
func safeError(err error) string {
	if err == nil {
		return ""
	}
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		return typed.SafeMessage()
	}
	return "the operation failed for an internal reason"
}
