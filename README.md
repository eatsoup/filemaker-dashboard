# FileMaker Usage Dashboard

A small, self-hosted Go service that ingests a FileMaker Server **Access.log**, stores per-database session data in SQLite, and serves a web dashboard with usage charts and a monthly billing report.

Designed to answer the question *"which databases were actually used last month, by whom, for how long?"* — useful for hosts who bill customers per active database.

## Features

- Tails the FileMaker Server access log on a configurable interval; resumes safely across restarts and log rotations using a (timestamp, line-hash) cursor.
- Imports rotated/historical log files via `--import` for backfill.
- Stores session opens/closes in SQLite (no external DB required) using the embedded [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) driver — single static binary, no CGO.
- Web dashboard with date range, group-by user/database, min-duration and min-unique-users thresholds, and exclude lists for service users / scratch databases.
- Monthly billing report: distinct active databases per month, hours per database, and distinct users per database per month.
- User accounts with bcrypt password hashing; admin users can manage other users from the UI.
- Configurable defaults so the dashboard opens with sensible filters on first load.

## Requirements

- Go 1.25 or newer (for building).
- Read access to a FileMaker Server `Access.log` file.

## Build

```sh
go build -o filemaker-dashboard .
```

The binary embeds all HTML templates and static assets — there are no extra files to deploy.

## Configure

Copy the example config and adjust the paths:

```sh
cp config.example.yaml config.yaml
```

Minimum required keys:

```yaml
logfile_path: /Library/FileMaker Server/Logs/Access.log
db_path: filemaker.db
listen_addr: :8080
ingest_interval: 10m
session_ttl: 168h

initial_admin:
  username: admin
  password: changeme
```

The `initial_admin` block creates the first admin account on first run only — change the password through the UI afterwards. See `config.example.yaml` for the full set of options including UI defaults.

## Run

```sh
./filemaker-dashboard --config config.yaml
```

Then open `http://localhost:8080` and sign in with the configured admin credentials.

### Importing an old/rotated log

To backfill from a rotated log file (without disturbing the live ingest cursor):

```sh
./filemaker-dashboard --config config.yaml --import /path/to/Access-old.log
```

This processes every line in the file and exits. It can be run multiple times against overlapping logs — sessions are deduplicated on `(start_time, client_id, account, database)`.

## How it works

The parser recognises four line shapes from the FileMaker access log:

- `Client "..." opening a connection from "..." using "..."` — connection open
- `Client "..." closing a connection.` — connection close
- `Client "..." opening database "..." as "..."` — session open
- `Client "..." closing database "..." as "..."` — session close

Open events insert a row with `end_time = NULL`; the matching close event finds the oldest in-flight row for that `(client_id, database, account)` and writes the end time and duration. Sessions left open at log-rotation time stay open in the database — they simply contribute no hours until they close.

The ingest cursor is `(last_timestamp, last_line_hash)`. Re-runs skip lines older than the cursor, look for the marker line within the same second, and (if the marker is never found) treat the file as rotated and process everything as new.

## Project layout

```
main.go                     entry point and embedded FS
config.example.yaml         annotated config template
internal/
  config/      YAML config loader
  parser/      access-log line parser (+ tests)
  ingest/      tailer / one-shot importer
  store/       SQLite schema, queries, migrations
  auth/        bcrypt + cookie sessions, middleware
  server/      HTTP handlers, templates, JSON API
web/
  templates/   html/template pages
  static/      CSS + vanilla-JS dashboard (Chart.js via CDN)
```

## Security notes

- The web UI is intended for trusted internal networks. There is no rate limiting on `/login`; put it behind a VPN or reverse proxy with auth/throttling if exposing publicly.
- Cookies are `HttpOnly`, `SameSite=Lax`, and `Secure` only when the request itself is TLS — terminate TLS at a reverse proxy in production.
- The config file holds the bootstrap admin password in plaintext until first run. After the admin signs in and changes their password, the value in `config.yaml` is no longer used — `EnsureAdminFromConfig` is a no-op once any user exists.

## License

Not yet specified.
