package system

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
)

// Options selects what an inspection or a provisioning run looks at.
type Options struct {
	// User is the account the daemon runs as. Empty means the current account.
	User string
	// Active permits checks that start a process: version probes and, for
	// doctor, a single no-argument helper invocation through sudo. Nothing in
	// this mode mounts, writes application state, or touches media.
	Active bool
	// SkipService omits systemd checks, for hosts that do not use it.
	SkipService bool
	// RclonePath, ResticPath and LsblkPath override tool discovery, mirroring
	// the daemon's own configuration variables.
	RclonePath string
	ResticPath string
	LsblkPath  string
}

// Inspection is a complete read-only assessment.
type Inspection struct {
	Report Report `json:"report"`
	// Layout is the resolved installation identity the checks were made
	// against.
	Layout Layout `json:"layout"`
	// Tools records what was discovered, for the report and for provisioning
	// to write into the environment file.
	Tools Tools `json:"tools"`
}

// Tools is the discovered external tooling.
type Tools struct {
	RclonePath    string `json:"rclonePath"`
	RcloneVersion string `json:"rcloneVersion"`
	ResticPath    string `json:"resticPath"`
	ResticVersion string `json:"resticVersion"`
	LsblkPath     string `json:"lsblkPath"`
	BlkidPath     string `json:"blkidPath"`
}

// Inspect performs every read-only check and changes nothing. It is what
// `rotupd doctor` runs, and what `rotupd setup` runs at the end of its own
// work to report final readiness.
func Inspect(ctx context.Context, host Host, options Options) Inspection {
	inspection := Inspection{}
	inspectPlatform(host, options, &inspection)
	inspectTools(ctx, host, options, &inspection)
	inspectHelper(ctx, host, options, &inspection)
	inspectRuntime(host, &inspection)
	inspectData(host, &inspection)
	if !options.SkipService {
		inspectService(ctx, host, &inspection)
	}
	return inspection
}

func inspectPlatform(host Host, options Options, in *Inspection) {
	report := &in.Report
	if host.GOOS() != "linux" {
		report.Fail(GroupPlatform, "platform.supported", "Operating system",
			fmt.Sprintf("this host reports %s; ROTUP's media, helper and service integration is Linux-only", host.GOOS()),
			"Run ROTUP on Linux. Other platforms build and run the daemon for development only.")
	} else {
		report.OK(GroupPlatform, "platform.supported", "Operating system", "Linux")
	}

	account := strings.TrimSpace(options.User)
	if account == "" {
		account = defaultServiceAccount(host)
	}
	in.Layout.User = account
	if err := ValidateAccountName(account); err != nil {
		report.Fail(GroupPlatform, "platform.account", "Service account",
			err.Error(),
			"Pass an explicit account with --user. It must exist already; setup does not create accounts.")
		return
	}
	uid, group, gid, err := host.LookupAccount(account)
	if err != nil {
		report.Fail(GroupPlatform, "platform.account", "Service account",
			fmt.Sprintf("account %q could not be resolved on this host", account),
			"Create the account first, or pass an existing one with --user.")
		return
	}
	in.Layout.UID, in.Layout.Group, in.Layout.GID = uid, group, gid
	detail := fmt.Sprintf("%s (uid %d, group %s gid %d)", account, uid, group, gid)
	if uid == 0 {
		report.Warn(GroupPlatform, "platform.account", "Service account", detail,
			"The daemon is meant to run unprivileged. Only the mount helper needs root; running rotupd as root gives the same privilege to its HTTP server and to every tool it spawns.")
		return
	}
	report.OK(GroupPlatform, "platform.account", "Service account", detail)
}

