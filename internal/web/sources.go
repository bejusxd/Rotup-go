package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/bejusxd/rotup/internal/collector"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/staging"
)

// maxFormBytes bounds an administration form body.
const maxFormBytes = 64 * 1024

// sourceRow is the list view model.
type sourceRow struct {
	Source        domain.Source
	Busy          bool
	HasCredential bool
	LastRun       *domain.Run
}

// sourceFormData backs both the create and the edit form.
type sourceFormData struct {
	IsNew         bool
	SourceID      string
	Values        collector.SourceInput
	IncludeText   string
	ExcludeText   string
	Policies      []domain.ReconciliationPolicy
	HasCredential bool
}

// sourceDetail is the source page view model.
type sourceDetail struct {
	Form          sourceFormData
	Source        domain.Source
	Busy          bool
	HasCredential bool
	Credential    domain.CredentialRef
	Runs          []domain.Run
	LatestResult  *domain.SourceResult
	Batches       []domain.RevisionBatch
}

func (h *handler) sourceRows(ctx context.Context) ([]sourceRow, error) {
	sources, err := h.deps.Repo.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]sourceRow, 0, len(sources))
	for _, source := range sources {
		row := sourceRow{
			Source:        source,
			HasCredential: source.HasCredential(),
		}
		if h.deps.Runner != nil {
			row.Busy = h.deps.Runner.Busy(source.ID)
		}
		latest, err := h.deps.Repo.LatestCollectionForSource(ctx, source.ID)
		if err != nil {
			return nil, err
		}
		row.LastRun = latest
		rows = append(rows, row)
	}
	return rows, nil
}

func (h *handler) listSources(response http.ResponseWriter, request *http.Request) {
	rows, err := h.sourceRows(request.Context())
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "sources", nil, "", safeError(err))
		return
	}
	h.render(response, request, http.StatusOK, "sources", rows)
}

func (h *handler) newSourceForm(response http.ResponseWriter, request *http.Request) {
	form := sourceFormData{
		IsNew: true,
		Values: collector.SourceInput{
			Enabled:              true,
			ReconciliationPolicy: domain.PolicyArchiveThenSync,
		},
		Policies: domain.SupportedPolicies,
	}
	h.render(response, request, http.StatusOK, "source_form", form)
}

func (h *handler) createSource(response http.ResponseWriter, request *http.Request) {
	input, form, failure := h.readSourceForm(response, request)
	form.IsNew = true
	if failure != "" {
		h.renderWithMessages(response, request, http.StatusBadRequest, "source_form", form, "", failure)
		return
	}
	id, err := h.deps.Admin.Create(request.Context(), input)
	if err != nil {
		h.renderWithMessages(response, request, http.StatusBadRequest, "source_form", form, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/sources/"+id, http.StatusSeeOther)
}

func (h *handler) updateSource(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	input, form, failure := h.readSourceForm(response, request)
	form.SourceID = id
	if failure != "" {
		h.renderDetailWithForm(response, request, id, form, http.StatusBadRequest, "", failure)
		return
	}
	if err := h.deps.Admin.Update(request.Context(), id, input); err != nil {
		h.renderDetailWithForm(response, request, id, form, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/sources/"+id, http.StatusSeeOther)
}

func (h *handler) showSource(response http.ResponseWriter, request *http.Request) {
	h.renderDetailWithForm(response, request, request.PathValue("id"), sourceFormData{}, http.StatusOK, "", "")
}

// renderDetailWithForm renders the source page. When form carries values from a
// rejected submission they are kept so the administrator does not lose input.
func (h *handler) renderDetailWithForm(response http.ResponseWriter, request *http.Request,
	id string, form sourceFormData, status int, notice, failure string) {

	ctx := request.Context()
	source, err := h.deps.Repo.Source(ctx, id)
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "sources", nil, "", safeError(err))
		return
	}
	if source == nil {
		http.NotFound(response, request)
		return
	}

	detail := sourceDetail{
		Source:        *source,
		HasCredential: source.HasCredential(),
	}
	if h.deps.Runner != nil {
		detail.Busy = h.deps.Runner.Busy(source.ID)
	}
	if source.HasCredential() {
		ref, err := h.deps.Repo.CredentialRef(ctx, source.ID)
		if err == nil {
			detail.Credential = ref
		}
	}
	if runs, err := h.deps.Repo.ListRunsForSource(ctx, source.ID, 15); err == nil {
		detail.Runs = runs
	}
	if latest, err := h.deps.Repo.LatestCollectionForSource(ctx, source.ID); err == nil && latest != nil {
		if result, err := h.deps.Repo.SourceResult(ctx, latest.ID, source.ID); err == nil {
			detail.LatestResult = result
		}
	}
	if batches, err := h.deps.Repo.ListRevisionBatches(ctx, source.ID, 20); err == nil {
		detail.Batches = batches
	}

	if form.Values.Name == "" && form.Values.Host == "" {
		form = formFromSource(*source)
	}
	form.IsNew = false
	form.SourceID = source.ID
	form.Policies = domain.SupportedPolicies
	form.HasCredential = source.HasCredential()
	detail.Form = form

	h.renderWithMessages(response, request, status, "source", detail, notice, failure)
}

func formFromSource(source domain.Source) sourceFormData {
	return sourceFormData{
		SourceID: source.ID,
		Values: collector.SourceInput{
			Name:                 source.Name,
			Host:                 source.Host,
			ShareName:            source.ShareName,
			Subdirectory:         source.Subdirectory,
			Username:             source.Username,
			StagingRelativePath:  source.StagingRelativePath,
			Enabled:              source.Enabled,
			IncludeRules:         source.IncludeRules,
			ExcludeRules:         source.ExcludeRules,
			ReconciliationPolicy: source.ReconciliationPolicy,
		},
		IncludeText: strings.Join(source.IncludeRules, "\n"),
		ExcludeText: strings.Join(source.ExcludeRules, "\n"),
	}
}

// readSourceForm parses and CSRF-checks a source submission.
func (h *handler) readSourceForm(_ http.ResponseWriter, request *http.Request) (collector.SourceInput, sourceFormData, string) {
	request.Body = http.MaxBytesReader(nil, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		return collector.SourceInput{}, sourceFormData{Policies: domain.SupportedPolicies},
			"the submitted form could not be read"
	}
	if failure := h.csrf.check(request); failure != "" {
		return collector.SourceInput{}, sourceFormData{Policies: domain.SupportedPolicies}, failure
	}

	includeText := request.PostFormValue("include_rules")
	excludeText := request.PostFormValue("exclude_rules")
	input := collector.SourceInput{
		Name:                 request.PostFormValue("name"),
		Host:                 request.PostFormValue("host"),
		ShareName:            request.PostFormValue("share_name"),
		Subdirectory:         request.PostFormValue("subdirectory"),
		Username:             request.PostFormValue("username"),
		StagingRelativePath:  request.PostFormValue("staging_relative_path"),
		Enabled:              request.PostFormValue("enabled") != "",
		IncludeRules:         splitRules(includeText),
		ExcludeRules:         splitRules(excludeText),
		ReconciliationPolicy: domain.ReconciliationPolicy(request.PostFormValue("reconciliation_policy")),
	}
	form := sourceFormData{
		Values:      input,
		IncludeText: includeText,
		ExcludeText: excludeText,
		Policies:    domain.SupportedPolicies,
	}
	return input, form, ""
}

func splitRules(value string) []string {
	var rules []string
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			rules = append(rules, trimmed)
		}
	}
	return rules
}

