# garmin-training

Let an AI assistant such as Claude read your Garmin Connect data, so you can ask it about
your training, sleep and recovery in plain language:

- "How did I sleep this week, and how does it compare with last week?"
- "Show my last five runs with pace and heart rate, and my per-km splits for the longest."
- "Has my resting heart rate or HRV changed since I started this training block?"
- "Summarise my strength sessions this month, exercise by exercise."

It is a small server that you run yourself, on your own computer or on a host you
control. It uses the **Garmin Connect Personal API**, which gives you access to your own
data, and speaks the **Model Context Protocol (MCP)**, the standard way AI assistants
connect to outside tools.

## How it works

```
 Claude (or another MCP client)  ──asks──▶  this server  ──fetches──▶  Garmin Connect
                                 ◀─answer─              ◀───────────
```

1. You create a personal API key in your Garmin account and give it to the server.
2. You add the server to your AI assistant as a connector.
3. When you ask a question, the assistant calls one of the server's tools, for example
   "get sleep for these dates". The server fetches that data from Garmin and hands it back.

The server is **read-only**: it cannot change anything in your Garmin account. It keeps
nothing between requests except, if you ask it to, your key. The key itself is never shown
to the assistant, never logged, and never sent anywhere but Garmin.

The Personal API is for one person's own data. Garmin's terms do not allow it in
commercial multi-user applications, so each person runs their own copy with their own key.

## What you need

- **A Garmin Personal API key.** Create one at
  <https://connect.garmin.com/app/settings/personalApiToken>. It is shown once and expires
  after the period you choose (30 days to a year). The server exchanges it for a 24-hour
  access token and renews that automatically, so you only deal with the key itself.
- **Somewhere to run the server** (see below).
- **An MCP client**, such as Claude Code, or claude.ai and the Claude apps through a custom
  connector.

## Choose where to run it

| Option | Good for | Guide |
|---|---|---|
| **Your own computer** | Trying it out, or using it with Claude Code on the same machine. Needs Go, or Docker. | [`docs/running-locally.md`](docs/running-locally.md) |
| **Fly.io** | Using it from claude.ai or your phone, whether or not your computer is on. No domain needed; about $2 a month. | [`docs/fly.md`](docs/fly.md) |
| **Your own server** | The same, on a server you already have, with Docker Compose and automatic HTTPS. | [`deploy/README.md`](deploy/README.md) |

### Quick start on your computer