// defaultServiceAccount prefers the account that invoked sudo. Running
// `sudo rotupd setup` should provision for the operator, not for root.
func defaultServiceAccount(host Host) string {
	if value, ok := host.LookupEnv("SUDO_USER"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return host.CurrentUser()
}

func inspectTools(ctx context.Context, host Host, options Options, in *Inspection) {
	report := &in.Report

	in.Tools.RclonePath = locateTool(host, options.RclonePath, "rclone")
	in.Tools.ResticPath = locateTool(host, options.ResticPath, "restic")
	in.Tools.LsblkPath = locateTool(host, options.LsblkPath, "lsblk")
	if in.Tools.LsblkPath == "" {
		in.Tools.LsblkPath = firstExisting(host, "/usr/bin/lsblk", "/bin/lsblk")
	}
	in.Tools.BlkidPath = firstExisting(host, "/usr/sbin/blkid", "/sbin/blkid")

	in.Tools.RcloneVersion = checkTool(ctx, host, options, report, toolSpec{
		id: "tools.rclone", title: "rclone", path: in.Tools.RclonePath,
		capability: "collecting a source into local staging",
		install:    "Install rclone (apt install rclone, or the upstream release), then set ROTUP_RCLONE_PATH to its absolute path.",
		args:       []string{"version"},
	})
	in.Tools.ResticVersion = checkTool(ctx, host, options, report, toolSpec{
		id: "tools.restic", title: "restic", path: in.Tools.ResticPath,
		capability: "backing up to a disk",
		install:    "Install restic (apt install restic, or the upstream release), then set ROTUP_RESTIC_PATH to its absolute path.",
		args:       []string{"version"},
	})

	// lsblk and blkid are util-linux and are required, not optional: without
	// them ROTUP cannot see or identify media at all.
	requireTool(host, report, "tools.lsblk", "lsblk", in.Tools.LsblkPath,
		"enumerating block devices", "Install util-linux, then set ROTUP_LSBLK_PATH if it is not at /usr/bin/lsblk.")
	requireTool(host, report, "tools.blkid", "blkid", in.Tools.BlkidPath,
		"reading filesystem identity inside the privileged helper",
		"Install util-linux. The helper uses a fixed /usr/sbin/blkid and accepts no path from its caller.")
}

func locateTool(host Host, configured, name string) string {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured)
	}
	if found, err := host.LookPath(name); err == nil {
		return found
	}
	return ""
}

func firstExisting(host Host, candidates ...string) string {
	for _, candidate := range candidates {
		if state, err := host.Lstat(candidate); err == nil && state.Exists {
			return candidate
		}
	}
	return ""
}

type toolSpec struct {
	id, title, path, capability, install string
	args                                 []string
}

// checkTool reports on an optional-but-important engine and returns the version
// it observed. A missing engine is a warning, not an error: the other half of
// ROTUP still works, and calling the whole system unusable would hide that.
func checkTool(ctx context.Context, host Host, options Options, report *Report, spec toolSpec) string {
	if spec.path == "" {
		report.Warn(GroupTools, spec.id, spec.title,
			"not found on PATH and not configured; "+spec.capability+" is unavailable", spec.install)
		return ""
	}
	state, err := host.Lstat(spec.path)
	if err != nil || !state.Exists {
		report.Fail(GroupTools, spec.id, spec.title,
			fmt.Sprintf("configured at %s, which does not exist", spec.path),
			"Correct the configured path, or clear it to fall back to PATH discovery.")
		return ""
	}
	if state.IsDir {
		report.Fail(GroupTools, spec.id, spec.title,
			fmt.Sprintf("%s is a directory, not an executable", spec.path),
			"Point the configured path at the executable itself.")
		return ""
	}
	if !options.Active {
		report.OK(GroupTools, spec.id, spec.title, spec.path+" (version not probed; use --active)")
		return ""
	}
	output, code, err := host.Run(ctx, spec.path, spec.args...)
	if err != nil || code != 0 {
		report.Fail(GroupTools, spec.id, spec.title,
			fmt.Sprintf("%s could not be run", spec.path),
			"Check that the file is executable by the service account and is a working build for this architecture.")
		return ""
	}
	version := firstLine(output)
	report.OK(GroupTools, spec.id, spec.title, spec.path+" — "+version)
	return version
}

func requireTool(host Host, report *Report, id, title, path, capability, install string) {
	if path == "" {
		report.Fail(GroupTools, id, title, "not found; "+capability+" is impossible without it", install)
		return
	}
	if state, err := host.Lstat(path); err != nil || !state.Exists {
		report.Fail(GroupTools, id, title, path+" does not exist", install)
		return
	}
	report.OK(GroupTools, id, title, path)
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return strings.TrimSpace(text)
}

