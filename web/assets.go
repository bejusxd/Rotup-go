// Package webui contains the production frontend. Node is needed only to rebuild
// dist, never to run rotupd. Keep generated dist in version control so a plain
// Go build from a clean checkout has the same UI as a release build.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var bundled embed.FS

func Assets() (fs.FS, error) { return fs.Sub(bundled, "dist") }
