package web

import (
	"net/http"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/collector"
	"github.com/bejusxd/rotup/internal/domain"
)

type sourceInputDTO struct {
	Name                string   `json:"name"`
	Host                string   `json:"host"`
	ShareName           string   `json:"shareName"`
	Subdirectory        string   `json:"subdirectory"`
	Username            string   `json:"username"`
	StagingRelativePath string   `json:"stagingRelativePath"`
	Enabled             *bool    `json:"enabled"`
	Policy              string   `json:"policy"`
	IncludeRules        []string `json:"includeRules"`
	ExcludeRules        []string `json:"excludeRules"`
}

func (d sourceInputDTO) input() collector.SourceInput {
	enabled := true
	if d.Enabled != nil {
		enabled = *d.Enabled
	}
	policy := d.Policy
	if policy == "" {
		policy = string(domain.PolicyArchiveThenSync)
	}
	return collector.SourceInput{Name: d.Name, Host: d.Host, ShareName: d.ShareName, Subdirectory: d.Subdirectory,
		Username: d.Username, StagingRelativePath: d.StagingRelativePath, Enabled: enabled,
		ReconciliationPolicy: domain.ReconciliationPolicy(policy), IncludeRules: d.IncludeRules, ExcludeRules: d.ExcludeRules}
}
func (h *handler) apiCreateSource(w http.ResponseWriter, r *http.Request) {
	var body sourceInputDTO
	if !h.apiBody(w, r, &body) {
		return
	}
	id, err := h.deps.Admin.Create(r.Context(), body.input())
	if err != nil {
		h.apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/sources/"+id)
	h.apiJSON(w, 201, struct {
		ID string `json:"id"`
	}{id})
}
func (h *handler) apiUpdateSource(w http.ResponseWriter, r *http.Request) {
	var body sourceInputDTO
	if !h.apiBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		h.apiFailure(w, 400, "enabled_required", "CONFIGURATION", "A full source replacement requires enabled.")
		return
	}
	if err := h.deps.Admin.Update(r.Context(), r.PathValue("id"), body.input()); err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		ID string `json:"id"`
	}{r.PathValue("id")})
}
func (h *handler) apiCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action   string `json:"action"`
		Password string `json:"password"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	var err error
	switch body.Action {
	case "replace":
		err = h.deps.Admin.ReplaceCredential(r.Context(), r.PathValue("id"), body.Password)
	case "clear":
		if body.Password != "" {
			h.apiFailure(w, 400, "invalid_credential_action", "CONFIGURATION", "Do not send a password with the clear action.")
			return
		}
		err = h.deps.Admin.ClearCredential(r.Context(), r.PathValue("id"))
	default:
		h.apiFailure(w, 400, "invalid_credential_action", "CONFIGURATION", "Choose an explicit replace or clear credential action.")
		return
	}
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		OK bool `json:"ok"`
	}{true})
}
func (h *handler) apiTestSource(w http.ResponseWriter, r *http.Request) {
	if !h.apiBody(w, r, &struct{}{}) {
		return
	}
	id, err := h.deps.Runner.StartConnectionTest(h.BackgroundContext(), r.PathValue("id"))
	h.apiAccepted(w, id, err)
}
func (h *handler) apiCollectSource(w http.ResponseWriter, r *http.Request) {
	if !h.apiBody(w, r, &struct{}{}) {
		return
	}
	id, err := h.deps.Runner.StartCollection(h.BackgroundContext(), r.PathValue("id"))
	h.apiAccepted(w, id, err)
}
func (h *handler) apiRegisterDisk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		FilesystemUUID string `json:"filesystemUuid"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	id, err := h.deps.Media.Register(r.Context(), body.FilesystemUUID, body.Name)
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 201, struct {
		ID string `json:"id"`
	}{id})
}
func (h *handler) apiUpdateDisk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		h.apiFailure(w, 400, "enabled_required", "CONFIGURATION", "A full disk settings replacement requires enabled.")
		return
	}
	if err := h.deps.Media.UpdateSettings(r.Context(), r.PathValue("id"), body.Name, *body.Enabled); err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		OK bool `json:"ok"`
	}{true})
}
func (h *handler) apiMountDisk(w http.ResponseWriter, r *http.Request) {
	if !h.apiBody(w, r, &struct{}{}) {
		return
	}
	if err := h.deps.Media.Mount(h.BackgroundContext(), r.PathValue("id")); err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		OK bool `json:"ok"`
	}{true})
}
func (h *handler) apiUnmountDisk(w http.ResponseWriter, r *http.Request) {
	if !h.apiBody(w, r, &struct{}{}) {
		return
	}
	if err := h.deps.Media.SyncAndUnmount(h.BackgroundContext(), r.PathValue("id")); err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, struct {
		OK bool `json:"ok"`
	}{true})
}

type setupInputDTO struct {
	DiskID             string `json:"diskId"`
	Mode               string `json:"mode"`
	Password           string `json:"password"`
	EscrowAcknowledged bool   `json:"escrowAcknowledged"`
}

func (h *handler) apiCreateRepository(w http.ResponseWriter, r *http.Request) {
	var body setupInputDTO
	if !h.apiBody(w, r, &body) {
		return
	}
	h.apiSetup(w, r, body)
}
func (h *handler) apiInitializeRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password           string `json:"password"`
		EscrowAcknowledged bool   `json:"escrowAcknowledged"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	v, err := h.deps.Backups.RepositoryDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiSetup(w, r, setupInputDTO{v.Repository.DiskID, string(backup.SetupInitialize), body.Password, body.EscrowAcknowledged})
}
func (h *handler) apiSetup(w http.ResponseWriter, r *http.Request, body setupInputDTO) {
	// Escrow, identity, exclusivity, existing-directory and repository checks
	// remain entirely in StartRepositorySetup. No automatic initialization.
	id, err := h.deps.Backups.StartRepositorySetup(h.BackgroundContext(), backup.SetupInput{
		DiskID: body.DiskID, Mode: backup.SetupMode(body.Mode), Password: body.Password, EscrowAcknowledged: body.EscrowAcknowledged,
	})
	h.apiAccepted(w, id, err)
}
func (h *handler) apiBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceIDs []string `json:"sourceIds"`
	}
	if !h.apiBody(w, r, &body) {
		return
	}
	id, err := h.deps.Backups.StartBackup(h.BackgroundContext(), r.PathValue("id"), body.SourceIDs)
	h.apiAccepted(w, id, err)
}
func (h *handler) apiRecover(w http.ResponseWriter, r *http.Request) {
	if !h.apiBody(w, r, &struct{}{}) {
		return
	}
	id, err := h.deps.Backups.StartRepositoryRecovery(h.BackgroundContext(), r.PathValue("id"))
	h.apiAccepted(w, id, err)
}
