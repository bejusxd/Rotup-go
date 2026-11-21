#!/bin/sh
# Remove ROTUP's system integration. Data is kept unless --purge-data is given:
# /var/lib/rotup holds the database, staging and the secret store, including the
# repository passwords needed to restore from a backup disk.
set -eu

PURGE=0
[ "${1:-}" = "--purge-data" ] && PURGE=1
[ "$(id -u)" -eq 0 ] || { echo "error: run with sudo" >&2; exit 1; }

if [ -d /run/systemd/system ]; then
	systemctl disable --now rotupd 2>/dev/null || true
	rm -f /etc/systemd/system/rotupd.service
	systemctl daemon-reload
fi
rm -f /etc/sudoers.d/rotup-mounthelper
rm -f /usr/lib/tmpfiles.d/rotup.conf
rm -f /usr/libexec/rotup-mounthelper /usr/libexec/rotup-mounthelper-sudo
rm -f /usr/local/bin/rotupd
rm -rf /run/rotup

if [ "$PURGE" -eq 1 ]; then
	printf 'This deletes /var/lib/rotup and /etc/rotup, including repository passwords.\n'
	printf 'Backups on disks become unrecoverable without an escrowed password.\nType DELETE to continue: '
	read -r answer
	[ "$answer" = DELETE ] || { echo "aborted; data kept"; exit 1; }
	rm -rf /var/lib/rotup /etc/rotup
	echo "ROTUP and its data were removed."
else
	echo "ROTUP was removed. Data kept in /var/lib/rotup and /etc/rotup."
fi
