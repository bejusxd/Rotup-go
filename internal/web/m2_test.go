package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/collector"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/staging"
)

// fakeBackend implements every M2 seam so the HTTP layer can be exercised
// without a database, a transfer engine, or background goroutines.
type fakeBackend struct {
	sources   []domain.Source
	runs      []domain.Run
	results   map[string]*domain.SourceResult
	batches   map[string][]domain.RevisionBatch
	busy      map[string]bool
	credRef   domain.CredentialRef
	events    []domain.Event
	createErr error

	created      []collector.SourceInput
	updated      []collector.SourceInput
	passwords    []string
	cleared      []string
	startedTests []string
	startedRuns  []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		results: make(map[string]*domain.SourceResult),
		batches: make(map[string][]domain.RevisionBatch),
		busy:    make(map[string]bool),
	}
}

func (f *fakeBackend) ListSources(context.Context) ([]domain.Source, error) { return f.sources, nil }

func (f *fakeBackend) Source(_ context.Context, id string) (*domain.Source, error) {
	for i := range f.sources {
		if f.sources[i].ID == id {
			return &f.sources[i], nil
		}
	}
	return nil, nil
}

func (f *fakeBackend) CredentialRef(context.Context, string) (domain.CredentialRef, error) {
	return f.credRef, nil
}

func (f *fakeBackend) Run(_ context.Context, id string) (*domain.Run, error) {
	for i := range f.runs {
		if f.runs[i].ID == id {
			return &f.runs[i], nil
		}
	}
	return nil, nil
}

func (f *fakeBackend) ListRuns(context.Context, int) ([]domain.Run, error) { return f.runs, nil }

func (f *fakeBackend) ListRunsForSource(_ context.Context, sourceID string, _ int) ([]domain.Run, error) {
	var result []domain.Run
	for _, run := range f.runs {
		if run.SourceID == sourceID {
			result = append(result, run)
		}
	}
	return result, nil
}

func (f *fakeBackend) LatestCollectionForSource(_ context.Context, sourceID string) (*domain.Run, error) {
	for i := range f.runs {
		if f.runs[i].SourceID == sourceID && f.runs[i].Kind == domain.RunKindCollection {
			return &f.runs[i], nil
		}
	}
	return nil, nil
}

func (f *fakeBackend) SourceResult(_ context.Context, runID, _ string) (*domain.SourceResult, error) {
	return f.results[runID], nil
}

func (f *fakeBackend) ListRevisionBatches(_ context.Context, sourceID string, _ int) ([]domain.RevisionBatch, error) {
	return f.batches[sourceID], nil
}

func (f *fakeBackend) RevisionBatchForRun(_ context.Context, runID string) (*domain.RevisionBatch, error) {
	for _, batches := range f.batches {
		for _, batch := range batches {
			if batch.CollectionRunID == runID {
				return &batch, nil
			}
		}
	}
	return nil, nil
}

func (f *fakeBackend) ListEvents(context.Context, int) ([]domain.Event, error) { return f.events, nil }

func (f *fakeBackend) Create(_ context.Context, input collector.SourceInput) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, input)
	return "created-id", nil
}

func (f *fakeBackend) Update(_ context.Context, _ string, input collector.SourceInput) error {
	f.updated = append(f.updated, input)
	return nil
}

func (f *fakeBackend) ReplaceCredential(_ context.Context, _, password string) error {
	f.passwords = append(f.passwords, password)
	return nil
}

func (f *fakeBackend) ClearCredential(_ context.Context, sourceID string) error {
	f.cleared = append(f.cleared, sourceID)
	return nil
}

func (f *fakeBackend) StartConnectionTest(_ context.Context, sourceID string) (string, error) {
	f.startedTests = append(f.startedTests, sourceID)
	return "test-run-id", nil
}

func (f *fakeBackend) StartCollection(_ context.Context, sourceID string) (string, error) {
	f.startedRuns = append(f.startedRuns, sourceID)
	return "collection-run-id", nil
}

