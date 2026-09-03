# M3 removable media — operator and development notes

This records the decisions the architecture deliberately left open for the M3
milestone, and what an operator needs in order to run it. It is not a full
recovery runbook; that belongs to the hardening milestone.

## Selected mechanisms

### Enumeration: `lsblk --json`

Block devices are enumerated with `lsblk --json --bytes --paths --output ...`
and parsed as structured JSON. No human-oriented column output is ever parsed.

`lsblk` ships with util-linux on every target distribution and gives the UUID,
filesystem type, size, label, removable flag and transport in one call. Walking sysfs would reimplement it,
and a libudev binding would pull cgo into the build for no extra information.
With a populated udev database this works without privilege. Without that
database, reading filesystem metadata requires block-device read access,
normally root. A root-only spike does not prove an unprivileged deployment's
udev/permission setup.

Mount state is deliberately **not** taken from `lsblk`. It comes from
`/proc/self/mountinfo`, which is the kernel's own answer and also reveals
multiple mount points for the same device.

### Mount privilege: a narrow privileged helper

`mount(2)` requires `CAP_SYS_ADMIN`, which is effectively root. The privilege is
confined to `rotup-mounthelper`, a separate binary with three subcommands.

The alternatives were rejected for concrete reasons:

- **udisks2** chooses its own mount point under `/run/media/<user>/<label>`,
  which ROTUP can neither derive nor verify, and it expects D-Bus and polkit
  policy that a headless unattended host should not need.
- **`CAP_SYS_ADMIN` on `rotupd` itself** would give the same privilege to the
  HTTP server, the SQLite layer, and every external tool the daemon spawns.

The helper contract is the security boundary:

| It receives | It derives or fixes itself |
|---|---|
| a ROTUP-generated disk identifier | the mount point, `/run/rotup/media/<disk-id>` |
| a filesystem UUID | the unique currently observed device, using the daemon's `lsblk` enumerator |
| a filesystem type from an allowlist | the mount flags, `nodev,nosuid,noexec` |

No path, no mount option, no device node and no shell ever crosses the
boundary. The helper clears its environment before doing anything, validates
every argument again on its own account, refuses a mount point that is not an
empty non-symlink directory, and never uses `MNT_FORCE` or `MNT_DETACH`.

Both sides use the same filesystem observation and UUID-normalization rules;
neither USB classification nor a `/dev/disk/by-uuid` symlink defines identity.
Those symlinks are asynchronous udev output, not kernel identity. Empty
(zero-size) block devices are excluded even if signature metadata is stale.
The helper independently refuses absent or duplicate UUIDs, holds the selected
block device open, and uses `blkid --probe` on that descriptor before and after
mounting. It checks the mounted kernel device number and re-resolves the UUID
before reporting success. The daemon still independently verifies mountinfo
and fresh enumeration. Failed verification never reports a successful mount.

The helper uses fixed `/usr/bin/lsblk` and `/usr/sbin/blkid` executables with an
empty environment; it accepts no executable or device path from its caller.

### Supported filesystems: ext4 only

Only **ext4** is supported in v1.

| Property | ext4 | exFAT | NTFS |
|---|---|---|---|
| Persistent filesystem UUID | yes | volume serial only | yes |
| POSIX ownership and permissions | yes | no (uid/gid/umask at mount) | emulated |
| Case-sensitive filenames | yes | no | no |
| Predictable `syncfs` and unmount | yes | driver dependent | driver dependent |
| Single in-kernel driver | yes | yes | ntfs3 or ntfs-3g (FUSE) |

exFAT and NTFS are observed and displayed, marked unsupported, and never
mounted. Adding a filesystem is an allowlist change in
`internal/domain/disk.go` and `internal/platform/media/contract.go`, plus a run
of the integration spike against it. `xfs` and `btrfs` are the obvious next
candidates.

## Identity model

The durable identity of a disk is its **filesystem UUID**. A device node is an
observation that is true until the next enumeration, and the `disks` table has
no column to store one — a test asserts that.

M3 registers no on-disk marker. Registration is metadata only: it never mounts,
writes, formats, repartitions, or changes a UUID. Marker verification is a
natural additional post-mount check and the verification code is written as a
chain of checks so it can be added in the milestone that first mounts a disk to
write to it.

If more than one connected filesystem claims the same UUID, ROTUP marks the
identity ambiguous, refuses every action, and records a durable event. It never
resolves the ambiguity by device order, insertion order, or first result.

## Installation

Build both binaries:

```sh
go build -o rotupd ./cmd/rotupd
go build -o rotup-mounthelper ./cmd/rotup-mounthelper
```

Install the helper with the privilege it needs and nothing more. Either:

```sh
# Recommended: a narrow sudo rule, helper not setuid.
install -o root -g root -m 0755 rotup-mounthelper /usr/libexec/rotup-mounthelper
echo 'rotup ALL=(root) NOPASSWD: /usr/libexec/rotup-mounthelper' \
  > /etc/sudoers.d/rotup-mounthelper
```

or:

```sh
# Alternative: setuid root, executable only by the service group.
install -o root -g rotup -m 4750 rotup-mounthelper /usr/libexec/rotup-mounthelper
```

