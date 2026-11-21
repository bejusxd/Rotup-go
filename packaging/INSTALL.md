# ROTUP — install on any Linux machine

ROTUP copies SMB shares into local staging and backs them up with restic onto
rotated USB disks. It runs on any systemd-based Linux host, amd64 or arm64
(a mini-PC, an old laptop, a Raspberry Pi 4/5, a VM with USB passthrough).

## 1. Install

Pick the archive for `uname -m` (`x86_64` → `amd64`, `aarch64` → `arm64`):

```sh
sha256sum -c SHA256SUMS --ignore-missing
tar xzf rotup-<version>-linux-<arch>.tar.gz
cd rotup-<version>-linux-<arch>
./install.sh --dry-run                # preview; changes nothing
sudo ./install.sh --install-deps      # install
```

`--install-deps` uses the system package manager (apt, dnf, yum, zypper,
pacman) for rclone, restic, util-linux and sudo. ROTUP needs **restic ≥ 0.17**;
the installer refuses an older one and tells you where to get a current binary.

The daemon runs as a dedicated `rotup` system account (created if missing;
`--user <name>` to choose another). The only privileged piece is a small mount
helper authorized by one narrow sudo rule. Rerun `install.sh` from a newer
release to upgrade; it is idempotent and never touches your data.

## 2. Open the interface

The web interface listens on `127.0.0.1:8080` only — there is no login page, so
it is never exposed to the network. From another computer:

```sh
ssh -L 8080:127.0.0.1:8080 you@backup-host
```

then open <http://127.0.0.1:8080/setup>.

## 3. Prepare a backup disk

ROTUP v1 uses **ext4** disks, identified by filesystem UUID. Formatting erases
the disk, so check the device with `lsblk -o NAME,SIZE,MODEL,TRAN` first:

```sh
sudo mkfs.ext4 -L ROTUP-A /dev/sdX     # sdX = the USB disk, NOT a partition you use
```

Leave it unmounted. In the interface, **Disks** shows it; register it.

## 4. First backup, and prove you can restore

1. **Sources** → add the SMB share (host, share, user) and set its password.
   Run *Test connection*, then *Collect*.
2. **Repositories** → create one on the registered disk. You will be shown the
   repository password: **store it outside this machine** (password manager,
   paper in a safe). Without it, a disk cannot be restored if this host dies.
3. Run a **Backup**, then prove the escrowed password works. Unplug the disk
   (ROTUP leaves it unmounted after a backup) and, on any machine with restic:

   ```sh
   sudo mount -o ro /dev/sdX /mnt          # the backup disk, read-only
   restic -r /mnt/<repository path shown on the Repositories page> snapshots
   restic -r /mnt/<repository path> restore latest --target /tmp/restore-test
   sudo umount /mnt
   ```

   restic asks for the password: type the escrowed one, not one read from this
   host.

   Do not rely on ROTUP until this restore has succeeded once.

## 5. Schedule it

Jobs are configured through the API for now (a UI is coming). Example: collect
every night at 03:00 and back up afterwards to the most recently connected disk.

```sh
B=http://127.0.0.1:8080/api/v1
TOKEN=$(curl -s -c jar $B/system | sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p')
curl -s -b jar -H "X-CSRF-Token: $TOKEN" -H 'Content-Type: application/json' \
  -H 'Origin: http://127.0.0.1:8080' -X POST $B/jobs -d '{
    "name": "nightly", "kind": "COLLECTION", "enabled": true,
    "sourceIds": ["<source-id from GET /api/v1/sources>"],
    "backupAfterCollection": true, "partialTriggersBackup": true,
    "scheduleEnabled": true, "daysOfWeekMask": 127,
    "localTime": "03:00", "timezone": "Europe/Warsaw"}'
curl -s $B/jobs
```

`daysOfWeekMask`: Sunday = 1, Monday = 2, Tuesday = 4 … all days = 127. Use your
own IANA timezone. If no backup disk is connected at 03:00 the backup waits and
runs when one is plugged in.

## Operating

```sh
systemctl status rotupd
journalctl -u rotupd -f
rotupd doctor --active          # read-only health check of the host integration
sudo ./uninstall.sh             # remove; keeps /var/lib/rotup unless --purge-data
```

Configuration lives in `/etc/rotup/rotupd.env` (paths and timeouts only — never
passwords). Data, staging and secrets live in `/var/lib/rotup`.
