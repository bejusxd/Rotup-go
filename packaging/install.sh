#!/bin/sh
# ROTUP installer for any systemd-based Linux host (amd64 or arm64).
#
# Run from an extracted release directory:
#   sudo ./install.sh                  # install for the dedicated "rotup" account
#   sudo ./install.sh --install-deps   # also install rclone/restic/util-linux/sudo
#   ./install.sh --dry-run             # report what would change; needs no root
#
# This script is a thin, readable wrapper. All host provisioning is done by
# `rotupd setup`, which is idempotent, validates the sudoers rule with visudo
# before installing it, and never touches application data. Rerunning this
# script is the supported way to upgrade or repair an installation.
set -eu

SERVICE_USER=rotup
INSTALL_DEPS=0
DRY_RUN=0
SERVICE=1
FORCE=0
MIN_RESTIC=0.17

usage() {
	cat <<'EOF'
Usage: install.sh [options]

  --user <name>     account the daemon runs as (default: rotup; created as a
                    system account if it does not exist)
  --install-deps    install rclone, restic, util-linux and sudo with the
                    system package manager (apt, dnf, yum, zypper or pacman)
  --no-service      do not install or start the systemd unit
  --force           replace an /etc/rotup/rotupd.env you have edited
  --dry-run         report every change and make none (no root needed)
  -h, --help        show this help
EOF
}

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--user) [ $# -ge 2 ] || die "--user needs a name"; SERVICE_USER=$2; shift 2 ;;
	--user=*) SERVICE_USER=${1#--user=}; shift ;;
	--install-deps) INSTALL_DEPS=1; shift ;;
	--no-service) SERVICE=0; shift ;;
	--force) FORCE=1; shift ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
done

HERE=$(cd "$(dirname "$0")" && pwd)
DAEMON=$HERE/rotupd
HELPER=$HERE/rotup-mounthelper

[ "$(uname -s)" = Linux ] || die "ROTUP installs on Linux only"
[ -x "$DAEMON" ] && [ -x "$HELPER" ] || die "run this from an extracted ROTUP release (rotupd and rotup-mounthelper must be next to it)"
"$DAEMON" version >/dev/null 2>&1 || die "this rotupd does not run here; download the release for $(uname -m)"

case "$SERVICE_USER" in
'' | *[!a-z0-9_-]* | -*) die "account name must use lowercase letters, digits, '_' or '-'" ;;
esac

if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "run with sudo (or use --dry-run to preview)"
fi

if [ "$SERVICE" -eq 1 ] && [ ! -d /run/systemd/system ]; then
	die "systemd is not running on this host; rerun with --no-service and start '$DAEMON' yourself"
fi

# --- dependencies -----------------------------------------------------------

install_packages() {
	if command -v apt-get >/dev/null 2>&1; then
		DEBIAN_FRONTEND=noninteractive apt-get update -q
		DEBIAN_FRONTEND=noninteractive apt-get install -y -q rclone restic util-linux sudo
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y rclone restic util-linux sudo
	elif command -v yum >/dev/null 2>&1; then
		yum install -y rclone restic util-linux sudo
	elif command -v zypper >/dev/null 2>&1; then
		zypper --non-interactive install rclone restic util-linux sudo
	elif command -v pacman >/dev/null 2>&1; then
		pacman -S --needed --noconfirm rclone restic util-linux sudo
	else
		die "no supported package manager found; install rclone, restic (>= $MIN_RESTIC), util-linux and sudo yourself"
	fi
}

if [ "$INSTALL_DEPS" -eq 1 ]; then
	if [ "$DRY_RUN" -eq 1 ]; then
		say "would install rclone, restic, util-linux and sudo with the system package manager"
	else
		say "installing dependencies with the system package manager"
		install_packages
	fi
fi

missing=""
for tool in rclone restic lsblk blkid sudo visudo; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	printf 'missing:%s\n' "$missing" >&2
	die "install them (or rerun with --install-deps) and try again"
fi

# restic's exit codes are only trustworthy from 0.17 on; ROTUP relies on them to
# tell a locked or damaged repository from a clean failure.
restic_version=$(restic version 2>/dev/null | awk '{print $2; exit}')
restic_ok=$(printf '%s\n%s\n' "$MIN_RESTIC" "$restic_version" | sort -V | head -n1)
if [ -z "$restic_version" ] || [ "$restic_ok" != "$MIN_RESTIC" ]; then
	die "restic ${restic_version:-unknown} is too old; ROTUP needs >= $MIN_RESTIC (download it from https://github.com/restic/restic/releases and put it on PATH)"
fi
say "found rclone $(rclone version 2>/dev/null | awk 'NR==1{print $2}') and restic $restic_version"

# --- service account --------------------------------------------------------

if id "$SERVICE_USER" >/dev/null 2>&1; then
	say "using existing account '$SERVICE_USER'"
elif [ "$DRY_RUN" -eq 1 ]; then
	say "would create system account '$SERVICE_USER' (home /var/lib/rotup, no login shell)"
else
	nologin=$(command -v nologin 2>/dev/null || echo /usr/sbin/nologin)
	say "creating system account '$SERVICE_USER'"
	useradd --system --home-dir /var/lib/rotup --no-create-home --shell "$nologin" "$SERVICE_USER"
fi

# --- provisioning -----------------------------------------------------------

set -- --user "$SERVICE_USER" --helper "$HELPER" --daemon "$DAEMON"
[ "$SERVICE" -eq 1 ] && set -- "$@" --enable-service
[ "$FORCE" -eq 1 ] && set -- "$@" --force
if [ "$DRY_RUN" -eq 1 ]; then
	set -- "$@" --dry-run
	if ! id "$SERVICE_USER" >/dev/null 2>&1; then
		say "account '$SERVICE_USER' does not exist yet, so the preview below checks the current account instead"
		set -- --helper "$HELPER" --daemon "$DAEMON" --dry-run
	fi
fi

say "running rotupd setup $*"
"$DAEMON" setup "$@"

if [ "$DRY_RUN" -eq 1 ]; then
	say "dry run complete; nothing was changed"
	exit 0
fi

# Restart so an upgrade actually runs the newly installed binary.
if [ "$SERVICE" -eq 1 ]; then
	systemctl restart rotupd
	sleep 2
	systemctl is-active --quiet rotupd || die "rotupd did not start; see: journalctl -u rotupd -n 50"
fi

say "verifying the installation"
/usr/local/bin/rotupd doctor --active --user "$SERVICE_USER" || true

cat <<EOF

ROTUP $(/usr/local/bin/rotupd version | awk '{print $2}') is installed.

  Web interface:  http://127.0.0.1:8080/setup  (local only, by design)
  From another computer, tunnel it over SSH:
      ssh -L 8080:127.0.0.1:8080 <user>@$(hostname)
  then open http://127.0.0.1:8080/setup there.

  Status / logs:  systemctl status rotupd ; journalctl -u rotupd -f
  Configuration:  /etc/rotup/rotupd.env   Data: /var/lib/rotup
  Upgrade:        extract a newer release and run its install.sh again.
EOF
