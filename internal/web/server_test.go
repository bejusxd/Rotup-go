package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

type fakeStatusSource struct {
	applicationStatus string
	databaseError     error
	latestRun         *domain.Run
}

func (f fakeStatusSource) ApplicationStatus() string {
	return f.applicationStatus
}

func (f fakeStatusSource) DatabaseHealth(context.Context) error {
	return f.databaseError
}

func (f fakeStatusSource) LatestRun(context.Context) (*domain.Run, error) {
	return f.latestRun, nil
}

func TestHealthEndpoint(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t, fakeStatusSource{applicationStatus: "running"})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, fragment := range []string{`"overall_status":"ok"`, `"database_status":"ok"`, `"application_status":"running"`} {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("response body %q does not contain %q", response.Body.String(), fragment)
		}
	}
}

func TestHealthEndpointReportsDatabaseFailure(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t, fakeStatusSource{
		applicationStatus: "running",
		databaseError:     errors.New("database unavailable"),
	})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(response.Body.String(), `"database_status":"unavailable"`) {
		t.Fatalf("unexpected response body: %q", response.Body.String())
	}
}

func TestDashboardShowsLatestFakeRun(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t, fakeStatusSource{
		applicationStatus: "running",
		latestRun: &domain.Run{
			Kind:       domain.RunKindDemo,
			Status:     domain.RunStatusSucceeded,
			Summary:    "M1 fake run — not a backup",
			FinishedAt: time.Date(2026, time.September, 3, 8, 0, 0, 0, time.UTC),
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	for _, fragment := range []string{"ROTUP", "Version: dev", "Application status: running", "Database status: ok", "M1 fake run"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("dashboard does not contain %q: %s", fragment, body)
		}
	}
}

func newTestHandler(t *testing.T, source StatusSource) http.Handler {
	t.Helper()
	handler, err := NewHandler(source, "dev")
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}
