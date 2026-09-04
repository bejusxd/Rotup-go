package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedSPAAndNestedRoutes(t *testing.T) {
	h := newTestHandler(t, fakeStatusSource{applicationStatus: "running"})
	var index string
	for _, path := range []string{"/", "/sources", "/sources/source-id", "/sources/source-id/edit", "/disks/disk-id", "/repositories/repo-id", "/runs/run-id", "/debug"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatalf("SPA route %s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("index must revalidate on new builds")
		}
		if index != "" && index != w.Body.String() {
			t.Fatal("nested route did not receive the SPA entry")
		}
		index = w.Body.String()
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("unsafe or unusable SPA CSP: %s", csp)
		}
	}
	paths := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(paths) < 2 {
		t.Fatal("embedded build has no script/styles")
	}
	for _, path := range paths {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path[1], nil))
		if w.Code != 200 || w.Body.Len() == 0 || strings.Contains(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("asset %s: %d %s", path[1], w.Code, w.Header())
		}
	}
}

func TestSPACannotSwallowAPIErrorsMissingAssetsOrPosts(t *testing.T) {
	h := newTestHandler(t, fakeStatusSource{applicationStatus: "running"})
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/v1/does-not-exist", 404}, {"GET", "/api/v1/debug/status", 404}, {"POST", "/api/v1/system", 405},
		{"GET", "/assets/not-a-real-file.js", 404}, {"POST", "/sources", 405}, {"GET", "/healthz", 200},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status || strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatalf("%s %s swallowed: %d %s", test.method, test.path, w.Code, w.Body.String())
		}
		if strings.HasPrefix(test.path, "/api/") && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatal("API returned non-JSON")
		}
	}
}

func TestLegacyLinksAndFormsStayUnderLegacy(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/legacy/", "/legacy/sources", "/legacy/sources/new", "/legacy/sources/" + testSourceID, "/legacy/runs"} {
		w := s.get(t, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, w.Code)
		}
		links := regexp.MustCompile(`(?:href|action)="([^"]+)"`).FindAllStringSubmatch(w.Body.String(), -1)
		for _, link := range links {
			if link[1] != "/" && !strings.HasPrefix(link[1], "/legacy/") {
				t.Fatalf("legacy link escaped: %s", link[1])
			}
		}
	}
}
