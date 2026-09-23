#!/usr/bin/env bash
# Build the native daemon with its embedded UI and start it, per README Quick Start.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

# Report how long each build phase takes so slow restarts can be attributed
# from the journal (see `journalctl --user -u tandem`).
launch_started_ms="$(date +%s%3N)"
phase_started_ms="$launch_started_ms"
phase_done() {
  local now_ms
  now_ms="$(date +%s%3N)"
  echo "==> [timing] $1 took $((now_ms - phase_started_ms))ms (launcher elapsed $((now_ms - launch_started_ms))ms)"
  phase_started_ms="$now_ms"
}

# shellcheck source=scripts/development-build-info.sh
source ./scripts/development-build-info.sh
version="$(tandem_development_version)"
commit="$(git rev-parse HEAD)"
build_time="$(git show -s --format=%cI HEAD)"

# A release self-update replaces ./tandem and records what source-built version
# it replaced. Keep running that verified release while this checkout remains
# on the same clean revision. Otherwise this launcher's unconditional build
# would overwrite the update with the older source on every restart.
self_update_marker=./tandem.self-update
if [[ -x ./tandem && -r "$self_update_marker" ]]; then
  IFS=$'\t' read -r installed_version replaced_version installed_sha < "$self_update_marker" || true
  binary_version="$(./tandem version 2>/dev/null | sed -n 's/^tandem version=\([^ ]*\).*/\1/p' || true)"
  if command -v sha256sum >/dev/null 2>&1; then
    binary_sha="$(sha256sum ./tandem 2>/dev/null | awk '{print $1}' || true)"
  else
    binary_sha="$(shasum -a 256 ./tandem 2>/dev/null | awk '{print $1}' || true)"
  fi
  source_changes="$(git status --porcelain --untracked-files=normal)"
  if [[ -n "${installed_version:-}" && "$binary_version" == "$installed_version" && "$binary_sha" == "$installed_sha" && "$version" == "$replaced_version" && -z "$source_changes" ]]; then
    echo "==> Starting self-updated Tandem $installed_version; source remains $version"
    exec ./tandem
  fi
  echo "==> Source or binary changed since the self-update; rebuilding from $version"
  rm -f -- "$self_update_marker"
fi

go_cmd="${TANDEM_GO_CMD:-}"
if [[ -z "$go_cmd" ]]; then
  if command -v go >/dev/null 2>&1; then
    go_cmd="$(command -v go)"
  elif [[ -x /usr/local/go/bin/go ]]; then
    go_cmd=/usr/local/go/bin/go
  else
    echo "Go 1.24+ is required (set TANDEM_GO_CMD if go is not on PATH)." >&2
    exit 1
  fi
fi

phase_done "launcher preflight"

VERSION="$version" ./scripts/stage-go-ui.sh
phase_done "UI install + build + stage"

echo "==> Installing external agent and browser runtimes"
(cd runtime && npm install)
phase_done "runtime npm install"

echo "==> Building native daemon"
"$go_cmd" build -ldflags "-X github.com/aiguy110/tandem/internal/buildinfo.Version=$version -X github.com/aiguy110/tandem/internal/buildinfo.Commit=$commit -X github.com/aiguy110/tandem/internal/buildinfo.BuildTime=$build_time" -o tandem ./cmd/tandem
phase_done "go build"

echo "==> Starting native daemon with embedded UI"
exec ./tandem