func (f *fakeBackend) Busy(sourceID string) bool { return f.busy[sourceID] }

func (f *fakeBackend) ApplicationStatus() string            { return "running" }
func (f *fakeBackend) DatabaseHealth(context.Context) error { return nil }
func (f *fakeBackend) LatestRun(context.Context) (*domain.Run, error) {
	if len(f.runs) == 0 {
		return nil, nil
	}
	return &f.runs[0], nil
}

const testSourceID = "11111111-1111-4111-8111-111111111111"

func testSourceRow() domain.Source {
	return domain.Source{
		ID:                   testSourceID,
		Name:                 "documents",
		Host:                 "fileserver.example",
		ShareName:            "docs",
		Subdirectory:         "reports",
		Username:             "reader",
		StagingRelativePath:  "documents",
		Enabled:              true,
		ReconciliationPolicy: domain.PolicyArchiveThenSync,
		StagingStatus:        domain.StagingStable,
		StagingStateVersion:  3,
	}
}

type testServer struct {
	handler http.Handler
	backend *fakeBackend
	layout  *staging.Layout
	token   string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	backend := newFakeBackend()
	backend.sources = []domain.Source{testSourceRow()}

	layout, err := staging.NewLayout(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatalf("staging layout: %v", err)
	}
	handler, err := NewHandler(Deps{
		Version: "dev",
		Status:  backend,
		Repo:    backend,
		Admin:   backend,
		Runner:  backend,
		Staging: layout,
		Engine:  func() EngineStatus { return EngineStatus{Name: "rclone", Version: "1.66.0", Available: true} },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server := &testServer{handler: handler, backend: backend, layout: layout}
	server.token = server.fetchToken(t, "/legacy/sources/new")
	return server
}

var tokenPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// fetchToken loads a page and returns the CSRF token it issued.
func (s *testServer) fetchToken(t *testing.T, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)

	match := tokenPattern.FindStringSubmatch(response.Body.String())
	if len(match) < 2 {
		t.Fatalf("no CSRF token in %s: %s", path, response.Body.String())
	}
	cookie := response.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Value != match[1] {
		t.Fatal("the form token must match the cookie the response set")
	}
	return match[1]
}

func (s *testServer) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	return response
}

func (s *testServer) post(t *testing.T, path string, form url.Values, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	if withToken {
		form.Set(csrfFieldName, s.token)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withToken {
		request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: s.token})
	}
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	return response
}

func validSourceForm() url.Values {
	return url.Values{
		"name":                  {"documents"},
		"host":                  {"fileserver.example"},
		"share_name":            {"docs"},
		"subdirectory":          {"reports"},
		"username":              {"reader"},
		"staging_relative_path": {"documents"},
		"reconciliation_policy": {string(domain.PolicyArchiveThenSync)},
		"exclude_rules":         {"*.tmp\n\ncache/**"},
		"enabled":               {"1"},
	}
}

func TestSourceListRendersConfiguredSources(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.runs = []domain.Run{{
		ID: "run-1", Kind: domain.RunKindCollection, SourceID: testSourceID,
		Status: domain.RunStatusSucceeded, FinishedAt: time.Now().UTC(),
	}}

	response := server.get(t, "/legacy/sources")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, fragment := range []string{"documents", "fileserver.example", "archive_then_sync", "STABLE", "SUCCEEDED"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("source list does not contain %q", fragment)
		}
	}
}

func TestCreateSourceRequiresAFormToken(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := server.post(t, "/legacy/sources", validSourceForm(), false)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a request with no form token", response.Code)
	}
	if len(server.backend.created) != 0 {
		t.Fatal("a request without a form token must not create anything")
	}
}

