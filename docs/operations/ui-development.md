# UI development and validation

This is a local React/Vite migration over the existing Go services, not M5.
The backend remains authoritative for validation, credentials, collection,
staging barriers, UUID identity, setup/escrow, backup durability and recovery.
`docs/ARCHITECTURE.md` is unchanged.

## Current status

The React migration is implemented: versioned API, explicit development mode,
all requested page families, same-origin Vite development, embedded production
assets and an intact `/legacy` interface. A product-quality review and polish
pass over that UI is recorded in "Operator polish pass" below. The other
sections record historical checkpoints; their "remaining" lists describe that
point in time, not current missing features. M4 real-media acceptance remains
separate and is not declared complete by this UI work. No M5 work was started.

### Node is not installed on this workstation

`node` and `npm` are not on PATH on Windows or in WSL, and `web/node_modules`
was installed by an earlier session from a Windows host. Recent frontend work
used a portable Node 24 unpacked into the session scratchpad rather than
installing a toolchain:

```sh
# Download once, from WSL, into a scratch directory:
curl -sSL -o node.zip https://nodejs.org/dist/v24.20.0/node-v24.20.0-win-x64.zip
# Then, from PowerShell, prepend the extracted directory to PATH:
$env:Path = "<scratch>\node-v24.20.0-win-x64;$env:Path"
```

`node_modules` holds `@rolldown/binding-win32-x64-msvc`, so the Windows build
is the one that matches the installed tree. Reinstalling from WSL would replace
those native modules with Linux builds; the platform-neutral `web/dist` output
is identical either way and is what the Go build embeds.

### Commands

```sh
# Development, terminal 1 (keep existing engine/media environment variables)
ROTUP_DEV_MODE=true go run ./cmd/rotupd
```

From the repository root in a second terminal:

```sh
# Development, terminal 2
cd web
npm ci
npm run dev
# Open http://127.0.0.1:5173
```

From the repository root, after stopping Vite:

```sh
# Verification and production build
cd web
npm ci
npm test
npm run typecheck
npm run build
cd ..
go test ./...
go vet ./...
go build ./cmd/rotupd
go build -o .cache/ui/rotup-mounthelper ./cmd/rotup-mounthelper
# Run with ROTUP_DEV_MODE=false (the default); open http://127.0.0.1:8080
```

Run installs before Go's `./...` commands, not concurrently: Go walks the source
tree and can encounter disappearing package folders during `npm ci`. Windows
also holds Vite's native module open while Vite runs, so stop it before `npm ci`.
No extra permission or elevated daemon is needed. The install lockfile was
verified with Node 24/npm; WSL Go builds consume the same platform-neutral dist.

### Setup and onboarding

Installation, the `rotupd setup` and `rotupd doctor` commands, and everything
they provision are documented in [installation.md](installation.md). Two points
matter for UI development:

- Onboarding truth is derived in Go and served by `GET /api/v1/setup`. The
  browser renders those steps and never recomputes them; the dashboard reads
  the same endpoint, so the two views cannot disagree. Adding a step means
  changing `internal/web/api_setup.go`, not the React page.
- System provisioning is deliberately unreachable over HTTP. `/setup` reports
  host findings from the same read-only inspection as `rotupd doctor` and
  points at the command; it never offers to run it.

A development daemon started from a repository checkout will report warnings
about `/var/lib/rotup` and `/run/rotup`, because it is configured with the
repository-local paths instead. That is correct: doctor and the setup API check
the paths the daemon is actually configured with and say so.

### Routes

React: `/`, `/setup`, `/sources`, `/sources/new`, `/sources/:id`, `/sources/:id/edit`,
`/disks`, `/disks/:id`, `/repositories`, `/repositories/new`,
`/repositories/:id`, `/runs`, `/runs/:id`, `/debug` (development only).
The old staging browser, event log and revision coverage diagnostics remain
under `/legacy`, linked from the SPA. Production debug API returns JSON 404.

### Final validation and limits

- Frontend: clean `npm ci`, 14 tests, typecheck and build passed. No lint command
  is configured. Runtime dependencies are React, React DOM and React Router;
  there is no UI framework, state-management library or Node production server.
- Linux: formatting of all tracked Go files, `go test ./...`, `go vet ./...`,
  daemon and helper builds all passed (exit 0) again after the final regression
  changes. `gofmt -l` returned no files. Architecture and core service/adaptor
  directories have no diff from the starting commit.
- Browser: dashboard; source list/detail; real connection test and collection;
  disk list/detail (including disconnected identity); repository list/detail;
  existing-storage initialization guard; run list/detail; nested source refresh;
  validation refusal plus exact error details; legacy links; production debug
  hiding; mobile dashboard (390 px viewport, no horizontal overflow); embedded
  build with Vite stopped. No production-page JavaScript errors were observed.
