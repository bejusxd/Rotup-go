package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
)

const (
	EnvHTTPAddr = "ROTUP_HTTP_ADDR"
	EnvDBPath   = "ROTUP_DB_PATH"
	EnvLogLevel = "ROTUP_LOG_LEVEL"

	DefaultHTTPAddr = "127.0.0.1:8080"
	DefaultDBPath   = "./rotup-dev.db"
)

// Config contains the complete M1 daemon configuration.
type Config struct {
	HTTPAddr     string
	DatabasePath string
	LogLevel     slog.Level
}

// Load reads configuration from environment variables and applies M1 defaults.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddr:     valueOrDefault(lookup, EnvHTTPAddr, DefaultHTTPAddr),
		DatabasePath: valueOrDefault(lookup, EnvDBPath, DefaultDBPath),
		LogLevel:     slog.LevelInfo,
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

	return cfg, nil
}

func valueOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
