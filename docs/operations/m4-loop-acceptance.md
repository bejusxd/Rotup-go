# M4 loop-image backup and restore evidence

**This is not M4 USB acceptance.** Everything recorded here was produced against
a loop-mounted disk image inside WSL2, not a physically removable disk. It
proves that the first manual backup and an independent restore work end to end
on the real daemon; it proves nothing about physical removal, power loss, or a
real USB controller. The acceptance drill required by
[m4-backup.md](m4-backup.md) remains open.

## Fixture

| Item | Value |
|---|---|
| Commit at run time | `dd4d8f1` |
| Daemon | unprivileged `rotupd-ui` on `http://127.0.0.1:8080`, `ROTUP_DEV_MODE=false` |
| restic | 0.17.3 (`.cache/m4-spike/restic_0.17.3_linux_amd64`) |
| rclone | 1.75.0 |
| Backing image | `/home/bejo/rotup-test-disks/disk-a.img`, attached as `/dev/loop0` |
| Filesystem UUID | `063c94b9-bd1e-4496-b70c-85fadc635531` (ext4, 1 GiB) |
| Registered disk ID | `a8f5261e-e25e-4c69-8ebe-165fc2424ad8` ("test disk a") |
| Repository ID | `331f4905-4a1a-4177-87b5-20d202dbde0f` at `rotup/restic` |
| Repository restic ID | `1d80cb0f1dd8d6cb68ef32897c7fad6f870b7218996ab1efc3e7b7bad4909b61` |
| Source ID | `80eb2412-e5c2-4d10-97e0-0fbf67999836` ("WSL Test SMB") |

The loop node is a transport detail, never an identity. `lsblk -o NAME,FSTYPE,UUID`
on `/dev/loop0` reported the UUID above, and the daemon's own disk view reported
the same UUID for the registered disk, so both sides agree independently.

## What was captured before the backup

The source was `STABLE` at staging state version 2, published
`2026-09-04T16:59:36Z` by collection run `8b8d1ed2-7117-4702-864f-ced8a5608fe5`.
Its staging metadata records 3 files / 23 bytes and there are no revision
batches, so the read-set is the current tree plus the source's metadata
directory.

```
8c7905a37b2240af84621db8f58ce843a64a4669f67429e684208de402ce6232  metadata/80eb2412-e5c2-4d10-97e0-0fbf67999836/current.json
7e328a22d686c5868275c2a5803276cc01699b506cc758faa63ea523f1e9056c  sources/wsl-test/current/a.txt
4e0d29f86a6845176283023ee2eff5b740aaaf69d71adbe5b4aee99eb43e5876  sources/wsl-test/current/b.txt
5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  sources/wsl-test/current/docs/readme.txt
```

Paths are relative to `/mnt/c/dev/rotup/rotup-staging`.

## The backup run

Started through the supported API with the real double-submit CSRF token
(`GET /api/v1/system` for the cookie and token, then
`POST /api/v1/repositories/{id}/backup` with `X-CSRF-Token`). No safeguard was
bypassed and no repository was initialized, adopted, reset or re-passworded.

| Field | Value |
|---|---|
| Run ID | `215e1052-de95-4cfd-825a-2d03e3e3d425` |
| Kind / trigger | `BACKUP_LATEST` / `MANUAL` |
| Status | `SUCCEEDED`, stage `DURABLE_SUCCESS`, durability `NORMAL` |
| Snapshot ID | `d9f75cf767479ed3c66dde53f24b407855ed4e755c2cf8b90ff9ac3df8eb0a8f` |
| Candidate == verified snapshot | yes |
| Files / bytes processed | 4 files, 259 bytes (5834 bytes added) |
| Engine errors | 0 |
| Requested / finished | `2026-09-05T15:52:22.170Z` / `2026-09-05T15:52:26.234Z` (4027 ms) |
| Synced / unmount verified / durable | all `2026-09-05T15:52:26.234Z` |
| Read-set | source `80eb2412…`, collection run `8b8d1ed2…`, state version 2, `sources/wsl-test/current` |
| Revision batches | none (the source has none) |

Four files rather than three: the read-set adds the source's staging metadata
file, exactly as `captureReadSet` documents.

Disk status was `AVAILABLE_UNMOUNTED` before the run and `AVAILABLE_UNMOUNTED`
after it, and the host mount table showed no ROTUP mount once the run finished.

## Independent restore

