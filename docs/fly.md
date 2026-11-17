# Hosting on Fly.io

This puts the server on a small always-on machine at Fly.io, so Claude on your phone can
reach it whether or not your computer is on. Fly gives the app an HTTPS address of its own
(`<app-name>.fly.dev`), so you need neither a domain nor Caddy.

This is the single-user setup: one person, one Garmin key. The server's own `/auth`
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
(shared CPU, 256 MB, which is what `fly.toml` asks for) at about **$2.19 a month**. I could
not confirm whether new accounts get any free allowance or must add a payment card; check
[Fly's pricing](https://fly.io/docs/about/pricing/) before you start.

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

Choose a name that is unique across Fly, for example `garmin-mcp-yourname`:

```sh
APP=garmin-mcp-yourname
fly launch --copy-config --no-deploy --name "$APP" --region ams --ha=false --yes
git diff fly.toml
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
| `auto_stop_machines = "off"` and `auto_start_machines = false` | stays on around the clock |
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

## Things to know

- **One machine.** A deploy or secret change restarts it and ends any open MCP session. Clients
  start a new session; I have not checked how claude.ai behaves in that moment.
- **The audit log's `remote` is Fly's proxy**, not the caller. `forwarded_for` carries the
  `X-Forwarded-For` header, which I have not verified Fly sets itself, so treat it as a hint.
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

## Several people on one Fly app (advanced, untested)

Multi-user mode (see "Multiple users" in the README) stores its users in a file, so it needs a
volume. In `fly.toml`: remove `GARMIN_MCP_DISABLE_AUTH_ENDPOINT`, add

```toml
[mounts]
  source = "garmin_data"
  destination = "/data"
```

and set `GARMIN_MCP_USERS_FILE=/data/users.json`; create the volume with
`fly volumes create garmin_data --size 1 --region <region> -a "$APP"`; and replace the two
single-user secrets with `GARMIN_MCP_ADMIN_TOKEN` and `GARMIN_MCP_MASTER_KEY` (back the master
key up: without it the stored Garmin keys cannot be read). Caveats, none of which I could test:

- A volume attaches to one machine, so this stays a one-machine app with no failover.
- Fly's documentation does not say who owns a fresh volume. If it is root, the server (which
  runs as an unprivileged user) will fail to write `users.json`; tell me and I will add a root
  variant of the image.
- `/admin/users` is reachable from the internet, protected only by the admin token. Use a long
  random one and watch the audit log. To register people without exposing it you would
  need to run the registration through `fly proxy`, which I have not tried.
- Registering someone means holding their Garmin key; see the notes on Garmin's terms in the
  README.
