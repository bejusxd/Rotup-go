package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

type exactBatchBackend struct{ *fakeBackend }

func (f exactBatchBackend) ListRevisionBatches(context.Context, string, int) ([]domain.RevisionBatch, error) {
	return nil, errors.New("a run detail must not search a bounded source history")
}

func TestAPIRunFetchesItsExactRevisionBatch(t *testing.T) {
	backend := newFakeBackend()
	backend.sources = []domain.Source{{ID: "source-one"}}
	backend.runs = []domain.Run{{ID: "old-run", SourceID: "source-one", Kind: domain.RunKindCollection, Status: domain.RunStatusSucceeded}}
	backend.batches["source-one"] = []domain.RevisionBatch{{ID: "old-batch", CollectionRunID: "old-run", SourceID: "source-one"}}
	h, err := NewHandler(Deps{Status: backend, Repo: exactBatchBackend{backend}})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, h, "GET", "/api/v1/runs/old-run", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "old-batch") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAPIOperationTargetsAreAllowlisted(t *testing.T) {
	for _, kind := range []domain.RunKind{domain.RunKindVerify, domain.RunKindDiskSetup} {
		run := runInfo(domain.Run{Kind: kind, SummaryJSON: `{"repository_id":"repo-one","disk_id":"disk-one","password":"SECRET-CANARY","raw_output":"SECRET-CANARY"}`})
		encoded, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if run.RepositoryID != "repo-one" || run.DiskID != "disk-one" || strings.Contains(string(encoded), "SECRET-CANARY") {
			t.Fatalf("bad operation target DTO: %s", encoded)
		}
	}
}

