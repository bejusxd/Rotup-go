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
