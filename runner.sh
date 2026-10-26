#!/bin/sh
# Starts the wiki-go container on a Debian host, mounting data and CA
# certificates from the host. The container runs as the wikigo service user,
# created when missing, without capabilities and with a read-only root
# filesystem; only /wiki/data is writable.
set -eu

IMAGE="${IMAGE:-wiki-go:latest}"
NAME="${NAME:-wiki-go}"
PORT="${PORT:-8081}"
DATA_DIR="${DATA_DIR:-/srv/data/wiki/data}"
CA_CERTS="${CA_CERTS:-/etc/ssl/certs/ca-certificates.crt}"
SERVICE_USER="${SERVICE_USER:-wikigo}"
SERVICE_GROUP="${SERVICE_GROUP:-wikigo}"

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo"

# System account without home or login shell, used only to own the data
if ! getent group "$SERVICE_GROUP" >/dev/null; then
  echo "> creating group $SERVICE_GROUP"
  $SUDO groupadd --system "$SERVICE_GROUP"
fi
if ! getent passwd "$SERVICE_USER" >/dev/null; then
  echo "> creating user $SERVICE_USER"
  $SUDO useradd --system --gid "$SERVICE_GROUP" --no-create-home \
    --home-dir /nonexistent --shell /usr/sbin/nologin "$SERVICE_USER"
fi

PUID="$(id -u "$SERVICE_USER")"
PGID="$(getent group "$SERVICE_GROUP" | cut -d: -f3)"

DATA_DIR="$DATA_DIR" PUID="$PUID" PGID="$PGID" "$(dirname "$0")/fix-permissions.sh"

set -- -v "$DATA_DIR:/wiki/data"
[ -f "$CA_CERTS" ] && set -- "$@" -v "$CA_CERTS:/etc/ssl/certs/ca-certificates.crt:ro"

exec docker run --restart unless-stopped -d --name "$NAME" \
  --user "$PUID:$PGID" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  -p "$PORT:8080" "$@" "$IMAGE"
