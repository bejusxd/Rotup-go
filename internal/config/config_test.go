package config

import (
	"log/slog"
	"testing"
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
