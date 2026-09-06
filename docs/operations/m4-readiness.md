# M4 software readiness review

Reviewed at `e7b0ce4` against the normative milestone text in
`docs/ARCHITECTURE.md` (read only; not modified). The question this answers is
narrow: **is the M4 software complete enough for M5 development to start?** It
does not claim production readiness and it does not claim hardware acceptance.

## What M4 normatively requires

ARCHITECTURE defines M4 as:

> Explicit disk setup initializes its restic repository after escrow
> acknowledgement. A manual `BACKUP_LATEST` run locks staging, mounts the
> selected disk, creates a restic snapshot, records audit/result history, handles
> interruption, syncs and unmounts.
>
> M4 must demonstrate the complete manually triggered path:
> `SMB -> staging -> registered USB disk -> restic snapshot`.

Two things follow that this review takes seriously, because they decide what is
*not* an M4 blocker:

- **`restic check`, forget/prune, retention and low-space behaviour are M8**, not
  M4. A verification cadence and `--read-data` policy belong there.
- **"Crash/restart, surprise USB removal, network failures and SQLite recovery
  are tested; supported hardware/filesystems are exercised; systemd packaging and
  operator recovery/escrow runbooks are delivered" is M9**, not M4. M4 owns
  *handling* interruption; M9 owns the hardening test matrix.

Open question 7 (the escrow procedure and recovery drill) is recorded in
ARCHITECTURE as blocking **production use** of M4 — not M4 development, and not
M5 development.

## Evidence mapping

| M4 requirement | Evidence | Media |
|---|---|---|
| Explicit disk setup after escrow acknowledgement | enforced in `repository.go:55`; `EscrowAcknowledgedAt` persisted; `TestRepositoryInitializationRequiresEscrowAcknowledgement`; live repository `331f4905…` established this way | loop image |
| Never initializes over a nonempty location; explicit adopt | `inspectDirectory` + setup boundary tests; `TestFailedSetupStillOffersExplicitAdoption` | none |
| Manual `BACKUP_LATEST` locks staging | `barrier.AcquireRead` in `backup.go:129`; barrier tests | none |
| Mounts the selected disk, identified by UUID | live runs mount via the M3 helper; identity re-proved each run (`requireRepository` re-checks the filesystem UUID) | loop image |
| Creates a restic snapshot | 5 live snapshots on the test repository | loop image |
| Records audit/result history | run rows, backup rows, read-set, revision-batch capture, events; visible through `/api/v1/runs` and the repository page | loop image |
| Handles interruption | graceful SIGTERM mid-restic (live, run `72297e78…`), cancellation after restic starts (spike), lost completion commit (spike), **process kill across a real boundary** (`TestSpikeCrashedBackupIsSweptThenAdopted`) | loop image / fake |
| Syncs and unmounts | every live run returned the disk to `AVAILABLE_UNMOUNTED`; failure paths unmount through a bounded cleanup context | loop image |
| End-to-end `SMB -> staging -> disk -> snapshot` | live, repeatedly, against the real SMB share | loop image |

Supporting evidence that is not itself an M4 requirement:

- Independent restores compared byte for byte against pre-backup hashes, for the
  current tree and for archived revision batches.
- Recovery refuses without proof: an interrupted run with no snapshot became
  `FAILED`/`snapshot_absent`; a locked repository fails closed and resumes when
  released.
- Adoption happens exactly once and grants exactly the recorded read-set.
- 55 tests in `internal/backup`, 16 M4 web tests, plus the opt-in real-restic
  spikes.

### Bounded confidence check performed for this review

`restic check --read-data` was run read-only against the live test repository —
mounted and unmounted through the supported actions, password supplied only via
`--password-file`, `--no-lock`, nothing written:

```
check all packs / check snapshots, trees and blobs
[0:00] 100.00%  5 / 5 snapshots
read all data
[0:00] 100.00%  12 / 12 packs
no errors were found
```

Repository 50 MiB, exit 0, 1.1 s. The five snapshot IDs were identical before and
after, and no lock was left. Every byte ROTUP has written to this repository
across all six operator tasks reads back intact.

This was a confidence check only. It is **not** normative M4 scope, no product
feature was added for it, and verification cadence remains M8's decision.

## Genuine software blockers

**None identified.**

The review looked for concrete failure scenarios in the code rather than
accepting the docs' claims. The paths that would matter most — a run claiming
success it cannot prove, coverage granted without a read-set, adoption of an
unconfirmed snapshot, a repository returned to healthy without verification —
are each guarded and each have a test that fails when the guard is removed
(mutation-checked for the displaced-object count, the lock refusal and the crash
sweep).

Two defects were found and fixed during this work, both with regressions: the
M3 mount-argument pairing bug, and the displaced-object under-count. Neither is
outstanding.

## Not blockers, and why

| Item | Why it is not an M4 blocker |
|---|---|
| Crash-and-adopt proven only on fake media | M4 requires *handling* interruption, which is proven live and across a real process boundary. Exercising the hardware matrix is M9. |
| No `--read-data` in the product | `restic check` and verification cadence are M8. The ad-hoc check above already shows the data is sound. |
| Renames, directory removal, re-appearing files not covered in round trips | Reconciliation semantics are M2; these are edge cases for the M2/M8 matrices, not the M4 manual path. |
| Lock refusal fault-injected rather than live | Not demonstrable live with restic 0.17.3 without a destructive command; the behaviour is covered at both the adapter and service levels. |
| Broad DrvFS permissions, no systemd unit in the fixture | Properties of the development fixture. Packaging is M9. |
| Escrow procedure/runbook not written | ARCHITECTURE assigns this to open question 7 as a **production-use** blocker for M4, and the runbooks themselves to M9. |

## Deferred hardware and production checks

Explicitly out of scope here and **not** satisfied by anything above:

- Physical USB acceptance: real removable media, surprise removal, power loss,
  hotplug, and real mount permissions. Deferred by the user. All live evidence
  used a loop-mounted ext4 image registered by UUID through the same M3 path.
- The M3 hardware/filesystem matrix (ARCHITECTURE open questions 1-2).
- Escrow procedure and recovery drill (open question 7) before production use.
- systemd packaging, installation on a real target, operator runbooks (M9).

## Recommendation

**The M4 software is sufficient to begin M5 development.** The manually triggered
path is implemented, exercised end to end against a real SMB share and a real
restic repository, and its failure and interruption paths refuse to claim success
without proof. No software blocker was found. The remaining items are either
assigned to M8/M9 by ARCHITECTURE or are hardware acceptance the user has
deferred.

Two conditions on that recommendation:

1. **M4 must not be recorded as accepted.** Milestone acceptance requires the
   physical USB demonstration in the normative path. The correct status is
   "software complete, hardware acceptance deferred".
2. **Production use stays blocked** until open question 7 (escrow procedure and
   recovery drill) is answered, independently of M5 progress.

If the coordinator accepts this, the natural next scope is M5 design — schedules,
backup-after-collection, `BACKUP_LATEST` coalescing, retry/deadline, timeouts and
missing-media waiting through the in-memory resource coordinator — starting with
a written plan rather than code, since M5 introduces the first component that
starts runs without an operator. No M5 implementation has been started.
