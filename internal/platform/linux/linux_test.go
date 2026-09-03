//go:build linux

package linux

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/ports"
)

// scriptedRunner replays a fixed process result and records what it was asked
// to run, so the argument arrays that reach privileged code can be asserted.
type scriptedRunner struct {
	mu     sync.Mutex
	calls  []ports.CommandSpec
	result ports.ProcessResult
}

func (r *scriptedRunner) Run(_ context.Context, spec ports.CommandSpec) ports.ProcessResult {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	r.mu.Unlock()
	return r.result
}

func (r *scriptedRunner) last(t *testing.T) ports.CommandSpec {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("nothing was run")
	}
	return r.calls[len(r.calls)-1]
}

const sampleLsblk = `{
  "blockdevices": [
    {"name":"/dev/sda","path":"/dev/sda","type":"disk","fstype":null,"uuid":null,
     "size":512110190592,"rm":"0","ro":"0","tran":"nvme",
     "children":[
       {"name":"/dev/sda1","path":"/dev/sda1","type":"part","fstype":"vfat",
        "uuid":"1234-ABCD","label":"EFI","size":536870912,"rm":"0","ro":"0","pkname":"sda"},
       {"name":"/dev/sda2","path":"/dev/sda2","type":"part","fstype":"ext4",
        "uuid":"11111111-2222-3333-4444-555555555555","label":"root",
        "size":511573319680,"rm":"0","ro":"0","pkname":"sda"}
     ]},
    {"name":"/dev/sdb","path":"/dev/sdb","type":"disk","fstype":null,"uuid":null,
     "size":1000204886016,"rm":"1","ro":"0","tran":"usb",
     "children":[
       {"name":"/dev/sdb1","path":"/dev/sdb1","type":"part","fstype":"ext4",
        "uuid":"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE","label":"rotup-a",
        "size":1000203837440,"rm":"0","ro":"0","pkname":"sdb"}
     ]}
  ]
}`

func TestEnumerateParsesMachineReadableOutput(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{result: ports.ProcessResult{
		Started: true, Exited: true, Stdout: []byte(sampleLsblk),
	}}
	enumerator := NewEnumerator("/usr/bin/lsblk", runner)

	observed, err := enumerator.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	// Only filesystem-bearing nodes are reported; the bare whole disks are not.
	if len(observed) != 3 {
		t.Fatalf("observed %d filesystems, want 3: %#v", len(observed), observed)
	}

	byPath := make(map[string]domain.ObservedMedia, len(observed))
	for _, item := range observed {
		byPath[item.DevicePath] = item
	}

	usb, ok := byPath["/dev/sdb1"]
	if !ok {
		t.Fatalf("the USB partition was not observed: %#v", byPath)
	}
	// UUIDs are normalized so that identity comparison is case-insensitive.
	if usb.FilesystemUUID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("uuid = %q, want it normalized to lower case", usb.FilesystemUUID)
	}
	if usb.FilesystemType != "ext4" || usb.Label != "rotup-a" {
		t.Fatalf("observation = %#v", usb)
	}
	if usb.SizeBytes != 1000203837440 {
		t.Fatalf("size = %d", usb.SizeBytes)
	}
	// The transport and removable flags are inherited from the parent disk,
	// where lsblk normally reports them.
	if usb.Transport != "usb" || !usb.Removable {
		t.Fatalf("the USB classification was lost: %#v", usb)
	}
	if usb.ParentDevice != "/dev/sdb" {
		t.Fatalf("parent = %q", usb.ParentDevice)
	}
	if usb.ObservedAt.IsZero() {
		t.Fatal("every observation must be timestamped")
	}

	internal := byPath["/dev/sda2"]
	if internal.Transport != "nvme" || internal.Removable {
		t.Fatalf("an internal disk must not be classified as removable: %#v", internal)
	}
}

func TestEnumerateBuildsAnExplicitArgumentArray(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{result: ports.ProcessResult{
		Started: true, Exited: true, Stdout: []byte(`{"blockdevices":[]}`),
	}}
	if _, err := NewEnumerator("/usr/bin/lsblk", runner).Enumerate(context.Background()); err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	spec := runner.last(t)
	if spec.Path != "/usr/bin/lsblk" {
		t.Fatalf("path = %q, want the explicitly configured executable", spec.Path)
	}
	if len(spec.Env) != 0 {
		t.Fatalf("the child must not inherit an environment, got %#v", spec.Env)
	}
	joined := strings.Join(spec.Args, " ")
	for _, wanted := range []string{"--json", "--bytes", "--paths", "--output"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("argument array is missing %s: %v", wanted, spec.Args)
		}
	}
	if strings.Contains(joined, "MOUNTPOINT") {
		t.Fatal("mount state must come from the kernel mount table, not from lsblk")
	}
}

