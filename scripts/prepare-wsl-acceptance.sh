#!/bin/sh
# Restore the existing WSL test disk/runtime. No formatting or service restart.
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo 'Run this script through sudo in Ubuntu WSL.' >&2
  exit 1
fi

image=/home/bejo/rotup-test-disks/disk-a.img
expected_uuid=063c94b9-bd1e-4496-b70c-85fadc635531
build=/mnt/c/dev/rotup/.cache/operator
gid=$(id -g bejo)

test -f "$image" && test ! -L "$image"
test "$(readlink -f "$image")" = "$image"
test "$(blkid -p -s UUID -o value "$image")" = "$expected_uuid"
test "$(blkid -p -s TYPE -o value "$image")" = ext4
test -x "$build/rotup-mounthelper"
test -f "$build/rotup-mounthelper-sudo"
test -f "$build/rotup.conf"

# Refuse redirected managed paths before changing permissions or files.
for path in /run /run/rotup /run/rotup/media /usr /usr/libexec /usr/lib/tmpfiles.d; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -d "$path" ]; }; then
    echo "Unsafe directory: $path" >&2
    exit 1
  fi
done
for path in /usr/libexec/rotup-mounthelper /usr/libexec/rotup-mounthelper-sudo /usr/lib/tmpfiles.d/rotup.conf; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -f "$path" ]; }; then
    echo "Unsafe target: $path" >&2
    exit 1
  fi
done

install -o root -g root -m 0755 "$build/rotup-mounthelper" /usr/libexec/rotup-mounthelper
install -o root -g root -m 0755 "$build/rotup-mounthelper-sudo" /usr/libexec/rotup-mounthelper-sudo
install -o root -g root -m 0644 "$build/rotup.conf" /usr/lib/tmpfiles.d/rotup.conf
install -d -o root -g "$gid" -m 0750 /run/rotup /run/rotup/media

# Reuse the established sudo policy; verify it as the daemon account.
probe=$(sudo -n -u bejo -- /usr/libexec/rotup-mounthelper-sudo 2>&1) || :
case "$probe" in
  *'"error_code":"usage"'*) ;;
  *) echo 'Existing helper permission failed for bejo; disk remains unattached.' >&2; exit 1 ;;
esac

# Reuse an existing association; never attach the same image twice.
if [ -z "$(losetup -j "$image")" ]; then
  losetup --find --show --nooverlap "$image"
else
  echo 'Test image is already attached.'
fi
echo 'WSL test runtime prepared. No filesystem was formatted or mounted.'
