# Installation and diagnostics

This is the operator path from a built binary to a usable ROTUP. It covers what
`rotupd setup` provisions, exactly which files it writes, and how `rotupd
doctor` reports a host's state.

Two kinds of setup exist and they have different remedies. **System setup** is
provisioning this host — the privileged mount helper, its narrow sudo policy,
the volatile runtime directory, the data locations and the service unit — and
is performed once at a terminal. **Application setup** is configuring a source
and running the first backup, and is performed in the web interface at
`/setup`. ROTUP never performs system setup in response to a browser request:
repairing a sudo policy is an administrator's action at a terminal.

## Prerequisites

Linux is the production target. The daemon builds and runs elsewhere for
development, but media management, the helper and the service unit are
Linux-only and `rotupd setup` refuses to run on any other platform.

| Requirement | Why | Missing means |
|---|---|---|
| `util-linux` (`lsblk`, `blkid`) | enumerating and identifying block devices | media management is impossible; doctor reports an error |
| `rclone` | collecting a source into local staging | collection is unavailable; doctor reports a warning |
| `restic` | backing up staging to a disk | backup is unavailable; doctor reports a warning |
| `sudo` | the one privilege escalation the design depends on | no media operation can run |
| an existing unprivileged account | the daemon runs as it | setup refuses; it never creates accounts |

ROTUP does not install these for you. Setup detects them, reports the exact
path and version it found, and prints the command to install what is missing.
It is deliberately not a package-manager wrapper, and it never downloads or
executes anything from the network.

Create a dedicated account if you want one; setup will not:

```sh
sudo useradd --system --home-dir /var/lib/rotup --shell /usr/sbin/nologin rotup
```

Without `--user`, setup provisions for the account that invoked `sudo`, which
is the single-operator model the existing deployments use.

## Install

Build both binaries as an ordinary user, then provision once as root:

```sh
go build -o rotupd ./cmd/rotupd
go build -o rotup-mounthelper ./cmd/rotup-mounthelper

sudo ./rotupd setup \
  --user rotup \
  --helper "$PWD/rotup-mounthelper" \
  --daemon "$PWD/rotupd" \
  --enable-service
```

Then open `http://127.0.0.1:8080/setup` and work through the application steps.

Inspect before committing to anything:

```sh
./rotupd setup --dry-run          # reports every change, makes none, needs no root
```

## What setup changes

Setup writes exactly these paths and nothing else. It never edits an unrelated
sudoers entry, never removes a file it did not write, and never touches
application state.

| Path | Mode | Owner | Purpose |
|---|---|---|---|
| `/usr/libexec/rotup-mounthelper` | 0755 | root:root | the privileged helper; the only binary the sudo rule authorizes |
| `/usr/libexec/rotup-mounthelper-sudo` | 0755 | root:root | the unprivileged shim the daemon executes |
| `/etc/sudoers.d/rotup-mounthelper` | 0440 | root:root | one rule authorizing one absolute path |
| `/usr/lib/tmpfiles.d/rotup.conf` | 0644 | root:root | recreates the runtime directory at every boot |
| `/etc/systemd/system/rotupd.service` | 0644 | root:root | the service unit (with `--service` or `--enable-service`) |
| `/usr/local/bin/rotupd` | 0755 | root:root | the daemon (with `--daemon`) |
| `/etc/rotup/` | 0750 | root:*group* | configuration directory |
| `/etc/rotup/rotupd.env` | 0640 | root:*group* | non-secret configuration; **never** a password |
| `/var/lib/rotup/` | 0750 | *user*:*group* | persistent state |
| `/var/lib/rotup/rotup.db` | — | *user* | SQLite database, created by the daemon |
| `/var/lib/rotup/staging/` | 0750 | *user*:*group* | staging root |
| `/var/lib/rotup/secrets/` | 0700 | *user* | secret store |
| `/run/rotup`, `/run/rotup/media` | 0750 | root:*group* | volatile runtime tree |

### Idempotence

