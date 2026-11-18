# Running the server on your own machine

Use this to run the server without Docker, for example to work on it, or to use it with
Claude Code on the same computer. It listens only on your machine, so nothing is
reachable from outside. To run it in Docker instead, use [`./setup.sh`](../README.md#local-setup);
to reach it from claude.ai or your phone, deploy it ([`docs/fly.md`](fly.md) or
[`deploy/README.md`](../deploy/README.md)).

You need Go (the version in `go.mod`) and a Garmin Personal API key, which you can
create at <https://connect.garmin.com/app/settings/personalApiToken>.

## 1. Quick start

The server reads the key once, when it starts. Save it to a file with `-set-key`, which
reads it from standard input (so it stays out of your shell history and the process
list), checks it with Garmin, and saves it only if Garmin accepts it:

```sh
printf 'Garmin API key: '; read -rs KEY; echo
printf '%s\n' "$KEY" | go run ./cmd/garmin-mcp -set-key -key-store ~/.config/garmin-mcp/key
unset KEY
go run ./cmd/garmin-mcp -key-store ~/.config/garmin-mcp/key
```

**If your key is already saved in a file**, pass that file to the server directly. In
this repository that file is `key` in the root (it is git-ignored); use the path to
yours:

```sh
GARMIN_PERSONAL_API_KEY_FILE=key go run ./cmd/garmin-mcp
```

Keep the file private (`chmod 600 key`) and never commit it.

Shortly after starting, the server checks the key with Garmin and logs `Garmin accepted
the API key`, or a warning if it did not. It never logs the key.

## 2. Install it as a command

```sh
go build -o ~/.local/bin/garmin-mcp ./cmd/garmin-mcp
garmin-mcp -version
```

`go install` would put the binary in `~/go/bin`, which is often not on your `PATH`.
Run the same `go build` again to update it.

## 3. Where the key comes from

The server needs a key to start. It takes it from, in this order:

1. `GARMIN_PERSONAL_API_KEY`, or `GARMIN_PERSONAL_API_KEY_FILE` naming a file that holds
   it. Do not put the key on the command line, where other users can see it.
2. The `-key-store` file, written by `-set-key`. It is created with mode `600` (a
   directory it creates is `700`); an existing file that others can read is made private
   at startup.

To change the key, save the new one with `-set-key` and restart the server. To forget it,
delete the file.

## 4. Connect Claude Code

```sh
claude mcp add --transport http garmin http://127.0.0.1:8808/mcp
claude mcp list
```

Add `--scope user` to make it available in every project. If you started the server with
a token (`GARMIN_MCP_TOKEN`), send it too:

```sh
claude mcp add --transport http garmin http://127.0.0.1:8808/mcp --header "Authorization: Bearer $TOKEN"
```

Remove it with `claude mcp remove garmin`. Then ask Claude something like "how did I
sleep this week?".

## 5. Keep it running in the background (Linux, systemd)

```sh
go build -o ~/.local/bin/garmin-mcp ./cmd/garmin-mcp
mkdir -p -m 700 ~/.config/garmin-mcp ~/.config/systemd/user

# The key, saved to the file the unit reads (-key-store in its ExecStart).
printf 'Garmin API key: '; read -rs KEY; echo
printf '%s\n' "$KEY" | garmin-mcp -set-key -key-store ~/.config/garmin-mcp/key
unset KEY

cp deploy/garmin-mcp.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now garmin-mcp
```

If the process dies, systemd starts it again. After changing the key with `-set-key`,
restart it.

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

## 6. Stopping and removing

| What | How |
|---|---|
| Stop `go run` or a terminal run | `Ctrl-C` |
| Forget the key | Delete `~/.config/garmin-mcp/key` |
| Remove the command | `rm ~/.local/bin/garmin-mcp` |
| Remove the Claude Code entry | `claude mcp remove garmin` |
| Remove the service | `systemctl --user disable --now garmin-mcp && rm ~/.config/systemd/user/garmin-mcp.service` |

## Troubleshooting

| You see | Cause and fix |
|---|---|
| `no Garmin API key` at startup | Save one with `-set-key`, or set `GARMIN_PERSONAL_API_KEY` (section 3). |
| A tool, or the log, says Garmin rejected the key | It has expired or been deleted. Create a new one, save it with `-set-key`, and restart the server. |
| `401 unauthorized` | A token is required and missing or wrong. Send `Authorization: Bearer <token>`. |
| `403 forbidden host` | You reached the server under a name it does not expect. Use `127.0.0.1` or `localhost`. |
| `address already in use` | Another program has the port. Use another one: `-addr 127.0.0.1:8809`. |
| `refusing to listen ... without a token` | You bound a non-loopback address. Set a token (`GARMIN_MCP_TOKEN`), or use a loopback address. |
| `the token must be at least 16 characters long` | Generate a longer one: `openssl rand -hex 32`. |
| `..._FILE names an empty file` | The secret file is empty, for example because a secret mount failed. Fix the file; the server will not start without its token. |
| Claude Code says the server failed to connect | Check `curl -s http://127.0.0.1:8808/healthz` prints `ok`, and look at the server's log. |
