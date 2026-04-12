#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Load environment variables
if [ ! -f "$SCRIPT_DIR/.env" ]; then
  echo "Error: $SCRIPT_DIR/.env not found. Copy .env.example and fill in values."
  exit 1
fi
set -a
source "$SCRIPT_DIR/.env"
set +a

if [ -z "${FLY_APP:-}" ]; then
  echo "Error: FLY_APP not set in .env"
  exit 1
fi

# Regions to deploy to (1 machine each)
REGIONS=(iad)

echo "Deploying $FLY_APP to ${REGIONS[*]}..."

# Create app if it doesn't exist
if ! fly apps list --json 2>/dev/null | grep -q "\"$FLY_APP\""; then
  echo "Creating app $FLY_APP..."
  fly apps create "$FLY_APP" --org personal
fi

# Set secrets from .env
fly secrets set --app "$FLY_APP" \
  UPSTREAM_RPC_WS_URL="$UPSTREAM_RPC_WS_URL"

# Deploy from repo root (build context) with the Dockerfile in this dir
fly deploy \
  --app "$FLY_APP" \
  --config "$SCRIPT_DIR/fly.toml" \
  --dockerfile "$SCRIPT_DIR/Dockerfile" \
  --ha=false \
  "$REPO_ROOT"

# Enforce exactly 1 machine per region
for region in "${REGIONS[@]}"; do
  fly scale count 1 --app "$FLY_APP" --region "$region" --yes
done

# Destroy any machines outside allowed regions
ALLOWED_RE=$(IFS='|'; echo "${REGIONS[*]}")
for mid in $(fly machines list --app "$FLY_APP" --json 2>/dev/null | \
  jq -r --arg re "$ALLOWED_RE" '.[] | select(.region | test($re) | not) | .id'); do
  echo "Destroying stray machine $mid..."
  fly machines destroy "$mid" --app "$FLY_APP" --force
done

echo "Done. One instance per region: ${REGIONS[*]}"
