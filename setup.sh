#!/usr/bin/env bash
#
# One-step setup: builds the server's Docker image, starts it on this machine
# (http://127.0.0.1:8808), stores your Garmin key, and connects Claude Code.
#
#   ./setup.sh               set up, or update to the latest code (keeps token and key)
#   ./setup.sh --new-key     replace the Garmin key (for example after it expired)
#   ./setup.sh --uninstall   remove the container, the stored key, the token and the image
#
# Needs Docker, curl and bash (macOS, Linux, or WSL on Windows).
#
# Environment overrides:
#   GARMIN_PERSONAL_API_KEY  use this key instead of asking for it
#   GARMIN_MCP_PORT          port on 127.0.0.1 to publish (default 8808)
#   GARMIN_MCP_CONTAINER     container name (default garmin-mcp)
#   GARMIN_MCP_VOLUME        volume holding the key (default garmin-mcp-data)

set -euo pipefail

PORT="${GARMIN_MCP_PORT:-8808}"
CONTAINER="${GARMIN_MCP_CONTAINER:-garmin-mcp}"
VOLUME="${GARMIN_MCP_VOLUME:-garmin-mcp-data}"
IMAGE="garmin-mcp:local"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/garmin-mcp"
TOKEN_FILE="$CONFIG_DIR/token"
BASE_URL="http://127.0.0.1:$PORT"
KEY_PAGE="https://connect.garmin.com/app/settings/personalApiToken"

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
interactive() { [ -t 0 ] && [ -r /dev/tty ]; }

# ask QUESTION: yes/no prompt, default yes. Non-interactive runs answer no.
ask() {
	interactive || return 1
	local reply
	printf '%s [Y/n] ' "$1" >/dev/tty
	read -r reply </dev/tty || return 1
	case "$reply" in [nN]*) return 1 ;; *) return 0 ;; esac
}

usage() { sed -n '3,16p' "$0" | sed 's/^# \{0,1\}//'; }

NEW_KEY=0
UNINSTALL=0
for arg in "$@"; do
	case "$arg" in
	--new-key) NEW_KEY=1 ;;
	--uninstall) UNINSTALL=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $arg" ;;
	esac
done

have docker || die "Docker is not installed. Get it from https://docs.docker.com/get-docker/"
docker info >/dev/null 2>&1 || die "Docker is installed but not running (or you lack permission to use it). Start Docker and try again."
have curl || die "curl is not installed."

# Refuse to touch a container with our name that this project did not create.
existing_image="$(docker inspect -f '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)"
case "$existing_image" in
"" | garmin-mcp | garmin-mcp:*) ;;
*) die "a container named '$CONTAINER' already exists and runs '$existing_image', not this project. Remove it or set GARMIN_MCP_CONTAINER to another name." ;;
esac

claude_configured() { have claude && claude mcp get garmin >/dev/null 2>&1; }

if [ "$UNINSTALL" = 1 ]; then
	ask "Remove the '$CONTAINER' container, the stored Garmin key (volume '$VOLUME'), the token and the image?" ||
		die "nothing removed (run in a terminal to confirm)"
	if docker rm -f "$CONTAINER" >/dev/null 2>&1; then say "removed container $CONTAINER"; fi
	if docker volume rm "$VOLUME" >/dev/null 2>&1; then say "removed volume $VOLUME"; fi
	if docker image rm "$IMAGE" >/dev/null 2>&1; then say "removed image $IMAGE"; fi
	rm -f "$TOKEN_FILE"
	rmdir "$CONFIG_DIR" 2>/dev/null || true
	say "removed token $TOKEN_FILE"
	if claude_configured && ask "Also remove the 'garmin' connector from Claude Code?"; then
		claude mcp remove garmin --scope user || claude mcp remove garmin || true
	fi
	say "done. Your API key is still valid at Garmin; revoke it at $KEY_PAGE if you no longer need it."
	exit 0
fi

# Build from the repository this script lives in.
cd "$(dirname "$0")"
[ -f deploy/Dockerfile ] || die "run this script from a copy of the garmin-training repository"

# 1. The access token every client must send. Created once, then reused.
umask 077
mkdir -p "$CONFIG_DIR"
if [ ! -s "$TOKEN_FILE" ]; then
	od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$TOKEN_FILE"
	say "created an access token in $TOKEN_FILE"
fi
TOKEN="$(cat "$TOKEN_FILE")"
[ -n "$TOKEN" ] || die "could not read the token from $TOKEN_FILE"

# 2. Build the image.
version="$(git describe --always --dirty 2>/dev/null || echo dev)"
say "building the image (the first build takes a minute or two)"
docker build -q -f deploy/Dockerfile --build-arg VERSION="$version" -t "$IMAGE" . >/dev/null

