package system

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newSetupHost builds a host rooted in a temporary directory that reports
// itself as a privileged Linux machine, so provisioning runs end to end without
// root and without touching the real system.
func newSetupHost(t *testing.T) *linuxRootHost {
	t.Helper()
	base := &OSHost{
		Root: t.TempDir(),
		Env:  map[string]string{},
		Accounts: map[string]struct {
			UID   int
			Group string
			GID   int
		}{"rotup": {UID: 1000, Group: "rotup", GID: 1000}},
		Path:      map[string]string{"visudo": "/usr/sbin/visudo", "systemctl": "/usr/bin/systemctl"},
		Commands:  map[string]CommandResult{"/usr/sbin/visudo": {}, "/usr/bin/systemctl": {}},
		SkipChown: true,
	}
	return &linuxRootHost{OSHost: base}
}

// linuxRootHost claims to be a privileged Linux host. Real chown needs root, so
// ownership is recorded and then reported back through Lstat. Modelling it
// rather than ignoring it is what makes the idempotence assertion honest: a
// converged host must look converged on the next read.
type linuxRootHost struct {
	*OSHost
	chowns []string
	owners map[string][2]int
}

func (h *linuxRootHost) GOOS() string { return "linux" }
func (h *linuxRootHost) Euid() int    { return 0 }

func (h *linuxRootHost) Chown(path string, uid, gid int) error {
	if h.owners == nil {
		h.owners = map[string][2]int{}
	}
	h.chowns = append(h.chowns, path)
	h.owners[path] = [2]int{uid, gid}
	return nil
}

func (h *linuxRootHost) Lstat(path string) (FileState, error) {
	state, err := h.OSHost.Lstat(path)
	if err != nil || !state.Exists {
		return state, err
	}
	if owner, ok := h.owners[path]; ok {
		state.UID, state.GID = owner[0], owner[1]
	}
	return state, nil
}

// stageSource writes a fake built binary outside the host root, as a real
// operator would pass from a build directory.
func stageSource(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func defaultSetup(t *testing.T, host *linuxRootHost) SetupOptions {
	t.Helper()
	return SetupOptions{
		Options:        Options{User: "rotup", SkipService: true},
		HelperSource:   stageSource(t, "#!fake helper\n"),
		DaemonSource:   stageSource(t, "#!fake daemon\n"),
		InstallService: true,
	}
}

func TestSetupProvisionsEveryPieceAndIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	options := defaultSetup(t, host)

	first, _ := Setup(context.Background(), host, options)
	if !first.Ready() {
		t.Fatalf("first setup left errors: %v", failureDetails(first))
	}
	if !first.Changed() {
		t.Fatal("the first run of setup on a bare host must report changes")
	}
	for _, id := range []string{"setup.helper", "setup.wrapper", "setup.sudoers", "setup.tmpfiles",
		"setup.runtime", "setup.mountroot", "setup.data", "setup.staging", "setup.secrets", "setup.env", "setup.unit"} {
		if got := find(t, first, id).Status; got != StatusFixed {
			t.Errorf("%s = %s on a bare host, want FIXED", id, got)
		}
	}

	second, inspection := Setup(context.Background(), host, options)
	if second.Changed() {
		var changed []string
		for _, c := range second.Checks {
			if c.Status == StatusFixed {
				changed = append(changed, c.ID+": "+c.Detail)
			}
		}
		t.Fatalf("rerunning setup on a converged host changed things: %v", changed)
	}
	if !second.Ready() {
		t.Fatalf("rerun left errors: %v", failureDetails(second))
	}
	_ = inspection
}

