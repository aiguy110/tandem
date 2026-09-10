#!/usr/bin/env bash
# Build the React UI and stage it for the future Go embed package.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui_dir="$repo_root/ui"
stage_dir="$repo_root/internal/ui/dist"

if [[ -z "${VERSION:-}" ]]; then
  # shellcheck source=development-build-info.sh
  source "$repo_root/scripts/development-build-info.sh"
  VERSION="$(cd "$repo_root" && tandem_development_version)"
fi

echo "==> Building React UI"
# This value is used only to tell whether a cached UI belongs to the daemon
# serving it. The displayed version always comes from the daemon at runtime.
(cd "$ui_dir" && npm ci && VITE_TANDEM_VERSION="$VERSION" npm run build)

echo "==> Staging UI at internal/ui/dist"
rm -rf "$stage_dir"
mkdir -p "$stage_dir"
cp -R "$ui_dir/dist/." "$stage_dir/"
