package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SetupOptions controls one provisioning run.
type SetupOptions struct {
	Options
	// HelperSource is the built rotup-mounthelper to install. Empty means the
	// helper must already be in place; setup then verifies it and does not
	// install anything.
	HelperSource string
	// DaemonSource is the built rotupd to install at DaemonPath. Empty means
	// leave whatever is installed alone.
	DaemonSource string
	// EnableService installs the systemd unit and enables and starts it.
	// Setup never starts a service implicitly: an install that silently began
	// running backups would be a surprise, so this is opt-in.
	EnableService bool
	// InstallService installs (but does not enable) the unit. EnableService
	// implies it.
	InstallService bool
	// Force permits replacing a configuration file an operator has edited.
	// Without it, a modified environment file is reported and left alone.
	Force bool
	// DryRun reports every action setup would take and performs none of them.
	DryRun bool
}

// Setup provisions the Linux integration and returns what it did, followed by a
// fresh read-only inspection of the result.
//
// It is idempotent: on a converged host every action reports OK and Changed is
// false. It never touches application state. It does not write, move, or read
// the secret store's contents, does not open the database, does not mount
// anything, and does not alter any registered disk's identity.
func Setup(ctx context.Context, host Host, options SetupOptions) (Report, Inspection) {
	var report Report

	if host.GOOS() != "linux" {
		report.Fail(GroupPlatform, "setup.platform", "Operating system",
			"setup provisions Linux system integration and this host is "+host.GOOS(),
			"Run setup on the Linux host that will run ROTUP.")
		return report, Inspect(ctx, host, options.Options)
	}
	if euid := host.Euid(); euid != 0 && !options.DryRun {
		report.Fail(GroupPlatform, "setup.privilege", "Privilege",
			fmt.Sprintf("setup writes to %s, %s and %s and is running as uid %d",
				HelperPath, SudoersPath, TmpfilesPath, euid),
			"Run `sudo rotupd setup`. The daemon itself must keep running unprivileged; only this one-off provisioning needs root.")
		return report, Inspect(ctx, host, options.Options)
	}

	// Resolve the service account first: every file setup writes names it, and
	// an unresolvable or unsafe name must stop the run before anything is
	// written rather than half-way through.
	layout, ok := resolveLayout(host, options, &report)
	if !ok {
		return report, Inspect(ctx, host, options.Options)
	}
	tools := discoverTools(host, options.Options)

	provisionDaemon(host, options, &report)
	provisionHelper(host, options, &report)
	provisionSudoers(ctx, host, options, layout, &report)
	provisionRuntime(host, options, layout, &report)
	provisionData(host, options, layout, &report)
	provisionConfig(host, options, layout, tools, &report)
	if options.InstallService || options.EnableService {
		provisionService(ctx, host, options, layout, &report)
	}

	verification := options.Options
	verification.User = layout.User
	return report, Inspect(ctx, host, verification)
}

func resolveLayout(host Host, options SetupOptions, report *Report) (Layout, bool) {
	account := strings.TrimSpace(options.User)
	if account == "" {
		account = defaultServiceAccount(host)
	}
	if err := ValidateAccountName(account); err != nil {
		report.Fail(GroupPlatform, "setup.account", "Service account", err.Error(),
			"Pass a valid existing account with --user.")
		return Layout{}, false
	}
	uid, group, gid, err := host.LookupAccount(account)
	if err != nil {
		report.Fail(GroupPlatform, "setup.account", "Service account",
			fmt.Sprintf("account %q does not exist on this host", account),
			"Create it first, for example `sudo useradd --system --home-dir "+DataDir+" --shell /usr/sbin/nologin "+account+"`, then rerun setup.")
		return Layout{}, false
	}
	if uid == 0 {
		report.Warn(GroupPlatform, "setup.account", "Service account",
			"provisioning for root; the daemon is designed to run unprivileged",
			"Pass --user with an ordinary account. Only the mount helper needs root.")
	} else {
		report.OK(GroupPlatform, "setup.account", "Service account",
			fmt.Sprintf("%s (uid %d, group %s gid %d)", account, uid, group, gid))
	}
	return Layout{User: account, Group: group, UID: uid, GID: gid}, true
}

func discoverTools(host Host, options Options) Tools {
	tools := Tools{
		RclonePath: locateTool(host, options.RclonePath, "rclone"),
		ResticPath: locateTool(host, options.ResticPath, "restic"),
		LsblkPath:  locateTool(host, options.LsblkPath, "lsblk"),
	}
	if tools.LsblkPath == "" {
		tools.LsblkPath = firstExisting(host, "/usr/bin/lsblk", "/bin/lsblk")
	}
	return tools
}

