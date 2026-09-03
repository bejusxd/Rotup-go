//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/ports"
)

type probeRunner struct {
	result ports.ProcessResult
	spec   ports.CommandSpec
}

func (r *probeRunner) Run(_ context.Context, spec ports.CommandSpec) ports.ProcessResult {
	r.spec = spec
	return r.result
}

func TestFilesystemProbeRequiresAnUnambiguousMatchingSignature(t *testing.T) {
	t.Parallel()
	file, err := os.Create(filepath.Join(t.TempDir(), "descriptor"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, tc := range []struct {
		name   string
		result ports.ProcessResult
		wantOK bool
	}{
		{"matching", ports.ProcessResult{Exited: true, Stdout: []byte("UUID=AAAA-BBBB\nTYPE=ext4\n")}, true},
		{"different UUID", ports.ProcessResult{Exited: true, Stdout: []byte("UUID=cccc-dddd\nTYPE=ext4\n")}, false},
		{"different type", ports.ProcessResult{Exited: true, Stdout: []byte("UUID=aaaa-bbbb\nTYPE=xfs\n")}, false},
		{"missing UUID", ports.ProcessResult{Exited: true, Stdout: []byte("TYPE=ext4\n")}, false},
		{"duplicate field", ports.ProcessResult{Exited: true, Stdout: []byte("UUID=aaaa-bbbb\nUUID=aaaa-bbbb\nTYPE=ext4\n")}, false},
		{"unreadable device", ports.ProcessResult{Exited: true, ExitCode: 2}, false},
		{"ambiguous signatures", ports.ProcessResult{Exited: true, ExitCode: 8}, false},
		{"truncated", ports.ProcessResult{Exited: true, StdoutTruncated: true, Stdout: []byte("UUID=aaaa-bbbb\nTYPE=ext4\n")}, false},
		{"timeout", ports.ProcessResult{TimedOut: true}, false},
		{"canceled", ports.ProcessResult{Canceled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &probeRunner{result: tc.result}
			err := verifyFilesystem(context.Background(), runner, file, "aaaa-bbbb", "ext4")
			if (err == nil) != tc.wantOK {
				t.Fatalf("error = %v, want success = %v", err, tc.wantOK)
			}
			if runner.spec.Path != "/usr/sbin/blkid" || len(runner.spec.Env) != 0 {
				t.Fatalf("probe must use a fixed executable and empty environment: %#v", runner.spec)
			}
			args := runner.spec.Args
			if len(args) != 5 || !reflect.DeepEqual(args[:4], []string{"--probe", "--output", "export", "--"}) ||
				!strings.HasPrefix(args[4], "/proc/") || !strings.Contains(args[4], "/fd/") {
				t.Fatalf("probe must bypass caches and read the pinned descriptor: %v", args)
			}
		})
	}
}
