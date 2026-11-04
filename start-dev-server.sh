#!/usr/bin/env bash
# Build the native daemon with its embedded UI (unless an identical build is
# already in place, e.g. prebuilt by redeploy.sh) and start it.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

# Report how long each launch phase takes so slow restarts can be attributed
# from the journal (see `journalctl --user -u tandem`).
launch_started_ms="$(date +%s%3N)"
phase_started_ms="$launch_started_ms"
phase_done() {
  local now_ms
  now_ms="$(date +%s%3N)"
  echo "==> [timing] $1 took $((now_ms - phase_started_ms))ms (launcher elapsed $((now_ms - launch_started_ms))ms)"
  phase_started_ms="$now_ms"
}

./scripts/build-daemon.sh
phase_done "build (or freshness check)"

echo "==> Installing external agent and browser runtimes"
(cd runtime && npm install)
phase_done "runtime npm install"

echo "==> Starting native daemon with embedded UI"
exec ./tandem
