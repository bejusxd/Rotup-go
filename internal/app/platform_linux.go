//go:build linux

package app

import (
	"log/slog"

	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/platform/linux"
	"github.com/bejusxd/rotup/internal/platform/unsupported"
	"github.com/bejusxd/rotup/internal/ports"
)

// newMediaPlatform selects the Linux removable-media adapters. This is the one
// place a platform choice is made; nothing below it branches on the operating
// system at runtime.
//
// A daemon with no configured mount helper still enumerates media, so an
// administrator can see what is connected and understand why the mount actions
// are unavailable, rather than facing an empty page with no explanation.
func newMediaPlatform(cfg config.Config, runner ports.ProcessRunner,
	logger *slog.Logger) (ports.MediaEnumerator, ports.MountManager) {

	mounts := linux.NewMountManager(cfg.MountHelperPath, runner)
	if !mounts.Available() {
		logger.Warn("removable media management is unavailable",
			"reason", "no mount helper is configured; set ROTUP_MOUNT_HELPER")
	}
	if cfg.LsblkPath == "" {
		logger.Warn("block device enumeration is unavailable",
			"reason", "no lsblk executable is configured; set ROTUP_LSBLK_PATH")
		return unsupported.NewEnumerator(), mounts
	}
	return linux.NewEnumerator(cfg.LsblkPath, runner), mounts
}
