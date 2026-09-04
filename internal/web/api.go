package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"runtime"
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
)

// API v1 is a presentation facade over the same seams used by the HTML UI.
// It never resolves paths, starts tools, or changes orchestration policy.
func (h *handler) apiRoutes(root *http.ServeMux) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", h.health)
	mux.HandleFunc("GET /api/v1/system", h.apiSystem)
	mux.HandleFunc("GET /api/v1/dashboard", h.apiDashboard)
	mux.HandleFunc("GET /api/v1/setup", h.apiSetupStatus)
	if h.deps.Repo != nil {
		mux.HandleFunc("GET /api/v1/sources", h.apiSources)
		mux.HandleFunc("GET /api/v1/sources/{id}", h.apiSource)
		mux.HandleFunc("GET /api/v1/runs", h.apiRuns)
		mux.HandleFunc("GET /api/v1/runs/{id}", h.apiRun)
	}
	if h.deps.Admin != nil {
		mux.HandleFunc("POST /api/v1/sources", h.apiCreateSource)
		mux.HandleFunc("PUT /api/v1/sources/{id}", h.apiUpdateSource)
		mux.HandleFunc("POST /api/v1/sources/{id}/credential", h.apiCredential)
	}
	if h.deps.Runner != nil {
		mux.HandleFunc("POST /api/v1/sources/{id}/test", h.apiTestSource)
		mux.HandleFunc("POST /api/v1/sources/{id}/collect", h.apiCollectSource)
	}
	if h.deps.Media != nil {
		mux.HandleFunc("GET /api/v1/disks", h.apiDisks)
		mux.HandleFunc("GET /api/v1/disks/{id}", h.apiDisk)
		mux.HandleFunc("POST /api/v1/disks/register", h.apiRegisterDisk)
		mux.HandleFunc("PUT /api/v1/disks/{id}", h.apiUpdateDisk)
		mux.HandleFunc("POST /api/v1/disks/{id}/mount", h.apiMountDisk)
		mux.HandleFunc("POST /api/v1/disks/{id}/unmount", h.apiUnmountDisk)
	}
	if h.deps.Backups != nil {
		mux.HandleFunc("GET /api/v1/repositories", h.apiRepositories)
		mux.HandleFunc("GET /api/v1/repositories/{id}", h.apiRepository)
		mux.HandleFunc("POST /api/v1/repositories", h.apiCreateRepository)
		mux.HandleFunc("POST /api/v1/repositories/{id}/initialize", h.apiInitializeRepository)
		mux.HandleFunc("POST /api/v1/repositories/{id}/backup", h.apiBackup)
		mux.HandleFunc("POST /api/v1/repositories/{id}/recover", h.apiRecover)
	}
	if h.deps.DevMode {
		mux.HandleFunc("GET /api/v1/debug/status", h.apiDebug)
	}
	// Unknown API paths must never fall through to HTML or the SPA.
	root.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		allowed := []string{}
		for _, method := range []string{"GET", "HEAD", "POST", "PUT"} {
			probe := r.Clone(r.Context())
			probe.Method = method
			if _, pattern := mux.Handler(probe); pattern != "" {
				allowed = append(allowed, method)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			h.apiFailure(w, 405, "method_not_allowed", "CONFIGURATION", "This HTTP method is not supported.")
			return
		}
		h.apiFailure(w, http.StatusNotFound, "api_not_found", "CONFIGURATION", "This API endpoint does not exist.")
	}))
}

type apiErrorDTO struct {
	Code        string `json:"code"`
	Class       string `json:"class"`
	Message     string `json:"message"`
	UserMessage string `json:"userMessage"`
}

