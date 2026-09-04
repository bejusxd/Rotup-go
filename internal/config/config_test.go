package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.HTTPAddr != DefaultHTTPAddr {
		t.Fatalf("HTTP address = %q, want %q", cfg.HTTPAddr, DefaultHTTPAddr)
	}
	if cfg.DatabasePath != DefaultDBPath {
		t.Fatalf("database path = %q, want %q", cfg.DatabasePath, DefaultDBPath)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("log level = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.BackupTimeout != DefaultBackupTimeout {
		t.Fatalf("backup timeout = %v, want %v", cfg.BackupTimeout, DefaultBackupTimeout)
	}
	if cfg.DevMode {
		t.Fatal("development mode must be disabled by default")
	}
}

func TestDevelopmentModeIsExplicit(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"true", "false", "invalid", ""} {
		cfg, err := load(func(key string) (string, bool) { return value, key == EnvDevMode })
		if value == "invalid" || value == "" {
			if err == nil {
				t.Fatalf("accepted %q", value)
			}
			continue
		}
		if err != nil || cfg.DevMode != (value == "true") {
			t.Fatalf("dev mode %q: %v, %v", value, cfg.DevMode, err)
		}
	}
}

func TestResticConfiguration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range []string{"45m", "0", "-1h", "bad"} {
		cfg, err := load(func(key string) (string, bool) {
			switch key {
			case EnvResticPath:
				return path, true
			case EnvBackupTimeout:
				return timeout, true
			default:
				return "", false
			}
		})
		if timeout != "45m" {
			if err == nil {
				t.Fatalf("accepted invalid backup timeout %q", timeout)
			}
			continue
		}
		if err != nil || cfg.BackupTimeout != 45*time.Minute || cfg.ResticPath != path || cfg.ResticFromPath {
			t.Fatalf("explicit restic configuration = %#v / %v", cfg, err)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		EnvHTTPAddr: "127.0.0.1:9090",
		EnvDBPath:   "state/test.db",
		EnvLogLevel: "debug",
	}
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	if cfg.HTTPAddr != values[EnvHTTPAddr] {
		t.Fatalf("HTTP address = %q, want %q", cfg.HTTPAddr, values[EnvHTTPAddr])
	}
	if cfg.DatabasePath != values[EnvDBPath] {
		t.Fatalf("database path = %q, want %q", cfg.DatabasePath, values[EnvDBPath])
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("log level = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]string{
		"HTTP address": {EnvHTTPAddr: "not-an-address"},
		"log level":    {EnvLogLevel: "verbose"},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := load(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			})
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLoadResolvesStagingAndSecretRoots(t *testing.T) {
	t.Parallel()

	cfg, err := load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if !filepath.IsAbs(cfg.StagingRoot) {
		t.Fatalf("staging root %q must be absolute", cfg.StagingRoot)
	}
	if !filepath.IsAbs(cfg.SecretRoot) {
		t.Fatalf("secret root %q must be absolute", cfg.SecretRoot)
	}
	if cfg.CollectionTimeout != DefaultCollectionTimeout {
		t.Fatalf("collection timeout = %v, want %v", cfg.CollectionTimeout, DefaultCollectionTimeout)
	}
}

func TestLoadRejectsOverlappingStagingAndSecretRoots(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]string{
		"identical":      {EnvStagingRoot: "/srv/rotup", EnvSecretRoot: "/srv/rotup"},
		"secrets inside": {EnvStagingRoot: "/srv/rotup", EnvSecretRoot: "/srv/rotup/secrets"},
		"staging inside": {EnvStagingRoot: "/srv/rotup/secrets/staging", EnvSecretRoot: "/srv/rotup/secrets"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			})
			if err == nil {
				t.Fatal("secret material must never be allowed inside staging")
			}
		})
	}
}

func TestLoadAcceptsAnExplicitRclonePath(t *testing.T) {
	t.Parallel()

	executable := filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("create fake rclone: %v", err)
	}
	values := map[string]string{EnvRclonePath: executable}
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RclonePath != executable {
		t.Fatalf("rclone path = %q, want %q", cfg.RclonePath, executable)
	}
	if cfg.RcloneFromPath {
		t.Fatal("an explicitly configured path must not be marked as discovered on PATH")
	}
}

func TestLoadRejectsAnUnusableRclonePath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"missing file": filepath.Join(t.TempDir(), "not-there"),
		"a directory":  t.TempDir(),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{EnvRclonePath: path}
			if _, err := load(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			}); err == nil {
				t.Fatalf("rclone path %q should have been rejected", path)
			}
		})
	}
}

func TestLoadParsesTheCollectionTimeout(t *testing.T) {
	t.Parallel()

	values := map[string]string{EnvCollectionTimeout: "45m"}
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.CollectionTimeout != 45*time.Minute {
		t.Fatalf("collection timeout = %v, want 45m", cfg.CollectionTimeout)
	}

	for _, invalid := range []string{"soon", "-5m", "0"} {
		values[EnvCollectionTimeout] = invalid
		if _, err := load(func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}); err == nil {
			t.Fatalf("collection timeout %q should have been rejected", invalid)
		}
	}
}

func TestLoadOverridesStagingAndSecretRoots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	values := map[string]string{
		EnvStagingRoot: filepath.Join(root, "staging"),
		EnvSecretRoot:  filepath.Join(root, "secrets"),
	}
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StagingRoot != values[EnvStagingRoot] || cfg.SecretRoot != values[EnvSecretRoot] {
		t.Fatalf("roots = %q / %q", cfg.StagingRoot, cfg.SecretRoot)
	}
}