func apiRequest(t *testing.T, handler http.Handler, method, path, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if csrf {
		bootstrap := httptest.NewRecorder()
		handler.ServeHTTP(bootstrap, httptest.NewRequest("GET", "/api/v1/system", nil))
		var system systemDTO
		if err := json.Unmarshal(bootstrap.Body.Bytes(), &system); err != nil {
			t.Fatal(err)
		}
		for _, cookie := range bootstrap.Result().Cookies() {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", system.CSRFToken)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestAPIReadsDoNotExposeSecretMetadataOrRawJSON(t *testing.T) {
	s := newTestServer(t)
	s.backend.sources[0].CredentialRefID = "SECRET-REFERENCE-CANARY"
	s.backend.credRef.ProviderLocator = "SECRET-LOCATOR-CANARY"
	s.backend.runs = []domain.Run{{ID: "run-one", SourceID: testSourceID, Kind: domain.RunKindCollection, Status: domain.RunStatusSucceeded, SummaryJSON: `{"password":"RAW-JSON-CANARY"}`}}
	for _, path := range []string{"/api/v1/system", "/api/v1/health", "/api/v1/dashboard", "/api/v1/sources", "/api/v1/sources/" + testSourceID, "/api/v1/runs", "/api/v1/runs/run-one"} {
		t.Run(path, func(t *testing.T) {
			w := apiRequest(t, s.handler, "GET", path, "", false)
			if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			for _, secret := range []string{"SECRET-REFERENCE-CANARY", "SECRET-LOCATOR-CANARY", "RAW-JSON-CANARY"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatalf("secret metadata escaped at %s", path)
				}
			}
		})
	}
	if len(s.backend.created)+len(s.backend.startedRuns)+len(s.backend.startedTests) != 0 {
		t.Fatal("read triggered work")
	}
}

func TestAPIRequiresCSRFEvenForDevMode(t *testing.T) {
	for _, dev := range []bool{false, true} {
		backend := newFakeBackend()
		h, err := NewHandler(Deps{Status: backend, Repo: backend, Admin: backend, Runner: backend, DevMode: dev})
		if err != nil {
			t.Fatal(err)
		}
		w := apiRequest(t, h, "POST", "/api/v1/sources", `{"name":"safe"}`, false)
		if w.Code != 403 || len(backend.created) != 0 {
			t.Fatalf("csrf bypass in dev=%v: %d", dev, w.Code)
		}
		bootstrap := httptest.NewRecorder()
		h.ServeHTTP(bootstrap, httptest.NewRequest("GET", "/api/v1/system", nil))
		var system systemDTO
		_ = json.Unmarshal(bootstrap.Body.Bytes(), &system)
		r := httptest.NewRequest("POST", "/api/v1/sources", strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://attacker.example")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", system.CSRFToken)
		for _, c := range bootstrap.Result().Cookies() {
			r.AddCookie(c)
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || len(backend.created) != 0 {
			t.Fatal("cross-origin mutation accepted")
		}
	}
}

func TestAPIJSONValidationAndMethodErrors(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{`null`, `[]`, `{} {}`, `{"unexpected":"x"}`, `{"name":`} {
		w := apiRequest(t, s.handler, "POST", "/api/v1/sources", body, true)
		if w.Code != 400 {
			t.Fatalf("body %q = %d", body, w.Code)
		}
	}
	w := apiRequest(t, s.handler, "POST", "/api/v1/sources", `{"name":"`+strings.Repeat("x", maxFormBytes)+`"}`, true)
	if w.Code != 413 {
		t.Fatalf("oversized body = %d", w.Code)
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"DELETE", "/api/v1/sources", 405}, {"GET", "/api/v1/unknown", 404}, {"GET", "/api/v1/sources/missing", 404},
	} {
		w := apiRequest(t, s.handler, tc.method, tc.path, "", false)
		if w.Code != tc.code || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if len(s.backend.created) != 0 {
		t.Fatal("invalid input reached the service")
	}
}

func TestAPIMutationsDelegateWithoutReturningPasswords(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/v1/sources", `{"name":"Docs","host":"server","shareName":"docs","stagingRelativePath":"docs"}`, 201},
		{"PUT", "/api/v1/sources/" + testSourceID, `{"name":"Docs","host":"server","shareName":"docs","stagingRelativePath":"docs","enabled":false}`, 200},
		{"POST", "/api/v1/sources/" + testSourceID + "/credential", `{"action":"replace","password":"never-echo-this-canary"}`, 200},
		{"POST", "/api/v1/sources/" + testSourceID + "/credential", `{"action":"clear"}`, 200},
		{"POST", "/api/v1/sources/" + testSourceID + "/test", `{}`, 202},
		{"POST", "/api/v1/sources/" + testSourceID + "/collect", `{}`, 202},
	} {
		w := apiRequest(t, s.handler, tc.method, tc.path, tc.body, true)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "never-echo-this-canary") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if len(s.backend.created) != 1 || len(s.backend.updated) != 1 || s.backend.updated[0].Enabled || len(s.backend.passwords) != 1 || len(s.backend.cleared) != 1 || len(s.backend.startedTests) != 1 || len(s.backend.startedRuns) != 1 {
		t.Fatal("unexpected service calls")
	}
	if s.backend.created[0].ReconciliationPolicy != domain.PolicyArchiveThenSync {
		t.Fatal("unsafe default policy")
	}
}

func TestAPIErrorsPreserveTypedSafetyFailures(t *testing.T) {
	s := newTestServer(t)
	s.backend.createErr = domain.NewError(domain.ErrClassIntegrity, "identity_mismatch", "The filesystem UUID does not match.")
	w := apiRequest(t, s.handler, "POST", "/api/v1/sources", `{}`, true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"class":"INTEGRITY"`) || !strings.Contains(w.Body.String(), `"code":"identity_mismatch"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	s.backend.createErr = errors.New("SECRET-RAW-ERROR")
	w = apiRequest(t, s.handler, "POST", "/api/v1/sources", `{}`, true)
	if w.Code != 500 || strings.Contains(w.Body.String(), "SECRET-RAW-ERROR") {
		t.Fatalf("unsafe error: %s", w.Body.String())
	}
}

func TestAPIDebugAndEphemeralPathsAreGated(t *testing.T) {
	for _, dev := range []bool{false, true} {
		backend := newFakeBackend()
		media := &fakeMedia{available: true, snapshot: removable.Snapshot{PlatformAvailable: true, Views: []removable.MediaView{{Disk: testDisk(), Observed: testObservation("/dev/loop-secret-path", testDisk().FilesystemUUID, "ext4"), Status: domain.DiskAvailableUnmounted, Supported: true}}}}
		h, err := NewHandler(Deps{Status: backend, Repo: backend, Media: media, DevMode: dev})
		if err != nil {
			t.Fatal(err)
		}
		w := apiRequest(t, h, "GET", "/api/v1/debug/status", "", false)
		want := 404
		if dev {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("dev=%v debug=%d", dev, w.Code)
		}
		w = apiRequest(t, h, "GET", "/api/v1/disks", "", false)
		if strings.Contains(w.Body.String(), "/dev/loop-secret-path") != dev {
			t.Fatalf("ephemeral path gating failed: %s", w.Body.String())
		}
	}
}

func TestAPIRepositoryAndMediaActionsKeepServiceContextAndSafetyInputs(t *testing.T) {
	b := &fakeBackups{repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryUninitialized)}}
	m := &fakeMedia{available: true, snapshot: removable.Snapshot{Views: []removable.MediaView{{Disk: testDisk(), Status: domain.DiskAvailableUnmounted}}}}
	s := newBackupServer(t, b, m)
	for _, path := range []string{"/api/v1/disks", "/api/v1/disks/" + testDiskID, "/api/v1/repositories", "/api/v1/repositories/" + testRepositoryID} {
		if w := apiRequest(t, s.handler, "GET", path, "", false); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/v1/disks/register", `{"name":"A","filesystemUuid":"aaaa-bbbb"}`, 201},
		{"/api/v1/disks/" + testDiskID + "/mount", `{}`, 200},
		{"/api/v1/disks/" + testDiskID + "/unmount", `{}`, 200},
		{"/api/v1/repositories", `{"diskId":"` + testDiskID + `","mode":"INITIALIZE","password":"repository-secret-canary","escrowAcknowledged":false}`, 202},
		{"/api/v1/repositories/" + testRepositoryID + "/backup", `{"sourceIds":["` + testSourceID + `"]}`, 202},
		{"/api/v1/repositories/" + testRepositoryID + "/recover", `{}`, 202},
	} {
		w := apiRequest(t, s.handler, "POST", tc.path, tc.body, true)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "repository-secret-canary") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	// A facade must not invent escrow acknowledgement. The real service rejects
	// false; this recording fake proves that the handler passes it unchanged.
	if len(b.setups) != 1 || b.setups[0].EscrowAcknowledged {
		t.Fatal("facade changed escrow input")
	}
	for _, ctx := range append(b.contexts, m.contexts...) {
		if ctx.Value(backgroundMarker{}) != true || ctx.Err() != nil {
			t.Fatal("request-scoped service context")
		}
	}
	b.setupErr = domain.NewError(domain.ErrClassConfiguration, domain.RepositoryErrorEscrowNotAcknowledged, "Escrow must be acknowledged.")
	w := apiRequest(t, s.handler, "POST", "/api/v1/repositories", `{"mode":"INITIALIZE"}`, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), domain.RepositoryErrorEscrowNotAcknowledged) {
		t.Fatal(w.Body.String())
	}
}

func TestAPIDashboardFailsClosedOnDatabaseError(t *testing.T) {
	h, err := NewHandler(Deps{Status: fakeStatusSource{applicationStatus: "running", databaseError: errors.New("database canary")}})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, h, "GET", "/api/v1/dashboard", "", false)
	if w.Code != 503 || strings.Contains(w.Body.String(), "database canary") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
