#!/bin/sh
# Starts the wiki-go container, mounting data and CA certificates from the host.
set -eu

IMAGE="${IMAGE:-wiki-go:latest}"
NAME="${NAME:-wiki-go}"
PORT="${PORT:-8080}"
DATA_DIR="${DATA_DIR:-$PWD/data}"
CA_CERTS="${CA_CERTS:-/etc/ssl/certs/ca-certificates.crt}"

mkdir -p "$DATA_DIR"

set -- -v "$DATA_DIR:/wiki/data"
[ -f "$CA_CERTS" ] && set -- "$@" -v "$CA_CERTS:/etc/ssl/certs/ca-certificates.crt:ro"

exec docker run -d --name "$NAME" --restart unless-stopped \
  --user "$(id -u):$(id -g)" -p "$PORT:8080" "$@" "$IMAGE"