func inspectHelper(ctx context.Context, host Host, options Options, in *Inspection) {
	report := &in.Report

	state, err := host.Lstat(HelperPath)
	switch {
	case err != nil:
		report.Fail(GroupHelper, "helper.installed", "Mount helper", HelperPath+" could not be inspected",
			"Run `sudo rotupd setup` to install it.")
	case !state.Exists:
		report.Fail(GroupHelper, "helper.installed", "Mount helper", HelperPath+" is not installed",
			"Run `sudo rotupd setup --helper <path to built rotup-mounthelper>`.")
	default:
		report.Add(evaluatePrivilegedFile(HelperPath, "helper.installed", "Mount helper", state, HelperMode))
	}

	wrapper, err := host.Lstat(WrapperPath)
	switch {
	case err != nil || !wrapper.Exists:
		report.Fail(GroupHelper, "helper.wrapper", "Sudo wrapper", WrapperPath+" is not installed",
			"Run `sudo rotupd setup`. The daemon executes the wrapper, never the helper directly.")
	default:
		check := evaluatePrivilegedFile(WrapperPath, "helper.wrapper", "Sudo wrapper", wrapper, HelperMode)
		if check.Status == StatusOK {
			if content, readErr := host.ReadFile(WrapperPath); readErr == nil && string(content) != WrapperContent {
				check = Check{ID: "helper.wrapper", Group: GroupHelper, Title: "Sudo wrapper",
					Status: StatusWarning,
					Detail: WrapperPath + " differs from the wrapper this version installs",
					Remedy: "Run `sudo rotupd setup` to reinstall it, or keep the local edit deliberately."}
			}
		}
		report.Add(check)
	}

	inspectSudoers(host, in)

	if options.Active {
		probeHelperInvocation(ctx, host, report)
	} else {
		report.OK(GroupHelper, "helper.invocation", "Helper invocation",
			"not probed; use --active to run the wrapper once with no arguments")
	}
}

// evaluatePrivilegedFile is the rule for anything the daemon must not be able
// to rewrite: it has to be a plain root-owned file that is not writable by
// group or other. This is a pure function so every failure shape is testable.
func evaluatePrivilegedFile(path, id, title string, state FileState, want fs.FileMode) Check {
	switch {
	case state.IsSymlink:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusError,
			Detail: path + " is a symbolic link",
			Remedy: "A privileged path must be a real file. Remove the link and run `sudo rotupd setup`."}
	case state.IsDir:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusError,
			Detail: path + " is a directory",
			Remedy: "Remove it and run `sudo rotupd setup`."}
	case state.UID < 0:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusWarning,
			Detail: path + " exists but its ownership cannot be read on this platform",
			Remedy: "Verify ownership on the Linux host that will run ROTUP."}
	case state.UID != 0:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusError,
			Detail: fmt.Sprintf("%s is owned by uid %d, not root", path, state.UID),
			Remedy: "A privileged binary the daemon account can rewrite is not a privilege boundary. Run `sudo rotupd setup`."}
	case state.Mode.Perm()&0o022 != 0:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusError,
			Detail: fmt.Sprintf("%s is mode %04o and writable beyond its owner", path, state.Mode.Perm()),
			Remedy: fmt.Sprintf("Run `sudo rotupd setup`, or `sudo chmod %04o %s`.", want.Perm(), path)}
	case state.Mode.Perm()&0o111 == 0:
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusError,
			Detail: fmt.Sprintf("%s is mode %04o and not executable", path, state.Mode.Perm()),
			Remedy: fmt.Sprintf("Run `sudo rotupd setup`, or `sudo chmod %04o %s`.", want.Perm(), path)}
	}
	detail := fmt.Sprintf("%s, root-owned, mode %04o", path, state.Mode.Perm())
	if state.Mode&fs.ModeSetuid != 0 {
		// setuid is the documented alternative installation and is not wrong,
		// but it should be a deliberate choice rather than a surprise.
		return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusOK,
			Detail: detail + " (setuid root; the sudo rule is then unnecessary)"}
	}
	return Check{ID: id, Group: GroupHelper, Title: title, Status: StatusOK, Detail: detail}
}