func TestCreateSourceRejectsAnotherOrigin(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	form := validSourceForm()
	form.Set(csrfFieldName, server.token)
	request := httptest.NewRequest(http.MethodPost, "/legacy/sources", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://evil.example")
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: server.token})
	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want a cross-origin submission to be refused", response.Code)
	}
	if len(server.backend.created) != 0 {
		t.Fatal("a cross-origin submission must not create anything")
	}
}

func TestCreateSourceSucceedsAndRedirects(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := server.post(t, "/legacy/sources", validSourceForm(), true)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a post-redirect-get response", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/legacy/sources/created-id" {
		t.Fatalf("redirect location = %q", location)
	}
	if len(server.backend.created) != 1 {
		t.Fatalf("created %d sources, want 1", len(server.backend.created))
	}
	input := server.backend.created[0]
	if input.Name != "documents" || input.Host != "fileserver.example" || !input.Enabled {
		t.Fatalf("submitted input = %#v", input)
	}
	if len(input.ExcludeRules) != 2 || input.ExcludeRules[1] != "cache/**" {
		t.Fatalf("exclude rules = %#v, want blank lines dropped", input.ExcludeRules)
	}
}

func TestCreateSourceRendersValidationErrors(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.createErr = domain.NewError(domain.ErrClassConfiguration, "staging_overlap",
		"staging destination \"documents\" overlaps another source")

	response := server.post(t, "/legacy/sources", validSourceForm(), true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "overlaps another source") {
		t.Fatalf("the rejection reason is not shown: %s", body)
	}
	// The submitted values are kept so the administrator does not lose input.
	if !strings.Contains(body, `value="fileserver.example"`) {
		t.Fatal("the rejected form must keep what was typed")
	}
}

func TestSourcePageNeverShowsAStoredPassword(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.sources[0].CredentialRefID = "cred-1"
	server.backend.credRef = domain.CredentialRef{
		ID: "cred-1", Provider: "file", ProviderLocator: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Purpose: domain.SecretPurposeSMBPassword,
	}

	response := server.get(t, "/legacy/sources/"+testSourceID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatal("the provider locator must not be rendered")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Fatal("the page must offer a write-only password field")
	}
	if strings.Contains(body, `type="password" name="password" value=`) {
		t.Fatal("the password field must never be pre-filled")
	}
	if !strings.Contains(body, "never displayed again") {
		t.Fatal("the page should state that stored values are not shown")
	}
}

func TestReplaceAndClearCredentialAreExplicitActions(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := server.post(t, "/legacy/sources/"+testSourceID+"/credential",
		url.Values{"password": {"hunter2"}}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect after storing a password", response.Code)
	}
	if len(server.backend.passwords) != 1 || server.backend.passwords[0] != "hunter2" {
		t.Fatalf("stored passwords = %#v", server.backend.passwords)
	}
	// The password must not be echoed back anywhere in the response.
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatal("the response must not echo the submitted password")
	}
	if strings.Contains(response.Header().Get("Location"), "hunter2") {
		t.Fatal("the redirect must not carry the password")
	}

	response = server.post(t, "/legacy/sources/"+testSourceID+"/credential/clear", url.Values{}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect after clearing", response.Code)
	}
	if len(server.backend.cleared) != 1 {
		t.Fatalf("cleared = %#v", server.backend.cleared)
	}

	// Both actions are protected.
	if code := server.post(t, "/legacy/sources/"+testSourceID+"/credential",
		url.Values{"password": {"x"}}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("an unprotected credential replacement returned %d", code)
	}
	if code := server.post(t, "/legacy/sources/"+testSourceID+"/credential/clear",
		url.Values{}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("an unprotected credential clear returned %d", code)
	}
}