func provisionDaemon(host Host, options SetupOptions, report *Report) {
	if strings.TrimSpace(options.DaemonSource) == "" {
		state, err := host.Lstat(DaemonPath)
		if err != nil || !state.Exists {
			report.Warn(GroupService, "setup.daemon", "Daemon binary",
				DaemonPath+" is not installed",
				"Pass --daemon <path to built rotupd> so the service unit has something to start, or install it yourself.")
			return
		}
		report.OK(GroupService, "setup.daemon", "Daemon binary", DaemonPath+" already installed")
		return
	}
	installExecutable(host, options, report, GroupService, "setup.daemon", "Daemon binary",
		options.DaemonSource, DaemonPath, HelperMode)
}

func provisionHelper(host Host, options SetupOptions, report *Report) {
	if strings.TrimSpace(options.HelperSource) == "" {
		state, err := host.Lstat(HelperPath)
		if err != nil || !state.Exists {
			report.Fail(GroupHelper, "setup.helper", "Mount helper", HelperPath+" is not installed",
				"Build it with `go build ./cmd/rotup-mounthelper` and rerun setup with --helper <path>.")
		} else {
			report.Add(evaluatePrivilegedFile(HelperPath, "setup.helper", "Mount helper", state, HelperMode))
		}
	} else {
		installExecutable(host, options, report, GroupHelper, "setup.helper", "Mount helper",
			options.HelperSource, HelperPath, HelperMode)
	}
	writeManagedFile(host, options, report, GroupHelper, "setup.wrapper", "Sudo wrapper",
		WrapperPath, []byte(WrapperContent), HelperMode, true)
}

// installExecutable copies a built binary into place root-owned. It refuses a
// source the service account could have replaced between build and install
// only in the sense that it records the source path; the operator chooses what
// to install, and the sudo policy is what constrains what may then run as root.
func installExecutable(host Host, options SetupOptions, report *Report, group, id, title, source, destination string, mode fs.FileMode) {
	sourceState, err := host.StatSource(source)
	if err != nil || !sourceState.Exists {
		report.Fail(group, id, title, "the source binary "+source+" does not exist",
			"Build it first, then rerun setup with the correct path.")
		return
	}
	if sourceState.IsDir {
		report.Fail(group, id, title, source+" is a directory", "Name the built executable itself.")
		return
	}

	current, _ := host.Lstat(destination)
	if current.Exists && sameFileContent(host, source, destination) &&
		current.UID == 0 && current.Mode.Perm() == mode.Perm() {
		report.OK(group, id, title, destination+" is already the requested binary, root-owned, mode "+fmt.Sprintf("%04o", mode.Perm()))
		return
	}
	if options.DryRun {
		report.Fixed(group, id, title, "would install "+source+" to "+destination+" root-owned, mode "+fmt.Sprintf("%04o", mode.Perm()))
		return
	}
	if err := host.CopyExecutableAtomic(source, destination, mode); err != nil {
		report.Fail(group, id, title, "installing "+destination+" failed: "+safeError(err), "Check the destination directory and available space.")
		return
	}
	if err := host.Chown(destination, 0, 0); err != nil && !errors.Is(err, ErrNotSupported) {
		report.Fail(group, id, title, "setting root ownership on "+destination+" failed: "+safeError(err),
			"A privileged binary the daemon account can rewrite is not a privilege boundary.")
		return
	}
	report.Fixed(group, id, title, "installed "+destination+" root-owned, mode "+fmt.Sprintf("%04o", mode.Perm()))
}