Rerunning setup on a converged host reports no changes at all. Concretely it
will not duplicate a sudoers line, loosen a permission, overwrite a secret,
destroy configuration, reinitialize a repository, mount a disk, alter backup
state or change a registered disk's identity. A directory that already exists
is only ever `chmod`/`chown`ed; it is never emptied or replaced.

Two files are treated as yours once you edit them:

- `/etc/rotup/rotupd.env` is left alone and reported when it differs from what
  setup generates. Use `--force` to replace it. It holds no secrets, so
  replacing it loses only local edits.
- `/usr/libexec/rotup-mounthelper-sudo` is reported when it differs, and
  reinstalled on the next run.

### Statuses

| Status | Meaning |
|---|---|
| `ok` | already correct; nothing was done |
| `fix` | setup changed something, and says exactly what |
| `warn` | ROTUP will run, but something is worth knowing |
| `FAIL` | this part of the integration is not usable as it is |

## Privilege model

The daemon runs unprivileged. Exactly one binary runs as root, and exactly one
sudo rule authorizes it:

```
rotup ALL=(root) NOPASSWD: /usr/libexec/rotup-mounthelper
```

There is no wildcard, no shell, no `mount(8)` and no other target user. The
rule intentionally names no argument pattern, because the helper validates its
own arguments on its own account and *is* the security boundary: it accepts no
path, no mount option and no device node, derives the mount point from a
validated ROTUP disk identifier, and resolves the device from the filesystem
UUID itself. What must never widen is the command.

Setup validates the generated rule with `visudo -cf` in a scratch file and only
then renames it into place, because an invalid file in `/etc/sudoers.d` can
break `sudo` for every account on the host. If validation fails, nothing is
installed and the existing policy is untouched.

Doctor refuses a helper the daemon account could rewrite: it must be a real
file, root-owned, not writable by group or other, and executable. A symlink, a
directory, or service-account ownership is an error, not a warning. Doctor also
recognises a hand-widened policy — `ALL`, a wildcard command, a shell, or
`mount(8)` — and setup reports what it replaced.

The account name is validated against sudoers syntax before it is interpolated
into any file, so a name containing whitespace, `,`, `=`, `!`, `#` or a glob
character is refused before anything is written.

setuid installation remains supported and is reported as such. The sudo rule is
then unnecessary.

## Runtime directory

`/run/rotup` is tmpfs and disappears on every reboot. It is provisioned through
`tmpfiles.d`, not through systemd's `RuntimeDirectory=`:

```
d /run/rotup       0750 root rotup -
d /run/rotup/media 0750 root rotup -
```

`RuntimeDirectory=` was rejected for two concrete reasons. It creates the
directory owned by the **service account**, which would let the unprivileged
daemon replace the parent of the privileged mount root and redirect a mount;
the ancestors of that root have to stay root-owned for the helper contract to
mean anything. And it deletes the directory when the unit stops.

The unit is ordered `After=systemd-tmpfiles-setup.service`, so the tree exists
with the right ownership before the daemon starts. Setup also creates it
immediately so the current boot does not have to wait for the next one.

## systemd unit

The unit runs the daemon as the configured account and is deliberately **not**
hardened beyond that. Each rejected directive breaks something ROTUP needs, and
each would look like an improvement:

- `NoNewPrivileges=yes` stops `sudo` working, which removes every media
  operation. The whole design depends on that one escalation, so the unit sets
  `NoNewPrivileges=no` explicitly.
- `PrivateMounts=`, `ProtectSystem=strict` and `ReadOnlyPaths=` put the service
  in its own mount namespace, where a mount the root helper makes in the host
  namespace is not reliably visible. A backup would then read an empty
  directory instead of the disk.
- `ProtectHome=` breaks an installation whose staging or database lives under an
  operator's home directory, which is the supported single-operator model.

`TimeoutStopSec=120s` gives a backup in flight room to unmount its disk.

Setup installs the unit with `--service`, and enables and starts it with
`--enable-service`. It never starts a service implicitly. Starting the daemon
does not start a backup: every collection and backup in ROTUP is an explicit
operator action.