func TestManualRunsAreTriggeredAndRedirectToTheirRun(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)

	response := server.post(t, "/legacy/sources/"+testSourceID+"/test", url.Values{}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("Location") != "/legacy/runs/test-run-id" {
		t.Fatalf("redirect = %q", response.Header().Get("Location"))
	}
	if len(server.backend.startedTests) != 1 {
		t.Fatal("the connection test was not started")
	}

	response = server.post(t, "/legacy/sources/"+testSourceID+"/collect", url.Values{}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("Location") != "/legacy/runs/collection-run-id" {
		t.Fatalf("redirect = %q", response.Header().Get("Location"))
	}
	if len(server.backend.startedRuns) != 1 {
		t.Fatal("the collection was not started")
	}

	// Neither action may be triggered without a form token.
	if code := server.post(t, "/legacy/sources/"+testSourceID+"/collect", url.Values{}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("an unprotected collection trigger returned %d", code)
	}
	if len(server.backend.startedRuns) != 1 {
		t.Fatal("an unprotected request must not start a run")
	}
}

func TestRunPagePollsWhileRunningAndStopsWhenTerminal(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.runs = []domain.Run{
		{ID: "run-running", Kind: domain.RunKindCollection, SourceID: testSourceID,
			Status: domain.RunStatusRunning, Summary: "collecting"},
		{ID: "run-done", Kind: domain.RunKindCollection, SourceID: testSourceID,
			Status: domain.RunStatusSucceeded, Summary: "collected 2 files"},
	}
	server.backend.results["run-done"] = &domain.SourceResult{
		RunID: "run-done", SourceID: testSourceID, Status: domain.RunStatusSucceeded,
		FilesCopied: 2, BytesCopied: 2048, StagingStateVersion: 3,
		Warnings: []string{"one file was skipped"},
	}

	running := server.get(t, "/legacy/runs/run-running")
	if !strings.Contains(running.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("a running run must refresh its page")
	}
	if !strings.Contains(running.Body.String(), "closing the browser does not affect it") {
		t.Fatal("the page should explain that the run is independent of the browser")
	}

	done := server.get(t, "/legacy/runs/run-done")
	body := done.Body.String()
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("a finished run must not keep refreshing")
	}
	for _, fragment := range []string{"collected 2 files", "2.0 KiB", "one file was skipped", "SUCCEEDED"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("run page does not contain %q", fragment)
		}
	}

	if server.get(t, "/legacy/runs/does-not-exist").Code != http.StatusNotFound {
		t.Fatal("an unknown run must be a 404")
	}
}

func TestHistoryPageListsRuns(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.runs = []domain.Run{
		{ID: "run-1", Kind: domain.RunKindConnectionTest, Status: domain.RunStatusSucceeded, Summary: "reachable"},
		{ID: "run-2", Kind: domain.RunKindCollection, Status: domain.RunStatusFailed, Summary: "the SMB server did not answer"},
	}

	server.backend.events = []domain.Event{{
		OccurredAt: time.Now().UTC(), Severity: domain.SeverityWarning,
		EventType: "collection.blocked_empty_source", Component: "collector",
		PayloadJSON: `{"staged_files":2}`,
	}}

	response := server.get(t, "/legacy/runs")
	body := response.Body.String()
	for _, fragment := range []string{"CONNECTION_TEST", "COLLECTION", "reachable", "did not answer",
		"collection.blocked_empty_source", "staged_files"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("history does not contain %q", fragment)
		}
	}
}

func TestStagingBrowserIsReadOnlyAndConfined(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	source := testSourceRow()
	current, err := server.layout.CurrentDir(source)
	if err != nil {
		t.Fatalf("current dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(current, "reports"), 0o700); err != nil {
		t.Fatalf("create staging tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(current, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write staged file: %v", err)
	}

	response := server.get(t, "/legacy/sources/"+testSourceID+"/staging")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "reports/") {
		t.Fatalf("the staging listing is missing entries: %s", body)
	}
	if strings.Contains(body, "hello") {
		t.Fatal("staged file contents must never be served")
	}

	// The query value is passed raw so that percent-encoded traversal is
	// decoded by the URL parser exactly as a browser would send it.
	for _, escape := range []string{"..", "../..", "%2e%2e%2f", "%2E%2E%2Fescape", "/etc"} {
		code := server.get(t, "/legacy/sources/"+testSourceID+"/staging?path="+escape).Code
		if code == http.StatusOK {
			t.Fatalf("staging path %q was accepted", escape)
		}
	}

	if code := server.get(t, "/legacy/sources/"+testSourceID+"/staging?area=revisions").Code; code != http.StatusOK {
		t.Fatalf("the revisions area returned %d", code)
	}
	if server.get(t, "/legacy/sources/does-not-exist/staging").Code != http.StatusNotFound {
		t.Fatal("an unknown source must be a 404")
	}
}