// sameFileContent compares an installed file with its intended source. This is
// what makes reinstalling the same build a no-op instead of a rewrite.
func sameFileContent(host Host, source, destination string) bool {
	left, err := host.ReadSource(source)
	if err != nil {
		return false
	}
	right, err := host.ReadFile(destination)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func provisionSudoers(ctx context.Context, host Host, options SetupOptions, layout Layout, report *Report) {
	content, err := SudoersContent(layout.User)
	if err != nil {
		report.Fail(GroupHelper, "setup.sudoers", "Sudo policy", err.Error(),
			"Pass an account name that is safe to place in a sudoers rule.")
		return
	}

	existing, readErr := host.ReadFile(SudoersPath)
	state, _ := host.Lstat(SudoersPath)
	if readErr == nil && string(existing) == content && state.UID == 0 && state.Mode.Perm() == SudoersMode.Perm() {
		report.OK(GroupHelper, "setup.sudoers", "Sudo policy",
			SudoersPath+" already authorizes "+layout.User+" to run "+HelperPath+" as root")
		return
	}
	if readErr == nil && string(existing) != content {
		if finding := auditSudoersRule(string(existing)); finding != "" {
			// A hand-widened policy is replaced with the narrow one; that is a
			// repair, and the operator is told exactly what was there.
			report.Warn(GroupHelper, "setup.sudoers.previous", "Previous sudo policy",
				"replacing a policy that was wider than ROTUP needs: "+finding,
				"Confirm nothing else depended on that rule. ROTUP only ever needs "+HelperPath+".")
		}
	}
	if options.DryRun {
		report.Fixed(GroupHelper, "setup.sudoers", "Sudo policy",
			"would write "+SudoersPath+" authorizing "+layout.User+" to run "+HelperPath+" as root, mode 0440")
		return
	}

	// Validate before installing. An invalid file in sudoers.d can break sudo
	// for every account on the host, so it is checked in a scratch location
	// first and only then renamed into place.
	if err := validateSudoers(ctx, host, content, report); err != nil {
		return
	}
	if err := host.WriteFileAtomic(SudoersPath, []byte(content), SudoersMode); err != nil {
		report.Fail(GroupHelper, "setup.sudoers", "Sudo policy", "writing "+SudoersPath+" failed: "+safeError(err), "")
		return
	}
	if err := host.Chown(SudoersPath, 0, 0); err != nil && !errors.Is(err, ErrNotSupported) {
		report.Fail(GroupHelper, "setup.sudoers", "Sudo policy",
			"setting root ownership on "+SudoersPath+" failed: "+safeError(err),
			"sudo ignores a policy file it does not trust.")
		return
	}
	report.Fixed(GroupHelper, "setup.sudoers", "Sudo policy",
		"wrote "+SudoersPath+" authorizing "+layout.User+" to run "+HelperPath+" as root, mode 0440")
}

// validateSudoers runs visudo over the candidate content. A host without visudo
// gets a warning rather than a refusal: the generated rule is a fixed single
// line built from a validated account name, so the syntax risk is small, but
// the check is worth having wherever it is available.
func validateSudoers(ctx context.Context, host Host, content string, report *Report) error {
	visudo, err := host.LookPath("visudo")
	if err != nil {
		// A root shell often has /usr/sbin on PATH and a service context does
		// not, so the conventional locations are tried before giving up.
		visudo = firstExisting(host, "/usr/sbin/visudo", "/sbin/visudo")
	}
	if visudo == "" {
		report.Warn(GroupHelper, "setup.sudoers.validated", "Sudo policy syntax",
			"visudo is not available, so the generated rule was installed without an external syntax check",
			"Install sudo's own tooling if you want setup to validate the file before installing it.")
		return nil
	}
	scratch, err := os.CreateTemp("", "rotup-sudoers-*")
	if err != nil {
		report.Warn(GroupHelper, "setup.sudoers.validated", "Sudo policy syntax",
			"a scratch file for validation could not be created", "")
		return nil
	}
	name := scratch.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := scratch.WriteString(content); err != nil {
		_ = scratch.Close()
		return nil
	}
	if err := scratch.Close(); err != nil {
		return nil
	}
	output, code, err := host.Run(ctx, visudo, "-cf", name)
	if err != nil {
		report.Warn(GroupHelper, "setup.sudoers.validated", "Sudo policy syntax",
			"visudo could not be run, so the rule was installed without an external syntax check", "")
		return nil
	}
	if code != 0 {
		message := fmt.Errorf("visudo rejected the generated rule: %s", firstLine(output))
		report.Fail(GroupHelper, "setup.sudoers", "Sudo policy", message.Error(),
			"This is a ROTUP defect. Nothing was installed; the existing policy is untouched.")
		return message
	}
	report.OK(GroupHelper, "setup.sudoers.validated", "Sudo policy syntax", "visudo accepted the generated rule")
	return nil
}

func provisionRuntime(host Host, options SetupOptions, layout Layout, report *Report) {
	content, err := TmpfilesContent(layout.Group)
	if err != nil {
		report.Fail(GroupRuntime, "setup.tmpfiles", "Boot-time recreation", err.Error(), "")
		return
	}
	writeManagedFile(host, options, report, GroupRuntime, "setup.tmpfiles", "Boot-time recreation",
		TmpfilesPath, []byte(content), PlainFileMode, true)

	// tmpfiles.d handles every boot from here on. Create the tree now so the
	// current boot does not have to wait for the next one.
	for _, path := range []string{RuntimeDir, MediaMountRoot} {
		id := "setup.runtime"
		title := "Runtime directory"
		if path == MediaMountRoot {
			id, title = "setup.mountroot", "Media mount root"
		}
		if !ensureDirectory(host, options, report, GroupRuntime, id, title, path, RuntimeDirMode, 0, layout.GID) {
			return
		}
	}
}

func provisionData(host Host, options SetupOptions, layout Layout, report *Report) {
	if !ensureDirectory(host, options, report, GroupData, "setup.data", "Data directory",
		DataDir, DataDirMode, layout.UID, layout.GID) {
		return
	}
	ensureDirectory(host, options, report, GroupData, "setup.staging", "Staging root",
		StagingRoot, DataDirMode, layout.UID, layout.GID)
	// The secret store's mode is the whole protection for stored credentials.
	// Setup creates it correctly; it never reads, lists or copies its contents.
	ensureDirectory(host, options, report, GroupData, "setup.secrets", "Secret store",
		SecretRoot, SecretDirMode, layout.UID, layout.GID)
}

func provisionConfig(host Host, options SetupOptions, layout Layout, tools Tools, report *Report) {
	if !ensureDirectory(host, options, report, GroupData, "setup.configdir", "Configuration directory",
		ConfigDir, ConfigDirMode, 0, layout.GID) {
		return
	}

	content := EnvFileContent(layout, tools.RclonePath, tools.ResticPath, tools.LsblkPath)
	existing, err := host.ReadFile(EnvFilePath)
	if err == nil && string(existing) != content && !options.Force {
		// An operator who tuned a timeout or a listen address must not lose it
		// to a rerun. Setup says what it would have written and stops there.
		report.Warn(GroupData, "setup.env", "Configuration file",
			EnvFilePath+" exists and differs from what setup generates; it was left unchanged",
			"Review it, or rerun with --force to replace it. It holds no secrets, so replacing it loses only local edits.")
		return
	}
	writeManagedFile(host, options, report, GroupData, "setup.env", "Configuration file",
		EnvFilePath, []byte(content), EnvFileMode, false)
	if !options.DryRun {
		if err := host.Chown(EnvFilePath, 0, layout.GID); err != nil && !errors.Is(err, ErrNotSupported) {
			report.Warn(GroupData, "setup.env.owner", "Configuration file ownership",
				"group ownership of "+EnvFilePath+" could not be set: "+safeError(err),
				"The service account must be able to read it.")
		}
	}
}

func provisionService(ctx context.Context, host Host, options SetupOptions, layout Layout, report *Report) {
	content, err := UnitContent(layout)
	if err != nil {
		report.Fail(GroupService, "setup.unit", "systemd unit", err.Error(), "")
		return
	}
	writeManagedFile(host, options, report, GroupService, "setup.unit", "systemd unit",
		UnitPath, []byte(content), PlainFileMode, true)
	if options.DryRun {
		if options.EnableService {
			report.Fixed(GroupService, "setup.service", "Service state",
				"would run systemctl daemon-reload, enable rotupd.service and start it")
		}
		return
	}

	systemctl, err := host.LookPath("systemctl")
	if err != nil {
		report.Warn(GroupService, "setup.service", "Service state",
			"systemctl is not available; the unit was written but cannot be loaded here",
			"On a host without systemd, start "+DaemonPath+" with your own supervisor and load "+EnvFilePath+" yourself.")
		return
	}
	if output, code, err := host.Run(ctx, systemctl, "daemon-reload"); err != nil || code != 0 {
		report.Warn(GroupService, "setup.service", "Service state",
			"systemctl daemon-reload did not succeed: "+firstLine(output), "Run it by hand and check the unit file.")
		return
	}
	if !options.EnableService {
		report.OK(GroupService, "setup.service", "Service state",
			"unit installed and loaded; not enabled or started. Use --enable-service, or `sudo systemctl enable --now rotupd`.")
		return
	}
	// Starting the daemon does not start a backup. Every collection and backup
	// in ROTUP is an explicit operator action, so this is safe to do on
	// request; it is still never done implicitly.
	if output, code, err := host.Run(ctx, systemctl, "enable", "--now", "rotupd.service"); err != nil || code != 0 {
		report.Fail(GroupService, "setup.service", "Service state",
			"enabling rotupd.service failed: "+firstLine(output),
			"Check `systemctl status rotupd` and `journalctl -u rotupd`.")
		return
	}
	report.Fixed(GroupService, "setup.service", "Service state", "rotupd.service enabled and started")
}

// writeManagedFile installs a file setup owns. Identical content is a no-op,
// which is what makes a rerun report no changes.
func writeManagedFile(host Host, options SetupOptions, report *Report, group, id, title, path string, content []byte, mode fs.FileMode, rootOwned bool) {
	existing, err := host.ReadFile(path)
	state, _ := host.Lstat(path)
	if err == nil && bytes.Equal(existing, content) && state.Mode.Perm() == mode.Perm() &&
		(!rootOwned || state.UID <= 0) {
		report.OK(group, id, title, path+" is already correct")
		return
	}
	if options.DryRun {
		action := "would write"
		if state.Exists {
			action = "would update"
		}
		report.Fixed(group, id, title, action+" "+path+", mode "+fmt.Sprintf("%04o", mode.Perm()))
		return
	}
	if err := host.WriteFileAtomic(path, content, mode); err != nil {
		report.Fail(group, id, title, "writing "+path+" failed: "+safeError(err), "")
		return
	}
	if rootOwned {
		if err := host.Chown(path, 0, 0); err != nil && !errors.Is(err, ErrNotSupported) {
			report.Fail(group, id, title, "setting root ownership on "+path+" failed: "+safeError(err), "")
			return
		}
	}
	report.Fixed(group, id, title, "wrote "+path+", mode "+fmt.Sprintf("%04o", mode.Perm()))
}

// ensureDirectory creates a directory with an exact mode and owner, and repairs
// only those two things on an existing one. It never removes or empties a
// directory that is already there, so rerunning setup cannot destroy staging,
// secrets or a database.
func ensureDirectory(host Host, options SetupOptions, report *Report, group, id, title, path string, mode fs.FileMode, uid, gid int) bool {
	// Lstat only protects the final component. Check parents from the root
	// down before observing or mutating a child of a redirected directory.
	var parents []string
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		parents = append(parents, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	for i := len(parents) - 1; i >= 0; i-- {
		parent, err := host.Lstat(parents[i])
		if err != nil || parent.IsSymlink || (parent.Exists && !parent.IsDir) {
			report.Fail(group, id, title, parents[i]+" is not a safe directory ancestor",
				"Resolve the ancestor path by hand. Setup will not follow symbolic links when provisioning directories.")
			return false
		}
	}
	state, err := host.Lstat(path)
	if err != nil {
		report.Fail(group, id, title, path+" could not be inspected: "+safeError(err), "")
		return false
	}
	if state.Exists && !state.IsDir {
		report.Fail(group, id, title, path+" exists and is not a directory",
			"Move it aside by hand. Setup will not remove anything that is already there.")
		return false
	}

	correctMode := state.Exists && state.Mode.Perm() == mode.Perm()
	correctOwner := state.Exists && (state.UID < 0 || (state.UID == uid && (gid <= 0 || state.GID == gid)))
	if correctMode && correctOwner {
		report.OK(group, id, title, fmt.Sprintf("%s, mode %04o", path, mode.Perm()))
		return true
	}
	if options.DryRun {
		action := "would create"
		if state.Exists {
			action = "would correct permissions on"
		}
		report.Fixed(group, id, title, fmt.Sprintf("%s %s to mode %04o", action, path, mode.Perm()))
		return true
	}

	if !state.Exists {
		if err := host.MkdirAll(path, mode); err != nil {
			report.Fail(group, id, title, "creating "+path+" failed: "+safeError(err), "")
			return false
		}
	}
	// MkdirAll leaves an existing directory's mode alone and applies umask to a
	// new one, so the mode is always set explicitly afterwards.
	if err := host.Chmod(path, mode); err != nil {
		report.Fail(group, id, title, "setting permissions on "+path+" failed: "+safeError(err), "")
		return false
	}
	if err := host.Chown(path, uid, gid); err != nil && !errors.Is(err, ErrNotSupported) {
		report.Fail(group, id, title, "setting ownership on "+path+" failed: "+safeError(err), "")
		return false
	}
	action := "created"
	if state.Exists {
		action = "corrected"
	}
	report.Fixed(group, id, title, fmt.Sprintf("%s %s, mode %04o", action, path, mode.Perm()))
	return true
}

// safeError keeps a filesystem error's shape without repeating a path that may
// already be in the message twice, and without leaking anything else.
func safeError(err error) string {
	if err == nil {
		return ""
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}
	return filepath.Base(err.Error())
}