- The two real source operations and their results are recorded below. Browser
  password replacement, new-media registration, repository initialization,
  backup and recovery were not executed against real media for this UI task;
  facade delegation and frontend safety tests cover their request boundaries.
- History remains bounded to 100 runs, as before. Latest successful verification
  is available only when found in that window. No pagination or new retention
  behavior was introduced. Current display names are not part of the immutable
  backup read-set; run details retain the recorded identifiers and state versions.
- M4 loop/USB/restic acceptance and opt-in real-engine spikes were not rerun as
  part of this UI-only change. Do not infer M4 acceptance from UI tests.

### Next task

Continue the existing M4 acceptance drill through the new UI with the intended
test disk connected. The polish pass below did not touch that drill. Verify explicit setup/adoption on disposable storage,
manual backup durable success, interrupted/suspect recovery, and idle unmount.
Keep the existing helper, UUID, staging and success-boundary checks unchanged.
Do not initialize over the established repository, do not modify the architecture,
and do not start M5. UI features are not blocked on that separate acceptance drill.

## Operator polish pass

A critical product-quality review of the React UI against real WSL data
(one SMB source, one registered-but-disconnected disk with established
storage, one recorded mount failure, four enumerated filesystems). No
backup, staging, media-identity or repository semantics were changed, no
safety invariant was weakened, `docs/ARCHITECTURE.md` was not touched, and
the legacy interface is unchanged and still linked from every page footer.

### Defects found and fixed

1. Every disk size was printed in gigabytes, so a 374 MB volume, a 167 MB
   volume and a 1 TB volume read as "0,4 GB", "0,2 GB" and "1099,5 GB".
   Size is the only field that distinguishes unnamed filesystems, so this
   removed the operator's ability to tell them apart. Sizes now scale.
2. Run duration was always seconds with one decimal, unusable for a backup
   measured in hours. It now switches to ms, seconds, minutes and hours.
3. Re-reading a resource cleared its data first, so every "Refresh" button
   and every reload after a mutation replaced the page with the loading
   message and lost the operator's scroll position. `useQuery` now keeps
   the previous values for the same path and exposes a `refreshing` flag,
   which the refresh buttons use to prevent a second in-flight read.
   Switching paths still shows nothing until the new resource answers, and
   a failed read still clears stale healthy data.
4. Unregisterable filesystems were refused with a generic sentence plus an
   error code that is empty for the two cases an operator actually meets
   (no filesystem UUID, unsupported filesystem type). The refusal now says
   which condition applies and what to do.
5. The disk page's only action always went to the storage preparation form,
   which then reported that this disk already has prepared storage. It now
   links straight to that storage when it exists.
6. The dashboard's primary action read "Continue setup" and every checklist
   step linked to a list page. Each step now names its action and links to
   the page that performs it, and the final step asks for the disk to be
   connected when it is not.
7. The most recent recorded failure was rendered below the source and
   storage panels, so the top of the page could read as untroubled. It is
   now directly under the status summary, with the operation name and the
   timestamp needed to judge whether it still applies.
8. A backup run's detail page did not resolve the recorded repository and
   disk that the run list already resolved, leaving no route back to the
   storage the run wrote to. Fixed in `apiRun`; the history test asserts
   that the list and the detail agree.
9. Internal vocabulary appeared in normal prose: state versions, staging
   paths, policies, run IDs, triggers, internal status values, restic
   identity, filesystem UUIDs, "displaced/deleted". All of it moved into
   technical details, which stays open in development mode; none of it was
   removed, and each page still states the exact recorded values an audit
   would quote.
10. A failed run repeated its raw internal summary as the body of "Recorded
    result", directly under an error alert explaining the same failure in
    better words.
11. A run detail page polled a finished run forever.
12. Emptying an include/exclude rules box submitted one blank rule.
13. The storage preparation form asked for a repository password and an
    escrow acknowledgement before the operator had chosen a disk.
14. The tool-unavailable alert named rclone and restic but never said which
    one was missing; it now names the lost capability and keeps the engine
    name, version and the backend's own detail in technical details.
15. An unknown address and the disabled-diagnostics page had no level-one
    heading and kept the previous page's document title.
16. Diagnostics was a single JSON dump. It now leads with platform, both
    engines with availability and reason, media-enumeration state and its
    observation time, and a table pairing each filesystem UUID with the
    device path, mount point, external mounts and lease state observed
    right now. The raw allowlisted response is still printed underneath.

### Deliberately not changed

- Demo runs still appear in History and on the dashboard's recent activity.
  They are real audit records; filtering them is a product decision, not a
  presentation fix.
- The disk list still offers every supported filesystem the platform
  enumerates, including host system volumes under WSL. That is the media
  layer's enumeration; the page warns to register only a disk the operator
  recognises, and the backend still performs every registration check.