func TestDashboardShowsSourcesAndEngineStatus(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := server.get(t, "/legacy/")
	body := response.Body.String()

	for _, fragment := range []string{"ROTUP", "Application status: running", "Database status: ok",
		"rclone 1.66.0", "documents", "STABLE"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("dashboard does not contain %q", fragment)
		}
	}
}

func TestHealthEndpointStillWorksWithTheFullBackend(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := server.get(t, "/healthz")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"overall_status":"ok"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestResponsesCarrySecurityHeaders(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	for _, path := range []string{"/", "/healthz", "/legacy/sources"} {
		response := server.get(t, path)
		policy := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "frame-ancestors 'none'") {
			t.Fatalf("%s has policy %q", path, policy)
		}
		if response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s is missing the nosniff header", path)
		}
	}
}

func TestUntrustedTextIsEscaped(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.sources[0].Name = `<script>alert(1)</script>`
	server.backend.runs = []domain.Run{{
		ID: "run-1", Kind: domain.RunKindCollection, SourceID: testSourceID,
		Status: domain.RunStatusFailed, Summary: `<img src=x onerror=alert(1)>`,
	}}

	for _, path := range []string{"/legacy/sources", "/legacy/runs", "/legacy/runs/run-1"} {
		body := server.get(t, path).Body.String()
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Fatalf("%s rendered unescaped markup", path)
		}
		if strings.Contains(body, "<img src=x onerror") {
			t.Fatalf("%s rendered unescaped markup from run text", path)
		}
	}
}

func TestUpdateSourceSubmitsSettings(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	// The edit form lives on the source page, so its token comes from there.
	server.token = server.fetchToken(t, "/legacy/sources/"+testSourceID)

	form := validSourceForm()
	form.Set("name", "renamed")
	form.Del("enabled")
	response := server.post(t, "/legacy/sources/"+testSourceID, form, true)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(server.backend.updated) != 1 {
		t.Fatalf("updated %d sources, want 1", len(server.backend.updated))
	}
	if server.backend.updated[0].Name != "renamed" || server.backend.updated[0].Enabled {
		t.Fatalf("submitted input = %#v", server.backend.updated[0])
	}
}

func TestSourcePageWarnsWhenRecoveryIsRequired(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.sources[0].StagingStatus = domain.StagingRecoveryRequired

	body := server.get(t, "/legacy/sources/"+testSourceID).Body.String()
	if !strings.Contains(body, "RECOVERY_REQUIRED") {
		t.Fatal("the staging state must be shown")
	}
	if !strings.Contains(body, "not reported as up to date") {
		t.Fatal("the page must say plainly that the staged view cannot be trusted")
	}
}

func TestRevisionBatchCompletionIsShown(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.backend.batches[testSourceID] = []domain.RevisionBatch{
		{ID: "batch-1", SourceID: testSourceID, CollectionRunID: "run-1",
			RelativePath: "revisions/x/y", ItemCount: 2, ByteCount: 100,
			ReconciliationCompleted: false, CreatedAt: time.Now().UTC()},
	}

	body := server.get(t, "/legacy/sources/"+testSourceID).Body.String()
	if !strings.Contains(body, "revisions/x/y") {
		t.Fatal("the revision batch is not listed")
	}
	if !strings.Contains(body, "may be incomplete") {
		t.Fatal("an incomplete batch must be labelled as such")
	}
}
