# UI modernization checkpoint

This is a local React/Vite migration over the existing Go services, not M5.
The backend remains authoritative for validation, credentials, collection,
staging barriers, UUID identity, setup/escrow, backup durability and recovery.
`docs/ARCHITECTURE.md` is unchanged.

## Checkpoint 1: JSON API and development-mode foundation

Implemented in `internal/web/api*.go` alongside the unchanged HTML routes.
The JSON API also drives the separate Vite preview added in checkpoint 2.
The daemon still serves the existing HTML UI at this checkpoint.

- `GET /api/v1/health` retains the `/healthz` JSON shape and status semantics.
- `GET /api/v1/system` returns safe version/engine information, `devMode`, and
  a CSRF token. The matching HttpOnly cookie is issued on this request.
- `GET /api/v1/dashboard` returns source/storage/repository summaries, the last
  persisted successful-backup timestamp and up to 100 recent runs.
- Sources: list/detail GET, create POST, full replacement PUT, and POST actions
  `test`, `collect`, `credential` (`action: replace|clear`; password write-only).
- Disks: list/detail GET, POST `register` with `filesystemUuid` and `name`,
  settings PUT, explicit POST `mount` and `unmount`. There is no invented probe
  operation; reading the disk list obtains a fresh, non-mutating observation.
- Repositories: list/detail GET, POST collection with
  `{diskId, mode: INITIALIZE|ADOPT, password, escrowAcknowledged}`, and POST
  `:id/initialize`, `:id/backup` with `{sourceIds: [...]}`, `:id/recover`.
  Creation always means the existing explicit setup operation, never an empty
  record followed by automatic initialization. Source eligibility is supplied
  by the existing service on repository detail.
- Runs: list/detail GET. Detail carries explicit collection and backup DTOs,
  including the recorded read-set, revision batches and durability boundaries.
- `GET /api/v1/debug/status` exists only with `ROTUP_DEV_MODE=true`. Device
  paths and mount diagnostics are omitted otherwise. This switch reaches only
  HTTP presentation; it never changes service or adapter configuration.

### Wire contract and safety

Field names are camelCase except the existing health compatibility response.
Timestamps are UTC RFC3339 strings or null. Collections are arrays, never null.
Read responses name their contents (`sources`, `disks`, `repositories`, `runs`);
details use singular keys. Setup/run actions return 202 with `{runId}` and a
Location header. Synchronous creation returns 201 with `{id}`. Structured errors
contain `error: {code, class, message, userMessage}`; raw untyped errors are not
exposed. Credential references, provider locators and arbitrary internal JSON
are deliberately not serialized. Existing legacy diagnostics remain available.

Mutations require application/json, an object body (use `{}` for empty actions),
the CSRF cookie, and `X-CSRF-Token` from `/system`. Same-origin checks are shared
with legacy forms. Unknown fields, trailing JSON and bodies over 64 KiB fail.
There is no permissive CORS mode. Long-running actions use daemon-lifetime
contexts and preserve the existing service result/error behavior.

History endpoints are bounded to the latest 100 entries, identified in responses;
absence within this window must not be presented as proof an event never happened.
Dashboard backup success comes from repository success metadata, not that window.
No schema, migration, scheduler, retry, unlock or recovery semantics were changed.

## Current validation commands

Checkpoint 1 passed under WSL Ubuntu: `go test ./...`, `go vet ./...`, and
both Linux executable builds. Focused API/configuration tests also passed with
`-count=1`. Existing real-media/restic spikes remain opt-in and were not rerun
for this presentation-only checkpoint.

```sh
go test ./...
go vet ./...
go build ./cmd/rotupd
go build -o .cache/ui/rotup-mounthelper ./cmd/rotup-mounthelper
ROTUP_DEV_MODE=true go run ./cmd/rotupd
```

The helper output path preserves the tracked prebuilt helper artifact. For real
media use the installed privileged wrapper, never elevate the daemon:
`ROTUP_MOUNT_HELPER=/usr/libexec/rotup-mounthelper-sudo`.

## Remaining work

1. Create `web/` React + TypeScript + Vite with typed API client, CSRF bootstrap,
   central errors, routing, reusable accessible components and focused tests.
