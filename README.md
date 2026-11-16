# garmin-training

A Go client for the **Garmin Connect Personal API** (your own data, pull-based, no
developer-program approval), and an **MCP server** that lets AI agents read it.

- `github.com/dianabarsan/garmin-training` is an importable library. The root
  package depends on the standard library only.
- `cmd/garmin-mcp` is the MCP server. It is the only place that uses a third-party
  module (the official MCP Go SDK).

The Personal API is for one person's own data. Garmin's terms do not allow it in
commercial multi-user applications.

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
[`docs/running-locally.md`](docs/running-locally.md).

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
| `-key-store` / `GARMIN_MCP_KEY_STORE` | Persist the key in this file. |
| `GARMIN_PERSONAL_API_KEY` | Key to use at startup (wins over the key store). |
| `GARMIN_MCP_TOKEN_FILE`, `GARMIN_PERSONAL_API_KEY_FILE` | Read the token / key from a file instead (Docker secrets); setting both forms of one is an error. |
| `-healthcheck` | Probe a running server at `-addr` and exit 0 if healthy (container health checks). |
| `-log-level` | `debug`, `info`, `warn`, `error`. |
| `-garmin-url` / `GARMIN_PERSONAL_API_BASE_URL` | Garmin base URL (testing). |

### Security model

This server exposes your health data and accepts your Garmin key, so by default it is
built for a single user on one machine:

- It binds to **loopback** and rejects requests whose `Host` header is not
  `localhost`, `127.0.0.1` or `::1` (DNS-rebinding defence).
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