func TestSetupDoesNotDuplicateOrWidenTheSudoPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	options := defaultSetup(t, host)

	for i := 0; i < 3; i++ {
		Setup(context.Background(), host, options)
	}

	content, err := os.ReadFile(filepath.Join(host.Root, SudoersPath))
	if err != nil {
		t.Fatal(err)
	}
	rules := 0
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			rules++
		}
	}
	if rules != 1 {
		t.Fatalf("three setup runs produced %d rules, want exactly 1:\n%s", rules, content)
	}
	if finding := auditSudoersRule(string(content)); finding != "" {
		t.Fatalf("installed policy is wider than the helper: %s", finding)
	}
	info, err := os.Stat(filepath.Join(host.Root, SudoersPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != SudoersMode.Perm() {
		t.Fatalf("sudoers mode = %04o, want %04o; sudo ignores a file it does not trust", info.Mode().Perm(), SudoersMode.Perm())
	}
}

func TestSetupReplacesAWiderHandEditedPolicyAndSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	place(t, host.OSHost, SudoersPath, 0o440, "rotup ALL=(ALL) NOPASSWD: ALL\n")

	report, _ := Setup(context.Background(), host, defaultSetup(t, host))

	previous := find(t, report, "setup.sudoers.previous")
	if previous.Status != StatusWarning || !strings.Contains(previous.Detail, "unrestricted root") {
		t.Fatalf("replacing a wide policy must be reported, got %s %q", previous.Status, previous.Detail)
	}
	content, err := os.ReadFile(filepath.Join(host.Root, SudoersPath))
	if err != nil {
		t.Fatal(err)
	}
	if finding := auditSudoersRule(string(content)); finding != "" {
		t.Fatalf("the replacement is still too wide: %s", finding)
	}
}

func TestSetupRefusesToInstallWhenVisudoRejectsTheRule(t *testing.T) {
	host := newSetupHost(t)
	host.Commands["/usr/sbin/visudo"] = CommandResult{Output: "syntax error near line 1", ExitCode: 1}

	report, _ := Setup(context.Background(), host, defaultSetup(t, host))

	check := find(t, report, "setup.sudoers")
	if check.Status != StatusError {
		t.Fatalf("sudoers status = %s, want ERROR when visudo rejects the rule", check.Status)
	}
	// Nothing may be installed when validation fails: a broken file in
	// sudoers.d can break sudo for every account on the host.
	if _, err := os.Stat(filepath.Join(host.Root, SudoersPath)); err == nil {
		t.Fatal("a rejected rule must not be installed")
	}
}

func TestSetupKeepsAnEditedConfigurationFileUnlessForced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	options := defaultSetup(t, host)
	Setup(context.Background(), host, options)

	edited := "ROTUP_HTTP_ADDR=127.0.0.1:9999\n"
	place(t, host.OSHost, EnvFilePath, EnvFileMode, edited)

	report, _ := Setup(context.Background(), host, options)
	if got := find(t, report, "setup.env").Status; got != StatusWarning {
		t.Fatalf("edited configuration = %s, want WARNING", got)
	}
	content, err := os.ReadFile(filepath.Join(host.Root, EnvFilePath))
	if err != nil || string(content) != edited {
		t.Fatalf("setup overwrote an operator's edit: %q", content)
	}

	options.Force = true
	report, _ = Setup(context.Background(), host, options)
	if got := find(t, report, "setup.env").Status; got != StatusFixed {
		t.Fatalf("forced configuration rewrite = %s, want FIXED", got)
	}
}

func TestSetupNeverRemovesExistingApplicationState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	// Pre-existing state a rerun must not touch: a database, a staging tree
	// and a stored credential.
	place(t, host.OSHost, DatabasePath, 0o640, "sqlite-contents")
	place(t, host.OSHost, StagingRoot+"/wsl-test/current/report.txt", 0o640, "collected file")
	place(t, host.OSHost, SecretRoot+"/some-credential", 0o600, "SECRET-CANARY")
	if err := os.Chmod(filepath.Join(host.Root, SecretRoot), 0o755); err != nil {
		t.Fatal(err)
	}

	report, _ := Setup(context.Background(), host, defaultSetup(t, host))

	for _, path := range []string{DatabasePath, StagingRoot + "/wsl-test/current/report.txt", SecretRoot + "/some-credential"} {
		if _, err := os.Stat(filepath.Join(host.Root, path)); err != nil {
			t.Errorf("setup removed %s: %v", path, err)
		}
	}
	// The credential's own bytes are never read, and its directory's unsafe
	// mode is repaired.
	info, err := os.Stat(filepath.Join(host.Root, SecretRoot))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != SecretDirMode.Perm() {
		t.Errorf("secret store mode = %04o, want %04o", info.Mode().Perm(), SecretDirMode.Perm())
	}
	for _, c := range report.Checks {
		if strings.Contains(c.Detail, "SECRET-CANARY") {
			t.Errorf("check %s leaked stored credential material: %q", c.ID, c.Detail)
		}
	}
}

