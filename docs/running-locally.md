# Running the server on your own machine

Use this to try the server, or to use it with Claude Code on the same computer. It
listens only on your machine, so nothing is reachable from outside unless you add a
tunnel (section 7).

It cannot serve your phone while your computer is off. For that, see
[`deploy/README.md`](../deploy/README.md).

You need Go (the version in `go.mod`) and a Garmin Personal API key, which you can
create at <https://connect.garmin.com/app/settings/personalApiToken>. Docker is only
needed for section 6.

## 1. Quick start

```sh
go run ./cmd/garmin-mcp
```

Then give it your key, either in a browser at <http://127.0.0.1:8808/> or from a
terminal (the key goes through stdin so it stays out of your shell history and process
list):

```sh
printf 'Garmin API key: '; read -rs KEY; echo
printf '{"api_key":"%s"}' "$KEY" | curl -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth
unset KEY
```

**If your key is already saved in a file**, send it from there instead of typing it. In
this repository that file is `key` in the root (it is git-ignored); use the path to
yours:

```sh
printf '{"api_key":"%s"}' "$(tr -d '[:space:]' < key)" | curl -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth
```

The shell reads the file, and `printf` is a shell builtin, so the key never appears on a
command line or in the process list. `tr` strips the trailing newline and any stray
whitespace, so the file just needs to hold the key. Keep it private (`chmod 600 key`) and
never commit it. If the path is wrong the server answers `400` (`api_key is required`)
and nothing changes.

Garmin checks the key first. A wrong one is refused (`401`) and changes nothing. Check
the state at any time with `curl -s http://127.0.0.1:8808/auth`; it never shows the key.

## 2. Install it as a command

```sh
go build -o ~/.local/bin/garmin-mcp ./cmd/garmin-mcp
garmin-mcp -version
```

`go install` would put the binary in `~/go/bin`, which is often not on your `PATH`.
Run the same `go build` again to update it.

## 3. Remember the key between restarts

By default the key lives in memory only. To keep it, give the server a file:

```sh
garmin-mcp -key-store ~/.config/garmin-mcp/key
```

The file is written with mode `600` (a directory the server creates is `700`); an existing
file that others can read is made private at startup. It is loaded at startup without
contacting Garmin. `DELETE /auth` (see below) removes it.

Another way is to pass the key at startup through the `GARMIN_PERSONAL_API_KEY`
environment variable. That wins over the stored one. Do not put the key on the command
line, where other users can see it.

```sh
curl -X DELETE http://127.0.0.1:8808/auth      # forget the key (and delete the file)
```

## 4. Connect Claude Code

```sh
claude mcp add --transport http garmin http://127.0.0.1:8808/mcp
claude mcp list
```

Add `--scope user` to make it available in every project. If you started the server with
a token (`-token` or `GARMIN_MCP_TOKEN`), send it too:

```sh
claude mcp add --transport http garmin http://127.0.0.1:8808/mcp --header "Authorization: Bearer $TOKEN"
```

Remove it with `claude mcp remove garmin`. Then ask Claude something like "how did I
sleep this week?".

## 5. Keep it running in the background (Linux, systemd)

```sh
go build -o ~/.local/bin/garmin-mcp ./cmd/garmin-mcp
mkdir -p -m 700 ~/.config/garmin-mcp ~/.config/systemd/user
cp deploy/garmin-mcp.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now garmin-mcp
```

Then provide your key once as in section 1. The unit stores it with `-key-store`, so it
survives restarts. If the process dies, systemd starts it again.

| Task | Command |
|---|---|
| Status | `systemctl --user status garmin-mcp` |
| Logs | `journalctl --user -u garmin-mcp -f` |
| After rebuilding the binary | `systemctl --user restart garmin-mcp` |
| Stop and disable | `systemctl --user disable --now garmin-mcp` |
| Run without being logged in / at boot | `loginctl enable-linger $USER` |

Extra settings (for example `GARMIN_MCP_TOKEN=...`) go in `~/.config/garmin-mcp/env`, one
`KEY=value` per line; keep that file private (`chmod 600`). The unit is sandboxed: the
service can read its binary and write only `~/.config/garmin-mcp`. The state directory
must exist before the service starts, which is why the command above creates it.

To keep a separate audit file (who called which tool, and every refused request; see the
README), add `-audit-log %h/.config/garmin-mcp/audit.log` to `ExecStart` in the unit. That
path is inside the one directory the sandbox lets the service write to. Without it the
records appear in the journal, tagged `audit=true`:

```sh
journalctl --user -u garmin-mcp | grep audit=true
```

On macOS this unit does not apply; run the binary in a terminal, or write a launchd
agent (not covered or tested here).

## 6. Run it in Docker instead

No Go toolchain needed on the machine that runs it. The container listens on all of its
own interfaces, so the server insists on a token. Publish the port on loopback only:

```sh
docker build -f deploy/Dockerfile -t garmin-mcp .
export TOKEN=$(openssl rand -hex 32)
docker run -d --name garmin-mcp --restart unless-stopped \
  -p 127.0.0.1:8808:8808 -e GARMIN_MCP_TOKEN="$TOKEN" \
  -v garmin-mcp-data:/data \
  --read-only --cap-drop ALL --security-opt no-new-privileges garmin-mcp

printf 'Garmin API key: '; read -rs KEY; echo
printf '{"api_key":"%s"}' "$KEY" | curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth
unset KEY
```

