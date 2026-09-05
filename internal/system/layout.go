package system

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/bejusxd/rotup/internal/platform/media"
)

// Installed file locations. These are fixed rather than configurable because
// the sudo policy names the helper by absolute path: a path an operator could
// redirect would move the privileged binary out from under the rule that
// authorizes it.
const (
	// HelperPath is the privileged binary. Only this exact path is authorized
	// by the generated sudo rule.
	HelperPath = "/usr/libexec/rotup-mounthelper"
	// WrapperPath is the unprivileged shim the daemon actually executes. It
	// calls sudo noninteractively with the fixed helper path.
	WrapperPath = "/usr/libexec/rotup-mounthelper-sudo"
	// SudoersPath holds the single generated rule. Nothing else in
	// /etc/sudoers.d is read, written or removed.
	SudoersPath = "/etc/sudoers.d/rotup-mounthelper"
	// TmpfilesPath recreates the volatile runtime directory at every boot.
	TmpfilesPath = "/usr/lib/tmpfiles.d/rotup.conf"
	// UnitPath is the systemd service unit.
	UnitPath = "/etc/systemd/system/rotupd.service"
	// DaemonPath is where setup expects the daemon executable to be installed.
	DaemonPath = "/usr/local/bin/rotupd"

	// ConfigDir and EnvFilePath hold non-secret daemon configuration. No
	// password of any kind is ever written here; secrets live in the secret
	// store, which this package only ever checks the permissions of.
	ConfigDir   = "/etc/rotup"
	EnvFilePath = "/etc/rotup/rotupd.env"

	// DataDir and its children are the persistent application state.
	DataDir      = "/var/lib/rotup"
	DatabasePath = "/var/lib/rotup/rotup.db"
	StagingRoot  = "/var/lib/rotup/staging"
	SecretRoot   = "/var/lib/rotup/secrets"

	// RuntimeDir is the parent of the media mount root. The mount root itself
	// is a compile-time constant in the media contract and is not repeated
	// here; it is derived from it so the two can never drift.
	RuntimeDir = "/run/rotup"
)

// Modes for the files and directories setup owns.
const (
	// HelperMode is not setuid. Privilege comes from the sudo rule, so the
	// binary itself stays an ordinary root-owned executable.
	HelperMode fs.FileMode = 0o755
	// SudoersMode is what sudo itself requires; it refuses to read a file in
	// sudoers.d that is group- or world-writable.
	SudoersMode fs.FileMode = 0o440
	// PlainFileMode is used for world-readable non-secret files.
	PlainFileMode fs.FileMode = 0o644
	// ConfigDirMode lets the service group read the configuration directory
	// without letting it write there.
	ConfigDirMode fs.FileMode = 0o750
	// EnvFileMode keeps the environment file readable by the service group
	// only. It holds no secrets, but it names every path ROTUP uses.
	EnvFileMode fs.FileMode = 0o640
	// DataDirMode is the service-owned persistent root.
	DataDirMode fs.FileMode = 0o750
	// SecretDirMode matches what the secret store enforces for itself.
	SecretDirMode fs.FileMode = 0o700
	// RuntimeDirMode keeps the runtime tree traversable by the service group
	// and by nobody else.
	RuntimeDirMode fs.FileMode = 0o750
)

// MediaMountRoot is the privileged mount root, taken from the contract the
// helper enforces so setup can never provision a different directory than the
// one the helper will actually use.
const MediaMountRoot = media.MountRoot

// Layout is one installation's resolved identity: which account runs the
// daemon, and which group must be able to traverse the runtime and data trees.
type Layout struct {
	StagingRoot  string
	SecretRoot   string
	DatabasePath string
	// User is the unprivileged account the daemon runs as.
	User string
	// Group is the account's primary group.
	Group string
	// UID and GID are that account resolved on this host.
	UID int
	GID int
	// DaemonSource is the executable setup installs to DaemonPath, and
	// HelperSource the one it installs to HelperPath. Empty means "already in
	// place; verify but do not install".
	DaemonSource string
	HelperSource string
	// EnableService requests that the unit be enabled and started. Setup never
	// does this implicitly.
	EnableService bool
}

// ValidateAccountName rejects anything that could change the meaning of a
// generated sudoers line or a systemd unit. This runs before any file is
// written, and before the name is ever interpolated into either.
//
// The accepted shape is deliberately narrower than what useradd permits:
// a leading letter or underscore, then letters, digits, underscore or hyphen,
// with an optional trailing '$'. Notably absent are whitespace, ',', '=', ':',
// '!', '%', '#', '\\' and every glob character, each of which is meaningful in
// sudoers syntax.
func ValidateAccountName(name string) error {
	if name == "" {
		return fmt.Errorf("account name must not be empty")
	}
	if len(name) > 32 {
		return fmt.Errorf("account name %q is longer than 32 characters", name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-'):
		case i == len(name)-1 && r == '$':
		default:
			return fmt.Errorf("account name %q contains an unacceptable character", name)
		}
	}
	return nil
}

// SudoersContent is the complete authorized policy: one account, one target
// user, one absolute command, no arguments pattern and no wildcard. sudo treats
// a rule with no argument list as "any arguments", which is intended here: the
// helper validates its own arguments on its own account and is the security
// boundary. What must never widen is the command itself.
func SudoersContent(user string) (string, error) {
	if err := ValidateAccountName(user); err != nil {
		return "", err
	}
	return "# Managed by `rotupd setup`. Edit through setup, not by hand.\n" +
		"# Authorizes exactly one root-owned binary and nothing else: no shell,\n" +
		"# no mount(8), no wildcard command and no other target user.\n" +
		user + " ALL=(root) NOPASSWD: " + HelperPath + "\n", nil
}

