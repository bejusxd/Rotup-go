# ROTUP operator checkpoint

Updated: 2026-09-05. Baseline reviewed: `f7c6de8`.

## Operating mandate

The user delegated routine project operation: select bounded tasks, coordinate
coding agents, inspect commits, run terminal/WSL/tests, verify local UI, and keep
this checkpoint current. Escalate passwords/sudo interaction, destructive work
on real media, and genuine product or architecture decisions. Never print secrets.
Use one writer per file area; require evidence before accepting agent reports.
Preserve `docs/ARCHITECTURE.md` and existing media identity/recovery invariants.

## Milestones

| Milestone | Evidence and current status |
|---|---|
| M1 skeleton | Implemented; current Go suite and live health endpoint pass. |
| M2 collection | Implemented; live UI records successful SMB test and published collection. Not rerun against SMB in this checkpoint. |
| M3 media | Implemented; prior loop-device evidence exists. Physical removal acceptance remains separate. Registered test disk is currently disconnected. |
| M4 backup | Implemented, acceptance OPEN. Live UI records no completed disk backup. Requires real SMB/USB, changed/deleted-file round trip, independent restore and interruption evidence per m4-backup.md. |
| M5 scheduler | Not started; held until M4 acceptance and explicit scope review. |
| M6 rotation | Planned. |
| M7 reporting | Planned. |
| M8 retention | Planned. |
| M9 hardening | Planned; bootstrap work is supporting infrastructure, not milestone completion. |

## Current validation

- `go test ./...` and `go vet ./...` passed under Ubuntu WSL.
- Frontend: 28 tests in 4 files passed; TypeScript check passed.
- Browser `/setup`: 6/8 application steps confirmed; disk disconnected and first
  backup missing. System setup reports missing runtime directories, old wrapper,
  missing boot integration, and broad DrvFS data-directory modes.
- No live installation, secret access, data migration or real-media mutation
  performed during operator takeover.

## Work queue

1. Implemented installer fix: reject unsafe directory ancestors and stop dependent
   runtime/data/config mutations after parent refusal. Isolated regression tests
   cover redirected data, runtime, configuration and `/var/lib` directories.
2. Resolve setup custom-path mismatch: provisioning and generated service config
   use defaults while inspection honors environment overrides.
3. Ensure active helper verification tests the intended service account, rather
   than reporting a root invocation as evidence for that account.
4. Reconcile the live WSL runtime with the approved installation layout, then
   resume M4 acceptance only with positively identified test media. Do not treat
   local/fake-media tests as USB acceptance.

## Continuation

Read this checkpoint, actual HEAD/status and installation/m4 operations docs.
Check for active writers before dispatch. Finish the first unresolved queue item,
review its diff and tests, commit a coherent checkpoint, then update this file.
Do not start future milestones or infer acceptance from a green unit test suite.
