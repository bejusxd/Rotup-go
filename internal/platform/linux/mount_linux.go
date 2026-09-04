//go:build linux

package linux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/ports"
)

const (
	mountTimeout   = 60 * time.Second
	syncTimeout    = 10 * time.Minute
	unmountTimeout = 60 * time.Second

	// mountInfoPath is the kernel's authoritative mount table for this process.
	mountInfoPath = "/proc/self/mountinfo"
)

// MountManager implements ports.MountManager by delegating the three
// privileged operations to the rotup-mounthelper binary and by reading the
// mount table directly, which needs no privilege.
type MountManager struct {
	helperPath string
	runner     ports.ProcessRunner
}

// NewMountManager builds the Linux mount manager. helperPath must be an
// explicit path to the privileged helper; an empty path means media management
// is unavailable and every operation reports that plainly.
func NewMountManager(helperPath string, runner ports.ProcessRunner) *MountManager {
	return &MountManager{helperPath: strings.TrimSpace(helperPath), runner: runner}
}

// Available reports whether a mount helper is configured.
func (m *MountManager) Available() bool { return m.helperPath != "" }

// MountPointFor derives where a disk would be mounted.
func (m *MountManager) MountPointFor(diskID string) (string, error) {
	point, err := media.MountPointFor(diskID)
	if err != nil {
		return "", domain.WrapError(domain.ErrClassInvariant, "mount_point_invalid",
			"the disk identifier cannot be turned into a mount point", err)
	}
	return point, nil
}

// Observe reads the kernel mount table. This is the authoritative answer to
// "what is mounted right now"; nothing ROTUP remembers is trusted instead.
func (m *MountManager) Observe(ctx context.Context) ([]domain.MountObservation, error) {
	handle, err := os.Open(mountInfoPath)
	if err != nil {
		return nil, domain.WrapError(domain.ErrClassInternal, "mountinfo_unreadable",
			"the kernel mount table could not be read", err)
	}
	defer handle.Close()
	return parseMountInfo(handle)
}

// parseMountInfo decodes /proc/self/mountinfo. The format is documented in
// proc(5): fields up to a "-" separator describe the mount, and the three
// fields after it are the filesystem type, the mount source, and the
// superblock options.
func parseMountInfo(reader interface{ Read([]byte) (int, error) }) ([]domain.MountObservation, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var observations []domain.MountObservation
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		// Before the separator: at least mount id, parent id, major:minor, root,
		// mount point. After it: filesystem type, source, super options.
		if separator < 5 || len(fields) < separator+3 {
			continue
		}

		observation := domain.MountObservation{
			MountPoint:     unescapeMountField(fields[4]),
			FilesystemType: fields[separator+1],
			Source:         unescapeMountField(fields[separator+2]),
		}
		for _, option := range strings.Split(fields[5], ",") {
			if option == "ro" {
				observation.ReadOnly = true
			}
		}
		observations = append(observations, observation)
	}
	if err := scanner.Err(); err != nil {
		return nil, domain.WrapError(domain.ErrClassInternal, "mountinfo_unreadable",
			"the kernel mount table could not be read", err)
	}
	return observations, nil
}

// unescapeMountField decodes the octal escapes the kernel uses for space, tab,
// newline and backslash in mountinfo paths.
func unescapeMountField(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			var code int
			valid := true
			for _, digit := range value[i+1 : i+4] {
				if digit < '0' || digit > '7' {
					valid = false
					break
				}
				code = code*8 + int(digit-'0')
			}
			if valid {
				builder.WriteByte(byte(code))
				i += 3
				continue
			}
		}
		builder.WriteByte(value[i])
	}
	return builder.String()
}

// Mount asks the helper to mount the disk and returns what it observed. The
// caller is responsible for comparing the returned identity with the one it
// registered: this method does not decide that the mount is trustworthy.
func (m *MountManager) Mount(ctx context.Context, request ports.MountRequest) (ports.MountResult, error) {
	result, err := m.invoke(ctx, mountTimeout, media.CommandMount,
		media.FlagDiskID, request.DiskID,
		media.FlagFSUUID, request.FilesystemUUID,
		media.FlagFSType, request.FilesystemType)
	if err != nil {
		return ports.MountResult{}, err
	}
	return ports.MountResult{
		MountPoint:     result.MountPoint,
		ResolvedDevice: result.ResolvedDevice,
		FilesystemUUID: domain.NormalizeFilesystemUUID(result.FilesystemUUID),
		FilesystemType: domain.NormalizeFilesystemType(result.FilesystemType),
		Mounted:        result.Mounted,
	}, nil
}

// Sync flushes the mounted filesystem.
func (m *MountManager) Sync(ctx context.Context, diskID string) error {
	_, err := m.invoke(ctx, syncTimeout, media.CommandSync, media.FlagDiskID, diskID)
	return err
}