- `apiRuns` still resolves each backup run's target with one query per run.
  With history bounded to 100 runs and few of them backups, the N+1 is not
  worth the risk of restructuring a read path.

### Validation for this pass

- Frontend: 23 tests, typecheck and production build passed with Node
  24.20.0. Sources are Prettier-formatted. No lint command is configured.
- Linux (WSL Ubuntu, Go 1.27): `gofmt -l ./internal ./cmd ./web` returned
  no files, `go vet ./...` passed, `go test ./... -count=1` passed, and
  both `rotupd` and `rotup-mounthelper` built.
- `web/dist` was rebuilt from this source and the daemon rebuilt around it,
  so a plain Go build serves this UI.
- Browser, Vite development against the real WSL database with
  `ROTUP_DEV_MODE=true`: overview, sources list/detail/new, disks
  list/detail, backup storage list/detail/preparation, history, run detail
  for a success and for a recorded mount failure, diagnostics, legacy
  dashboard and legacy repositories, unknown-route fallback, and a real
  connection test started from the UI (run
  `06754369-d88e-4a05-9079-797c21df900e`, succeeded, page updated in place
  and stopped polling when it finished). No console errors.
- Browser, embedded production bundle with `ROTUP_DEV_MODE=false`:
  Diagnostics is absent from navigation and `/debug` renders "Page not
  found"; `GET /api/v1/debug/status` returns JSON 404; an unknown
  `/api/v1/...` path returns JSON 404 rather than the SPA; a nested route
  refreshes directly; `/healthz` unchanged; `/api/v1/disks` contains no
  `devicePath`; technical details render collapsed. No console errors.
- Narrow viewport: `documentElement.scrollWidth` equals `clientWidth` on
  the overview, disks, diagnostics and repository detail pages at 485 px,
  and no element in `main` extends past the viewport.
- No destructive real-media operation was performed. Disk registration,
  storage preparation, backup and recovery were exercised only through
  their guards and unit tests. M4 real-media acceptance is unchanged and
  must not be inferred from this pass.

### Remaining UX debt

- History has no filter, so development demo records sit above real
  operations. A "hide demo records" toggle would be presentation-only, but
  it is a product decision.
- History and per-source activity are bounded to the latest 100 runs with
  no pagination; older audit records exist and are unreachable from the UI.
- The dashboard states that a durable backup completed somewhere, not
  whether each source is covered. Per-source backup coverage would need a
  read model that does not exist yet.
- Nothing streams progress inside a long backup beyond its run status; the
  orchestration phase is recorded but not surfaced as a progress view.
- There is no keyboard shortcut layer and no focus trap of our own inside
  the confirmation dialog beyond what the native `<dialog>` provides.

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

## Checkpoint 5: single-binary SPA and legacy transition

`web/assets.go` embeds the checked-in `web/dist` build; `internal/web/spa.go`
serves the entry point for client routes and hashed assets with immutable caching.
The entry point revalidates; missing assets return 404. API paths and `/healthz`
remain Go routes and never fall through to the SPA. The production CSP permits
only same-origin scripts/connections (no inline scripts/eval); legacy inline CSS
is retained. No Node process is needed to run the built daemon.

The complete old HTML interface is at `/legacy/`, including staging inspection,
events, revision coverage and all original forms. Its links/actions/redirects were
prefix-adjusted, not removed. Existing tests were updated only to use `/legacy`
URLs, retaining their behavior assertions. Added embedded-asset, route-refresh,
API isolation, method, CSP and legacy-link regressions. The application lifecycle
test checks both the SPA entry and the legacy persisted-demo dashboard.

Linux `go test ./...`, `go vet ./...`, and builds of `rotupd` and
`rotup-mounthelper` passed. Helper builds use `.cache/ui/rotup-mounthelper` to
preserve the repository's tracked prebuilt helper. Frontend build/typecheck and
13 tests passed. Source files were formatted for readable future maintenance.
The production bundle was started as the normal WSL user, using the existing
database and helper wrapper, with `ROTUP_DEV_MODE=false` for browser validation.

## Checkpoint 6: final regressions and handoff

Added explicit errors for common configuration/mount refusals while retaining
the original class/code/safe message. Dashboard tool/media failures are surfaced
instead of being obscured by historical success. History shows source names,
stored repository targets, summaries and finish times; the exact captured backup
read-set is tested independently of current source configuration. Candidate-only
snapshots remain unproven. The production bundle and source were rebuilt together.

Final checks: 14 frontend tests; clean dependency installation; typecheck and
build; full Linux Go tests, vet, daemon build and helper build all passed. A
transient Windows native-module lock during an initial install was resolved by
stopping Vite. An overlapping Go package scan/install was rerun sequentially and
passed. No tests were disabled or assertions removed to resolve these issues.
