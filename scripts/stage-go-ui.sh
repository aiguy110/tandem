#!/usr/bin/env bash
# Build the React UI and stage it for the future Go embed package.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui_dir="$repo_root/ui"
stage_dir="$repo_root/internal/ui/dist"

echo "==> Building React UI"
(cd "$ui_dir" && npm ci && npm run build)

echo "==> Staging UI at internal/ui/dist"
rm -rf "$stage_dir"
mkdir -p "$stage_dir"
cp -R "$ui_dir/dist/." "$stage_dir/"
