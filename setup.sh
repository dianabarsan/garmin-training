#!/usr/bin/env bash
#
# One-step setup: builds the server's Docker image, stores your Garmin key,
# starts the server on this machine (http://127.0.0.1:8808), and connects
# Claude Code.
#
#   ./setup.sh               set up, or update to the latest code (keeps token and key)
#   ./setup.sh --new-key     replace the Garmin key (for example after it expired)
#   ./setup.sh --audit-log   print the audit log (tool calls and refused requests, as JSON lines)
#   ./setup.sh --uninstall   remove the container, the stored key and audit log, the token and the image
#
# Needs Docker, curl and bash (macOS, Linux, or WSL on Windows).
# If a run is interrupted, run it again: the token and the stored key are kept.
#
# Environment overrides:
#   GARMIN_PERSONAL_API_KEY  use this key instead of asking for it
#   GARMIN_MCP_PORT          port on 127.0.0.1 to publish (default 8808)
#   GARMIN_MCP_CONTAINER     container name (default garmin-mcp)
#   GARMIN_MCP_VOLUME        volume holding the key and audit log (default garmin-mcp-data)

set -euo pipefail

PORT="${GARMIN_MCP_PORT:-8808}"
CONTAINER="${GARMIN_MCP_CONTAINER:-garmin-mcp}"
PREVIOUS="$CONTAINER-previous" # the old container, kept until the new one is healthy
VOLUME="${GARMIN_MCP_VOLUME:-garmin-mcp-data}"
IMAGE="garmin-mcp:local"
LABEL_KEY="garmin-training.setup" # marks containers this script created
LABEL="$LABEL_KEY=1"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/garmin-mcp"
TOKEN_FILE="$CONFIG_DIR/token"
BASE_URL="http://127.0.0.1:$PORT"
MCP_URL="$BASE_URL/mcp"
KEY_FILE="/data/garmin.key"  # inside the volume; the image's default -key-store
AUDIT_FILE="/data/audit.log" # next to the key
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

# usage: prints the comment block at the top of this file.
usage() { awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0"; }

MODE=setup
for arg in "$@"; do
	case "$arg" in
	--new-key | --audit-log | --uninstall)
		[ "$MODE" = setup ] || die "use only one of --new-key, --audit-log and --uninstall"
		MODE="${arg#--}"
		;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $arg" ;;
	esac
done

case "$PORT" in
'' | *[!0-9]*) die "GARMIN_MCP_PORT must be a port number, got '$PORT'" ;;
esac
{ [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ]; } || die "GARMIN_MCP_PORT must be between 1 and 65535, got $PORT"

have docker || die "Docker is not installed. Get it from https://docs.docker.com/get-docker/"
docker info >/dev/null 2>&1 || die "Docker is installed but not running (or you lack permission to use it). Start Docker and try again."
have curl || die "curl is not installed."

# --type container: a plain `docker inspect NAME` also matches an image of that name.
exists() { docker inspect --type container "$1" >/dev/null 2>&1; }