// Unmount unmounts the ROTUP mount and verifies it is gone.
func (m *MountManager) Unmount(ctx context.Context, diskID string) error {
	_, err := m.invoke(ctx, unmountTimeout, media.CommandUnmount, media.FlagDiskID, diskID)
	return err
}

// invoke runs the helper and decodes its result. A helper that exits zero
// without a decodable OK result is treated as a failure, so an unexpected
// binary at the configured path cannot be mistaken for a successful mount.
func (m *MountManager) invoke(ctx context.Context, timeout time.Duration, command string, args ...string) (media.Result, error) {
	if !m.Available() {
		return media.Result{}, domain.NewError(domain.ErrClassConfiguration, "mount_helper_unconfigured",
			"no mount helper is configured; set ROTUP_MOUNT_HELPER")
	}
	// Keep the subcommand separate from the flag/value pairs: treating it as
	// a flag shifts every pair and silently bypasses all value validation.
	if err := validateHelperArguments(command, args); err != nil {
		return media.Result{}, domain.WrapError(domain.ErrClassInvariant, "mount_argument_invalid",
			"a media operation was built with an argument ROTUP will not pass to privileged code", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	process := m.runner.Run(runCtx, ports.CommandSpec{
		Path:            m.helperPath,
		Args:            append([]string{command}, args...),
		Env:             []string{},
		StdoutTailLimit: 64 * 1024,
		StderrTailLimit: 64 * 1024,
	})

	switch {
	case process.TimedOut:
		return media.Result{}, domain.NewError(domain.ErrClassTimeout, "mount_helper_timeout",
			"the media operation took too long and was stopped; the disk may still be mounted")
	case process.Canceled:
		return media.Result{}, domain.NewError(domain.ErrClassCancellation, "mount_helper_canceled",
			"the media operation was canceled; the disk may still be mounted")
	case process.Err != nil:
		return media.Result{}, domain.WrapError(domain.ErrClassExternalTool, "mount_helper_start_failed",
			"the mount helper could not be run", process.Err)
	case !process.Exited:
		return media.Result{}, domain.NewError(domain.ErrClassExternalTool, "mount_helper_no_exit",
			"the mount helper did not report an exit status")
	}

	var result media.Result
	if err := json.Unmarshal(trimToJSON(process.Stdout), &result); err != nil {
		return media.Result{}, domain.WrapError(domain.ErrClassExternalTool, "mount_helper_unparsed",
			"the mount helper produced output ROTUP could not read", err)
	}
	if !result.OK {
		class := domain.ErrClassUnavailable
		if result.ErrorCode == "privilege_required" || result.ErrorCode == "device_access_denied" {
			class = domain.ErrClassConfiguration
		}
		return result, domain.NewError(class, helperErrorCode(result.ErrorCode),
			helperErrorMessage(result))
	}
	if process.ExitCode != 0 {
		// A helper that claims success while exiting non-zero is contradicting
		// itself, and ROTUP believes neither half.
		return result, domain.NewError(domain.ErrClassExternalTool, "mount_helper_contradiction",
			"the mount helper reported success but exited with a failure status")
	}
	return result, nil
}

// validateHelperArguments accepts only the complete invocation shapes ROTUP
// constructs. Missing, extra, unknown or duplicate flags fail closed, before
// any privileged helper process is started.
func validateHelperArguments(command string, args []string) error {
	flags := []string{media.FlagDiskID}
	switch command {
	case media.CommandMount:
		flags = append(flags, media.FlagFSUUID, media.FlagFSType)
	case media.CommandSync, media.CommandUnmount:
	default:
		return fmt.Errorf("unknown media operation %q", command)
	}
	if len(args) != 2*len(flags) {
		return fmt.Errorf("invalid argument count for media operation %q", command)
	}
	for i, flag := range flags {
		if args[2*i] != flag {
			return fmt.Errorf("expected media flag %q", flag)
		}
		value := args[2*i+1]
		var err error
		switch flag {
		case media.FlagDiskID:
			err = media.ValidateDiskID(value)
		case media.FlagFSUUID:
			err = media.ValidateFilesystemUUID(value)
		case media.FlagFSType:
			err = media.ValidateFilesystemType(value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func helperErrorCode(code string) string {
	if code == "" {
		return "mount_helper_failed"
	}
	return "mount_helper_" + code
}

func helperErrorMessage(result media.Result) string {
	if result.ErrorMessage == "" {
		return "the media operation failed"
	}
	return result.ErrorMessage
}

func trimToJSON(raw []byte) []byte {
	index := strings.IndexByte(string(raw), '{')
	if index <= 0 {
		return raw
	}
	return raw[index:]
}
