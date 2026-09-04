package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variables that configure the daemon.
const (
	EnvHTTPAddr          = "ROTUP_HTTP_ADDR"
	EnvDevMode           = "ROTUP_DEV_MODE"
	EnvDBPath            = "ROTUP_DB_PATH"
	EnvLogLevel          = "ROTUP_LOG_LEVEL"
	EnvStagingRoot       = "ROTUP_STAGING_ROOT"
	EnvSecretRoot        = "ROTUP_SECRET_ROOT"
	EnvRclonePath        = "ROTUP_RCLONE_PATH"
	EnvResticPath        = "ROTUP_RESTIC_PATH"
	EnvCollectionTimeout = "ROTUP_COLLECTION_TIMEOUT"
	EnvBackupTimeout     = "ROTUP_BACKUP_TIMEOUT"
	EnvLsblkPath         = "ROTUP_LSBLK_PATH"
	EnvMountHelperPath   = "ROTUP_MOUNT_HELPER"
	EnvMediaPollInterval = "ROTUP_MEDIA_POLL_INTERVAL"
)

// Development defaults. Production deployments are expected to set explicit
// state, staging, and secret locations.
const (
	DefaultHTTPAddr          = "127.0.0.1:8080"
	DefaultDBPath            = "./rotup-dev.db"
	DefaultStagingRoot       = "./rotup-staging"
	DefaultSecretRoot        = "./rotup-secrets"
	DefaultCollectionTimeout = 2 * time.Hour
	DefaultBackupTimeout     = 6 * time.Hour
	DefaultLsblkPath         = "/usr/bin/lsblk"
	DefaultMediaPollInterval = 15 * time.Second
)

// Config contains the complete daemon configuration.
type Config struct {
	// DevMode changes HTTP diagnostics/presentation only, never service policy.
	DevMode      bool
	HTTPAddr     string
	DatabasePath string
	LogLevel     slog.Level

	// StagingRoot is the root of the ROTUP-owned staging layout.
	StagingRoot string
	// SecretRoot is the service-owned directory holding secret material. It is
	// deliberately separate from staging and is never backed up or served.
	SecretRoot string

	// RclonePath is the explicit rclone executable path.
	RclonePath string
	// RcloneFromPath records that RclonePath was discovered on the inherited
	// PATH rather than configured. Production deployments must configure it,
	// so the daemon warns when this is set.
	RcloneFromPath bool

	// ResticPath is the explicit restic executable path.
	ResticPath string
	// ResticFromPath records that ResticPath was discovered on the inherited
	// PATH rather than configured. Production deployments must configure it,
	// so the daemon warns when this is set.
	ResticFromPath bool

	// CollectionTimeout bounds one collection transfer.
	CollectionTimeout time.Duration
	// BackupTimeout bounds one restic snapshot operation.
	BackupTimeout time.Duration

	// LsblkPath is the explicit path to the block-device listing tool.
	LsblkPath string
	// MountHelperPath is the explicit path to the privileged mount helper. An
	// empty value means removable-media management is unavailable, which the
	// daemon reports rather than working around.
	MountHelperPath string
	// MediaPollInterval is how often ROTUP re-enumerates media so that a
	// missed hotplug hint cannot lose a disk permanently.
	MediaPollInterval time.Duration
}