The disk was mounted and unmounted only through the daemon's supported media
actions (`POST /api/v1/disks/{id}/mount` and `/unmount`). restic itself was
driven directly, so the verification does not depend on the orchestration under
test. The stored repository password was supplied only as
`--password-file` pointing at the existing protected secret file resolved from
the repository's `credential_refs` locator; its value was never read, printed,
copied or passed as an argument. `--no-lock` was used so a read-only
verification could not write a lock into the repository.

`restic cat config` reported repository ID
`1d80cb0f1dd8d6cb68ef32897c7fad6f870b7218996ab1efc3e7b7bad4909b61`, matching the
identity ROTUP recorded.

`restic snapshots --json` returned exactly one snapshot,
`d9f75cf767479ed3c66dde53f24b407855ed4e755c2cf8b90ff9ac3df8eb0a8f`, tagged
`rotup`, `rotup-run:215e1052-de95-4cfd-825a-2d03e3e3d425`,
`rotup-repo:331f4905-4a1a-4177-87b5-20d202dbde0f`,
`rotup-disk:a8f5261e-e25e-4c69-8ebe-165fc2424ad8`,
`rotup-semantics:BACKUP_LATEST`, `rotup-schema:1`, with both requested roots:

```
/mnt/c/dev/rotup/rotup-staging/metadata/80eb2412-e5c2-4d10-97e0-0fbf67999836
/mnt/c/dev/rotup/rotup-staging/sources/wsl-test/current
```

restic's own snapshot summary agrees with ROTUP's record: 4 new files, 259 bytes
processed, 5834 bytes added.

Restoring into the fresh scratch directory
`/home/bejo/rotup-restore-verify/215e1052-de95-4cfd-825a-2d03e3e3d425` restored
15 files/dirs (259 B) with exit code 0. The restored files hash identically to
the pre-backup read-set — the checksum lists compare with no differences — and
`diff -r` against the live staging tree reports the source tree and the metadata
directory identical.

Staging was unchanged by the restore: rehashing the same paths afterwards
reproduces the pre-backup list exactly. The restore target is a separate
directory outside staging, the repository and the source share.

## Final state

- Onboarding `FIRST_BACKUP_COMPLETED` is `done: true`, `complete: true`,
  evidence "Last completed backup 2026-09-05T15:52:26Z."
- Repository `331f4905…` is `HEALTHY`, `lastSnapshotId`
  `d9f75cf767479ed3c66dde53f24b407855ed4e755c2cf8b90ff9ac3df8eb0a8f`,
  `lastSuccessfulBackupAt` `2026-09-05T15:52:26.234Z`.
- `restic list locks` is empty: neither the backup nor the restore left a lock
  that would block a later recovery.
- The disk was synced and unmounted through the daemon after every step. Final
  status `AVAILABLE_UNMOUNTED`, `busy: false`, and no ROTUP entry in the host
  mount table. The image remains attached to `/dev/loop0`.

No code defect was encountered, so no code change was made for this run.

## Limitations

- Loop image, not removable hardware: no physical disconnect, no surprise
  removal, no power loss, no USB controller or hotplug path was exercised.
- Only one collection and one backup. The changed/deleted-file round trip, the
  second collection and the archived-revision restore required by
  [m4-backup.md](m4-backup.md) were not performed here.
- Interruption and reconnection behaviour was not exercised in this run.
- The read-set is tiny (4 files, 259 bytes) and contains no large files,
  hard links, unusual permissions or non-ASCII names.
- The daemon ran from a working-tree build with data directories on DrvFS
  (`/mnt/c`), whose permission modes the system setup check still reports as too
  broad. That is a fixture property, not the approved installation layout.
- The restore ran on the same host, as the same account, with local access to
  the secret store. It is an independence check against ROTUP's orchestration,
  not a disaster-recovery drill on a foreign machine.

## Reproduction

The scripts used are kept under the gitignored
`.cache/operator/m4-loop/` tree: `01-capture-readset.sh` (pre-backup
checksums), `02-backup.sh` (CSRF handshake, backup, polling, disk state),
`03-locator.sh` with `locator.go` (read-only lookup of the credential locator —
a file name, never the secret), `04-restore-verify.sh` (mount, independent
restore, comparison, unmount on every exit path), `05-final-state.sh`
(onboarding, lock check, final disk state) and `06-identity.sh` (UUID
re-verification).
