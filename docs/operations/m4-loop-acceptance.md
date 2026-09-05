# M4 loop-image backup and restore evidence

**This is not M4 USB acceptance.** Everything recorded here was produced against
a loop-mounted disk image inside WSL2, not a physically removable disk. It
proves that manual backup, the `archive_then_sync` changed/deleted round trip
and independent restore work end to end on the real daemon; it proves nothing
about physical removal, power loss, or a real USB controller. The acceptance
drill required by [m4-backup.md](m4-backup.md) remains open.

Two exercises are recorded, in order:

1. [The first manual backup](#exercise-1-the-first-manual-backup) and its
   independent restore.
2. [The changed and deleted file round trip](#exercise-2-changed-and-deleted-file-round-trip),
   with a baseline backup, a mutation, a second collection that archived the
   displaced objects, a second backup, and an independent restore of both
   snapshots.

## Fixture

| Item | Value |
|---|---|
| Commit at run time | `dd4d8f1` (exercise 1), `0bc13c1` (exercise 2) |
| Daemon | unprivileged `rotupd-ui` on `http://127.0.0.1:8080`, `ROTUP_DEV_MODE=false` |
| restic | 0.17.3 (`.cache/m4-spike/restic_0.17.3_linux_amd64`) |
| rclone | 1.75.0 |
| Backing image | `/home/bejo/rotup-test-disks/disk-a.img`, attached as `/dev/loop0` |
| Filesystem UUID | `063c94b9-bd1e-4496-b70c-85fadc635531` (ext4, 1 GiB) |
| Registered disk ID | `a8f5261e-e25e-4c69-8ebe-165fc2424ad8` ("test disk a") |
| Repository ID | `331f4905-4a1a-4177-87b5-20d202dbde0f` at `rotup/restic` |
| Repository restic ID | `1d80cb0f1dd8d6cb68ef32897c7fad6f870b7218996ab1efc3e7b7bad4909b61` |
| Source ID | `80eb2412-e5c2-4d10-97e0-0fbf67999836` ("WSL Test SMB") |
| Source policy | `archive_then_sync` |
| SMB share | `rotup-test` on `localhost`, backed by `/home/bejo/rotup-smb-source` |

The loop node is a transport detail, never an identity. `lsblk -o NAME,FSTYPE,UUID`
on `/dev/loop0` reported the UUID above, and the daemon's own disk view reported
the same UUID for the registered disk, so both sides agree independently.

The share's backing directory was identified from the host's own Samba
configuration (`testparm -s`, which carries no password material), not guessed:
`[rotup-test]` declares `path = /home/bejo/rotup-smb-source`. Its three
pre-existing files hash identically to the staged copies, which confirms the
same directory is what ROTUP has been collecting.

## Exercise 1: the first manual backup

### What was captured before the backup

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

### The backup run

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

### Independent restore

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

No code defect was encountered in this exercise.

## Exercise 2: changed and deleted file round trip

### The test fixture in the share

A uniquely named, test-only directory was created inside the share's backing
directory. Nothing pre-existing was read for content, modified or removed, and
the three pre-existing files were hashed before and after the whole exercise to
prove it.

Fixture directory: `/home/bejo/rotup-smb-source/m4-roundtrip-20260905T161558Z`

| File | Role | SHA-256 at creation |
|---|---|---|
| `keep.txt` | never changed | `eaae7be40241afa5fee7c991dcb1e6d1d56a77a1e719227ea61e48b209ebc0a0` |
| `change.txt` | replaced later | `0fb3a18dcfb9a326c0df71aba5e1785ca580190d94e51b73eb0bce6572f10677` |
| `remove.txt` | removed later | `cd51ec7c6ea1f6cf76d1150713e5c52a6a00a8c109837a04fd03b34d981b088a` |

After the mutation, `change.txt` hashes
`92978719b5a29f876085e05ae8084c317441976a84b095aa44e518cda9f84752` and
`remove.txt` is gone. `keep.txt` is untouched throughout.

### Baseline collection and backup

| Field | Value |
|---|---|
| Collection run | `d754887a-0a86-46bb-89da-5c75fef59944` |
| Staging state version | 3 (published `2026-09-05T16:16:54.505Z`) |
| Collection result | 6 files scanned, 3 copied, 147 bytes, 0 displaced, no revision batch |
| Backup run | `79940590-0008-4239-9d7d-02b5bc53c95d` |
| Snapshot ID | `3caa1587a427609dce8d49e0c17713237983ee50b96c7f8e4e5209653a536edd` |
| Status | `SUCCEEDED`, `DURABLE_SUCCESS`, `NORMAL`, 0 engine errors |
| Files / bytes | 7 files, 407 bytes (7035 added) |
| Read-set | state version 3, `sources/wsl-test/current` |

### Second collection: the archive

`change.txt` was rewritten and `remove.txt` deleted, both inside the fixture
directory only. The next collection displaced exactly those two objects.

| Field | Value |
|---|---|
| Collection run | `0aacb8c0-cc12-4de0-a52f-22efcb65f94f` |
| Staging state version | 4 (published `2026-09-05T16:17:43.194Z`) |
| Collection result | 5 files scanned, 1 copied, 49 bytes |
| Revision batch ID | `abd67870-0955-4792-be0e-32dac78b2e1b` |
| Batch path | `revisions/80eb2412-e5c2-4d10-97e0-0fbf67999836/0aacb8c0-cc12-4de0-a52f-22efcb65f94f` |
| Batch contents | 2 items, 102 bytes, `reconciliationCompleted: true` |

The batch holds `change.txt` at `0fb3a18d…` (the superseded version) and
`remove.txt` at `cd51ec7c…` — byte for byte the fixture as it was before the
mutation. The current tree holds `change.txt` at `92978719…` and `keep.txt`
unchanged, and no longer holds `remove.txt`.

### Second backup

| Field | Value |
|---|---|
| Backup run | `66a5aac3-89c3-48c3-9e4d-8a683b84de5e` |
| Snapshot ID | `f41cbc262e078ecbb083d00600fe20181a8afcee701d21d0167cb62315688efb` |
| Status | `SUCCEEDED`, `DURABLE_SUCCESS`, `NORMAL`, 0 engine errors |
| Files / bytes | 8 files, 456 bytes (8886 added) |
| Read-set | state version 4, collection run `0aacb8c0…` |
| Revision batches captured | `abd67870-0955-4792-be0e-32dac78b2e1b` |

Its three snapshot roots are the current tree, the source metadata directory and
the revision batch directory — the revision batch only became part of a snapshot
once it existed and its reconciliation had completed.

### Independent restore of both snapshots

Same rules as exercise 1: the disk was mounted and unmounted only through the
daemon's media actions, restic was driven directly with `--no-lock`, and the
password reached it only as `--password-file` pointing at the existing protected
secret file.

`restic snapshots` listed all three snapshots, each tagged with its own
`rotup-run:` tag, and `restic cat config` again reported repository ID
`1d80cb0f…`.

Restoring `f41cbc26…` (24 files/dirs, 456 B) into a fresh scratch directory
gives:

```
0fb3a18dcfb9a326c0df71aba5e1785ca580190d94e51b73eb0bce6572f10677  revisions/80eb2412-…/0aacb8c0-…/m4-roundtrip-20260905T161558Z/change.txt
cd51ec7c6ea1f6cf76d1150713e5c52a6a00a8c109837a04fd03b34d981b088a  revisions/80eb2412-…/0aacb8c0-…/m4-roundtrip-20260905T161558Z/remove.txt
92978719b5a29f876085e05ae8084c317441976a84b095aa44e518cda9f84752  sources/wsl-test/current/m4-roundtrip-20260905T161558Z/change.txt
eaae7be40241afa5fee7c991dcb1e6d1d56a77a1e719227ea61e48b209ebc0a0  sources/wsl-test/current/m4-roundtrip-20260905T161558Z/keep.txt
```

The archived hashes equal the fixture's original hashes; the current hashes
equal the live share's files as they are now; `remove.txt` is absent from the
restored current tree. The restored current tree also compares identical to the
live staging tree published by collection `0aacb8c0…`.

Restoring `3caa1587…` (19 files/dirs, 407 B) into a second fresh scratch
directory shows the baseline snapshot is untouched by anything that happened
afterwards: it still holds `change.txt` at `0fb3a18d…`, `keep.txt` at
`eaae7be4…` and `remove.txt` at `cd51ec7c…`, and contains no `revisions`
directory at all.

Neither restore altered staging, and the share's three pre-existing files hash
exactly as they did before the exercise began.

### Defect found and fixed: displaced objects were under-counted

The second collection reported `filesDeletedOrArchived: 1` while its own
revision batch durably recorded `itemCount: 2`. The run's history contradicted
the data it had just archived.

The collector set that figure from the engine's delete counter alone. Under
`archive_then_sync` nothing is deleted: rclone runs `sync --backup-dir`, which
*moves* every displaced object into the batch. An isolated probe against rclone
1.75.0 — one replaced file and one removed file, local directories only —
reported `deletes: 1, renames: 2`: the replaced file is counted only as a
rename, and the removed file is counted as both. The count that matches the
batch is therefore renames, not deletes.

The fix adds `displacedObjects` in `internal/collector/service.go`, which
returns renames for an archiving policy and deletes otherwise. The collector's
fake engine did not model this — it incremented only deletes — which is why no
test caught it; it now mirrors the counters real rclone reports, with the probe
result recorded in a comment. The regression
`TestArchivingRunReportsEveryDisplacedObject` collects one replaced and one
removed file and asserts the run's reported figure equals its revision batch's
item count. Reverted against the old expression it fails with
"run reported 1 displaced object(s) but its revision batch holds 2", which is
exactly what the live daemon did.

Nothing about which bytes were archived, restored or backed up was affected:
the revision batch, the snapshots and every hash above were already correct.
The defect was in an operator-facing count only. The live run recorded above
predates the fix, so run `0aacb8c0…` keeps `filesDeletedOrArchived: 1` in its
history; a rebuilt daemon reports 2.

## Final state

- Onboarding is `complete: true`; `FIRST_BACKUP_COMPLETED` is `done: true`.
- Repository `331f4905…` is `HEALTHY`, `lastSnapshotId`
  `f41cbc262e078ecbb083d00600fe20181a8afcee701d21d0167cb62315688efb`,
  `lastSuccessfulBackupAt` `2026-09-05T16:17:49.938Z`. Three snapshots exist.
- `restic list locks` was empty after exercise 1; no backup or restore in either
  exercise left a lock that would block a later recovery.
- The disk was synced and unmounted through the daemon after every step, on
  success and on the failure path. Final status `AVAILABLE_UNMOUNTED`,
  `busy: false`, no ROTUP entry in the host mount table. The image remains
  attached to `/dev/loop0`.
- The test fixture is deliberately left in place, in the share, in staging and
  in the snapshots, so this evidence stays reproducible.

## Limitations

- Loop image, not removable hardware: no physical disconnect, no surprise
  removal, no power loss, no USB controller or hotplug path was exercised.
- Interruption and reconnection behaviour has still not been exercised.
- No `restic check`/recovery path, no retention, forget or prune was run.
- The data is tiny (at most 8 files, 456 bytes) with no large files, hard links,
  unusual permissions or non-ASCII names. The round trip covered one replaced
  file and one removed file; it did not cover renames, directory removal, or a
  file that reappears after being archived.
- The `filesDeletedOrArchived` fix is proven by the regression test and the
  rclone probe, not by a live re-run: the daemon under test was the pre-fix
  build, and re-running it would only have added more state to this fixture.
- The daemon ran from a working-tree build with data directories on DrvFS
  (`/mnt/c`), whose permission modes the system setup check still reports as too
  broad. That is a fixture property, not the approved installation layout.
- The restores ran on the same host, as the same account, with local access to
  the secret store. They are independence checks against ROTUP's orchestration,
  not a disaster-recovery drill on a foreign machine.

## Reproduction

The scripts used are kept under the gitignored `.cache/operator/m4-loop/` tree.

Exercise 1: `01-capture-readset.sh` (pre-backup checksums), `02-backup.sh`
(CSRF handshake, backup, polling, disk state), `03-locator.sh` with `locator.go`
(read-only lookup of the credential locator — a file name, never the secret),
`04-restore-verify.sh` (mount, independent restore, comparison, unmount on every
exit path), `05-final-state.sh` (onboarding, lock check, final disk state),
`06-identity.sh` (UUID re-verification).

Exercise 2: `10-identify-share.sh` (share backing directory from Samba's own
configuration), `11-create-fixture.sh` (fixture creation and pre-existing-file
hashes), `20-cycle.sh <label>` (one supported collection followed by one
supported backup, with unmount on every exit path), `21-mutate-fixture.sh`
(change one file, remove one file), `30-rclone-counter-probe.sh` (the isolated
rclone counter probe), `31-affected-validation.sh` and
`32-prove-regression-catches.sh` (validation for the fix and proof the
regression fails without it), `40-restore-roundtrip.sh` (independent restore and
comparison of both snapshots).
