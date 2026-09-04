package system

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestHost builds a host rooted in a temporary directory so provisioning and
// inspection run against real files with real modes, without root and without
// touching the machine.
func newTestHost(t *testing.T) *OSHost {
	t.Helper()
	return &OSHost{
		Root: t.TempDir(),
		Env:  map[string]string{},
		Accounts: map[string]struct {
			UID   int
			Group string
			GID   int
		}{"rotup": {UID: 1000, Group: "rotup", GID: 1000}},
		Path:      map[string]string{"rclone": "/usr/bin/rclone", "restic": "/usr/bin/restic", "systemctl": "/usr/bin/systemctl"},
		SkipChown: true,
	}
}

// place writes a file inside the host's root with a chosen mode. Ownership is
// whatever the test process is, which is why the ownership rules are tested
// separately as pure functions.
func place(t *testing.T, host *OSHost, path string, mode fs.FileMode, content string) {
	t.Helper()
	full := filepath.Join(host.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, report Report, id string) Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in report; have %v", id, ids(report))
	return Check{}
}

func ids(report Report) []string {
	var out []string
	for _, c := range report.Checks {
		out = append(out, c.ID)
	}
	return out
}

func TestInspectReportsEveryMissingPieceWithoutChangingAnything(t *testing.T) {
	host := newTestHost(t)
	before := treeOf(t, host.Root)

	inspection := Inspect(context.Background(), host, Options{User: "rotup", SkipService: true})

	for _, id := range []string{"helper.installed", "helper.wrapper", "helper.sudoers", "runtime.dir", "runtime.mountroot"} {
		if got := find(t, inspection.Report, id).Status; got != StatusError {
			t.Errorf("%s = %s, want ERROR on a bare host", id, got)
		}
	}
	if inspection.Report.Ready() {
		t.Error("a host with no helper and no runtime directory must not report ready")
	}
	// Inspection is read-only. This is the property doctor's whole contract
	// rests on, so it is asserted rather than assumed.
	if after := treeOf(t, host.Root); after != before {
		t.Errorf("inspection modified the filesystem:\nbefore %q\nafter  %q", before, after)
	}
}

func TestInspectRejectsAHelperTheDaemonCouldRewrite(t *testing.T) {
	cases := []struct {
		name  string
		state FileState
		want  Status
	}{
		{"root owned and not writable by others", FileState{Exists: true, Mode: 0o755, UID: 0}, StatusOK},
		{"setuid root is a documented alternative", FileState{Exists: true, Mode: 0o4750 | fs.ModeSetuid, UID: 0}, StatusOK},
		{"owned by the service account", FileState{Exists: true, Mode: 0o755, UID: 1000}, StatusError},
		{"group writable", FileState{Exists: true, Mode: 0o775, UID: 0}, StatusError},
		{"world writable", FileState{Exists: true, Mode: 0o777, UID: 0}, StatusError},
		{"not executable", FileState{Exists: true, Mode: 0o644, UID: 0}, StatusError},
		{"a symlink that could be redirected", FileState{Exists: true, Mode: 0o777, UID: 0, IsSymlink: true}, StatusError},
		{"a directory", FileState{Exists: true, Mode: 0o755, UID: 0, IsDir: true}, StatusError},
		{"ownership unreadable on this platform", FileState{Exists: true, Mode: 0o755, UID: -1}, StatusWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluatePrivilegedFile(HelperPath, "helper.installed", "Mount helper", tc.state, HelperMode)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (detail %q)", got.Status, tc.want, got.Detail)
			}
			if got.Status != StatusOK && got.Remedy == "" {
				t.Error("a failing privileged-file check must tell the operator what to do")
			}
		})
	}
}

func TestInspectFlagsASudoPolicyWiderThanTheHelper(t *testing.T) {
	cases := map[string]bool{
		"rotup ALL=(root) NOPASSWD: /usr/libexec/rotup-mounthelper\n": false,
		"rotup ALL=(ALL) NOPASSWD: ALL\n":                             true,
		"rotup ALL=(root) NOPASSWD: /usr/libexec/*\n":                 true,
		"rotup ALL=(root) NOPASSWD: /bin/bash\n":                      true,
		"rotup ALL=(root) NOPASSWD: /usr/bin/mount\n":                 true,
		"# a comment only\n":                                          false,
	}
	for content, wantFinding := range cases {
		finding := auditSudoersRule(content)
		if (finding != "") != wantFinding {
			t.Errorf("auditSudoersRule(%q) = %q, wantFinding=%v", content, finding, wantFinding)
		}
	}
}

func TestInspectRefusesAWorldReadableSecretStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newTestHost(t)
	if err := os.MkdirAll(filepath.Join(host.Root, SecretRoot), 0o755); err != nil {
		t.Fatal(err)
	}

	inspection := Inspect(context.Background(), host, Options{User: "rotup", SkipService: true})

	check := find(t, inspection.Report, "data.secrets")
	if check.Status != StatusError {
		t.Fatalf("secret store status = %s, want ERROR for a 0755 store", check.Status)
	}
	if !strings.Contains(check.Remedy, "chmod 0700") {
		t.Errorf("remedy should name the exact repair, got %q", check.Remedy)
	}
	// Whatever else it reports, it must never have opened the store.
	for _, c := range inspection.Report.Checks {
		if strings.Contains(strings.ToLower(c.Detail), "password") && c.ID != "data.secrets" {
			t.Errorf("check %s mentions a password: %q", c.ID, c.Detail)
		}
	}
}

func TestInspectReportsAMissingEngineAsDegradedNotUnusable(t *testing.T) {
	host := newTestHost(t)
	host.Path = map[string]string{"restic": "/usr/bin/restic"} // rclone absent

	inspection := Inspect(context.Background(), host, Options{User: "rotup", SkipService: true})

	rclone := find(t, inspection.Report, "tools.rclone")
	if rclone.Status != StatusWarning {
		t.Fatalf("missing rclone = %s, want WARNING: backing up still works without it", rclone.Status)
	}
	if !strings.Contains(rclone.Remedy, "ROTUP_RCLONE_PATH") {
		t.Errorf("remedy should name the configuration variable, got %q", rclone.Remedy)
	}
}

func TestInspectRefusesAConfiguredEngineThatIsNotThere(t *testing.T) {
	host := newTestHost(t)

	inspection := Inspect(context.Background(), host,
		Options{User: "rotup", SkipService: true, ResticPath: "/opt/restic-that-was-removed"})

	if got := find(t, inspection.Report, "tools.restic").Status; got != StatusError {
		t.Fatalf("explicitly configured but absent restic = %s, want ERROR", got)
	}
}

func TestInspectWarnsWhenTheDaemonWouldRunAsRoot(t *testing.T) {
	host := newTestHost(t)
	host.Accounts["root"] = struct {
		UID   int
		Group string
		GID   int
	}{UID: 0, Group: "root", GID: 0}

	inspection := Inspect(context.Background(), host, Options{User: "root", SkipService: true})

	check := find(t, inspection.Report, "platform.account")
	if check.Status != StatusWarning {
		t.Fatalf("root service account = %s, want WARNING", check.Status)
	}
	if !strings.Contains(check.Remedy, "unprivileged") {
		t.Errorf("remedy should explain the privilege model, got %q", check.Remedy)
	}
}

func TestInspectPrefersTheAccountThatInvokedSudo(t *testing.T) {
	host := newTestHost(t)
	host.Env["SUDO_USER"] = "rotup"

	inspection := Inspect(context.Background(), host, Options{SkipService: true})

	if inspection.Layout.User != "rotup" {
		t.Fatalf("service account = %q, want the invoking account rather than root", inspection.Layout.User)
	}
}

func TestInspectRejectsAnAccountNameThatCouldRewriteTheSudoRule(t *testing.T) {
	for _, name := range []string{"", "ro tup", "rotup,root", "ALL", "rotup=x", "ro#tup", "rotup!", "-rotup", strings.Repeat("a", 33)} {
		if err := ValidateAccountName(name); err == nil && name != "ALL" {
			t.Errorf("ValidateAccountName(%q) accepted an unsafe name", name)
		}
	}
	for _, name := range []string{"rotup", "_rotup", "rotup-svc", "rotup1", "machine$"} {
		if err := ValidateAccountName(name); err != nil {
			t.Errorf("ValidateAccountName(%q) rejected a legitimate name: %v", name, err)
		}
	}
	if _, err := SudoersContent("root ALL=(ALL) NOPASSWD: ALL #"); err == nil {
		t.Fatal("SudoersContent must refuse a name that would inject another rule")
	}
}

