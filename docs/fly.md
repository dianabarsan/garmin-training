# Hosting on Fly.io

This puts the server on a small machine at Fly.io that wakes on demand, so Claude on your phone can
reach it whether or not your computer is on. Fly gives the app an HTTPS address of its own
(`<app-name>.fly.dev`), so you need neither a domain nor Caddy.

The server serves one person with one Garmin key. Its own `/auth`
endpoint is switched off, and the key and token reach it as Fly secrets.

## What has and has not been checked

Checked here, by running the real image the way Fly will (same environment variables, the
same command override, the secrets as `NAME=VALUE` lines): it starts, accepts the key from
the environment, answers health checks whatever `Host` header they carry, returns 404 for
`/auth`, demands the token on `/mcp`, serves real Garmin data, writes nothing to disk, and
stops within 0.2 s of a SIGINT. The `fly.toml` parses as TOML and uses the field names and
values from Fly's documentation.

**Not checked, because it needs your Fly account:** `fly launch`, `fly deploy`, Fly's own
config validation, and everything that happens on Fly's network. Where a step below depends
on something I could not confirm, it says so.

## Cost

Fly's [2026 pricing update](https://fly.io/pricing-update/) lists the smallest machine
(shared CPU, 256 MB, which is what `fly.toml` asks for) at about **$2.19 a month**. **New
accounts are on a trial, and a trial machine is stopped after five minutes.** The log says
`Trial machine stopping. To run for longer than 5m0s, add a credit card by visiting
https://fly.io/trial`. So the app needs a payment card on the account. This is easy
to miss, because the deploy itself succeeds and the app works for those five minutes. Add the
card first (at [fly.io/trial](https://fly.io/trial)), or expect to restart the machine by hand
for each short test.

## 1. Set up the Fly command-line tool

Install `flyctl` ([Fly's instructions](https://fly.io/docs/flyctl/install/)) and log in:

```sh
fly auth login
fly platform regions      # pick the one closest to you
```

## 2. Create the secrets file

From the repository root, where your Garmin key is saved in the file `key`:

```sh
umask 077
cat > fly-secrets.env <<EOF
GARMIN_MCP_TOKEN=$(openssl rand -hex 32)
GARMIN_PERSONAL_API_KEY=$(tr -d '[:space:]' < key)
EOF
grep '^GARMIN_MCP_TOKEN=' fly-secrets.env | cut -d= -f2     # the token Claude will send; keep it
```

The shell fills in the values, so neither appears in your shell history or a process list.
`fly-secrets.env` is git-ignored and excluded from the Docker build.

## 3. Create the app

Choose a name that is unique across Fly, for example `garmin-mcp-yourname`, and write the
app's `fly.toml` in the repository root. The file names your app, so it is git-ignored:
each deployer keeps their own.

```sh
APP=garmin-mcp-yourname
cat > fly.toml <<EOF
app = '$APP'
primary_region = 'ams'

[experimental]
  cmd = ['-addr', ':8808']

[build]
  dockerfile = 'deploy/Dockerfile'

[env]
  GARMIN_MCP_DISABLE_AUTH_ENDPOINT = 'true'

[http_service]
  internal_port = 8808
  force_https = true
  auto_stop_machines = 'suspend'
  auto_start_machines = true
  min_machines_running = 0

  [[http_service.checks]]
    interval = '30s'
    timeout = '5s'
    grace_period = '10s'
    method = 'GET'
    path = '/healthz'

[[vm]]
  size = 'shared-cpu-1x'
  memory = '256mb'
EOF
cp fly.toml fly.toml.orig
fly launch --copy-config --no-deploy --name "$APP" --region ams --ha=false --yes
diff fly.toml.orig fly.toml; rm fly.toml.orig
```

`--ha=false` matters: by default Fly creates two machines, and this server keeps its MCP
sessions in memory, so a second machine would receive requests for sessions it has never
seen. Fly's documentation does not say whether `fly launch` rewrites an existing
`fly.toml`, so read the diff. These settings must still be present:

| Setting | Why |
|---|---|
| `[build] dockerfile = "deploy/Dockerfile"` | builds this project's image |
| `[experimental] cmd = ["-addr", ":8808"]` | keeps the image from writing your key to disk |
| `GARMIN_MCP_DISABLE_AUTH_ENDPOINT = "true"` | removes `/auth` |
| `auto_stop_machines = "suspend"`, `auto_start_machines = true`, `min_machines_running = 0` | sleeps when idle, wakes on the next request (see "Suspend when idle") |
| the check on `/healthz` | lets Fly tell when the server is up |

## 4. Load the secrets and deploy

```sh
fly secrets import --stage -a "$APP" < fly-secrets.env
fly deploy -a "$APP" --ha=false
fly status -a "$APP"
```

`--stage` stores the secrets without deploying; the deploy then starts the machine with
them. (Fly's documentation implies, but does not state, that the first deploy picks up staged
secrets. The next step shows whether it did.) `fly status` should show exactly **one**
machine; if it shows two, run `fly scale count 1 -a "$APP"`.

The deploy uploads the repository to Fly's builder, minus what `.dockerignore` excludes, which
includes your `key` file and `fly-secrets.env`.

## 5. Check that it works

If the account has no payment card, the machine stops after five minutes (see Cost). Add one
first, or start the machine again with `fly machine start <machine-id> -a "$APP"` (the id is in
`fly status`) whenever you want a five-minute test window.

```sh
fly logs -a "$APP" -n | grep -E 'API key|listening'
curl -s  https://$APP.fly.dev/healthz                                         # ok
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://$APP.fly.dev/mcp     # 401: needs the token
curl -s -o /dev/null -w '%{http_code}\n' https://$APP.fly.dev/auth            # 404: the endpoint is gone
```

The log should say `API key from the environment accepted`. If it says the key was not
accepted, or `needs a key from the start`, the secrets did not reach the machine: run
`fly secrets deploy -a "$APP"`.

## 6. Connect Claude

- **claude.ai:** Settings, Connectors, add a custom connector. URL `https://<APP>.fly.dev/mcp`;
  request header `Authorization` = `Bearer <your token>`. Connectors added there should be
  available in the mobile app as well; check Anthropic's connector documentation if one does
  not appear on your phone.
- **Claude Code:**
  `claude mcp add --transport http garmin https://<APP>.fly.dev/mcp --header "Authorization: Bearer <your token>"`

## 7. Remove the local copy of the secrets

```sh
rm fly-secrets.env
```

Put the token in your password manager first: connectors need it, and Fly will not show it
to you again (`fly secrets list` shows names only).

## Day to day

| Task | How |
|---|---|
| Garmin key expired (30 days to a year) | Put the new key in a file as `GARMIN_PERSONAL_API_KEY=...` and run `fly secrets import -a "$APP" < thatfile`. The machine restarts. |
| New token (if it may have leaked) | Same, with a new `GARMIN_MCP_TOKEN=...`; then update the connector's header. |
| Update the server | `git pull && fly deploy -a "$APP" --ha=false` |
| Logs and the audit trail | `fly logs -a "$APP"`; filter with `grep audit=true` (see the audit section of the README) |
| Status | `fly status -a "$APP"` |
| Remove everything | `fly apps destroy "$APP"` |

Tool errors tell Claude when the key has expired and what to do.

## If Claude cannot connect

Look at `fly status` and `fly logs -a "$APP"` first. What they show:

| You see | Meaning |
|---|---|
| Machine `stopped`, `Trial machine stopping`, and proxy errors `machine was recently stopped` | The trial limit (see Cost). Add a card, then `fly machine start <id> -a "$APP"`. |
| Machine `suspended` | Normal when idle (see "Suspend when idle"). The next request wakes it. |
| `access_denied reason=missing_token` from an address in `160.79.104.0/21` | Claude reached the server but sent no `Authorization` header. In the connector, set Authentication to **No sign-in** and add the header (step 6). |
| `access_denied reason=invalid_token` | The header arrived with the wrong value. Check the space after `Bearer`, and re-add the connector, since its settings cannot be edited. |
| Nothing in the logs | The request never reached the app: check the URL and the machine's state. |

## Suspend when idle

`fly.toml` follows Fly's advice for MCP servers: when no requests arrive for a few minutes,
Fly's proxy **suspends** the machine, and the next request wakes it. Suspending saves the
machine's memory, so the server resumes where it left off: open MCP sessions and the
cached Garmin token (valid 24 hours) are still there. While suspended you pay for storage
only, not CPU or memory.

- `min_machines_running` must be `0`. With `1`, the proxy never suspends the only machine.
- The first request after a pause waits for the wake-up. Fly calls resuming faster than a
  cold start; I have not measured it here.
- Fly lists caveats for suspend (for example, memory size limits). At 256 MB this app is
  well inside them. If a machine cannot be suspended, Fly stops it instead, and the next
  request starts it from scratch: clients then open a new session, which MCP clients do on
  their own.
- To keep it always on instead, set `auto_stop_machines = "off"`, `auto_start_machines = false`
  and `min_machines_running = 1`, and redeploy.

## Things to know

- **One machine.** A deploy or secret change restarts it and ends any open MCP session. Clients
  start a new session; I have not checked how claude.ai behaves in that moment.
- **The audit log's `remote` is Fly's proxy**, not the caller. `forwarded_for` carries the
  `X-Forwarded-For` header. In a real deployment it listed the caller's own address first and
  Fly's edge address second, and claude.ai's requests showed an address inside the range
  Anthropic documents for connectors (`160.79.104.0/21`). A client can still add its own entries
  to that header, so treat it as strong evidence, not proof.
- **Streaming and idle timeouts.** I found nothing in Fly's documentation about how long its
  proxy keeps an idle streaming response open. MCP clients normally reconnect; if connections
  drop after a minute or so, that is the place to look.
- **Garmin from Fly's addresses** is untested. I do not expect a problem, but I have only
  called Garmin from a home connection.
- **The health check ignores the `Host` header.** Fly's documentation does not say which `Host` header its
  health checks send, and a failing check takes the app out of service. The server therefore
  never applies its hostname check to `/healthz`, which only ever answers `ok`.
- **The whole server is public.** Unlike the Compose setup, where Caddy exposes only `/mcp`,
  Fly forwards every path. That is fine here: `/auth` does not exist, `/mcp` needs the token,
  and `/healthz` reveals nothing.
