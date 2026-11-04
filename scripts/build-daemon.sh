#!/usr/bin/env bash
# Build ./tandem (React UI embedded) from the current checkout and record a
# stamp so start-dev-server.sh can skip rebuilding an identical source tree.
#
# redeploy.sh runs this *before* asking the live daemon to exit, so the
# ~10s build happens while the old daemon is still serving. The binary is
# built to a temp path and renamed into place, so a running ./tandem keeps
# its old inode untouched.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# shellcheck source=scripts/development-build-info.sh
source ./scripts/development-build-info.sh

stamp_path=./tandem.build-stamp

sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi
}

# Identifies the source tree: HEAD plus any uncommitted or untracked
# (non-ignored) changes. Build outputs are all gitignored, so building does
# not change this value.
source_fingerprint() {
  {
    git rev-parse HEAD
    git describe --tags --abbrev=0 2>/dev/null || true
    git diff HEAD --binary
    git ls-files --others --exclude-standard -z | xargs -0 -r sha256sum 2>/dev/null || true
  } | sha256_stdin
}

binary_sha() {
  [[ -f ./tandem ]] || return 0
  sha256_stdin < ./tandem
}

stamp_fresh() {
  [[ -x ./tandem && -r "$stamp_path" ]] || return 1
  local stamped_source stamped_binary
  IFS=$'\t' read -r stamped_source stamped_binary < "$stamp_path" || return 1
  [[ "$stamped_source" == "$(source_fingerprint)" && "$stamped_binary" == "$(binary_sha)" ]]
}

# Serialize with a concurrent service start or another redeploy.
exec 9>./tandem.build-lock
flock 9

# A release self-update replaces ./tandem and records what source-built version
# it replaced. Keep running that verified release while this checkout remains
# on the same clean revision. Otherwise an unconditional build would overwrite
# the update with the older source on every restart.
version="$(tandem_development_version)"
self_update_marker=./tandem.self-update
if [[ -x ./tandem && -r "$self_update_marker" ]]; then
  IFS=$'\t' read -r installed_version replaced_version installed_sha < "$self_update_marker" || true
  binary_version="$(./tandem version 2>/dev/null | sed -n 's/^tandem version=\([^ ]*\).*/\1/p' || true)"
  source_changes="$(git status --porcelain --untracked-files=normal)"
  if [[ -n "${installed_version:-}" && "$binary_version" == "$installed_version" && "$(binary_sha)" == "$installed_sha" && "$version" == "$replaced_version" && -z "$source_changes" ]]; then
    echo "==> Keeping self-updated Tandem $installed_version; source remains $version"
    exit 0
  fi
  echo "==> Source or binary changed since the self-update; rebuilding from $version"
  rm -f -- "$self_update_marker"
fi

if stamp_fresh; then
  echo "==> Build is up to date with the source tree; skipping rebuild"
  exit 0
fi

build_started_ms="$(date +%s%3N)"
phase_started_ms="$build_started_ms"
phase_done() {
  local now_ms
  now_ms="$(date +%s%3N)"
  echo "==> [timing] $1 took $((now_ms - phase_started_ms))ms (build elapsed $((now_ms - build_started_ms))ms)"
  phase_started_ms="$now_ms"
}

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

fingerprint="$(source_fingerprint)"
commit="$(git rev-parse HEAD)"
build_time="$(git show -s --format=%cI HEAD)"

VERSION="$version" ./scripts/stage-go-ui.sh
phase_done "UI install + build + stage"

echo "==> Building native daemon"
tmp_binary="$(mktemp ./tandem.build.XXXXXX)"
trap 'rm -f -- "$tmp_binary"' EXIT
"$go_cmd" build -ldflags "-X github.com/aiguy110/tandem/internal/buildinfo.Version=$version -X github.com/aiguy110/tandem/internal/buildinfo.Commit=$commit -X github.com/aiguy110/tandem/internal/buildinfo.BuildTime=$build_time" -o "$tmp_binary" ./cmd/tandem
chmod +x "$tmp_binary"
mv -f -- "$tmp_binary" ./tandem
trap - EXIT
printf '%s\t%s\n' "$fingerprint" "$(binary_sha)" > "$stamp_path"
phase_done "go build"