# owned NAME: the container was created by this script, either now (label) or by
# an earlier version of it (which ran exactly $IMAGE, a tag nothing else uses).
owned() {
	local image label
	image="$(docker inspect --type container -f '{{.Config.Image}}' "$1" 2>/dev/null)" || return 1
	label="$(docker inspect --type container -f "{{index .Config.Labels \"$LABEL_KEY\"}}" "$1" 2>/dev/null)" || return 1
	[ "$label" = 1 ] || [ "$image" = "$IMAGE" ]
}

# other_installs: true if any container from this script still exists. Older,
# unlabelled ones are found by the image name they were created from, which
# survives rebuilds of the tag (unlike Docker's ancestor filter).
other_installs() {
	local id
	[ -z "$(docker ps -aq --filter "label=$LABEL")" ] || return 0
	for id in $(docker ps -aq); do
		[ "$(docker inspect --type container -f '{{.Config.Image}}' "$id" 2>/dev/null)" != "$IMAGE" ] || return 0
	done
	return 1
}

if exists "$CONTAINER" && ! owned "$CONTAINER" && [ "$MODE" != audit-log ]; then
	die "a container named '$CONTAINER' exists that this script did not create (image '$(docker inspect --type container -f '{{.Config.Image}}' "$CONTAINER")').
If it is a container you started yourself, remove it with 'docker rm -f $CONTAINER' and run this script again; if it used the '$VOLUME' volume, the key stored there is reused.
Otherwise set GARMIN_MCP_CONTAINER and GARMIN_MCP_VOLUME to other names."
fi

# The connector in Claude Code, if any. Its details include the token, so they
# stay in this variable: never printed, never passed to another program.
connector=""
# A missing connector makes `claude mcp get` fail; only a successful lookup counts.
if have claude && out="$(claude mcp get garmin 2>/dev/null)"; then connector="$out"; fi
unset out
connector_scope() { printf '%s\n' "$connector" | sed -n 's/.*claude mcp remove garmin -s \([a-z]*\).*/\1/p'; }
remove_connector() {
	local scope
	scope="$(connector_scope)"
	claude mcp remove garmin ${scope:+-s "$scope"} >/dev/null
}

if [ "$MODE" = audit-log ]; then
	exists "$CONTAINER" || die "no '$CONTAINER' container; run ./setup.sh first"
	# The image has no shell, so the files are read with `docker cp`, which streams
	# a tar archive. The server rotates the log to audit.log.1; that one is older.
	for f in "$AUDIT_FILE.1" "$AUDIT_FILE"; do
		docker cp "$CONTAINER:$f" - 2>/dev/null | tar -xO 2>/dev/null || true
	done | awk '{ print } END { if (NR == 0) print "No audit records yet." > "/dev/stderr" }'
	exit 0
fi

if [ "$MODE" = uninstall ]; then
	ask "Remove the '$CONTAINER' container, the stored Garmin key and audit log (volume '$VOLUME'), the token and the image?" ||
		die "nothing removed (run in a terminal to confirm)"
	for name in "$CONTAINER" "$PREVIOUS"; do
		if exists "$name" && owned "$name"; then docker rm -f "$name" >/dev/null && say "removed container $name"; fi
	done
	if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
		if docker volume rm "$VOLUME" >/dev/null 2>&1; then
			say "removed volume $VOLUME"
		else
			say "kept volume $VOLUME: another container still uses it"
		fi
	fi
	# A second install (under another GARMIN_MCP_CONTAINER) shares the image and the token.
	if other_installs; then
		say "kept image $IMAGE and token $TOKEN_FILE: another install still uses them"
	else
		if docker image inspect "$IMAGE" >/dev/null 2>&1 && docker image rm "$IMAGE" >/dev/null 2>&1; then
			say "removed image $IMAGE"
		fi
		if [ -e "$TOKEN_FILE" ]; then rm -f "$TOKEN_FILE" && say "removed token $TOKEN_FILE"; fi
		rmdir "$CONFIG_DIR" 2>/dev/null || true
	fi
	if [ -n "$connector" ] && ask "Also remove the 'garmin' connector from Claude Code?"; then
		remove_connector && say "removed the 'garmin' connector"
	fi
	say "done. Your API key is still valid at Garmin; revoke it at $KEY_PAGE if you no longer need it."
	exit 0
fi

# Build from the repository this script lives in, following symlinks to it.
script="$0"
hops=0
while [ -L "$script" ]; do
	hops=$((hops + 1))
	[ "$hops" -le 40 ] || die "too many symbolic links leading to $0"
	link="$(readlink "$script")"
	case "$link" in /*) script="$link" ;; *) script="$(dirname "$script")/$link" ;; esac
done
cd "$(dirname "$script")"
[ -f deploy/Dockerfile ] || die "run this script from a copy of the garmin-training repository"

# 1. The access token every client must send. Created once, then reused.
umask 077
mkdir -p "$CONFIG_DIR"
if [ ! -s "$TOKEN_FILE" ]; then
	od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$TOKEN_FILE"
	say "created an access token in $TOKEN_FILE"
fi
TOKEN="$(cat "$TOKEN_FILE")" || die "cannot read the token in $TOKEN_FILE"
[ -n "$TOKEN" ] || die "the token file $TOKEN_FILE is empty"

# 2. The port must be free, or already ours.
port_in_use() {
	local rc=0
	curl -s --noproxy '*' -o /dev/null --max-time 2 "$BASE_URL/" || rc=$?
	[ "$rc" -ne 7 ] # 7: connection refused, so nothing listens there
}
ours_on_port() {
	local ports
	ports="$(docker port "$CONTAINER" 8808/tcp 2>/dev/null || true)"
	printf '%s\n' "$ports" | grep -q ":$PORT\$"
}
if port_in_use && ! ours_on_port; then
	die "port $PORT on 127.0.0.1 is already in use by another program. Stop it, or pick another port: GARMIN_MCP_PORT=8809 ./setup.sh"
fi

# 3. Build the image. The running server is untouched if this fails.
version="$(git describe --always --dirty 2>/dev/null || echo dev)"
say "building the image (the first build takes a minute or two)"
docker build -q -f deploy/Dockerfile --build-arg VERSION="$version" -t "$IMAGE" . >/dev/null

# 4. The Garmin key: asked for only when none is stored, or with --new-key. It
#    is checked and saved by a one-off run of the image with the volume, so it
#    never travels over HTTP and is never on a command line.
keytool() {
	docker run --rm -i -v "$VOLUME:/data" --read-only --cap-drop ALL --security-opt no-new-privileges \
		"$IMAGE" -key-store "$KEY_FILE" "$@"
}
key_stored() { keytool -key-stored </dev/null >/dev/null 2>&1; }
set_key() {
	local key
	key="$(printf '%s' "$1" | tr -d '[:space:]')"
	[ -n "$key" ] || { echo "No key entered." >&2; return 1; }
	# Garmin keys use letters, digits and ._~+/=- only. Anything else is a paste error.
	case "$key" in
	*[!A-Za-z0-9._~+/=-]*)
		echo "That does not look like a Garmin API key (unexpected characters). Paste the whole key exactly as Garmin showed it." >&2
		return 1
		;;
	esac
	# On failure the tool says why (for example that Garmin rejected the key).
	printf '%s\n' "$key" | keytool -set-key >/dev/null || return 1
	say "Garmin accepted the key; it is stored in the '$VOLUME' volume"
}

if [ "$MODE" = new-key ] || ! key_stored; then
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

# 5. Replace the container. The old one is stopped and kept aside until the new
#    one is healthy, and restored if it is not. It reads the key at startup.
if exists "$PREVIOUS"; then # left over from an interrupted run
	owned "$PREVIOUS" || die "a container named '$PREVIOUS' exists that this script did not create; remove or rename it"
	docker rm -f "$PREVIOUS" >/dev/null
fi
had_old=0
if exists "$CONTAINER"; then
	docker stop "$CONTAINER" >/dev/null
	docker rename "$CONTAINER" "$PREVIOUS"
	had_old=1
fi

start_new() {
	# The token is passed by name (-e VAR), so it is not on docker's command line.
	# Like any container environment, it is visible to `docker inspect`.
	GARMIN_MCP_TOKEN="$TOKEN" docker run -d --name "$CONTAINER" --restart unless-stopped \
		-p "127.0.0.1:$PORT:8808" -e GARMIN_MCP_TOKEN \
		-e GARMIN_MCP_AUDIT_LOG="$AUDIT_FILE" --label "$LABEL" \
		-v "$VOLUME:/data" \
		--read-only --cap-drop ALL --security-opt no-new-privileges \
		"$IMAGE" >/dev/null
}
healthy() {
	for _ in $(seq 1 60); do
		curl -fsS --noproxy '*' "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
		sleep 0.5
	done
	return 1
}

say "starting $CONTAINER on $BASE_URL"
if ! start_new || ! healthy; then
	if exists "$CONTAINER"; then docker logs --tail 20 "$CONTAINER" >&2 || true; fi
	docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
	if [ "$had_old" = 1 ]; then
		if ! docker rename "$PREVIOUS" "$CONTAINER"; then
			echo "The previous server is kept, stopped, as '$PREVIOUS'. Run ./setup.sh again, or restore it with: docker rename $PREVIOUS $CONTAINER && docker start $CONTAINER" >&2
		elif ! docker start "$CONTAINER" >/dev/null; then
			echo "The previous server could not be started again; try: docker start $CONTAINER" >&2
		else
			say "restored the previous container"
		fi
	fi
	die "the new server did not start on $BASE_URL (see the log above)"
fi
if [ "$had_old" = 1 ]; then docker rm -f "$PREVIOUS" >/dev/null; fi

# 6. Connect Claude Code. `claude mcp add` takes the header as an argument, so
#    the token is briefly on its command line; Claude Code stores it in its own
#    configuration either way.
add_connector() {
	claude mcp add --transport http --scope user garmin "$MCP_URL" \
		--header "Authorization: Bearer $TOKEN" >/dev/null
}
add_cmd="claude mcp add --transport http --scope user garmin $MCP_URL --header \"Authorization: Bearer \$(cat \"$TOKEN_FILE\")\""
connected=0
if [ -n "$connector" ]; then
	# Pattern matching is done by the shell, so the token is not passed to grep.
	case "$connector" in
	*"URL: $MCP_URL"*"Bearer $TOKEN"*)
		say "Claude Code's 'garmin' connector is up to date"
		connected=1
		;;
	*)
		say "Claude Code has a 'garmin' connector with a different URL or token"
		removed=0
		if ask "Replace it with one for this server?" && remove_connector; then
			removed=1
			add_connector && connected=1 && say "replaced the 'garmin' connector"
		fi
		# While the old connector exists, `claude mcp add` would fail: remove it first.
		[ "$removed" = 1 ] || add_cmd="claude mcp remove garmin $(connector_scope | sed 's/^/-s /') && $add_cmd"
		;;
	esac
elif have claude && ask "Add the 'garmin' connector to Claude Code (all projects)?"; then
	add_connector && connected=1 && say "added the 'garmin' connector to Claude Code"
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
View the audit log:         ./setup.sh --audit-log
Remove everything:          ./setup.sh --uninstall
EOF