func TestSetupRefusesToRunUnprivilegedAndOffLinux(t *testing.T) {
	unprivileged := &OSHost{Root: t.TempDir(), Env: map[string]string{}}
	report, _ := Setup(context.Background(), unprivileged, SetupOptions{Options: Options{User: "rotup", SkipService: true}})
	if report.Ready() {
		t.Fatal("setup must refuse to provision without the privilege to do it")
	}

	host := newSetupHost(t)
	report, _ = Setup(context.Background(), host, SetupOptions{
		Options: Options{User: "nobody-here", SkipService: true},
	})
	check := find(t, report, "setup.account")
	if check.Status != StatusError || !strings.Contains(check.Remedy, "useradd") {
		t.Fatalf("an unknown account must stop setup with a usable remedy, got %s %q", check.Status, check.Remedy)
	}
	// Nothing may be written before the account resolves, because every file
	// setup writes names it.
	if _, err := os.Stat(filepath.Join(host.Root, SudoersPath)); err == nil {
		t.Fatal("setup wrote a policy for an account that does not exist")
	}
}

func TestSetupDryRunChangesNothing(t *testing.T) {
	host := newSetupHost(t)
	options := defaultSetup(t, host)
	options.DryRun = true
	before := treeOf(t, host.Root)

	report, _ := Setup(context.Background(), host, options)

	if !report.Changed() {
		t.Fatal("a dry run should still report the changes it would make")
	}
	if after := treeOf(t, host.Root); after != before {
		t.Fatalf("dry run modified the filesystem:\nbefore %q\nafter  %q", before, after)
	}
	if len(host.chowns) != 0 {
		t.Fatalf("dry run changed ownership of %v", host.chowns)
	}
}

func TestSetupDoesNotStartTheServiceUnlessAsked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)
	options := defaultSetup(t, host)

	report, _ := Setup(context.Background(), host, options)

	state := find(t, report, "setup.service")
	if state.Status != StatusOK || !strings.Contains(state.Detail, "not enabled or started") {
		t.Fatalf("setup started a service without being asked: %s %q", state.Status, state.Detail)
	}

	options.EnableService = true
	report, _ = Setup(context.Background(), host, options)
	if got := find(t, report, "setup.service"); got.Status != StatusFixed {
		t.Fatalf("--enable-service = %s, want FIXED", got.Status)
	}
}

func TestSetupInstallsTheHelperRootOwnedAndPointsTheDaemonAtTheWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not modelled on this platform")
	}
	host := newSetupHost(t)

	Setup(context.Background(), host, defaultSetup(t, host))

	for _, path := range []string{HelperPath, WrapperPath, SudoersPath, TmpfilesPath, UnitPath} {
		owned := false
		for _, chowned := range host.chowns {
			if chowned == path {
				owned = true
			}
		}
		if !owned {
			t.Errorf("%s was installed without being given root ownership", path)
		}
	}
	env, err := os.ReadFile(filepath.Join(host.Root, EnvFilePath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "ROTUP_MOUNT_HELPER="+WrapperPath) {
		t.Fatalf("the daemon must be pointed at the wrapper:\n%s", env)
	}
	wrapper, err := os.ReadFile(filepath.Join(host.Root, WrapperPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrapper), "sudo -n -- "+HelperPath) {
		t.Fatalf("the wrapper must call sudo noninteractively against the fixed helper path:\n%s", wrapper)
	}
}

func failureDetails(report Report) []string {
	var out []string
	for _, c := range report.Failures() {
		out = append(out, string(c.Status)+" "+c.ID+": "+c.Detail)
	}
	return out
}
