// Package media holds the contract shared between rotupd and the privileged
// mount helper: the fixed mount root, the helper's subcommand names, its
// machine-readable result shape, and the validators both sides apply.
//
// It is deliberately portable and dependency-free so that the helper stays tiny
// and so that the validation both sides rely on can be tested on any platform.
// Nothing here performs privileged work.
//
// The central rule this package exists to enforce: no administrator-supplied
// path, mount option, or device node ever crosses into privileged code. The
// helper receives a ROTUP-generated disk identifier, a filesystem UUID, and a
// filesystem type from a fixed allowlist, and derives everything else itself.
package media

import (
	"errors"
	"fmt"
	"strings"
)

// MountRoot is the service-owned volatile directory below which every ROTUP
// mount lives. It is a compile-time constant on purpose: making it
// configurable would mean a path crossing into the privileged helper, which is
// exactly what the helper contract is designed to prevent.
const MountRoot = "/run/rotup/media"

// Helper subcommands. The helper accepts nothing else.
const (
	CommandMount   = "mount"
	CommandSync    = "sync"
	CommandUnmount = "unmount"
)

// Helper flags.
const (
	FlagDiskID = "--disk-id"
	FlagFSUUID = "--fs-uuid"
	FlagFSType = "--fstype"
)

// Mode bits the helper applies to the directories it owns.
const (
	// MountRootMode keeps the root traversable only by the service identity.
	MountRootMode = 0o750
	// MountPointMode is applied to each per-disk mount point.
	MountPointMode = 0o750
)

// Result is the helper's machine-readable answer on stdout. A helper that
// cannot produce this has failed, and the caller must not infer success from
// an exit status alone.
type Result struct {
	// OK is true only when the requested operation completed and the helper
	// verified it.
	OK bool `json:"ok"`
	// Operation echoes the subcommand.
	Operation string `json:"operation"`
	// MountPoint is the path the helper derived for this disk.
	MountPoint string `json:"mount_point"`
	// ResolvedDevice is the device node the UUID resolved to at the moment of
	// the operation. It is a diagnostic only and must never be persisted as
	// identity.
	ResolvedDevice string `json:"resolved_device,omitempty"`
	// FilesystemUUID is what the helper read back from the mounted filesystem
	// after mounting. The caller compares it with what it asked for.
	FilesystemUUID string `json:"filesystem_uuid,omitempty"`
	// FilesystemType is what the kernel reports for the completed mount.
	FilesystemType string `json:"filesystem_type,omitempty"`
	// Mounted reports whether a ROTUP mount is in place at MountPoint after
	// the operation.
	Mounted bool `json:"mounted"`
	// ErrorCode and ErrorMessage carry a typed, safe failure reason.
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// ErrInvalidArgument is returned by the validators.
var ErrInvalidArgument = errors.New("invalid helper argument")

// ValidateDiskID accepts only the UUID form ROTUP generates for its own
// identifiers. This is the value that becomes a path segment below MountRoot,
// so it is the single most important thing to constrain.
func ValidateDiskID(value string) error {
	if len(value) != 36 {
		return fmt.Errorf("%w: disk id must be a ROTUP identifier", ErrInvalidArgument)
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return fmt.Errorf("%w: disk id must be a ROTUP identifier", ErrInvalidArgument)
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return fmt.Errorf("%w: disk id must be a ROTUP identifier", ErrInvalidArgument)
			}
		}
	}
	return nil
}

// ValidateFilesystemUUID accepts the hexadecimal-and-dash forms real
// filesystems use. It is an identity to resolve from fresh block-device
// observations, never a device path or an option for a privileged command.
func ValidateFilesystemUUID(value string) error {
	if value == "" || len(value) > 64 {
		return fmt.Errorf("%w: filesystem uuid has an unacceptable length", ErrInvalidArgument)
	}
	for _, r := range value {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex && r != '-' {
			return fmt.Errorf("%w: filesystem uuid contains an unacceptable character", ErrInvalidArgument)
		}
	}
	return nil
}

// AllowedFilesystems is the helper's own copy of the supported filesystem set.
// It duplicates the domain's list deliberately: the privileged side must not
// depend on the caller having filtered correctly.
var AllowedFilesystems = []string{"ext4"}

// ValidateFilesystemType accepts only an allowlisted filesystem type.
func ValidateFilesystemType(value string) error {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, allowed := range AllowedFilesystems {
		if normalized == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: filesystem type %q is not one ROTUP mounts", ErrInvalidArgument, value)
}

// MountPointFor derives the mount point for a disk. Both sides compute it the
// same way from the validated disk identifier, so the path is never
// transmitted and can never be chosen by a caller.
func MountPointFor(diskID string) (string, error) {
	if err := ValidateDiskID(diskID); err != nil {
		return "", err
	}
	return MountRoot + "/" + strings.ToLower(diskID), nil
}
