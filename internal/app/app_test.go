package app

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/domain"
)

func TestApplicationServesAndShutsDownGracefully(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	cfg := config.Config{
		HTTPAddr:     "127.0.0.1:0",
		DatabasePath: filepath.Join(root, "rotup.db"),
		LogLevel:     slog.LevelInfo,
		StagingRoot:  filepath.Join(root, "staging"),
		SecretRoot:   filepath.Join(root, "secrets"),
	}
	application, err := New(context.Background(), cfg, logger, "dev")
	if err != nil {
		t.Fatalf("new application: %v", err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Errorf("close application: %v", err)
		}
	})

	latest, err := application.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("read demo run: %v", err)
	}
	if latest == nil || latest.Kind != domain.RunKindDemo || !strings.Contains(latest.Summary, "not a backup") {
		t.Fatalf("unexpected demo run: %#v", latest)
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- application.serve(ctx, listener)
	}()

	client := &http.Client{Timeout: time.Second}
	response := getEventually(t, client, "http://"+listener.Addr().String()+"/healthz")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	dashboard, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	dashboardBody, readErr := io.ReadAll(dashboard.Body)
	_ = dashboard.Body.Close()
	if readErr != nil {
		t.Fatalf("read dashboard: %v", readErr)
	}
	if dashboard.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d", dashboard.StatusCode, http.StatusOK)
	}
	if !strings.Contains(html.UnescapeString(string(dashboardBody)), latest.Summary) {
		t.Fatalf("dashboard does not show persisted demo run: %s", dashboardBody)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serve after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("application did not shut down within two seconds")
	}
}

func getEventually(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, err := client.Get(url)
		if err == nil {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