// TmpfilesContent recreates the runtime tree at every boot, root-owned and
// group-traversable.
//
// systemd's RuntimeDirectory= was rejected for this. It would create /run/rotup
// owned by the service account, which would let the unprivileged daemon replace
// the parent of the privileged mount root, and it deletes the directory when
// the unit stops. The mount-root ancestors have to stay root-owned for the
// helper contract to mean anything.
func TmpfilesContent(group string) (string, error) {
	if err := ValidateAccountName(group); err != nil {
		return "", err
	}
	mode := fmt.Sprintf("%04o", RuntimeDirMode.Perm())
	return "# Managed by `rotupd setup`. Edit through setup, not by hand.\n" +
		"# Root-owned so the unprivileged daemon cannot replace the parent of the\n" +
		"# privileged mount root; group-traversable so the daemon and restic can\n" +
		"# reach a mounted backup disk.\n" +
		"d " + RuntimeDir + " " + mode + " root " + group + " -\n" +
		"d " + MediaMountRoot + " " + mode + " root " + group + " -\n", nil
}

// EnvFileContent is the non-secret configuration the unit loads. Every value is
// a path or a duration. No password, token or credential reference is ever
// written here, and setup refuses to carry one in.
func EnvFileContent(l Layout, rclonePath, resticPath, lsblkPath string) string {
	staging, secret, database, _ := (Options{StagingRoot: l.StagingRoot, SecretRoot: l.SecretRoot, DatabasePath: l.DatabasePath}).resolved()
	var b strings.Builder
	b.WriteString("# Managed by `rotupd setup`. Non-secret configuration only.\n")
	b.WriteString("# Passwords are never stored here: source and repository secrets live in\n")
	b.WriteString("# the secret store at " + secret + ".\n")
	b.WriteString("ROTUP_HTTP_ADDR=127.0.0.1:8080\n")
	b.WriteString("ROTUP_DB_PATH=" + database + "\n")
	b.WriteString("ROTUP_STAGING_ROOT=" + staging + "\n")
	b.WriteString("ROTUP_SECRET_ROOT=" + secret + "\n")
	b.WriteString("ROTUP_MOUNT_HELPER=" + WrapperPath + "\n")
	if lsblkPath != "" {
		b.WriteString("ROTUP_LSBLK_PATH=" + lsblkPath + "\n")
	}
	if rclonePath != "" {
		b.WriteString("ROTUP_RCLONE_PATH=" + rclonePath + "\n")
	}
	if resticPath != "" {
		b.WriteString("ROTUP_RESTIC_PATH=" + resticPath + "\n")
	}
	return b.String()
}

// UnitContent is the systemd service.
//
// It is deliberately not hardened beyond running as an unprivileged account.
// Each rejected directive breaks something ROTUP needs:
//
//   - NoNewPrivileges=yes stops sudo working, which removes every media
//     operation. The whole design depends on that one escalation.
//   - PrivateMounts / ProtectSystem=strict / ReadOnlyPaths put the service in
//     its own mount namespace, where a mount the root helper makes in the host
//     namespace is not reliably visible. A backup would then read an empty
//     directory instead of the disk.
//   - ProtectHome breaks an installation whose staging or database lives under
//     an operator's home directory, which is the supported single-user model.
//
// Ordering after systemd-tmpfiles-setup.service is what guarantees /run/rotup
// exists with the right ownership before the daemon starts.
func UnitContent(l Layout) (string, error) {
	if err := ValidateAccountName(l.User); err != nil {
		return "", err
	}
	if err := ValidateAccountName(l.Group); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Managed by `rotupd setup`. Edit through setup, not by hand.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=ROTUP local backup daemon\n")
	// No Documentation= line: ROTUP ships no man page, and systemd-analyze
	// verify rightly reports a reference to one that does not exist. Read
	// docs/operations/installation.md, or run `rotupd help`.
	b.WriteString("# systemd-tmpfiles-setup creates /run/rotup root-owned before this starts.\n")
	b.WriteString("After=local-fs.target network-online.target systemd-tmpfiles-setup.service\n")
	b.WriteString("Wants=network-online.target\n")
	staging, secret, database, _ := (Options{StagingRoot: l.StagingRoot, SecretRoot: l.SecretRoot, DatabasePath: l.DatabasePath}).resolved()
	b.WriteString("RequiresMountsFor=" + DataDir + " " + staging + " " + secret + " " + database + "\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("User=" + l.User + "\n")
	b.WriteString("Group=" + l.Group + "\n")
	b.WriteString("ExecStart=" + DaemonPath + "\n")
	b.WriteString("EnvironmentFile=" + EnvFilePath + "\n")
	b.WriteString("WorkingDirectory=" + DataDir + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5s\n")
	b.WriteString("# A backup in flight unmounts its disk on shutdown; give it room.\n")
	b.WriteString("KillSignal=SIGTERM\n")
	b.WriteString("TimeoutStopSec=120s\n")
	b.WriteString("# Media operations need sudo, so privilege escalation stays permitted and\n")
	b.WriteString("# no mount-namespace isolation is applied. See rotupd setup docs.\n")
	b.WriteString("NoNewPrivileges=no\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String(), nil
}

// WrapperContent is the unprivileged shim. It is a fixed two-line script with
// no interpolation at all, so nothing an operator configures can change what
// gets executed.
const WrapperContent = "#!/bin/sh\n" +
	"# Managed by `rotupd setup`. The daemon stays unprivileged; this shim is the\n" +
	"# only path to the root-owned helper, and the sudo rule authorizes only that\n" +
	"# one absolute path. -n so a missing policy fails instead of prompting.\n" +
	"exec /usr/bin/sudo -n -- " + HelperPath + " \"$@\"\n"