func inspectSudoers(host Host, in *Inspection) {
	report := &in.Report
	state, err := host.Lstat(SudoersPath)
	if err != nil || !state.Exists {
		report.Fail(GroupHelper, "helper.sudoers", "Sudo policy", SudoersPath+" is not installed",
			"Run `sudo rotupd setup`. Without it the daemon cannot perform any media operation.")
		return
	}
	if state.UID > 0 {
		report.Fail(GroupHelper, "helper.sudoers", "Sudo policy",
			fmt.Sprintf("%s is owned by uid %d, not root", SudoersPath, state.UID),
			"sudo ignores a policy file it does not trust. Run `sudo rotupd setup`.")
		return
	}
	// sudo refuses to read a sudoers.d file that anyone but its owner can
	// write, so a wrong mode here silently removes every media operation.
	if state.Mode.Perm()&0o177 != 0 {
		report.Fail(GroupHelper, "helper.sudoers", "Sudo policy",
			fmt.Sprintf("%s is mode %04o; sudo requires 0440", SudoersPath, state.Mode.Perm()),
			fmt.Sprintf("Run `sudo rotupd setup`, or `sudo chmod 0440 %s`.", SudoersPath))
		return
	}
	content, err := host.ReadFile(SudoersPath)
	if err != nil {
		// Being unable to read a 0440 root-owned file is the normal case for an
		// unprivileged doctor run, and says nothing bad about the policy.
		report.OK(GroupHelper, "helper.sudoers", "Sudo policy",
			fmt.Sprintf("%s installed, root-owned, mode %04o (contents readable only by root)",
				SudoersPath, state.Mode.Perm()))
		return
	}
	expected, err := SudoersContent(in.Layout.User)
	if err == nil && string(content) == expected {
		report.OK(GroupHelper, "helper.sudoers", "Sudo policy",
			fmt.Sprintf("%s authorizes %s to run %s as root", SudoersPath, in.Layout.User, HelperPath))
		return
	}
	if finding := auditSudoersRule(string(content)); finding != "" {
		report.Fail(GroupHelper, "helper.sudoers", "Sudo policy", finding,
			"Review the file by hand. `sudo rotupd setup` will replace it with a rule naming only "+HelperPath+".")
		return
	}
	report.Warn(GroupHelper, "helper.sudoers", "Sudo policy",
		fmt.Sprintf("%s does not match the rule this version generates for %s", SudoersPath, in.Layout.User),
		"Run `sudo rotupd setup` to regenerate it, or keep the local edit deliberately.")
}

// auditSudoersRule looks for grants that would be far wider than the one thing
// ROTUP needs. It is a safety net for a hand-edited file, not a sudoers parser:
// it reports what it recognises and stays quiet otherwise.
func auditSudoersRule(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		command := trimmed
		if index := strings.Index(trimmed, "NOPASSWD:"); index >= 0 {
			command = strings.TrimSpace(trimmed[index+len("NOPASSWD:"):])
		} else if index := strings.LastIndex(trimmed, "="); index >= 0 {
			command = strings.TrimSpace(trimmed[index+1:])
		}
		switch {
		case command == "ALL":
			return SudoersPath + " grants unrestricted root command execution (ALL)"
		case strings.Contains(command, "*"):
			return SudoersPath + " grants a wildcard command, which is wider than the single helper path"
		}
		for _, shell := range []string{"/bin/sh", "/bin/bash", "/usr/bin/sh", "/usr/bin/bash", "/usr/bin/env", "/bin/mount", "/usr/bin/mount"} {
			if strings.HasPrefix(command, shell) {
				return SudoersPath + " grants root execution of " + shell + ", which is far wider than the mount helper"
			}
		}
	}
	return ""
}

// probeHelperInvocation runs the wrapper once with no arguments. The helper
// answers with its usage refusal before it looks at privilege or touches any
// device, so this proves the whole path — wrapper, sudo policy, helper binary —
// without mounting anything.
func probeHelperInvocation(ctx context.Context, host Host, report *Report) {
	output, code, err := host.Run(ctx, WrapperPath)
	switch {
	case err != nil:
		report.Fail(GroupHelper, "helper.invocation", "Helper invocation",
			WrapperPath+" could not be executed",
			"Run `sudo rotupd setup`, then confirm the service account may run it.")
	case strings.Contains(output, "\"error_code\":\"usage\""):
		report.OK(GroupHelper, "helper.invocation", "Helper invocation",
			"the wrapper reached the helper through sudo and it refused an empty request, as expected")
	case strings.Contains(output, "password is required"), strings.Contains(output, "a password is required"):
		report.Fail(GroupHelper, "helper.invocation", "Helper invocation",
			"sudo asked for a password, so the NOPASSWD rule does not apply to this account",
			"Run `sudo rotupd setup --user <account>` for the account the daemon actually runs as.")
	default:
		report.Fail(GroupHelper, "helper.invocation", "Helper invocation",
			fmt.Sprintf("the wrapper exited %d without the helper's usage answer", code),
			"Inspect `"+WrapperPath+"` by hand. ROTUP treats an unrecognised answer as a failure rather than assuming the helper ran.")
	}
}