func (h *handler) apiJSON(w http.ResponseWriter, status int, value any) {
	// Encode before writing the status: a serialization error is never a 200.
	encoded, err := json.Marshal(value)
	if err != nil {
		h.logger.Error("API response could not be encoded")
		h.apiFailure(w, 500, "api_encoding_failed", "INTERNAL", "The response could not be prepared.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

func (h *handler) apiFailure(w http.ResponseWriter, status int, code, class, message string) {
	h.apiJSON(w, status, struct {
		Error apiErrorDTO `json:"error"`
	}{
		apiErrorDTO{Code: code, Class: class, Message: message, UserMessage: userErrorMessage(class, code)},
	})
}

func userErrorMessage(class, code string) string {
	switch code {
	case "invalid_host":
		return "Enter a server name or IP address, without a URL prefix or share path."
	case "invalid_staging_path", "staging_overlap":
		return "Choose a separate relative folder for this source's local copy."
	case "csrf_rejected":
		return "Reload this page before submitting again. The page security token is no longer valid."
	case "mount_helper_device_not_found":
		return "The backup disk could not be mounted. Check that the registered disk is connected."
	}
	switch domain.ErrorClass(class) {
	case domain.ErrClassAuthentication:
		return "Check the username and saved password."
	case domain.ErrClassUnavailable:
		return "The required source or backup disk is not available."
	case domain.ErrClassIntegrity:
		return "This operation was stopped to protect your data. Review the technical details."
	case domain.ErrClassCapacity:
		return "There is not enough storage space to continue safely."
	case domain.ErrClassConfiguration:
		return "Check the settings and technical details before trying again."
	case domain.ErrClassTimeout:
		return "The operation took too long. Check its recorded result before retrying."
	default:
		return "The operation could not be completed. Review the technical details."
	}
}

func (h *handler) apiError(w http.ResponseWriter, err error) {
	var typed *domain.Error
	if !domain.AsError(err, &typed) {
		// Do not send raw driver/process errors (or credentials in them) to a browser.
		h.logger.Error("API operation failed", "class", "INTERNAL")
		h.apiFailure(w, 500, "internal_error", "INTERNAL", "The operation failed for an internal reason.")
		return
	}
	status := http.StatusInternalServerError
	switch typed.Class {
	case domain.ErrClassConfiguration, domain.ErrClassAuthentication:
		status = http.StatusBadRequest
	case domain.ErrClassIntegrity, domain.ErrClassCapacity:
		status = http.StatusConflict
	case domain.ErrClassUnavailable:
		status = http.StatusServiceUnavailable
	case domain.ErrClassTimeout:
		status = http.StatusGatewayTimeout
	case domain.ErrClassCancellation:
		status = http.StatusConflict
	case domain.ErrClassExternalTool:
		status = http.StatusBadGateway
	}
	switch typed.Code {
	case "source_missing", "disk_missing", "repository_missing":
		status = http.StatusNotFound
	}
	if strings.Contains(typed.Code, "busy") {
		status = http.StatusConflict
	}
	h.apiFailure(w, status, typed.Code, string(typed.Class), typed.SafeMessage())
}

// Every mutation uses the existing same-origin double-submit CSRF mechanism.
// The token is issued by /system and sent in X-CSRF-Token for JSON requests.
func (h *handler) apiBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if failure := h.csrf.checkToken(r, r.Header.Get("X-CSRF-Token")); failure != "" {
		h.apiFailure(w, 403, "csrf_rejected", "CONFIGURATION", failure)
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		h.apiFailure(w, 415, "json_required", "CONFIGURATION", "Send an application/json request.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := 400
		if errors.As(err, &tooLarge) {
			status = 413
		}
		h.apiFailure(w, status, "invalid_json", "CONFIGURATION", "The request is invalid or too large.")
		return false
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		h.apiFailure(w, 400, "invalid_json", "CONFIGURATION", "Send exactly one JSON object.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		h.apiFailure(w, 400, "invalid_json", "CONFIGURATION", "The request is invalid; check its fields.")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		h.apiFailure(w, 400, "invalid_json", "CONFIGURATION", "Send exactly one JSON object.")
		return false
	}
	return true
}

func (h *handler) apiAccepted(w http.ResponseWriter, runID string, err error) {
	if err != nil {
		h.apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+runID)
	h.apiJSON(w, 202, struct {
		RunID string `json:"runId"`
	}{runID})
}

type systemDTO struct {
	Name                            string    `json:"name"`
	Version                         string    `json:"version"`
	Platform                        string    `json:"platform"`
	DevMode                         bool      `json:"devMode"`
	CSRFToken                       string    `json:"csrfToken"`
	TransferEngine                  engineDTO `json:"transferEngine"`
	BackupEngine                    engineDTO `json:"backupEngine"`
	MinimumRepositoryPasswordLength int       `json:"minimumRepositoryPasswordLength"`
}

type engineDTO struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}

func engineInfo(get func() EngineStatus) engineDTO {
	if get == nil {
		return engineDTO{}
	}
	e := get()
	return engineDTO{e.Name, e.Version, e.Available, e.Detail}
}

func (h *handler) apiSystem(w http.ResponseWriter, r *http.Request) {
	h.apiJSON(w, 200, systemDTO{"ROTUP", h.deps.Version, runtime.GOOS, h.deps.DevMode,
		h.csrf.token(w, r), engineInfo(h.deps.Engine), engineInfo(h.deps.BackupEngine), domain.MinimumRepositoryPasswordLength})
}