func (h *handler) replaceCredential(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	request.Body = http.MaxBytesReader(nil, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "",
			"the submitted form could not be read")
		return
	}
	if failure := h.csrf.check(request); failure != "" {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", failure)
		return
	}

	password := request.PostFormValue("password")
	if err := h.deps.Admin.ReplaceCredential(request.Context(), id, password); err != nil {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", safeError(err))
		return
	}
	// Post-redirect-get: the password must not survive in a resubmittable form.
	http.Redirect(response, request, "/legacy/sources/"+id, http.StatusSeeOther)
}

func (h *handler) clearCredential(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if failure := h.checkMutation(request); failure != "" {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", failure)
		return
	}
	if err := h.deps.Admin.ClearCredential(request.Context(), id); err != nil {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/sources/"+id, http.StatusSeeOther)
}

func (h *handler) startConnectionTest(response http.ResponseWriter, request *http.Request) {
	h.startRun(response, request, h.deps.Runner.StartConnectionTest)
}

func (h *handler) startCollection(response http.ResponseWriter, request *http.Request) {
	h.startRun(response, request, h.deps.Runner.StartCollection)
}

func (h *handler) startRun(response http.ResponseWriter, request *http.Request,
	start func(context.Context, string) (string, error)) {

	id := request.PathValue("id")
	if failure := h.checkMutation(request); failure != "" {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", failure)
		return
	}
	// The run is started against the request context only for reading
	// configuration; the work itself is bound to the daemon lifetime, so a
	// browser that disconnects cannot cancel or corrupt it.
	runID, err := start(request.Context(), id)
	if err != nil {
		h.renderDetailWithForm(response, request, id, sourceFormData{}, http.StatusBadRequest, "", safeError(err))
		return
	}
	http.Redirect(response, request, "/legacy/runs/"+runID, http.StatusSeeOther)
}

func (h *handler) checkMutation(request *http.Request) string {
	request.Body = http.MaxBytesReader(nil, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		return "the submitted form could not be read"
	}
	return h.csrf.check(request)
}

// stagingView is the read-only staging inspection model.
type stagingView struct {
	Source    domain.Source
	Area      string
	Listing   staging.Listing
	Batches   []domain.RevisionBatch
	CurrentOK bool
}

// browseStaging renders a bounded, read-only listing of a source's staging
// tree. It never serves file contents: the goal is to let an administrator
// confirm what collection produced, not to expose staged data over HTTP.
func (h *handler) browseStaging(response http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	source, err := h.deps.Repo.Source(ctx, request.PathValue("id"))
	if err != nil {
		h.renderWithMessages(response, request, http.StatusInternalServerError, "sources", nil, "", safeError(err))
		return
	}
	if source == nil {
		http.NotFound(response, request)
		return
	}

	area := request.URL.Query().Get("area")
	if area != "revisions" {
		area = "current"
	}
	view := stagingView{Source: *source, Area: area}

	var base string
	if area == "revisions" {
		base, err = h.deps.Staging.SourceRevisionsDir(source.ID)
	} else {
		base, err = h.deps.Staging.CurrentDir(*source)
	}
	if err != nil {
		h.renderWithMessages(response, request, http.StatusBadRequest, "staging", view, "", safeError(err))
		return
	}

	listing, err := h.deps.Staging.List(base, request.URL.Query().Get("path"))
	if err != nil {
		h.renderWithMessages(response, request, http.StatusBadRequest, "staging", view, "", safeError(err))
		return
	}
	view.Listing = listing
	view.CurrentOK = true
	if batches, err := h.deps.Repo.ListRevisionBatches(ctx, source.ID, 20); err == nil {
		view.Batches = batches
	}
	h.render(response, request, http.StatusOK, "staging", view)
}
