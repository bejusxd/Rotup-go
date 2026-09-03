//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

func verifyFilesystem(ctx context.Context, runner ports.ProcessRunner, device *os.File, uuid, fsType string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The child opens the helper's pinned descriptor, not a potentially reused
	// /dev name. --probe reads the device directly and bypasses the blkid cache.
	path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), device.Fd())
	result := runner.Run(ctx, ports.CommandSpec{
		Path: "/usr/sbin/blkid", Args: []string{"--probe", "--output", "export", "--", path},
		Env: []string{}, StdoutTailLimit: 16 * 1024, StderrTailLimit: 16 * 1024,
	})
	if result.Err != nil || result.Canceled || result.TimedOut || !result.Exited ||
		result.ExitCode != 0 || result.StdoutTruncated {
		return errors.New("the block device signature could not be read unambiguously")
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "UUID" && key != "TYPE") {
			continue
		}
		if _, exists := values[key]; exists {
			return errors.New("the block device probe returned duplicate identity fields")
		}
		values[key] = value
	}
	if domain.NormalizeFilesystemUUID(values["UUID"]) != uuid ||
		domain.NormalizeFilesystemType(values["TYPE"]) != fsType {
		return errors.New("the block device signature does not match the requested filesystem")
	}
	return nil
}
