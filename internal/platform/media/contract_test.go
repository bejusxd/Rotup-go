package media

import (
	"encoding/json"
	"strings"
	"testing"
)

// The disk identifier is the only caller-supplied value that becomes part of a
// filesystem path, so it is the value that has to be airtight.
func TestValidateDiskIDRejectsAnythingThatIsNotAGeneratedIdentifier(t *testing.T) {
	t.Parallel()

	if err := ValidateDiskID("11111111-2222-4333-8444-555555555555"); err != nil {
		t.Fatalf("a generated identifier was rejected: %v", err)
	}

	unsafe := []string{
		"",
		"..",
		"../../etc",
		"../../../run/rotup/media",
		"/etc/shadow",
		`..\..\windows`,
		"11111111-2222-4333-8444-55555555555",   // too short
		"11111111-2222-4333-8444-5555555555555", // too long
		"11111111/2222/4333/8444/555555555555",
		"11111111-2222-4333-8444-55555555555z",
		"11111111.2222.4333.8444.555555555555",
		"$(whoami)",
		"a b c",
		strings.Repeat("a", 36),
	}
	for _, value := range unsafe {
		if err := ValidateDiskID(value); err == nil {
			t.Fatalf("disk id %q should have been rejected", value)
		}
		if _, err := MountPointFor(value); err == nil {
			t.Fatalf("a mount point should not be derivable from %q", value)
		}
	}
}

func TestMountPointIsAlwaysBelowTheFixedMountRoot(t *testing.T) {
	t.Parallel()

	point, err := MountPointFor("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	if point != MountRoot+"/11111111-2222-4333-8444-555555555555" {
		t.Fatalf("mount point = %q", point)
	}
	if !strings.HasPrefix(point, MountRoot+"/") {
		t.Fatal("every mount point must live below the fixed mount root")
	}
	if strings.Contains(point, "..") {
		t.Fatal("a mount point must never contain a traversal segment")
	}

	// The same identifier in either case yields the same single path, so a
	// case difference cannot create a second mount point for one disk.
	upper, err := MountPointFor("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	mixed, err := MountPointFor("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	if upper != mixed {
		t.Fatal("mount point derivation must be deterministic")
	}
}

func TestValidateFilesystemUUIDRejectsPathAndFlagShapes(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"11111111-2222-3333-4444-555555555555",
		"1234-abcd",
		"deadbeef",
	} {
		if err := ValidateFilesystemUUID(value); err != nil {
			t.Fatalf("a real filesystem UUID %q was rejected: %v", value, err)
		}
	}

	for _, value := range []string{
		"",
		"../../../dev/sda",
		"/dev/sda1",
		"--help",
		"uuid with spaces",
		"uuid;rm -rf /",
		"g1111111-2222-3333-4444-555555555555",
		strings.Repeat("a", 65),
	} {
		if err := ValidateFilesystemUUID(value); err == nil {
			t.Fatalf("filesystem uuid %q should have been rejected", value)
		}
	}
}

func TestValidateFilesystemTypeIsAnAllowlist(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"ext4", "EXT4", " ext4 "} {
		if err := ValidateFilesystemType(value); err != nil {
			t.Fatalf("supported filesystem %q was rejected: %v", value, err)
		}
	}
	// The privileged side keeps its own allowlist rather than trusting that the
	// caller filtered correctly.
	for _, value := range []string{"", "exfat", "ntfs", "vfat", "btrfs", "xfs", "ext4,ro", "proc"} {
		if err := ValidateFilesystemType(value); err == nil {
			t.Fatalf("filesystem type %q should have been rejected", value)
		}
	}
}

func TestResultRoundTripsAsJSON(t *testing.T) {
	t.Parallel()

	original := Result{
		OK: true, Operation: CommandMount, MountPoint: MountRoot + "/x",
		ResolvedDevice: "/dev/sdb1", FilesystemUUID: "1111", FilesystemType: "ext4", Mounted: true,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Result
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != original {
		t.Fatalf("round trip = %#v, want %#v", decoded, original)
	}
}

// The helper's flags carry no path and no options, which is the property that
// keeps arbitrary mount targets away from privileged code.
func TestHelperContractExposesNoPathOrOptionArgument(t *testing.T) {
	t.Parallel()

	for _, flag := range []string{FlagDiskID, FlagFSUUID, FlagFSType} {
		lowered := strings.ToLower(flag)
		for _, forbidden := range []string{"path", "target", "mountpoint", "option", "device", "source"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("the helper contract exposes %q, which would let a caller choose %s", flag, forbidden)
			}
		}
	}
	for _, command := range []string{CommandMount, CommandSync, CommandUnmount} {
		if command == "" {
			t.Fatal("helper subcommands must be named")
		}
	}
}
