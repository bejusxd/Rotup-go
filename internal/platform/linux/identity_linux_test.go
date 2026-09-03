//go:build linux

package linux

import (
	"context"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

func TestEnumerationIgnoresEmptyDevicesEvenWithStaleSignatureMetadata(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{result: ports.ProcessResult{Exited: true, Stdout: []byte(`{
		"blockdevices":[
			{"path":"/dev/loop0","type":"loop","size":0,"fstype":"ext4","uuid":"aaaa"},
			{"path":"/dev/loop1","type":"loop","size":0,"fstype":null,"uuid":null},
			{"path":"/dev/sdz","type":"disk","size":0,"fstype":"ext4","uuid":"bbbb"},
			{"path":"/dev/loop2","type":"loop","size":67108864,"fstype":"ext4","uuid":"CCCC","rm":false}
		]}`)}}
	observed, err := NewEnumerator("/usr/bin/lsblk", runner).Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].DevicePath != "/dev/loop2" || observed[0].FilesystemUUID != "cccc" {
		t.Fatalf("only the attached filesystem should be observed: %#v", observed)
	}
}

func TestResolveFilesystemUsesTheSameUnfilteredUUIDObservations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, listing, wantPath, wantCode string
	}{
		{"loop without USB classification or symlinks", `{"blockdevices":[{"path":"/dev/loop7","type":"loop","size":67108864,"fstype":"ext4","uuid":"AAAA-BBBB","rm":false}]}`, "/dev/loop7", ""},
		{"absent", `{"blockdevices":[]}`, "", "device_not_found"},
		{"duplicate", `{"blockdevices":[{"path":"/dev/loop7","size":67108864,"fstype":"ext4","uuid":"aaaa-bbbb"},{"path":"/dev/sdb1","size":67108864,"fstype":"ext4","uuid":"AAAA-BBBB"}]}`, "", "ambiguous_identity"},
		{"empty node with stale UUID", `{"blockdevices":[{"path":"/dev/loop7","size":0,"fstype":"ext4","uuid":"aaaa-bbbb"}]}`, "", "device_not_found"},
		{"unreadable listing", `not JSON`, "", "lsblk_unparsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{result: ports.ProcessResult{Exited: true, Stdout: []byte(tc.listing)}}
			observed, err := NewEnumerator("/usr/bin/lsblk", runner).ResolveFilesystem(context.Background(), "AAAA-BBBB")
			if tc.wantCode != "" {
				var typed *domain.Error
				if !domain.AsError(err, &typed) || typed.Code != tc.wantCode {
					t.Fatalf("error = %v, want %s", err, tc.wantCode)
				}
				return
			}
			if err != nil || observed.DevicePath != tc.wantPath {
				t.Fatalf("resolved = %#v, error = %v", observed, err)
			}
		})
	}
}
