//go:build linux

package linux

import (
	"context"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/ports"
)

func TestMountManagerRejectsMalformedInvocationsBeforeRunningHelper(t *testing.T) {
	t.Parallel()

	const diskID = "11111111-2222-4333-8444-555555555555"
	cases := []struct {
		name    string
		command string
		args    []string
	}{
		{"unknown command", "format", []string{media.FlagDiskID, diskID}},
		{"missing disk id", media.CommandSync, nil},
		{"dangling flag", media.CommandSync, []string{media.FlagDiskID}},
		{"unknown flag", media.CommandSync, []string{"--path", diskID}},
		{"extra flags", media.CommandUnmount, []string{media.FlagDiskID, diskID, media.FlagFSType, "ext4"}},
		{"missing mount flags", media.CommandMount, []string{media.FlagDiskID, diskID}},
		{"duplicate flag", media.CommandMount, []string{media.FlagDiskID, diskID, media.FlagDiskID, diskID, media.FlagFSType, "ext4"}},
		{"empty filesystem uuid", media.CommandMount, []string{media.FlagDiskID, diskID, media.FlagFSUUID, "", media.FlagFSType, "ext4"}},
		{"empty filesystem type", media.CommandMount, []string{media.FlagDiskID, diskID, media.FlagFSUUID, "1111-2222", media.FlagFSType, ""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{result: ports.ProcessResult{
				Started: true, Exited: true, Stdout: []byte(`{"ok":true}`),
			}}
			manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)
			_, err := manager.invoke(context.Background(), mountTimeout, test.command, test.args...)
			assertRefusedBeforeHelper(t, runner, err)
		})
	}
}

func TestMountManagerRejectsUnsafeSyncAndUnmountIdentifiersBeforeRunningHelper(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{media.CommandSync, media.CommandUnmount} {
		t.Run(operation, func(t *testing.T) {
			runner := &scriptedRunner{result: ports.ProcessResult{
				Started: true, Exited: true, Stdout: []byte(`{"ok":true}`),
			}}
			manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)
			var err error
			if operation == media.CommandSync {
				err = manager.Sync(context.Background(), "../../etc")
			} else {
				err = manager.Unmount(context.Background(), "../../etc")
			}
			assertRefusedBeforeHelper(t, runner, err)
		})
	}
}

func assertRefusedBeforeHelper(t *testing.T, runner *scriptedRunner, err error) {
	t.Helper()
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "mount_argument_invalid" {
		t.Fatalf("error = %v, want pre-invocation argument rejection", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 0 {
		t.Fatalf("a refused request reached the helper %d times", len(runner.calls))
	}
}

func TestMountManagerReportsMissingPrivilegeAsConfigurationNotMissingMedia(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"privilege_required", "device_access_denied"} {
		t.Run(code, func(t *testing.T) {
			runner := &scriptedRunner{result: ports.ProcessResult{
				Started: true, Exited: true, ExitCode: 1,
				Stdout: []byte(`{"ok":false,"error_code":"` + code + `","error_message":"permission required"}`),
			}}
			manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)
			_, err := manager.Mount(context.Background(), ports.MountRequest{
				DiskID: "11111111-2222-4333-8444-555555555555", FilesystemUUID: "aaaa-bbbb", FilesystemType: "ext4",
			})
			var typed *domain.Error
			if !domain.AsError(err, &typed) || typed.Class != domain.ErrClassConfiguration || typed.Code != "mount_helper_"+code {
				t.Fatalf("error = %v, want configuration error for %s", err, code)
			}
		})
	}
}