// Load reads configuration from environment variables and applies defaults.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddr:          valueOrDefault(lookup, EnvHTTPAddr, DefaultHTTPAddr),
		DatabasePath:      valueOrDefault(lookup, EnvDBPath, DefaultDBPath),
		StagingRoot:       valueOrDefault(lookup, EnvStagingRoot, DefaultStagingRoot),
		SecretRoot:        valueOrDefault(lookup, EnvSecretRoot, DefaultSecretRoot),
		LogLevel:          slog.LevelInfo,
		CollectionTimeout: DefaultCollectionTimeout,
		BackupTimeout:     DefaultBackupTimeout,
		LsblkPath:         valueOrDefault(lookup, EnvLsblkPath, DefaultLsblkPath),
		MountHelperPath:   strings.TrimSpace(valueOrDefault(lookup, EnvMountHelperPath, "")),
		MediaPollInterval: DefaultMediaPollInterval,
	}
	if raw, ok := lookup(EnvDevMode); ok {
		dev, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, fmt.Errorf("%s must be a boolean", EnvDevMode)
		}
		cfg.DevMode = dev
	}

	if _, err := net.ResolveTCPAddr("tcp", cfg.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvHTTPAddr, err)
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return Config{}, fmt.Errorf("%s must not be empty", EnvDBPath)
	}

	if raw, ok := lookup(EnvLogLevel); ok && strings.TrimSpace(raw) != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(raw)))); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvLogLevel, err)
		}
	}

	if raw, ok := lookup(EnvCollectionTimeout); ok && strings.TrimSpace(raw) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvCollectionTimeout, err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("%s must be positive", EnvCollectionTimeout)
		}
		cfg.CollectionTimeout = duration
	}

	if raw, ok := lookup(EnvBackupTimeout); ok && strings.TrimSpace(raw) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvBackupTimeout, err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("%s must be positive", EnvBackupTimeout)
		}
		cfg.BackupTimeout = duration
	}

	if raw, ok := lookup(EnvMediaPollInterval); ok && strings.TrimSpace(raw) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvMediaPollInterval, err)
		}
		if duration < time.Second {
			return Config{}, fmt.Errorf("%s must be at least one second", EnvMediaPollInterval)
		}
		cfg.MediaPollInterval = duration
	}

	if err := resolveStagingAndSecretRoots(&cfg); err != nil {
		return Config{}, err
	}
	if err := resolveRclone(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := resolveRestic(&cfg, lookup); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// resolveStagingAndSecretRoots makes both roots absolute and refuses to let one
// live inside the other. Secret material must never end up inside a tree that a
// backup or a staging listing can reach.
func resolveStagingAndSecretRoots(cfg *Config) error {
	staging, err := filepath.Abs(cfg.StagingRoot)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvStagingRoot, err)
	}
	secret, err := filepath.Abs(cfg.SecretRoot)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvSecretRoot, err)
	}
	if pathsOverlap(staging, secret) {
		return fmt.Errorf("%s and %s must not overlap: secret material must stay outside staging",
			EnvStagingRoot, EnvSecretRoot)
	}
	cfg.StagingRoot = staging
	cfg.SecretRoot = secret
	return nil
}

func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	separator := string(filepath.Separator)
	return strings.HasPrefix(a, b+separator) || strings.HasPrefix(b, a+separator)
}

// resolveRclone selects the transfer engine executable. An explicit path is
// used verbatim; otherwise the inherited PATH is consulted purely as a
// development convenience and the result is flagged so the daemon can warn.
func resolveRclone(cfg *Config, lookup func(string) (string, bool)) error {
	if raw, ok := lookup(EnvRclonePath); ok && strings.TrimSpace(raw) != "" {
		path := strings.TrimSpace(raw)
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvRclonePath, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvRclonePath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s must name an executable, not a directory", EnvRclonePath)
		}
		cfg.RclonePath = absolute
		return nil
	}
	if discovered, err := exec.LookPath("rclone"); err == nil {
		cfg.RclonePath = discovered
		cfg.RcloneFromPath = true
	}
	return nil
}

// resolveRestic selects the backup engine executable, using the same rule as
// the transfer engine: an explicit path is used verbatim, and the inherited
// PATH is only a development convenience whose use is flagged so the daemon can
// warn about it.
func resolveRestic(cfg *Config, lookup func(string) (string, bool)) error {
	if raw, ok := lookup(EnvResticPath); ok && strings.TrimSpace(raw) != "" {
		path := strings.TrimSpace(raw)
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvResticPath, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvResticPath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s must name an executable, not a directory", EnvResticPath)
		}
		cfg.ResticPath = absolute
		return nil
	}
	if discovered, err := exec.LookPath("restic"); err == nil {
		cfg.ResticPath = discovered
		cfg.ResticFromPath = true
	}
	return nil
}

func valueOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
