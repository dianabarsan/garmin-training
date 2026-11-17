# garmin-training

A Go client for the **Garmin Connect Personal API** (your own data, pull-based, no
developer-program approval), and an **MCP server** that lets AI agents read it.

- `github.com/dianabarsan/garmin-training` is an importable library. The root
  package depends on the standard library only.
- `cmd/garmin-mcp` is the MCP server. It is the only place that uses a third-party
  module (the official MCP Go SDK).

The Personal API is for people's own data. Garmin's FAQ says it is "not intended for
commercial applications serving multiple end users". By default this server serves one
person; [multi-user mode](#multiple-users) lets several people each use their own key.
Read Garmin's terms before using it for anyone but yourself.

## Get an API key

Create a key at <https://connect.garmin.com/app/settings/personalApiToken>. It is
shown once and expires after the period you choose (30 days to a year). The key is
exchanged for a 24-hour access token, which this project renews automatically.

## The MCP server

```sh
go run ./cmd/garmin-mcp          # listens on http://127.0.0.1:8808
```

Then provide your key in any of these ways:

| How | Command |
|---|---|
| Browser form | open <http://127.0.0.1:8808/> and paste the key |
| HTTP endpoint | `printf '{"api_key":"%s"}' "$KEY" \| curl -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth` |
| Environment | `GARMIN_PERSONAL_API_KEY=... go run ./cmd/garmin-mcp` |

Point your MCP client at `http://127.0.0.1:8808/mcp` (streamable HTTP).

More ways to run it on your own machine (install as a command, a background service,
Docker, trying it from claude.ai through a tunnel) are in
[`docs/running-locally.md`](docs/running-locally.md). To host it always-on for your phone,
see [`docs/fly.md`](docs/fly.md) (Fly.io, no domain needed) or
[`deploy/README.md`](deploy/README.md) (your own server with Caddy).

### The `/auth` endpoint

| Request | Effect |
|---|---|
| `POST /auth` `{"api_key": "..."}` (JSON, or the form on `/`) | Checks the key with Garmin; if accepted it becomes the active key. A rejected key changes nothing. `401` means Garmin refused it. |
| `GET /auth` | `{"authenticated": bool, "token_expires_at": ..., "persisted": bool}`. Never returns the key. |
| `DELETE /auth` | Forgets the key (and deletes the stored copy, if any). |

The key stays in memory. To survive restarts, pass `-key-store path` (a file created
with mode `0600` in a `0700` directory); `DELETE /auth` removes it.

### Flags

| Flag / environment | Meaning |
|---|---|
| `-addr` (default `127.0.0.1:8808`) | Address to listen on. |
| `-token` / `GARMIN_MCP_TOKEN` | Require `Authorization: Bearer <token>` on `/mcp` and `/auth`. |
| `-allowed-host` / `GARMIN_MCP_ALLOWED_HOSTS` | Public hostname(s), comma separated, that the server may be reached as. Set it behind a reverse proxy; other `Host` values are refused. |
| `-disable-auth-endpoint` / `GARMIN_MCP_DISABLE_AUTH_ENDPOINT` | Serve no `/auth` or key page (both answer 404), so a leaked token can never replace the key. The key must then come from `GARMIN_PERSONAL_API_KEY[_FILE]` or an existing `-key-store`; the server refuses to start without one. Use it whenever the server is reachable from outside. |
| `-audit-log` / `GARMIN_MCP_AUDIT_LOG` | Append the audit trail (see below) to this file as JSON lines, mode `0600`. Without it the records appear in the normal log, tagged `audit=true`. |
| `-users-file` / `GARMIN_MCP_USERS_FILE` | Run in [multi-user mode](#multiple-users), keeping the registered users in this file. Needs an admin token and a master key; replaces `-token`, `-key-store` and `/auth`. |
| `-admin-token` / `GARMIN_MCP_ADMIN_TOKEN[_FILE]` | Multi-user mode: the token (at least 16 characters) that protects `/admin/users`. |
| `GARMIN_MCP_MASTER_KEY[_FILE]` | Multi-user mode: 32 random bytes as 64 hex characters or base64 (`openssl rand -hex 32`). Encrypts every stored Garmin key. |
| `-key-store` / `GARMIN_MCP_KEY_STORE` | Persist the key in this file. |
| `GARMIN_PERSONAL_API_KEY` | Key to use at startup (wins over the key store). |
| `GARMIN_MCP_TOKEN_FILE`, `GARMIN_PERSONAL_API_KEY_FILE` | Read the token / key from a file instead (Docker secrets); setting both forms of one is an error. |
| `-healthcheck` | Probe a running server at `-addr` and exit 0 if healthy (container health checks). |
| `-log-level` | `debug`, `info`, `warn`, `error`. |
| `-garmin-url` / `GARMIN_PERSONAL_API_BASE_URL` | Garmin base URL (testing). |

### Security model

This server exposes your health data and accepts your Garmin key, so by default it is
built, by default, for a single user on one machine:

- It binds to **loopback** and rejects requests whose `Host` header is not
  `localhost`, `127.0.0.1` or `::1` (DNS-rebinding defence), except `/healthz`, which only answers `ok`.
- State-changing requests carrying a foreign `Origin` or a cross-site `Sec-Fetch-Site`
  are refused (browser CSRF defence).
- It **refuses to start on a non-loopback address without `-token`**.
- `-disable-auth-endpoint` removes `/auth` and the key page entirely, even for a request
  that carries the token. Use it when the server is exposed through a tunnel or proxy.
- When `-token` is set the browser form at `/` is not served (a form cannot send the
  bearer header); use the JSON endpoint.
- The key is never returned by any endpoint or tool, never logged, and never part of an
  error message. Tests check this on every path.
- If you expose it beyond localhost, put it behind TLS (a reverse proxy); the server
  speaks plain HTTP. **[`deploy/`](deploy/README.md) has a ready-to-run setup** (Docker
  Compose with Caddy and automatic HTTPS, exposing only `/mcp`) for reaching it from your
  phone.

### Multiple users

With `-users-file`, one server serves several people. Each is registered by you, the
administrator, with their own Garmin key, and receives their own token. A token opens only
that person's data.

```sh
export GARMIN_MCP_ADMIN_TOKEN=$(openssl rand -hex 24)   # protects /admin/users
export GARMIN_MCP_MASTER_KEY=$(openssl rand -hex 32)    # encrypts the stored Garmin keys; keep it safe
garmin-mcp -users-file ~/.config/garmin-mcp/users.json
```

Register someone (their Garmin key is checked with Garmin first; the response contains
their token **once**):

```sh
printf '{"name":"alice","api_key":"%s"}' "$ALICE_GARMIN_KEY" | curl -H "Authorization: Bearer $GARMIN_MCP_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/admin/users
```

Alice then connects her MCP client to `/mcp` with `Authorization: Bearer <her token>`.

| Request (admin token) | Effect |
|---|---|
| `POST /admin/users` `{"name","api_key"}` | Register a user. `201` with `id` and `token`; `422` if Garmin rejects the key; `409` if the name is taken. |
| `GET /admin/users` | List users (`id`, `name`, `created_at`). Never keys or tokens. |
| `DELETE /admin/users/{id}` | Remove a user. Their token stops working immediately, existing sessions included. |
| `POST /admin/users/{id}/token` | Issue a new token; the old one stops working. Returned once. |
| `PUT /admin/users/{id}/key` `{"api_key"}` | Replace the Garmin key (they expire after 30 days to a year). Checked with Garmin first. |

How it is kept safe:

- **Two kinds of token, never interchangeable.** The admin token opens only `/admin`, a user
  token opens only `/mcp`. Neither opens `/auth`, which does not exist in this mode.
- **Isolation.** Every user gets their own Garmin client and their own MCP sessions, so one
  user's session id means nothing to another user, even one with a valid token. This is
  tested by sabotaging the design and checking the tests fail.
- **At rest.** A user's token is stored only as a SHA-256 hash. Their Garmin key is
  encrypted with AES-256-GCM under the master key, bound to their user id so it cannot be
  moved to another record. A copy of the users file is useless without the master key, and
  the server refuses to start with the wrong one. Both the file and its directory are
  private (`0600`, `0700`). Someone with root on the server can still read the master key
  from its environment, so this protects backups and stray copies, not a compromised host.
- **The administrator is trusted.** Registering someone means handing the server their
  Garmin key; the server can read their data. Only register people who agree to that.
- **Audit.** Every `tool_call` and every registration, removal, token rotation and key
  change is recorded with the user's name and id (see the audit log below).
- **Not combinable** with `-token`, `-key-store`, `-disable-auth-endpoint` or
  `GARMIN_PERSONAL_API_KEY`: the server refuses to start, with the reason, rather than
  guess which mode you meant.

Garmin's terms are your responsibility. Its FAQ describes the Personal API as for people
accessing their own data and not for commercial applications serving multiple end users;
a household or friends each using their own key is the closest fit, a service for
customers is not.

### Audit log

The server records who asked for what, and who was turned away, so you can tell what was
read if a token ever leaks.

| Event | When | Main fields |
|---|---|---|
| `tool_call` | A tool was called | `tool`, `args`, `outcome` (`ok` or `error`), `error`, `duration_ms`, `result_bytes`, `client`, `session`, and in multi-user mode `user` and `user_id` |
| `client_connected` | A client finished the MCP handshake over HTTP | `client`, `session` |
| `access_denied` | A request was refused | `reason` (`missing_token`, `invalid_token`, `forbidden_host`, `cross_origin`), `method`, `path`, `remote`, `forwarded_for`, `user_agent`, plus the `host` or `origin` that was rejected |
| `key_set`, `key_rejected`, `key_set_failed`, `key_cleared` | The Garmin key was provided, refused, failed to validate, or removed at `/auth` | `remote`, `forwarded_for` |
| `user_registered`, `user_removed`, `user_token_rotated`, `user_key_updated`, `user_key_rejected` | Multi-user mode: an administrator changed a user | `user`, `user_id`, `remote`, `forwarded_for` |

Records go into the normal log tagged `audit=true`, or, with `-audit-log path`, into a
separate append-only file of JSON lines (mode `0600`). Useful filters:

```sh
grep audit=true server.log                                       # the normal log
jq -c 'select(.msg=="access_denied")' audit.log                  # who was refused
jq -c 'select(.msg=="tool_call") | {time, tool, args, client}' audit.log
```

**Never recorded:** the Garmin key, the bearer token (neither the real one nor one a client
presented), or any data a tool returned. `result_bytes` says how much came back, not what.

**What is recorded about requests:** tool arguments are logged because they say what was
read (a date range, an activity id). They can contain free text such as a search term, so
keep the log as private as the data. Values are cut to a fixed length and cannot span
lines. `session` is a short hash of the MCP session id, enough to follow one client's
calls without exposing an identifier that could be replayed.

**Limits:**

- Behind a reverse proxy, `remote` is the proxy's address. `forwarded_for` is the
  `X-Forwarded-For` header exactly as received and a client can forge it, so treat it as a
  hint.
- It is a plain file: not rotated (use `logrotate` or similar) and not tamper-evident
  against someone who can write to it.
- A `tool_call` is written when the call finishes, so a call that never returns leaves no
  record.
- `client_connected` is only emitted for HTTP clients, which is how real clients connect.

### Tools

All tools are read-only. Dates are your local calendar dates (`YYYY-MM-DD`); range tools
default to the last 7 days ending today and accept up to 366 days.

| Tool | What it returns |
|---|---|
| `auth_status` | Whether a key has been provided. |
| `list_activities` | Activities with distance, duration, heart rate, elevation, training effect, HR-zone time and, for strength sessions, the per-exercise breakdown. Filters: dates, type, sub-type, text, distance/duration, sort, paging. |
| `get_activity` | One activity's summary. |
| `get_activity_splits` | Per-km (or per-mile) splits computed from the per-second data. |
| `get_activity_series` | Heart rate, speed, elevation, cadence, power over time (GPS is never included), thinned to `max_points`. |
| `get_daily_summaries` | Steps, calories, heart rate, stress, Body Battery, intensity minutes, respiration, SpO2, hydration. |
| `get_sleep` | Sleep stages, score, heart rate, SpO2, sleep need, naps. |
| `get_heart_rate` | Resting/min/max for a day, optionally the curve in buckets. |
| `get_hrv` | Nightly HRV, weekly average, status and baseline. |
| `get_body_battery`, `get_stress`, `get_steps` | Daily series. |
| `get_vo2max` | VO2 max history. |
| `get_heart_rate_zones` | Zones as bpm ranges for a sport. |

**Raw data.** Besides the cleaned-up fields above, every item a tool returns carries
Garmin's complete, unmodified record under `raw`: every field Garmin sent, including those
the curated fields leave out (for example an activity's sensors, lap count or running
dynamics). Every tool that returns it takes `include_raw_data` (default `true`); set it to
`false` to get only the cleaned-up fields. `get_activity_splits` and `get_activity_series`
have no raw data: they are computed from the per-second data, which is several megabytes
per activity and holds the full GPS track, so it is not passed through. Two consequences
of the default:

- **Location.** Activity records include the start and end coordinates
  (`startLatitude`, `startLongitude`, `endLatitude`, `endLongitude`) and `locationName`. For
  runs that start at home, that is your home address, and it becomes part of the
  conversation.
- **Size.** Outputs are several times larger. Measured on a real account: 7 days of sleep
  about 50 KB, 7 days of daily summaries about 53 KB, 10 activities about 39 KB, and a
  year of sleep about 1.25 MB, which is more than an agent can take in at once. For long
  ranges, ask for shorter ones or for `include_raw_data: false`.

If a tool is called before a key is provided, it returns an error telling the agent to
ask you for the key; an agent cannot provide it itself.

## The library

```go
ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
c := garmin.New(garmin.WithTokenSource(ts))

acts, err := c.Activities(ctx, garmin.ActivitiesOptions{SubTypeKey: "strength_training", Limit: 5})
series, err := c.ActivitySeries(ctx, acts[0].ID)
splits, err := series.Splits(1000)
```

It covers activities (list, paging, filters, detail, FIT download, per-second series,
splits, strength breakdown), daily summaries, steps, stress, sleep, heart rate, HRV,
Body Battery, VO2 max and heart-rate zones. Long date ranges are split automatically to
fit Garmin's per-endpoint caps, and HTTP 204 ("no data") is an empty result, not an
error. Garmin publishes no laps or per-set strength data for the Personal API.

## Development

```sh
go vet ./... && go test -race -cover ./...
```

Live checks against your own account are behind the `live` build tag and read the key
from a file named `key` in the repository root (git-ignored):

```sh
go test -tags live -run TestLive -v ./cmd/garmin-mcp   # the server, every tool
go test -tags live -run TestLiveTyped -v .             # the library
```

`testdata/shapes/` holds the structure of real responses with every value blanked.
`testdata/live/` (git-ignored) is where raw captures go; it can contain GPS and health
data and must not be committed.