With [Go](https://go.dev/dl/) installed, from a copy of this repository:

```sh
go run ./cmd/garmin-mcp          # listens on http://127.0.0.1:8808
```

Open <http://127.0.0.1:8808/> and paste your key. Then connect Claude Code:

```sh
claude mcp add --transport http garmin http://127.0.0.1:8808/mcp
```

and ask it something like "how did I sleep this week?". Other MCP clients connect to
`http://127.0.0.1:8808/mcp` (streamable HTTP).

This only listens on your own machine. To keep your key between restarts, run it in the
background, or reach it from claude.ai or your phone, follow one of the guides above.

## What your assistant can see

All tools are read-only. Dates are your local calendar dates (`YYYY-MM-DD`); tools that
take a date range default to the last 7 days ending today and accept up to 366 days.

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

Garmin does not offer laps or individual strength sets through the Personal API, so the
server cannot provide them either.

If a tool is called before a key is provided, it returns an error telling the assistant to
ask you for the key. An assistant cannot provide the key itself.

### Raw data, location and size

Besides the cleaned-up fields above, every item a tool returns carries Garmin's complete,
unmodified record under `raw`: every field Garmin sent, including those the curated fields
leave out (for example an activity's sensors, lap count or running dynamics). Every tool
that returns it takes `include_raw_data` (default `true`); set it to `false` to get only the
cleaned-up fields. `get_activity_splits` and `get_activity_series` have no raw data: they
are computed from the per-second data, which is several megabytes per activity and holds
the full GPS track, so it is not passed through. Two consequences of the default:

- **Location.** Activity records include the start and end coordinates
  (`startLatitude`, `startLongitude`, `endLatitude`, `endLongitude`) and `locationName`. For
  runs that start at home, that is your home address, and it becomes part of the
  conversation.
- **Size.** Outputs are several times larger. Measured on a real account: 7 days of sleep
  about 50 KB, 7 days of daily summaries about 53 KB, 10 activities about 39 KB, and a
  year of sleep about 1.25 MB, which is more than an assistant can take in at once. For long
  ranges, ask for shorter ones or for `include_raw_data: false`.

## Privacy and security

The server exposes your health data and holds your Garmin key, so it is locked down by
default:

- Out of the box it only listens on your own machine and **refuses to start on any other
  address unless you set an access token**, which every client must then send.
- When it is reachable from outside, you can remove the key-entry page altogether
  (`-disable-auth-endpoint`), so even someone holding the token cannot swap in a different
  key. The hosting guides set this up for you.
- The key is never returned by any endpoint or tool, never logged, and never part of an
  error message. Tests check this on every path.
- An **audit log** records every tool call and every refused request (what was asked for,
  by which client, and when), never the data returned. If a token ever leaks, it tells you
  what was read. See [Audit log](#audit-log).

The full details are under [Security model](#security-model).

## Reference

### Providing the key

| How | Command |
|---|---|
| Browser form | open <http://127.0.0.1:8808/> and paste the key |
| HTTP endpoint | `printf '{"api_key":"%s"}' "$KEY" \| curl -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth` |
| Environment | `GARMIN_PERSONAL_API_KEY=... go run ./cmd/garmin-mcp` |

The key stays in memory. To survive restarts, pass `-key-store path` (a file created
with mode `0600` in a `0700` directory); `DELETE /auth` removes it.

### The `/auth` endpoint

| Request | Effect |
|---|---|
| `POST /auth` `{"api_key": "..."}` (JSON, or the form on `/`) | Checks the key with Garmin; if accepted it becomes the active key. A rejected key changes nothing. `401` means Garmin refused it. |
| `GET /auth` | `{"authenticated": bool, "token_expires_at": ..., "persisted": bool}`. Never returns the key. |
| `DELETE /auth` | Forgets the key (and deletes the stored copy, if any). |

### Flags

| Flag / environment | Meaning |
|---|---|
| `-addr` (default `127.0.0.1:8808`) | Address to listen on. |
| `-token` / `GARMIN_MCP_TOKEN` | Require `Authorization: Bearer <token>` on `/mcp` and `/auth`. |
| `-allowed-host` / `GARMIN_MCP_ALLOWED_HOSTS` | Public hostname(s), comma separated, that the server may be reached as. Set it behind a reverse proxy; other `Host` values are refused. |
| `-disable-auth-endpoint` / `GARMIN_MCP_DISABLE_AUTH_ENDPOINT` | Serve no `/auth` or key page (both answer 404), so a leaked token can never replace the key. The key must then come from `GARMIN_PERSONAL_API_KEY[_FILE]` or an existing `-key-store`; the server refuses to start without one. Use it whenever the server is reachable from outside. |
| `-audit-log` / `GARMIN_MCP_AUDIT_LOG` | Append the audit trail (see below) to this file as JSON lines, mode `0600`. Without it the records appear in the normal log, tagged `audit=true`. |
| `-key-store` / `GARMIN_MCP_KEY_STORE` | Persist the key in this file. |
| `GARMIN_PERSONAL_API_KEY` | Key to use at startup (wins over the key store). |
| `GARMIN_MCP_TOKEN_FILE`, `GARMIN_PERSONAL_API_KEY_FILE` | Read the token / key from a file instead (Docker secrets); setting both forms of one is an error. |
| `-healthcheck` | Probe a running server at `-addr` and exit 0 if healthy (container health checks). |
| `-log-level` | `debug`, `info`, `warn`, `error`. |
| `-garmin-url` / `GARMIN_PERSONAL_API_BASE_URL` | Garmin base URL (testing). |

### Security model

By default the server is built for a single user on one machine:

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
  speaks plain HTTP. [`docs/fly.md`](docs/fly.md) and [`deploy/README.md`](deploy/README.md)
  both give you HTTPS.

### Audit log

The server records who asked for what, and who was turned away, so you can tell what was
read if a token ever leaks.

| Event | When | Main fields |
|---|---|---|
| `tool_call` | A tool was called | `tool`, `args`, `outcome` (`ok` or `error`), `error`, `duration_ms`, `result_bytes`, `client`, `session` |
| `client_connected` | A client finished the MCP handshake over HTTP | `client`, `session` |
| `access_denied` | A request was refused | `reason` (`missing_token`, `invalid_token`, `forbidden_host`, `cross_origin`), `method`, `path`, `remote`, `forwarded_for`, `user_agent`, plus the `host` or `origin` that was rejected |
| `key_set`, `key_rejected`, `key_set_failed`, `key_cleared` | The Garmin key was provided, refused, failed to validate, or removed at `/auth` | `remote`, `forwarded_for` |

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

## For developers

The repository has two parts:

- `garmin-connect/` is an importable Go library for the Personal API,
  `github.com/dianabarsan/garmin-training/garmin-connect` (package `garminconnect`), with its
  token handling in `garmin-connect/auth`. It depends on the standard library only.
- `cmd/garmin-mcp/` is the MCP server: the `main` package handles configuration, logging,
  listening and shutdown, and `internal/mcpserver` holds the tools and HTTP routes. It is
  the only place that uses a third-party module (the official MCP Go SDK).

### Using the library

```go
import (
	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
c := garminconnect.New(garminconnect.WithTokenSource(ts))

acts, err := c.Activities(ctx, garminconnect.ActivitiesOptions{SubTypeKey: "strength_training", Limit: 5})
series, err := c.ActivitySeries(ctx, acts[0].ID)
splits, err := series.Splits(1000)
```

It covers activities (list, paging, filters, detail, FIT download, per-second series,
splits, strength breakdown), daily summaries, steps, stress, sleep, heart rate, HRV,
Body Battery, VO2 max and heart-rate zones. Long date ranges are split automatically to
fit Garmin's per-endpoint caps, and HTTP 204 ("no data") is an empty result, not an
error.

### Development

```sh
go vet ./... && go test -race -cover ./...
```

Live checks against your own account are behind the `live` build tag and read the key
from a file named `key` in the repository root (git-ignored):

```sh
go test -tags live -run TestLive -v ./cmd/garmin-mcp        # the server, every tool
go test -tags live -run TestLiveTyped -v ./garmin-connect   # the library
```

`garmin-connect/testdata/shapes/` holds the structure of real responses with every value blanked.
`garmin-connect/testdata/live/` (git-ignored) is where raw captures go; it can contain GPS and health
data and must not be committed.