Either way the helper's own validation is what makes it safe, not the
invocation mechanism.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ROTUP_LSBLK_PATH` | `/usr/bin/lsblk` | Explicit path to the block-device listing tool |
| `ROTUP_MOUNT_HELPER` | unset | Explicit path to the privileged helper. Unset means media management is unavailable, and the daemon says so |
| `ROTUP_MEDIA_POLL_INTERVAL` | `15s` | How often media is re-enumerated |

The mount root `/run/rotup/media` is a compile-time constant. It is not
configurable on purpose: making it a setting would mean a path crossing into
privileged code, which is what the helper contract exists to prevent.

## Required packages

- `util-linux` (`lsblk` and `blkid`; additionally `losetup` for the spike)
- `e2fsprogs` (`mkfs.ext4`, for the spike only)

## Reconciliation

ROTUP re-enumerates on a timer and at startup, and derives every disk's status
from that fresh reading. Periodic reconciliation is authoritative rather than an
optimisation: hotplug notification is not a durable queue, so a dropped hint
must never be able to lose a disk. Adding udev event hints later only shortens
the latency; it does not change what is true.

A reboot with a disk connected therefore converges with no manual database
repair: the disk is found by UUID and lands in `AVAILABLE_UNMOUNTED`, or in
`READY` if a ROTUP-owned mount survived, or in `ERROR` if something else has it
mounted.

## Running the integration spike

Requires root, on Linux, and touches nothing but a filesystem image it creates
in its own temporary directory:

```sh
# From the repository root. Build as the ordinary development user.
go build -o .cache/m3-spike/rotup-mounthelper ./cmd/rotup-mounthelper
go test -c -o .cache/m3-spike/linux.test ./internal/platform/linux
sudo env ROTUP_LINUX_SPIKE=1 \
  ROTUP_MOUNT_HELPER="$PWD/.cache/m3-spike/rotup-mounthelper" \
  "$PWD/.cache/m3-spike/linux.test" -test.run Spike -test.v -test.count=1 -test.timeout=3m
```

It covers enumeration and UUID discovery, identity surviving a device-node
change, mount with post-mount identity verification, write/sync/unmount and
re-mount with the data intact, refusal of an absent identity, refusal of every
unsafe helper argument shape, and real service state after deferred loop
detachment. No existing spike is disabled.

Fixture UUIDs are obtained by uncached probing. Fixture setup waits (with a
deadline) for the asynchronous device metadata to reflect that known UUID.
For the node-change test, a second attachment is allocated while the first
node is still occupied; the original is then detached before verification.
No mounted image is double-attached or written through both mappings.

`losetup -d` under a live mount requests `AUTOCLEAR`; the filesystem remains
attached until its last reference closes. This is normal Linux behavior, not
a WSL workaround. The spike checks kernel loop status, then exercises the
real service's sync/unmount and requires `removed_during_operation` and
`DISCONNECTED` after actual detachment. The absent UUID assertion remains
mandatory once the backing file really is detached.

A loop detach cannot model physical USB disappearance during an in-flight
write on either WSL or native Linux. Portable fault tests retain that coverage;
a physical-device test is still required to validate the hardware path. Do not
interpret a green loop spike as M3 completion or as hardware-unplug validation.

Record the distribution, kernel version, util-linux version and filesystems
exercised alongside the run.

Validation on 2026-09-04: Ubuntu 26.04 LTS under WSL2, kernel
`6.18.33.2-microsoft-standard-WSL2`, util-linux `2.41.3`, ext4 test images.
All six spikes passed, including five consecutive repetitions (30 test runs).
The node-change test observed `/dev/loop1` -> `/dev/loop2`. This does not remove
the physical-unplug or unprivileged-deployment limitations above.

## Security notes

- Only `rotup-mounthelper` is privileged. `rotupd` runs as an ordinary service
  identity.
- Mounts are always `nodev,nosuid,noexec`. Content on backup media is never
  executed.
- ROTUP only ever unmounts mounts below its own mount root. A disk an
  administrator mounted elsewhere is reported as `ERROR / external_mount` and
  left completely alone.
- Unknown media is observe-only. It is never mounted, initialised, or written
  to without an explicit registration by an administrator.
- A failed flush or a failed unmount never results in a state that claims the
  media is safe to remove.

## Inspecting a stuck mount by hand

```sh
# What does the kernel think is mounted below ROTUP's root?
findmnt --json /run/rotup/media

# Which UUID is that device really carrying?
lsblk --json --paths --output PATH,FSTYPE,UUID,MOUNTPOINT

# What is holding it open?
sudo fuser -vm /run/rotup/media/<disk-id>
sudo lsof +f -- /run/rotup/media/<disk-id>
```

Unmount by hand only after the holder is gone:

```sh
sudo /usr/libexec/rotup-mounthelper unmount --disk-id <disk-id>
```

The helper refuses a lazy or forced unmount by design. If a plain unmount cannot
succeed, the filesystem is genuinely still in use, and forcing it would risk the
data rather than protect it. Resolve the holder first.