func TestGeneratedPolicyNamesOnlyTheHelper(t *testing.T) {
	content, err := SudoersContent("rotup")
	if err != nil {
		t.Fatal(err)
	}
	if finding := auditSudoersRule(content); finding != "" {
		t.Fatalf("the rule this version generates is itself too wide: %s", finding)
	}
	if !strings.Contains(content, "rotup ALL=(root) NOPASSWD: "+HelperPath) {
		t.Fatalf("unexpected policy:\n%s", content)
	}
	if strings.Contains(content, "*") {
		t.Error("the generated policy must not contain a wildcard")
	}
}

func TestGeneratedUnitKeepsPrivilegeEscalationAndTheHostMountNamespace(t *testing.T) {
	unit, err := UnitContent(Layout{User: "rotup", Group: "rotup"})
	if err != nil {
		t.Fatal(err)
	}
	// NoNewPrivileges would break sudo, and a private mount namespace would
	// hide the helper's mounts from the daemon. Both would look like hardening
	// and would silently remove every media operation.
	if !strings.Contains(unit, "NoNewPrivileges=no") {
		t.Error("the unit must keep privilege escalation available for sudo")
	}
	for _, forbidden := range []string{"PrivateMounts", "ProtectSystem=strict", "PrivateTmp", "ProtectHome"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("the unit sets %s, which isolates the mount namespace or the home tree", forbidden)
		}
	}
	if !strings.Contains(unit, "User=rotup") || strings.Contains(unit, "User=root") {
		t.Error("the daemon must run unprivileged")
	}
	if !strings.Contains(unit, "systemd-tmpfiles-setup.service") {
		t.Error("the unit must start after the runtime directory is created")
	}
}

func TestGeneratedEnvironmentFileCarriesNoSecret(t *testing.T) {
	content := EnvFileContent(Layout{User: "rotup", Group: "rotup"}, "/usr/bin/rclone", "/usr/bin/restic", "/usr/bin/lsblk")
	// Only the assignments carry values; the header comment is allowed to say
	// that passwords are deliberately absent.
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, forbidden := range []string{"PASSWORD", "SECRET=", "TOKEN", "CREDENTIAL"} {
			if strings.Contains(strings.ToUpper(line), forbidden) {
				t.Errorf("environment file assignment contains %q: %s", forbidden, line)
			}
		}
	}
	if !strings.Contains(content, "ROTUP_MOUNT_HELPER="+WrapperPath) {
		t.Error("the daemon must be pointed at the wrapper, never at the helper directly")
	}
	if strings.Contains(content, "ROTUP_MOUNT_HELPER="+HelperPath+"\n") {
		t.Error("the daemon must not be pointed at the unelevated helper binary")
	}
}

func TestRuntimeDirectoryIsRecreatedAtBootRootOwned(t *testing.T) {
	content, err := TmpfilesContent("rotup")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"d " + RuntimeDir + " 0750 root rotup", "d " + MediaMountRoot + " 0750 root rotup"} {
		if !strings.Contains(content, want) {
			t.Errorf("tmpfiles content missing %q:\n%s", want, content)
		}
	}
	if _, err := TmpfilesContent("rotup root"); err == nil {
		t.Fatal("a group name with a space must be refused before it reaches tmpfiles.d")
	}
}

func TestReportSeverityOrdering(t *testing.T) {
	var report Report
	report.OK("g", "a", "a", "")
	if !report.Ready() || report.Worst() != StatusOK || report.Changed() {
		t.Fatal("an all-OK report must be ready, OK and unchanged")
	}
	report.Fixed("g", "b", "b", "")
	if !report.Ready() || !report.Changed() {
		t.Fatal("a repair must leave the report ready and mark it changed")
	}
	report.Warn("g", "c", "c", "", "")
	if !report.Ready() || report.Worst() != StatusWarning {
		t.Fatal("a warning must not make the system unready")
	}
	report.Fail("g", "d", "d", "", "")
	if report.Ready() || report.Worst() != StatusError {
		t.Fatal("an error must make the system unready")
	}
	failures := report.Failures()
	if len(failures) != 2 || failures[0].ID != "d" {
		t.Fatalf("failures must be most severe first, got %v", ids(Report{Checks: failures}))
	}
}

// treeOf renders a directory tree with modes so a test can assert that nothing
// changed.
func treeOf(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		lines = append(lines, relative+" "+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}
