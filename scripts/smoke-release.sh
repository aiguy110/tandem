#!/usr/bin/env bash
# Exercise a native release artifact outside the source checkout.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/smoke-release.sh /path/to/tandem" >&2
  exit 2
fi

binary="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
test -x "$binary"
work="$(mktemp -d)"
home="$work/home"
log="$work/daemon.log"
mkdir -p "$home/projects" "$work/run"
pid=""

cleanup() {
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

(
  cd "$work/run"
  exec env TANDEM_HOME="$home" \
  TANDEM_BIND=127.0.0.1 \
  TANDEM_PORT=0 \
  TANDEM_PROJECT_ROOTS="$home/projects" \
  TANDEM_UI_DIR= \
  TANDEM_BROWSER_MCP=off \
    "$binary" >"$log" 2>&1
) &
pid=$!

ready=""
for _ in $(seq 1 100); do
  ready="$(grep -m1 '^TANDEM_READY ' "$log" 2>/dev/null || true)"
  [[ -n "$ready" ]] && break
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$log" >&2
    echo "release daemon exited before TANDEM_READY" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ -z "$ready" ]]; then
  cat "$log" >&2
  echo "timed out waiting for TANDEM_READY" >&2
  exit 1
fi

port="$(sed -n 's/^TANDEM_READY port=\([0-9][0-9]*\).*/\1/p' <<<"$ready")"
token="$(sed -n 's/^TANDEM_READY .* token=\([^[:space:]]*\).*/\1/p' <<<"$ready")"
[[ -n "$port" && -n "$token" ]]

index="$(curl --fail --silent --show-error "http://127.0.0.1:$port/?token=$token")"
grep -q '<div id="root"></div>' <<<"$index"
(
  cd "$work/run"
  TANDEM_HOME="$home" TANDEM_UI_DIR= TANDEM_BROWSER_MCP=off "$binary" debug config >/dev/null
)

kill -TERM "$pid"
for _ in $(seq 1 100); do
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.1
done
if kill -0 "$pid" 2>/dev/null; then
  echo "release daemon did not shut down cleanly" >&2
  exit 1
fi
wait "$pid"
pid=""

echo "release smoke passed: $ready"