func inspectRuntime(host Host, in *Inspection) {
	report := &in.Report

	for _, path := range []string{RuntimeDir, MediaMountRoot} {
		state, err := host.Lstat(path)
		id := "runtime.dir"
		title := "Runtime directory"
		if path == MediaMountRoot {
			id, title = "runtime.mountroot", "Media mount root"
		}
		switch {
		case err != nil || !state.Exists:
			report.Fail(GroupRuntime, id, title, path+" does not exist",
				"Run `sudo rotupd setup`. It installs "+TmpfilesPath+" so the directory is recreated at every boot, and creates it now.")
		case !state.IsDir:
			report.Fail(GroupRuntime, id, title, path+" exists and is not a directory",
				"Remove it and run `sudo rotupd setup`.")
		case state.UID > 0:
			report.Fail(GroupRuntime, id, title,
				fmt.Sprintf("%s is owned by uid %d, not root", path, state.UID),
				"The ancestors of the privileged mount root must stay root-owned; otherwise the daemon account could replace them and redirect a mount.")
		case state.Mode.Perm()&0o007 != 0:
			report.Fail(GroupRuntime, id, title,
				fmt.Sprintf("%s is mode %04o and reachable by every account on this host", path, state.Mode.Perm()),
				fmt.Sprintf("Run `sudo rotupd setup`, or `sudo chmod %04o %s`.", RuntimeDirMode.Perm(), path))
		case in.Layout.GID > 0 && state.GID >= 0 && state.GID != in.Layout.GID:
			report.Warn(GroupRuntime, id, title,
				fmt.Sprintf("%s has group gid %d; the service account's group is %s (gid %d)",
					path, state.GID, in.Layout.Group, in.Layout.GID),
				"The daemon and restic must be able to traverse it. Run `sudo rotupd setup` to set the group.")
		default:
			report.OK(GroupRuntime, id, title,
				fmt.Sprintf("%s, root-owned, mode %04o", path, state.Mode.Perm()))
		}
	}

	state, err := host.Lstat(TmpfilesPath)
	if err != nil || !state.Exists {
		report.Warn(GroupRuntime, "runtime.tmpfiles", "Boot-time recreation",
			TmpfilesPath+" is not installed, so the runtime directory will be missing after the next reboot",
			"Run `sudo rotupd setup`.")
		return
	}
	report.OK(GroupRuntime, "runtime.tmpfiles", "Boot-time recreation",
		TmpfilesPath+" recreates the runtime tree at boot")
}