func TestEnumerateRefusesTruncatedOrUnreadableOutput(t *testing.T) {
	t.Parallel()

	cases := map[string]ports.ProcessResult{
		"truncated":  {Started: true, Exited: true, Stdout: []byte(sampleLsblk), StdoutTruncated: true},
		"unparsable": {Started: true, Exited: true, Stdout: []byte("not json")},
		"failed":     {Started: true, Exited: true, ExitCode: 1},
		"timed out":  {TimedOut: true},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedRunner{result: result}
			// A partial listing could hide a duplicate UUID, so an incomplete
			// reading is an error rather than a shorter list.
			if _, err := NewEnumerator("/usr/bin/lsblk", runner).Enumerate(context.Background()); err == nil {
				t.Fatal("an incomplete enumeration must be an error")
			}
		})
	}
}

func TestEnumerateRequiresAConfiguredExecutable(t *testing.T) {
	t.Parallel()

	if _, err := NewEnumerator("", &scriptedRunner{}).Enumerate(context.Background()); err == nil {
		t.Fatal("an unconfigured enumerator must fail plainly")
	}
}

const sampleMountInfo = `21 26 0:20 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
26 1 259:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
40 26 8:17 / /run/rotup/media/11111111-2222-4333-8444-555555555555 rw,nosuid,nodev,noexec,relatime shared:9 - ext4 /dev/sdb1 rw
41 26 8:33 / /mnt/with\040space ro,relatime shared:10 - ext4 /dev/sdc1 ro
`

func TestParseMountInfoReadsTheAuthoritativeMountTable(t *testing.T) {
	t.Parallel()

	mounts, err := parseMountInfo(strings.NewReader(sampleMountInfo))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(mounts) != 4 {
		t.Fatalf("parsed %d mounts, want 4: %#v", len(mounts), mounts)
	}

	byPoint := make(map[string]domain.MountObservation, len(mounts))
	for _, mount := range mounts {
		byPoint[mount.MountPoint] = mount
	}

	rotup, ok := byPoint["/run/rotup/media/11111111-2222-4333-8444-555555555555"]
	if !ok {
		t.Fatalf("the ROTUP mount was not parsed: %#v", byPoint)
	}
	if rotup.Source != "/dev/sdb1" || rotup.FilesystemType != "ext4" {
		t.Fatalf("mount = %#v", rotup)
	}
	if rotup.ReadOnly {
		t.Fatal("a read-write mount must not be reported read-only")
	}

	// The kernel escapes spaces in mount points, and a path ROTUP compares
	// against has to be the real one.
	spaced, ok := byPoint["/mnt/with space"]
	if !ok {
		t.Fatalf("an escaped mount point was not decoded: %#v", byPoint)
	}
	if !spaced.ReadOnly {
		t.Fatal("a read-only mount must be reported as such")
	}
}

func TestParseMountInfoSkipsMalformedLines(t *testing.T) {
	t.Parallel()

	mounts, err := parseMountInfo(strings.NewReader("garbage\n\n26 1 259:2 / / rw - ext4 /dev/sda2 rw\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("parsed %d mounts, want the one well-formed line", len(mounts))
	}
}

func TestMountManagerRefusesArgumentsItWouldNotPassToPrivilegedCode(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)

	cases := map[string]ports.MountRequest{
		"traversing disk id": {
			DiskID: "../../etc", FilesystemUUID: "1111-2222", FilesystemType: "ext4",
		},
		"path-like filesystem uuid": {
			DiskID:         "11111111-2222-4333-8444-555555555555",
			FilesystemUUID: "/dev/sda1", FilesystemType: "ext4",
		},
		"unsupported filesystem": {
			DiskID:         "11111111-2222-4333-8444-555555555555",
			FilesystemUUID: "1111-2222", FilesystemType: "ntfs",
		},
		"option injection through the filesystem type": {
			DiskID:         "11111111-2222-4333-8444-555555555555",
			FilesystemUUID: "1111-2222", FilesystemType: "ext4,rw,suid",
		},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := manager.Mount(context.Background(), request); err == nil {
				t.Fatal("the request should have been refused")
			}
		})
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 0 {
		t.Fatalf("a refused request must never reach the helper, got %d invocations", len(runner.calls))
	}
}