2. Implement dashboard/setup presentation, sources, disks, repositories, history
   and dev diagnostics against these endpoints. Do not duplicate state machines.
3. Embed built assets in the Go binary; preserve HTML UI under `/legacy` until
   equivalent workflows are tested. Keep `/api/*` and `/healthz` outside SPA fallback.
4. Document/test Vite proxying with `changeOrigin: false` so Host and Origin remain
   consistent through the same-origin proxy; never strip Origin to bypass CSRF.
5. Run browser workflow and embedded-build validation. Use disposable source
   fixtures for mutating tests, and never initialize over established storage.

Commit each working slice and update this document. No M5 work is authorized.

## Checkpoint 2: React foundation and overview

`web/` contains a typed API client, same-origin CSRF bootstrap, abortable read
hooks, React Router shell, accessible primitives, system dark/light styles and
a real-data dashboard/setup checklist. Mutations are never automatically retried.
History/read snapshots are explicitly bounded; missing data is not success.
The remaining page routes currently explain that migration is in progress.

Use Node 22.12+ (Node 24 used here), with npm:

```sh
# Terminal 1, repository root; add the existing media/engine settings if needed.
ROTUP_DEV_MODE=true go run ./cmd/rotupd
# Terminal 2
cd web
npm ci
npm run dev
# http://127.0.0.1:5173 (proxies /api to 127.0.0.1:8080)
npm test
npm run typecheck
npm run build
```

Versions are pinned in `package-lock.json`. No Node runtime is needed by Go;
production embedding comes in a later checkpoint. Build output is ignored for
now. No lint framework is configured; TypeScript strict checks and component
regressions are present. Four component tests passed; typecheck and Vite build
passed. Browser verified the dashboard against the WSL database and checked the
dark layout. All management pages and embedded serving remain to be implemented.

## Checkpoint 3: sources, disks and run workflows

React source list/detail/new/edit pages use the existing Go use cases. Password
replace/clear is explicit, write-only, never stored in browser storage, and the
input is cleared after either success or failure. Collection requires a dialog
that distinguishes local staging from USB backup. Accepted runs open History.

Disk list/detail/registration/settings are implemented. Registration submits the
filesystem UUID, not a device node; manual mount/unmount diagnostics remain behind
technical details and confirmation. Current presence and identity still come from
Go. No new media logic was added. Run list/detail show exact errors, collection
results and backup durability/read-set data without inferring success from a
candidate snapshot. The repository screens still await checkpoint 4.

Validation: typecheck + production build passed; 10 frontend tests passed.
Browser verified real WSL source list/detail and started a successful connection
test through Vite's proxy (run `782662c4-a88e-428a-863f-2e9227e2fa74`).
The collection action was also submitted through its confirmation dialog.

## Checkpoint 4: backup storage and diagnostics

All requested React route families now have working pages: overview, sources,
disks, repositories, history and diagnostics. Storage preparation is explicit
initialize/adopt with an unchecked escrow acknowledgement and a separate review
dialog. Existing established storage links to its detail instead of offering
initialization. Backup selection uses backend eligibility and submits the selected
IDs only; verification/recovery is always an explicit confirmed action.

The debug page does not fetch diagnostics when `devMode` is false. New component
tests cover that guard, eligibility, escrow, password clearing and the snapshot
success boundary. Thirteen frontend tests, typecheck and production build passed.
The real WSL collection submitted via the SPA completed successfully as run
`8b8d1ed2-7117-4702-864f-ced8a5608fe5`, publishing local state version 2.

API follow-up: a run's revision batch is now fetched using the existing exact
`RevisionBatchForRun` read method, not searched in a bounded source history.
Disk-setup/verification run targets are decoded through a two-field allowlist;
arbitrary summary JSON remains private. Repository detail includes the most
recent successful verification identifiable within the latest 100 global runs,
or null. Null must be shown as unavailable in recent history, not never verified.

Remaining: embed the production build, move the intact Go UI to `/legacy`,
finish browser checks (including nested-route refresh and failures), and run the
full Linux regression/build suite again. No real disk setup/backup/recovery was
performed during UI development; these operations are covered by existing backend
tests and new facade delegation tests.
