# M4 manual backup and recovery

The M4 implementation extends the existing M2 collection and M3 media services.
It does not add scheduling, automatic retries, retention, unlock, forget or prune.
Linux/ext4 is the deployment target. Windows builds support development tests,
not media management; only the Windows restic invocation omits the unsupported
`--one-file-system` flag. Linux keeps it.

## Manual operation

1. Configure explicit `ROTUP_RCLONE_PATH`, `ROTUP_RESTIC_PATH` and the M3 mount
   helper as described in [M3 operations](m3-removable-media.md). Set staging and
   secret roots to separate service-owned locations.
2. Configure a test SMB source on `/sources`. Test connectivity and collect it.
   Confirm a successful collection and a stable published staging version.
3. Connect an authorized ext4 USB disk and register its filesystem UUID on
   `/disks`. It should be available and unmounted. Registration does not create
   a repository or erase anything.
4. On `/repositories`, initialize a new repository or explicitly adopt an existing
   one at `rotup/restic` on that disk. Supply a password already recorded outside
   this host and acknowledge escrow. A nonempty location is never initialized
   over. A failed setup remains visible with explicit setup/adoption actions.
5. Open the repository page, select published sources and start a manual backup.
   Review the run's captured staging versions, revision batches, snapshot ID,
   durability and events. The disk must return to unmounted before success.

The password is stored in the existing secret store. SQLite contains only its
reference. A private transient password file supplies restic; passwords are
never command arguments, output fields or prefilled UI values.

## Success and recovery boundaries

A single daemon-wide staging barrier excludes collection mutation while restic
reads. Each disk lease covers setup/backup/recovery through sync and unmount.
The backup reads only selected current trees, their available ROTUP metadata and
completed revision batches. It never includes work, lost+found or secrets.

Success requires a clean restic result with no reported read errors, an independent
snapshot listing proving the run tag, unique snapshot ID and requested root paths,
then successful sync and verified unmount. One SQLite transaction records the
successful run, repository history and coverage derived from the recorded read-set.
A failed completion transaction leaves the operation recoverable, not successful.

On startup, active runs become interrupted. Pending repository writes are also
checked independently of run status, closing the crash gap between run bookkeeping
and marking a repository suspect. New backups refuse pending work or unproven
repository health. Failure cleanup uses a separate bounded context so daemon
cancellation does not suppress unmount or failure records.

Recovery is explicit. It opens the recorded repository identity, lists snapshots,
runs structural `restic check`, syncs and verifies unmount **before** adopting any
backup or recording coverage. It does not re-run the interrupted backup. A failed
check, failed durability step or failed adoption commit cannot return the
repository to healthy. Structural check is not a full read-data or restore test.

Adoption requires a unique run-tagged snapshot matching a durably recorded clean
restic result and the recorded read-set paths. Restic 0.17.3 can create incomplete
snapshots and its snapshot metadata has no read-error/completion flag. Therefore:

- A crash after the candidate result was recorded, including a failed final
  completion commit, can be resolved by adoption without another backup.
- If the process result or candidate-result commit was lost, a tagged snapshot
  alone cannot prove completeness. Recovery retains it, declines success/coverage
  and explains why. A later backup is an explicit new operator action.
- Known partial reads, missing requested roots, missing/ambiguous tags and missing
  read-sets never earn revision coverage.

A failed unmount may leave a mount behind. Use the explicit M3 sync/unmount action
to resolve it before recovery; ROTUP never mounts over an existing mount. If restic
left a lock, recovery refuses it. No automatic unlock exists: an operator must
verify that the owner/process is gone before performing any restic lock cleanup.
If initialization stopped before recording a repository ID, explicit adoption
with the escrowed password is the recovery route; ROTUP never reinitializes over it.

## Repeatable validation

Run the portable suite and build both Linux executables:

```sh
go test ./...
go vet ./...
go build ./cmd/rotupd
go build ./cmd/rotup-mounthelper
```

Run both real-restic suites (validated with restic 0.17.3):

```sh
ROTUP_RESTIC_SPIKE=1 ROTUP_RESTIC_PATH=/absolute/path/to/restic \
  go test ./internal/adapters/restic ./internal/backup -run Spike -count=1 -v
```

From WSL in this checkout, using the downloaded, checksum-verified test binary:

```sh
cd /mnt/c/dev/rotup
ROTUP_RESTIC_SPIKE=1 \
ROTUP_RESTIC_PATH=/mnt/c/dev/rotup/.cache/m4-spike/restic_0.17.3_linux_amd64 \
go test ./internal/adapters/restic ./internal/backup -run Spike -count=1 -v
```

The adapter spike uses real subprocesses, passwords and temporary repositories.
The service spike additionally uses real SQLite, staging, read-set capture and
orchestration, with a temporary directory/fake disk lease replacing physical USB.
It checks normal snapshots, preserved history, failed sync/unmount (including
recovery), lost completion commits, missing roots, wrong identity, damaged
repositories and cancellation after observing restic's repository lock. No
timing-based cancellation skip is counted as a pass.

## Still pending before declaring M4 complete

Execute the manual steps above on the Linux target with a **real SMB share and
registered USB disk**, including a second collection with changed/deleted files
and a second backup. Verify the archived old files and current files from an
independent restic restore into a scratch directory using the escrowed password.
Keep the source and staging unchanged by the restore. Record run IDs, full
snapshot IDs, UUID, restored-content checks and final unmounted state.

Exercise interruption/reconnection on disposable test media, following the M3
hardware safety procedure, and confirm no premature success or coverage. The
portable and real-restic service spikes do not prove physical USB removal,
power-loss behavior, Linux mount permissions or SMB-server consistency. WSL2
restic results are not a substitute for that complete acceptance drill.
