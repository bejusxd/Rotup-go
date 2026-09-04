package web

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"time"

	webui "github.com/bejusxd/rotup/web"
)

func newSPAHandler() (http.Handler, error) {
	assets, err := webui.Assets()
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(path, "assets/") {
			content, err := fs.ReadFile(assets, path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			// Vite gives each asset a content hash. Missing assets never return HTML.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			if strings.HasSuffix(path, ".js") {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			}
			if strings.HasSuffix(path, ".css") {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
			http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(content))
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", mime.TypeByExtension(".html"))
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	}), nil
}
