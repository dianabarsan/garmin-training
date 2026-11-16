# Deploying the Garmin MCP server

This runs the server on an always-on machine so Claude can reach it from your phone,
whether or not your PC is on. It is two containers:

- `garmin-mcp`: the server (about 13 MB of RAM), running as an unprivileged user with a
  read-only filesystem and no capabilities.
- `caddy`: terminates HTTPS with a Let's Encrypt certificate it obtains and renews by
  itself, and forwards **only `/mcp`** to the server.

```
phone / claude.ai ──https──▶ Caddy ──/mcp only──▶ garmin-mcp ──▶ Garmin
                              │
                              └─ /auth, /, /healthz  →  404 from the internet
```

The Garmin key can therefore not be replaced from the internet, even with a leaked
token. The server's own `/auth` is reachable only on the machine itself.

## What you need

- A small Linux server with a public IP. Any provider and the smallest size are fine;
  the whole stack used about 35 MB of RAM when measured.
- A hostname for it, for example `garmin.example.com`. Let's Encrypt cannot issue
  certificates for a bare IP address.
- Docker with the Compose plugin.
- Your Garmin Personal API key (<https://connect.garmin.com/app/settings/personalApiToken>).

## Steps

**1. DNS.** Create an `A` record (and `AAAA` if the server has IPv6) for your hostname
pointing at the server.

**2. Firewall.** Allow only SSH (ideally key-only), 80 and 443. Port 80 is used by
Let's Encrypt. Nothing else needs to be open: the admin port is bound to loopback.

**3. Code.** Put this repository on the server (`git clone`, or `rsync`).

**4. Secrets.** Run these in `deploy/` on the server:

```sh
mkdir -p secrets && chmod 700 secrets
openssl rand -hex 32 > secrets/mcp_token          # the password Claude will send
read -rs -p 'Garmin API key: ' KEY; echo
printf '%s' "$KEY" > secrets/garmin_key; unset KEY
chmod 444 secrets/*                               # see the note below
cp .env.example .env && $EDITOR .env              # set MCP_DOMAIN
```

The files are mode `444` inside a `700` directory on purpose: the container runs as an
unprivileged user that has to read them, while other users on the host cannot get into
the directory. Do not commit `secrets/` or `.env` (both are git-ignored).

**5. Start.**

```sh
docker compose up -d --build
docker compose ps                  # both services should be running; garmin-mcp "healthy"
docker compose logs garmin-mcp     # look for: API key from the environment accepted
```

**6. Check it from another machine.** The first should be `401`, the second `404`:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://garmin.example.com/mcp
curl -s -o /dev/null -w '%{http_code}\n' https://garmin.example.com/auth
```

**7. Connect Claude.**

- **claude.ai:** Settings, Connectors, add a custom connector. URL
  `https://garmin.example.com/mcp`. Under request headers add
  `Authorization` = `Bearer <contents of secrets/mcp_token>`. Connectors added there
  should then be available in the mobile app too; if it does not appear on your phone,
  check Anthropic's connector documentation for the current behaviour.
- **Claude Code:**
  `claude mcp add --transport http garmin https://garmin.example.com/mcp --header "Authorization: Bearer $(cat secrets/mcp_token)"`

Then ask something like "how did I sleep this week?" or "split my last run per
kilometre".

## Day-to-day

| Task | How |
|---|---|
| Garmin key expired (30 days to a year) | Replace `secrets/garmin_key`, then `docker compose restart garmin-mcp`. |
| Replace the key without touching files | `ssh -L 8808:127.0.0.1:8808 you@server`, then locally: `printf '{"api_key":"%s"}' "$KEY" \| curl -H "Authorization: Bearer $(ssh you@server cat deploy/secrets/mcp_token)" -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth` |
| Rotate the Claude token | New value in `secrets/mcp_token`, `docker compose restart garmin-mcp`, update the connector's header. Do this if the token may have leaked. |
| Update | `git pull && docker compose up -d --build` |
| Logs | `docker compose logs -f` |
| Stop | `docker compose down`. Add `-v` to also delete the stored key and certificates. |

Tool errors tell Claude when the key has expired and what to do, so you will see that
in the conversation.

## Security notes

- **The token is the only credential for `/mcp`.** It is 256 bits of randomness, so it
  cannot be guessed, but anyone who holds it can read your Garmin data. Keep it out of
  chat logs and screenshots, and rotate it if it might have leaked.
- **Data at rest:** the Garmin key is stored in a Docker volume (file mode `600`, owned
  by the container's user) so the server survives restarts without you re-entering it.
  Anyone with root on the server can read it. Use a provider you trust, keep the OS
  updated (for example `unattended-upgrades`), and use SSH keys only.
- **Data in transit to Claude:** whatever a tool returns becomes part of your Claude
  conversation. The tools are read-only and never return GPS coordinates.
- **What the server refuses:** requests for any hostname other than yours, any request
  without the token, and (at Caddy) every path except `/mcp`.
- **Garmin's terms:** the Personal API is for your own data only. Do not share this
  server with other people.

## Try it locally first

You can rehearse the whole stack on your own machine with a throwaway name and ports
that do not clash with anything. Caddy then uses its own local certificate authority,
so use `-k` with curl:

```sh
printf 'MCP_DOMAIN=garmin.localhost\nHTTP_PORT=18080\nHTTPS_PORT=18443\nADMIN_PORT=18808\n' > .env
docker compose up -d --build
curl -sk --resolve garmin.localhost:18443:127.0.0.1 -o /dev/null -w '%{http_code}\n' https://garmin.localhost:18443/auth   # 404
docker compose down -v
```

## Not yet verified

This setup was built and exercised end to end locally (containers, Caddy, the secret
files, the real Garmin API from inside the container, and every check in step 6), but
two things could only be confirmed on a real deployment: Let's Encrypt issuing the
certificate for your domain, and Garmin accepting requests from your hosting provider's
IP range. Neither is expected to be a problem, but check them when you deploy.
