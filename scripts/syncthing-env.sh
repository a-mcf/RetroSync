#!/bin/sh
# Bring up (or tear down) a throwaway syncthing mesh for the -tags syncthing
# end-to-end tests. Three instances on a private podman network:
#
#   st-server   the machine RetroSync runs alongside. Its /shares directory is
#               bind-mounted into the Go test container too, so the tests can
#               read and write the share replicas exactly as RetroSync does.
#   st-device-a a peer device (think: a handheld)
#   st-device-b a second peer device
#
# The instances are started BARE — no folders, no pairing. Tests wire the
# topology they need over the REST API, so each test can build its own scenario
# without fighting a shared fixture.
#
# Every instance is fully isolated from the internet: global discovery, local
# discovery, relaying, NAT traversal, usage reporting and auto-upgrade are all
# switched off, and devices address each other by container name on a private
# network. A test mesh must never announce itself to the public relay pool.
#
# Usage: scripts/syncthing-env.sh up | down
set -eu

ST_IMAGE="${ST_IMAGE:-docker.io/syncthing/syncthing:2}"
NET="${ST_NET:-retrosync-st-net}"

# Deliberately OUTSIDE the repo. The repo is bind-mounted into the Go test
# container with a private SELinux label (:Z); nesting the mesh's trees inside
# it would have that relabel fight with the per-instance mounts below, which
# surfaces as "permission denied" writing the syncthing config and as SELinux
# denials in the host audit log. Everything here is mounted :z (shared label)
# for the same reason: these trees are used by several containers at once.
ROOT="${ST_ROOT:-/tmp/retrosync-syncthing-test}"

# Instance name : published GUI port : API key. The API keys are fixed so tests
# need no discovery step; these instances are unreachable from outside the host
# and are destroyed after every run.
INSTANCES="st-server:18384:serverkey st-device-a:18385:deviceakey st-device-b:18386:devicebkey"

down() {
	for spec in $INSTANCES; do
		name=$(echo "$spec" | cut -d: -f1)
		podman rm -f "retrosync-$name" >/dev/null 2>&1 || true
	done
	podman network rm -f "$NET" >/dev/null 2>&1 || true
	# The share trees are written by a container running as root; with rootless
	# podman that maps to the invoking user, but a plain rm can still trip over
	# modes the container chose. Clear it inside a userns to be safe.
	if [ -d "$ROOT" ]; then
		podman unshare rm -rf "$ROOT" >/dev/null 2>&1 || rm -rf "$ROOT"
	fi
}

wait_ready() {
	name="$1"
	port="$2"
	key="$3"
	i=0
	while [ "$i" -lt 90 ]; do
		if curl -sf -H "X-API-Key: $key" "http://127.0.0.1:$port/rest/system/status" >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "syncthing instance $name did not become ready" >&2
	podman logs "retrosync-$name" >&2 2>&1 || true
	return 1
}

# isolate turns off every outbound-announcing feature. Done over REST after
# startup because these are not all settable by environment variable.
isolate() {
	port="$1"
	key="$2"
	curl -sf -X PATCH -H "X-API-Key: $key" \
		-d '{"globalAnnounceEnabled":false,"localAnnounceEnabled":false,"relaysEnabled":false,"natEnabled":false,"urAccepted":-1,"autoUpgradeIntervalH":0,"crashReportingEnabled":false}' \
		"http://127.0.0.1:$port/rest/config/options" >/dev/null
}

up() {
	down
	mkdir -p "$ROOT"
	podman network create "$NET" >/dev/null

	for spec in $INSTANCES; do
		name=$(echo "$spec" | cut -d: -f1)
		port=$(echo "$spec" | cut -d: -f2)
		key=$(echo "$spec" | cut -d: -f3)

		mkdir -p "$ROOT/$name/config" "$ROOT/$name/data"
		# The container runs as root (PUID/PGID 0); under rootless podman that
		# maps back to the invoking user on the host. 777 keeps the Go test
		# container — which runs as a different uid — able to read and write the
		# same trees. See the userns note in docs.
		chmod -R 777 "$ROOT/$name"

		podman run -d --name "retrosync-$name" \
			--network "$NET" \
			--network-alias "$name" \
			-e STGUIADDRESS=0.0.0.0:8384 \
			-e STGUIAPIKEY="$key" \
			-e STNOUPGRADE=1 \
			-e PUID=0 -e PGID=0 \
			-p "127.0.0.1:$port:8384" \
			-v "$ROOT/$name/config:/var/syncthing/config:z" \
			-v "$ROOT/$name/data:/data:z" \
			"$ST_IMAGE" >/dev/null
	done

	for spec in $INSTANCES; do
		name=$(echo "$spec" | cut -d: -f1)
		port=$(echo "$spec" | cut -d: -f2)
		key=$(echo "$spec" | cut -d: -f3)
		wait_ready "$name" "$port" "$key"
		isolate "$port" "$key"
	done

	echo ">> syncthing test mesh up ($ROOT)"
}

case "${1:-}" in
up) up ;;
down) down ;;
*)
	echo "usage: $0 up|down" >&2
	exit 2
	;;
esac