Or, if the key is in a file (see section 1):

```sh
printf '{"api_key":"%s"}' "$(tr -d '[:space:]' < key)" | curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @- http://127.0.0.1:8808/auth
```

The key is kept in the `garmin-mcp-data` volume, so a new container with the same volume
still has it. Connect Claude Code with
`--header "Authorization: Bearer $TOKEN"` as in section 4. The token is visible to
anyone who can run `docker inspect` on your machine; for a server other people can reach,
use the Compose setup in `deploy/`, which reads it from a file instead.

Logs, including the audit trail (tagged `audit=true`), are in the container log:

```sh
docker logs garmin-mcp 2>&1 | grep audit=true
```

They are cleared when the container is removed. To keep the audit trail across
containers, add `-e GARMIN_MCP_AUDIT_LOG=/data/audit.log` to `docker run` so it is written
to the volume, or use `./setup.sh`, which does this for you.

To remove everything: `docker rm -f garmin-mcp && docker volume rm garmin-mcp-data`.

## 7. Trying claude.ai or your phone against your own machine

claude.ai calls connectors from Anthropic's cloud, so it needs a public HTTPS address
that leads to your machine. Pointing it at your home IP does not work well (no
certificate for a bare IP, an address that may change or sit behind your ISP's shared
address, and an open port on your home network), so use a tunnel, which connects out from
your machine and gives you an HTTPS name. Cloudflare Tunnel and Tailscale Funnel both do
this; neither was available on the machine this guide was written on, so **this section
has not been tested end to end**. Check the tool's own documentation for its command.

The safe way to do it is in two steps, so the tunnel never exposes the endpoint that can
replace your key:

```sh
# 1. Store your key once, with the endpoint on (no tunnel yet), then stop the server.
garmin-mcp -key-store ~/.config/garmin-mcp/key        # provide the key as in section 1

# 2. Restart with the endpoint removed, a token, and the tunnel's public hostname.
export GARMIN_MCP_TOKEN=$(openssl rand -hex 32)
garmin-mcp -key-store ~/.config/garmin-mcp/key \
  -disable-auth-endpoint -allowed-host your-tunnel-hostname.example
```

With `-disable-auth-endpoint`, `/auth` and the key page answer 404 even to someone holding
the token, so a leaked token can read your data but can never replace your key. The key
then has to come from the key store (as above) or from `GARMIN_PERSONAL_API_KEY`
(or `GARMIN_PERSONAL_API_KEY_FILE`); the server refuses to start with the flag and no key.
To change the key later, stop the server, restart it without the flag, provide the new
key, and restart with the flag again.

Then point the tunnel at `http://127.0.0.1:8808`, and in claude.ai add a custom connector
with URL `https://your-tunnel-hostname.example/mcp` and the request header
`Authorization: Bearer <your token>`.

Things to know:

- It works only while your computer, the server and the tunnel are all running. Stop the
  tunnel when you are done testing.
- Without `-allowed-host` the server answers `403 forbidden host` to tunneled requests,
  because the public hostname is not one it expects.
- If you start the tunnel without `-disable-auth-endpoint`, the whole server is exposed,
  including `/auth`, protected only by the token.

## 8. Stopping and removing

| What | How |
|---|---|
| Stop `go run` or a terminal run | `Ctrl-C` |
| Forget the key | `curl -X DELETE http://127.0.0.1:8808/auth`, or delete `~/.config/garmin-mcp/key` |
| Remove the command | `rm ~/.local/bin/garmin-mcp` |
| Remove the Claude Code entry | `claude mcp remove garmin` |
| Remove the service | `systemctl --user disable --now garmin-mcp && rm ~/.config/systemd/user/garmin-mcp.service` |

## Troubleshooting

| You see | Cause and fix |
|---|---|
| A tool says no Garmin API key has been provided | Provide the key (section 1). |
| A tool says Garmin rejected the key | It has expired or been deleted. Create a new one and provide it. |
| `401 unauthorized` | A token is required and missing or wrong. Send `Authorization: Bearer <token>`. |
| `403 forbidden host` | You reached the server under a name it does not expect. Use `127.0.0.1` or `localhost`, or set `-allowed-host`. |
| `address already in use` | Another program has the port. Use another one: `-addr 127.0.0.1:8809`. |
| `refusing to listen ... without a token` | You bound a non-loopback address. Set a token (`GARMIN_MCP_TOKEN`), or use a loopback address. |
| `the token must be at least 16 characters long` | Generate a longer one: `openssl rand -hex 32`. |
| `..._FILE names an empty file` | The secret file is empty, for example because a secret mount failed. Fix the file; the server will not start without its token. |
| `-disable-auth-endpoint needs a key from the start` | With the endpoint removed the key must come from `GARMIN_PERSONAL_API_KEY[_FILE]` or an existing `-key-store` file. Store it first (section 7). |
| `404` on `/auth` or `/` | The server was started with `-disable-auth-endpoint`, by design. |
| Claude Code says the server failed to connect | Check `curl -s http://127.0.0.1:8808/healthz` prints `ok`, and look at the server's log. |