# 3. (Re)start the container. The key lives in the volume, so it survives this.
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
say "starting $CONTAINER on $BASE_URL"
# The token is passed by name (-e VAR), so it never appears on a command line.
GARMIN_MCP_TOKEN="$TOKEN" docker run -d --name "$CONTAINER" --restart unless-stopped \
	-p "127.0.0.1:$PORT:8808" -e GARMIN_MCP_TOKEN \
	-v "$VOLUME:/data" \
	--read-only --cap-drop ALL --security-opt no-new-privileges \
	"$IMAGE" >/dev/null

for _ in $(seq 1 60); do
	curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1 && break
	sleep 0.5
done
if ! curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
	docker logs --tail 20 "$CONTAINER" >&2 || true
	die "the server did not come up on $BASE_URL (see the log above)"
fi

# api METHOD PATH [curl args...]: calls the server with the token, which goes
# through a config file descriptor rather than the command line. Prints the
# HTTP status; the body is left in $RESP.
RESP="$(mktemp)"
trap 'rm -f "$RESP"' EXIT
api() {
	local method="$1" path="$2"
	shift 2
	curl -sS -o "$RESP" -w '%{http_code}' -X "$method" \
		-K <(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN") \
		"$BASE_URL$path" "$@"
}

authenticated() {
	[ "$(api GET /auth)" = 200 ] && grep -q '"authenticated":true' "$RESP"
}

# 4. The Garmin key: asked for only when none is stored, or with --new-key.
set_key() {
	local key="$1" code
	key="$(printf '%s' "$key" | tr -d '[:space:]')"
	[ -n "$key" ] || { echo "No key entered." >&2; return 1; }
	key="${key//\\/\\\\}"
	key="${key//\"/\\\"}"
	code="$(printf '{"api_key":"%s"}' "$key" | api POST /auth -H 'Content-Type: application/json' --data-binary @-)"
	case "$code" in
	200) say "Garmin accepted the key; it is stored in the '$VOLUME' volume" ;;
	401)
		if grep -q 'Garmin rejected' "$RESP"; then
			echo "Garmin rejected that key. Create a new one at $KEY_PAGE" >&2
		else
			die "the server refused the access token in $TOKEN_FILE; run ./setup.sh --uninstall, then ./setup.sh"
		fi
		return 1
		;;
	*) echo "Could not set the key (HTTP $code): $(cat "$RESP")" >&2; return 1 ;;
	esac
}

if [ "$NEW_KEY" = 1 ] || ! authenticated; then
	if [ -n "${GARMIN_PERSONAL_API_KEY:-}" ]; then
		set_key "$GARMIN_PERSONAL_API_KEY" || die "the key in GARMIN_PERSONAL_API_KEY was not accepted"
	elif interactive; then
		echo "Paste your Garmin Personal API key (create one at $KEY_PAGE)."
		ok=0
		for _ in 1 2 3; do
			printf 'Garmin API key (input hidden): ' >/dev/tty
			read -rs key </dev/tty || true
			echo >/dev/tty
			if set_key "$key"; then ok=1; break; fi
		done
		unset key
		[ "$ok" = 1 ] || die "no key was accepted; run ./setup.sh again when you have one"
	else
		die "no Garmin key is stored. Run this in a terminal, or set GARMIN_PERSONAL_API_KEY."
	fi
else
	say "a Garmin key is already stored (use --new-key to replace it)"
fi

# 5. Connect Claude Code.
MCP_URL="$BASE_URL/mcp"
add_cmd="claude mcp add --transport http --scope user garmin $MCP_URL --header \"Authorization: Bearer \$(cat $TOKEN_FILE)\""
connected=0
if claude_configured; then
	say "Claude Code already has a 'garmin' connector"
	connected=1
elif have claude && ask "Add the 'garmin' connector to Claude Code (all projects)?"; then
	claude mcp add --transport http --scope user garmin "$MCP_URL" \
		--header "Authorization: Bearer $TOKEN" >/dev/null && connected=1 && say "added the 'garmin' connector to Claude Code"
fi

cat <<EOF

Done. The server runs at $MCP_URL and restarts with Docker.
EOF
if [ "$connected" = 1 ]; then
	echo 'Ask Claude something like "how did I sleep this week?"'
else
	cat <<EOF
To connect Claude Code, run:

  $add_cmd

Other MCP clients: URL $MCP_URL, header "Authorization: Bearer <token>",
with the token from $TOKEN_FILE.
EOF
fi
cat <<EOF

Update to the latest code:  git pull && ./setup.sh
Replace an expired key:     ./setup.sh --new-key
Remove everything:          ./setup.sh --uninstall
EOF
