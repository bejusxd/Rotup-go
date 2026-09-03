//go:build !linux

package app

import (
	"log/slog"

	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/platform/unsupported"
	"github.com/bejusxd/rotup/internal/ports"
)

// newMediaPlatform selects adapters that decline media work. Production v1 is
// Linux; other platforms build and run the rest of the daemon for development,
// and say plainly that removable media is not managed here rather than
// pretending otherwise.
func newMediaPlatform(_ config.Config, _ ports.ProcessRunner,
	logger *slog.Logger) (ports.MediaEnumerator, ports.MountManager) {

	logger.Info("removable media management is not available on this platform",
		"reason", unsupported.Reason)
	return unsupported.NewEnumerator(), unsupported.NewMountManager()
}