func inspectData(host Host, in *Inspection) {
	report := &in.Report
	layout := in.Layout

	checkDataDir(host, report, layout, "data.dir", "Data directory", DataDir, DataDirMode)
	checkDataDir(host, report, layout, "data.staging", "Staging root", StagingRoot, DataDirMode)

	state, err := host.Lstat(SecretRoot)
	switch {
	case err != nil || !state.Exists:
		report.Warn(GroupData, "data.secrets", "Secret store",
			SecretRoot+" does not exist yet; the daemon creates it on first start",
			"Run `sudo rotupd setup` to create it now with 0700 permissions.")
	case !state.IsDir:
		report.Fail(GroupData, "data.secrets", "Secret store", SecretRoot+" is not a directory",
			"Move the file aside. ROTUP will not store credentials anywhere else.")
	case state.Mode.Perm()&0o077 != 0:
		// This is the one permission finding that is unambiguously dangerous:
		// it exposes stored credential material to other accounts.
		report.Fail(GroupData, "data.secrets", "Secret store",
			fmt.Sprintf("%s is mode %04o and readable beyond its owner", SecretRoot, state.Mode.Perm()),
			fmt.Sprintf("Run `sudo chmod 0700 %s`. Stored source and repository passwords are protected by these bits alone.", SecretRoot))
	case layout.UID > 0 && state.UID >= 0 && state.UID != layout.UID:
		report.Fail(GroupData, "data.secrets", "Secret store",
			fmt.Sprintf("%s is owned by uid %d, but the daemon runs as %s (uid %d)", SecretRoot, state.UID, layout.User, layout.UID),
			"The daemon cannot read its own credentials. Run `sudo rotupd setup`.")
	default:
		report.OK(GroupData, "data.secrets", "Secret store",
			fmt.Sprintf("%s, mode %04o, contents never inspected", SecretRoot, state.Mode.Perm()))
	}

	database, err := host.Lstat(DatabasePath)
	switch {
	case err != nil || !database.Exists:
		report.OK(GroupData, "data.database", "Database",
			DatabasePath+" does not exist yet; the daemon creates and migrates it on first start")
	case database.IsDir:
		report.Fail(GroupData, "data.database", "Database", DatabasePath+" is a directory",
			"Point ROTUP_DB_PATH at a file, or move the directory aside.")
	case layout.UID > 0 && database.UID >= 0 && database.UID != layout.UID:
		report.Fail(GroupData, "data.database", "Database",
			fmt.Sprintf("%s is owned by uid %d, but the daemon runs as %s (uid %d)", DatabasePath, database.UID, layout.User, layout.UID),
			"The daemon cannot open its own database. Correct the ownership.")
	default:
		report.OK(GroupData, "data.database", "Database",
			fmt.Sprintf("%s, %d bytes", DatabasePath, database.Size))
	}
}

func checkDataDir(host Host, report *Report, layout Layout, id, title, path string, mode fs.FileMode) {
	state, err := host.Lstat(path)
	switch {
	case err != nil || !state.Exists:
		report.Warn(GroupData, id, title, path+" does not exist",
			"Run `sudo rotupd setup` to create it owned by the service account.")
	case !state.IsDir:
		report.Fail(GroupData, id, title, path+" is not a directory",
			"Move the file aside and run `sudo rotupd setup`.")
	case state.Mode.Perm()&0o007 != 0:
		report.Warn(GroupData, id, title,
			fmt.Sprintf("%s is mode %04o and reachable by every account on this host", path, state.Mode.Perm()),
			fmt.Sprintf("Run `sudo chmod %04o %s`.", mode.Perm(), path))
	case layout.UID > 0 && state.UID >= 0 && state.UID != layout.UID:
		report.Fail(GroupData, id, title,
			fmt.Sprintf("%s is owned by uid %d, but the daemon runs as %s (uid %d)", path, state.UID, layout.User, layout.UID),
			"The daemon cannot write there. Run `sudo rotupd setup`.")
	default:
		report.OK(GroupData, id, title, fmt.Sprintf("%s, mode %04o", path, state.Mode.Perm()))
	}
}

func inspectService(ctx context.Context, host Host, in *Inspection) {
	report := &in.Report

	state, err := host.Lstat(UnitPath)
	if err != nil || !state.Exists {
		report.Warn(GroupService, "service.unit", "systemd unit",
			UnitPath+" is not installed; the daemon has to be started by hand and will not come back after a reboot",
			"Run `sudo rotupd setup --enable-service` to install, enable and start it.")
		return
	}
	report.OK(GroupService, "service.unit", "systemd unit", UnitPath+" installed")

	systemctl, err := host.LookPath("systemctl")
	if err != nil {
		report.Warn(GroupService, "service.state", "Service state",
			"systemctl is not available, so the unit's state cannot be read",
			"On a host without systemd, start rotupd with your own supervisor and load "+EnvFilePath+" yourself.")
		return
	}
	output, _, err := host.Run(ctx, systemctl, "is-active", "rotupd.service")
	if err != nil {
		report.Warn(GroupService, "service.state", "Service state", "the unit's state could not be read", "")
		return
	}
	switch strings.TrimSpace(output) {
	case "active":
		report.OK(GroupService, "service.state", "Service state", "rotupd.service is active")
	case "activating":
		report.OK(GroupService, "service.state", "Service state", "rotupd.service is starting")
	default:
		report.Warn(GroupService, "service.state", "Service state",
			"rotupd.service is "+strings.TrimSpace(output),
			"Start it with `sudo systemctl start rotupd`, or run the daemon in the foreground for development.")
	}
}
