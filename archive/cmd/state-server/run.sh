#!/bin/bash
set -e

IMAGE_NAME="defi-state-server"

echo "Building Docker image..."
docker build -t "$IMAGE_NAME" -f cmd/state-server/Dockerfile .

echo "Starting state server on port 7449..."
docker rm -f state-server 2>/dev/null || true
docker run --rm -d \
  --name state-server \
  --network host \
  -e UPSTREAM_RPC_WS_URL="${UPSTREAM_RPC_WS_URL:-ws://127.0.0.1:9650/ext/bc/C/ws}" \
  "$IMAGE_NAME" "$@"
