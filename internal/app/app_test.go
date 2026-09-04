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
	spa, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET SPA: %v", err)
	}
	spaBody, err := io.ReadAll(spa.Body)
	_ = spa.Body.Close()
	if err != nil || spa.StatusCode != http.StatusOK || !strings.Contains(string(spaBody), `id="root"`) {
		t.Fatalf("embedded SPA failed: status=%d error=%v body=%s", spa.StatusCode, err, spaBody)
	}
	dashboard, err := client.Get("http://" + listener.Addr().String() + "/legacy/")
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
	case <-time.After(15 * time.Second):
		// The budget is deliberately looser than the daemon's own bounded
		// graceful shutdown timeout; a tighter bound tests machine load rather
		// than shutdown behaviour.
		t.Fatal("application did not shut down within its graceful shutdown budget")
	}
}

func TestShutdownDrainsAnAcceptedConnectionWithoutHeaders(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	application, err := New(context.Background(), config.Config{
		DatabasePath: filepath.Join(root, "rotup.db"), StagingRoot: filepath.Join(root, "staging"),
		SecretRoot: filepath.Join(root, "secrets"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	accepted := make(chan struct{}, 1)
	application.server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted <- struct{}{}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- application.serve(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("the server did not accept the connection")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle connection caused failed shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown did not drain the idle connection")
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