```sh
sudo systemctl status rotupd
sudo journalctl -u rotupd -f
sudo systemctl restart rotupd
```

## Configuration

`/etc/rotup/rotupd.env` holds paths and durations only. **No password, token or
credential reference is ever written there.** Source and repository passwords
live in the secret store at `/var/lib/rotup/secrets`, whose `0700` mode is the
entire protection boundary; setup creates it with that mode and repairs it, and
never reads, lists or copies its contents.

Every variable in `docs/operations/m3-removable-media.md` and
`docs/operations/m4-backup.md` still applies and still overrides the generated
file. `ROTUP_MOUNT_HELPER` must point at the **wrapper**, never at the
unelevated helper binary.

## Diagnostics

```sh
rotupd doctor                 # read-only; safe anywhere, including production
rotupd doctor --active        # also probes tool versions and the helper invocation
rotupd doctor --json          # stable machine-readable output for support
rotupd doctor --strict        # exit non-zero on warnings too
```

Doctor changes nothing. It does not mount a disk, write application state,
initialize a repository, collect, back up, or touch a registered disk's
identity. `--active` adds two things and no more: a `--version` probe of each
configured tool, and one invocation of the sudo wrapper with **no arguments**.
The helper answers that with its usage refusal before it looks at privilege or
touches any device, so it proves the whole path — wrapper, sudo policy, helper
binary — without mounting anything.

Doctor checks the paths this host is actually configured with. If
`ROTUP_STAGING_ROOT`, `ROTUP_SECRET_ROOT` or `ROTUP_DB_PATH` are set, it says
so and checks those, rather than reassuring you about a default layout the
daemon does not use.

### Exit status

| Code | Meaning |
|---|---|
| 0 | no error was found (warnings alone still exit 0) |
| 1 | at least one error, or a warning with `--strict` |
| 2 | the command line itself was wrong |

Warnings are not failure by default on purpose: a host with `rclone` but no
`restic` can collect and cannot back up, and calling that unusable would hide
which half works.

The web interface shows the same read-only findings on `/setup`, drawn from the
same inspection. The daemon does not run active probes on a page load.

## Rerunning and rolling back

Setup is safe to rerun at any time; that is the intended way to repair drift
after an upgrade or a hand edit.

To remove ROTUP's system integration, delete only what setup installed:

```sh
sudo systemctl disable --now rotupd
sudo rm -f /etc/systemd/system/rotupd.service
sudo systemctl daemon-reload
sudo rm -f /etc/sudoers.d/rotup-mounthelper
sudo rm -f /usr/lib/tmpfiles.d/rotup.conf
sudo rm -f /usr/libexec/rotup-mounthelper /usr/libexec/rotup-mounthelper-sudo
sudo rm -f /usr/local/bin/rotupd
sudo rm -rf /run/rotup
```

`/var/lib/rotup` and `/etc/rotup` are **left deliberately**. They hold the
database, staging, and the secret store; removing them destroys the record of
every backup and the passwords needed to restore from a repository. Remove them
only when you have decided that data is gone for good.

## Validation performed

Provisioning was exercised end to end on Ubuntu 26.04 under WSL2 inside a
private user and mount namespace (`unshare -Urm` with tmpfs staged over `/etc`,
`/usr/libexec`, `/usr/lib/tmpfiles.d`, `/usr/local/bin`, `/var/lib` and
`/run`), so the ordinary code path ran against the real absolute paths without
touching the machine. The first run installed every file with the modes in the
table above; the second run reported **zero** changes.

Two checks cannot be exercised in that sandbox and are limitations of the
validation, not of the code: `sudo` cannot escalate inside a user namespace, so
the helper invocation probe fails there, and there is no systemd PID 1, so unit
loading is not exercised. Both are reported honestly by doctor rather than
assumed to have worked.

Installing for real requires an interactive `sudo` password on this
workstation, so `/etc/sudoers.d/rotup-mounthelper`, `/usr/lib/tmpfiles.d` and
the systemd unit have not been written to the live WSL host. The existing
hand-provisioned helper and sudo rule there are unchanged.
