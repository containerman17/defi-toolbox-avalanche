#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

LIMIT=10
SKIP_FILE=""
STATE_SERVER_HOST="${STATE_SERVER_HOST:-localhost:7449}"
SUMMARY_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--limit)
      LIMIT="${2:?missing value for $1}"
      shift 2
      ;;
    --skip-file)
      SKIP_FILE="${2:?missing value for $1}"
      shift 2
      ;;
    --summary-only)
      SUMMARY_ONLY=1
      shift
      ;;
    *)
      echo "usage: $0 [-n LIMIT] [--skip-file FILE] [--summary-only]" >&2
      exit 1
      ;;
  esac
done

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

declare -A SKIP=()
if [[ -n "$SKIP_FILE" && -f "$SKIP_FILE" ]]; then
  while IFS= read -r line; do
    hash="$(awk '{print tolower($1)}' <<<"$line")"
    [[ -n "$hash" ]] && SKIP["$hash"]=1
  done < "$SKIP_FILE"
fi

parse_field() {
  local pattern="$1"
  local key="$2"
  local text="$3"
  awk -v pat="$pattern" -v key="$key" '
    index($0, pat) > 0 {
      for (i = 1; i <= NF; i++) {
        if ($i ~ ("^" key "=")) {
          sub("^" key "=", "", $i)
          print $i
          exit
        }
      }
    }
  ' <<<"$text"
}

parse_skip_reason() {
  local text="$1"
  awk '
    index($0, "[arb4] analyze: comparison skipped") > 0 {
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^reason=/) {
          sub("^reason=", "", $i)
          print $i
          exit
        }
      }
    }
  ' <<<"$text"
}

format_skip_verdict() {
  local reason="$1"
  tr '[:lower:]-' '[:upper:]_' <<<"$reason"
}

classify() {
  local exact="$1"
  local candidate="$2"
  node -e '
    const exact = BigInt(process.argv[1]);
    const candidate = BigInt(process.argv[2]);
    if (candidate > exact) {
      console.log("WIN");
    } else if (candidate < exact) {
      console.log("LOSS");
    } else {
      console.log("EQUAL");
    }
  ' "$exact" "$candidate"
}

processed=0

while IFS=$'\t' read -r block tx _; do
  [[ -z "${tx:-}" ]] && continue

  if [[ -z "${block:-}" || "$block" =~ ^0x[0-9a-fA-F]+$ ]]; then
    tx="${block:-$tx}"
    block="-"
  fi

  tx_lc="$(tr '[:upper:]' '[:lower:]' <<<"$tx")"
  if [[ -n "${SKIP[$tx_lc]:-}" ]]; then
    continue
  fi

  echo "===== $block $tx ====="
  output="$(timeout 120s go run ./experiments/arb4 --analyze-tx "$tx" 2>&1 || true)"
  if (( SUMMARY_ONLY == 0 )); then
    printf '%s\n' "$output"
  fi

  exact_net="$(parse_field "[arb4] analyze: tx exact out=" "net" "$output")"
  skip_reason="$(parse_skip_reason "$output")"
  compare_net="$(parse_field "[arb4] analyze: best@sized verified out=" "compare_net" "$output")"
  if [[ -z "$compare_net" ]]; then
    compare_net="$(parse_field "[arb4] analyze: best@tx verified out=" "compare_net" "$output")"
  fi
  if [[ -z "$compare_net" ]]; then
    compare_net="$(parse_field "[arb4] analyze: best@sized verified out=" "net" "$output")"
  fi
  if [[ -z "$compare_net" ]]; then
    compare_net="$(parse_field "[arb4] analyze: best@tx verified out=" "net" "$output")"
  fi

  if [[ -n "$skip_reason" ]]; then
    echo "SUMMARY block=$block tx=$tx verdict=$(format_skip_verdict "$skip_reason")"
  elif [[ -n "$exact_net" && -n "$compare_net" ]]; then
    verdict="$(classify "$exact_net" "$compare_net")"
    echo "SUMMARY block=$block tx=$tx verdict=$verdict exact_net=$exact_net candidate_net=$compare_net"
  else
    echo "SUMMARY block=$block tx=$tx verdict=UNKNOWN"
  fi
  if (( SUMMARY_ONLY == 0 )); then
    echo
  fi

  processed=$((processed + 1))
  if (( processed >= LIMIT )); then
    break
  fi
done
