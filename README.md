# ROTUP

ROTUP is currently at the M1 walking-skeleton milestone. The daemon starts a
local HTTP dashboard, initializes SQLite, and records a clearly labelled demo
run to prove the application, persistence, and web layers are connected.

## Run locally

```sh
go run ./cmd/rotupd
```

By default the dashboard listens on `http://127.0.0.1:8080`, and the
development SQLite database is created as `./rotup-dev.db` in the process
working directory.

Configuration is available through environment variables:

- `ROTUP_HTTP_ADDR` (default `127.0.0.1:8080`)
- `ROTUP_DB_PATH` (default `./rotup-dev.db`)
- `ROTUP_LOG_LEVEL` (default `info`; accepts `debug`, `info`, `warn`, or
  `error`)

The M1 run shown on the dashboard is demo data only. Collection, scheduling,
USB handling, and real backups are not implemented yet.

