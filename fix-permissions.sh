#!/bin/sh
# Prepares the host data directory mounted at /wiki/data so only the user the
# container runs as (PUID:PGID) can write it, its group can read it, and
# nobody else can access it. config.yaml holds password and token hashes, so
# it is restricted to the owner.
set -eu

DATA_DIR="${DATA_DIR:-$PWD/data}"
PUID="${PUID:-$(id -u)}"
PGID="${PGID:-$(id -g)}"
# Optional TLS files mounted into the container, see docker-compose-ssl.yml
SSL_CERT="${SSL_CERT:-}"
SSL_KEY="${SSL_KEY:-}"

case "$PUID:$PGID" in
  *[!0-9:]*) echo "PUID and PGID must be numeric ids" >&2; exit 1 ;;
  0:*) echo "refusing to give the volume to root, set PUID to a regular user" >&2; exit 1 ;;
esac

# Only escalate when the current user cannot do it: the files belong to
# someone else, they go to another user, or to a group we are not part of
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if [ "$PUID" != "$(id -u)" ] \
    || ! id -G | tr ' ' '\n' | grep -qx "$PGID" \
    || [ -n "$(find "$DATA_DIR" $SSL_CERT $SSL_KEY ! -user "$(id -u)" -print 2>/dev/null | head -n 1)" ]; then
    SUDO="sudo"
  fi
fi

$SUDO mkdir -p "$DATA_DIR"
# Resolve through the parent, the directory itself may not be readable by us
DATA_DIR="$(cd "$(dirname "$DATA_DIR")" && pwd -P)/$(basename "$DATA_DIR")"
case "$DATA_DIR" in
  /|//|/.|/..) echo "refusing to change permissions of /" >&2; exit 1 ;;
esac

echo "> setting owner $PUID:$PGID on $DATA_DIR"
$SUDO chown -R "$PUID:$PGID" "$DATA_DIR"

echo "> directories 750, files 640"
$SUDO chmod -R u=rwX,g=rX,o= "$DATA_DIR"

if $SUDO test -f "$DATA_DIR/config.yaml"; then
  echo "> config.yaml 600"
  $SUDO chmod 600 "$DATA_DIR/config.yaml"
fi

if [ -n "$SSL_CERT" ]; then
  echo "> certificate 644"
  $SUDO chown "$PUID:$PGID" "$SSL_CERT"
  $SUDO chmod 644 "$SSL_CERT"
fi

if [ -n "$SSL_KEY" ]; then
  echo "> private key 600"
  $SUDO chown "$PUID:$PGID" "$SSL_KEY"
  $SUDO chmod 600 "$SSL_KEY"
fi