func TestMountBuildsTheNarrowHelperInvocation(t *testing.T) {
	t.Parallel()

	const diskID = "11111111-2222-4333-8444-555555555555"
	runner := &scriptedRunner{result: ports.ProcessResult{
		Started: true, Exited: true,
		Stdout: []byte(`{"ok":true,"operation":"mount","mount_point":"/run/rotup/media/` + diskID +
			`","resolved_device":"/dev/sdb1","filesystem_uuid":"1111-2222","filesystem_type":"ext4","mounted":true}`),
	}}
	manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)

	result, err := manager.Mount(context.Background(), ports.MountRequest{
		DiskID: diskID, FilesystemUUID: "1111-2222", FilesystemType: "ext4",
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if result.MountPoint != media.MountRoot+"/"+diskID || !result.Mounted {
		t.Fatalf("result = %#v", result)
	}

	spec := runner.last(t)
	if spec.Path != "/usr/libexec/rotup-mounthelper" {
		t.Fatalf("path = %q", spec.Path)
	}
	if len(spec.Env) != 0 {
		t.Fatalf("the helper must not inherit an environment, got %#v", spec.Env)
	}
	if spec.Args[0] != media.CommandMount {
		t.Fatalf("first argument = %q, want the mount subcommand", spec.Args[0])
	}
	// No path and no mount option is ever handed across the boundary.
	for _, arg := range spec.Args {
		if strings.HasPrefix(arg, "/") {
			t.Fatalf("a filesystem path reached the helper: %q", arg)
		}
		for _, forbidden := range []string{"nosuid", "nodev", "noexec", "rw", "ro", "-o"} {
			if arg == forbidden {
				t.Fatalf("a mount option reached the helper: %q", arg)
			}
		}
	}
}

func TestHelperFailureBecomesATypedError(t *testing.T) {
	t.Parallel()

	const diskID = "11111111-2222-4333-8444-555555555555"
	runner := &scriptedRunner{result: ports.ProcessResult{
		Started: true, Exited: true, ExitCode: 1,
		Stdout: []byte(`{"ok":false,"error_code":"device_not_found","error_message":"no connected filesystem currently carries that UUID"}`),
	}}
	manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)

	_, err := manager.Mount(context.Background(), ports.MountRequest{
		DiskID: diskID, FilesystemUUID: "1111-2222", FilesystemType: "ext4",
	})
	if err == nil {
		t.Fatal("a helper failure must be an error")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) {
		t.Fatalf("error = %v, want a typed error", err)
	}
	if !strings.Contains(typed.Code, "device_not_found") {
		t.Fatalf("error code = %q, want it to carry the helper's reason", typed.Code)
	}
}

func TestUnreadableHelperOutputIsNotMistakenForSuccess(t *testing.T) {
	t.Parallel()

	const diskID = "11111111-2222-4333-8444-555555555555"
	cases := map[string]ports.ProcessResult{
		"no output at all":     {Started: true, Exited: true},
		"unparsable output":    {Started: true, Exited: true, Stdout: []byte("mounted!")},
		"success but non-zero": {Started: true, Exited: true, ExitCode: 1, Stdout: []byte(`{"ok":true,"mounted":true}`)},
		"could not start":      {Err: errNotFound{}},
		"no exit status":       {Started: true},
		"timed out":            {TimedOut: true},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedRunner{result: result}
			manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)
			if _, err := manager.Mount(context.Background(), ports.MountRequest{
				DiskID: diskID, FilesystemUUID: "1111-2222", FilesystemType: "ext4",
			}); err == nil {
				t.Fatal("ROTUP must not read this as a successful mount")
			}
		})
	}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "executable file not found" }

func TestUnconfiguredMountManagerIsUnavailable(t *testing.T) {
	t.Parallel()

	manager := NewMountManager("  ", &scriptedRunner{})
	if manager.Available() {
		t.Fatal("a manager with no helper path must report as unavailable")
	}
	if err := manager.Sync(context.Background(), "11111111-2222-4333-8444-555555555555"); err == nil {
		t.Fatal("an unconfigured manager must refuse operations")
	}
}

func TestMountPointForRejectsUnsafeIdentifiers(t *testing.T) {
	t.Parallel()

	manager := NewMountManager("/usr/libexec/rotup-mounthelper", &scriptedRunner{})
	if _, err := manager.MountPointFor("../../escape"); err == nil {
		t.Fatal("an unsafe identifier must not produce a mount point")
	}
	point, err := manager.MountPointFor("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	if !strings.HasPrefix(point, media.MountRoot+"/") {
		t.Fatalf("mount point %q escaped the mount root", point)
	}
}

func TestOperationsCarryDeadlines(t *testing.T) {
	t.Parallel()

	const diskID = "11111111-2222-4333-8444-555555555555"
	var deadline time.Time
	runner := &deadlineRecorder{deadline: &deadline, result: ports.ProcessResult{
		Started: true, Exited: true, Stdout: []byte(`{"ok":true,"mounted":false}`),
	}}
	manager := NewMountManager("/usr/libexec/rotup-mounthelper", runner)

	if err := manager.Unmount(context.Background(), diskID); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	if deadline.IsZero() {
		t.Fatal("a privileged operation must be given a deadline")
	}
}

type deadlineRecorder struct {
	deadline *time.Time
	result   ports.ProcessResult
}

func (r *deadlineRecorder) Run(ctx context.Context, _ ports.CommandSpec) ports.ProcessResult {
	if value, ok := ctx.Deadline(); ok {
		*r.deadline = value
	}
	return r.result
}
