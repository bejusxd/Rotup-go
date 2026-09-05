package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCustomPathsConverge(t *testing.T) {
	h := newSetupHost(t)
	o := defaultSetup(t, h)
	o.StagingRoot, o.SecretRoot, o.DatabasePath = "/srv/rotup/staging", "/srv/rotup/secrets", "/srv/rotup/database/state.db"
	r, _ := Setup(context.Background(), h, o)
	if !r.Ready() {
		t.Fatal(failureDetails(r))
	}
	for _, p := range []string{o.StagingRoot, o.SecretRoot, filepath.Dir(o.DatabasePath)} {
		s, e := h.Lstat(p)
		if e != nil || !s.IsDir {
			t.Fatalf("missing custom directory %s", p)
		}
	}
	env, _ := h.ReadFile(EnvFilePath)
	unit, _ := h.ReadFile(UnitPath)
	for _, p := range []string{o.StagingRoot, o.SecretRoot, o.DatabasePath} {
		if !strings.Contains(string(env), p) || !strings.Contains(string(unit), p) {
			t.Fatalf("configuration missed %s", p)
		}
	}
	r, _ = Setup(context.Background(), h, o)
	if !r.Ready() || r.Changed() {
		t.Fatalf("not converged: %+v", r)
	}
}

func TestSetupCustomDatabaseParentIsNotModified(t *testing.T) {
	h := newSetupHost(t)
	o := defaultSetup(t, h)
	o.DatabasePath = "/srv/shared/state.db"
	if e := h.MkdirAll("/srv/shared", 0755); e != nil {
		t.Fatal(e)
	}
	r, _ := Setup(context.Background(), h, o)
	if !r.Ready() {
		t.Fatal(failureDetails(r))
	}
	s, _ := h.Lstat("/srv/shared")
	if s.Mode.Perm() != 0755 {
		t.Fatal("changed shared parent mode")
	}
	for _, p := range h.chowns {
		if p == "/srv/shared" {
			t.Fatal("changed shared parent owner")
		}
	}
}

func TestSetupRejectsUnsafeCustomPathsBeforeWrites(t *testing.T) {
	for _, p := range []string{"relative", "/srv/../data", "/", "/etc", "/etc/rotup/state", "/home", "/home/operator", "/srv/staging%h", "/srv/has space", "/srv/line\nbreak", SecretRoot, SecretRoot + "/nested", "/var/lib"} {
		t.Run(strings.ReplaceAll(p, "/", "_"), func(t *testing.T) {
			h := newSetupHost(t)
			o := defaultSetup(t, h)
			o.StagingRoot = p
			r, _ := Setup(context.Background(), h, o)
			if r.Ready() || r.Changed() || len(h.chowns) > 0 {
				t.Fatalf("unsafe path accepted or writes occurred: %+v", r)
			}
			entries, e := os.ReadDir(h.Root)
			if e != nil || len(entries) != 0 {
				t.Fatal("setup wrote before refusing paths")
			}
		})
	}
}

func TestSetupPreservesCustomEnvironmentWithoutForce(t *testing.T) {
	h := newSetupHost(t)
	o := defaultSetup(t, h)
	o.StagingRoot = "/srv/rotup/staging"
	if e := h.WriteFileAtomic(EnvFilePath, []byte("ROTUP_HTTP_ADDR=127.0.0.1:9090\n"), EnvFileMode); e != nil {
		t.Fatal(e)
	}
	Setup(context.Background(), h, o)
	content, _ := h.ReadFile(EnvFilePath)
	if string(content) != "ROTUP_HTTP_ADDR=127.0.0.1:9090\n" {
		t.Fatal("overwrote operator configuration")
	}
	o.Force = true
	Setup(context.Background(), h, o)
	content, _ = h.ReadFile(EnvFilePath)
	if !strings.Contains(string(content), o.StagingRoot) {
		t.Fatal("force did not install selected paths")
	}
}
