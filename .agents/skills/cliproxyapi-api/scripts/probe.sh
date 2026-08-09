#!/usr/bin/env bash
set -euo pipefail

base_url="${CLIPROXY_BASE_URL:-http://127.0.0.1:8317}"
base_url="${base_url%/}"
api_key="${CLIPROXY_API_KEY:-}"

tmp_body="$(mktemp)"
trap 'rm -f "$tmp_body"' EXIT

probe() {
  local label="$1"
  local path="$2"
  shift 2

  local status
  status="$(curl -sS --connect-timeout 3 --max-time 15 \
    -o "$tmp_body" -w '%{http_code}' "$base_url$path" "$@")"

  printf '%s: HTTP %s\n' "$label" "$status"
  if [[ -s "$tmp_body" ]]; then
    local body_size
    body_size="$(wc -c < "$tmp_body" | tr -d ' ')"
    head -c 4096 "$tmp_body"
    if (( body_size > 4096 )); then
      printf '\n... (truncated, %s bytes total)\n' "$body_size"
    else
      printf '\n'
    fi
  fi
}

probe "health" "/healthz"
probe "readiness" "/readyz"

if [[ -n "$api_key" ]]; then
  probe "models" "/v1/models" -H "Authorization: Bearer $api_key"
else
  printf '%s\n' "models: trying without a key; set CLIPROXY_API_KEY if client authentication is configured"
  probe "models" "/v1/models"
fi
