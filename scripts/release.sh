#!/usr/bin/env bash
# Build one reproducible, self-contained Tandem release artifact for GOOS/GOARCH.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="${VERSION:-}"
if [[ -z "$version" ]]; then
  echo "VERSION is required (for example VERSION=0.2.0 scripts/release.sh)" >&2
  exit 2
fi
if [[ ! "$version" =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]]; then
  echo "VERSION contains unsupported characters: $version" >&2
  exit 2
fi

commit="${COMMIT:-$(git rev-parse HEAD)}"
if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
  build_time="${BUILD_TIME:-$(node -e 'process.stdout.write(new Date(Number(process.env.SOURCE_DATE_EPOCH) * 1000).toISOString())')}"
else
  build_time="${BUILD_TIME:-$(git show -s --format=%cI "$commit")}"
fi
goos="${GOOS:-$(go env GOOS)}"
goarch="${GOARCH:-$(go env GOARCH)}"

case "$goos/$goarch" in
  linux/amd64|linux/arm64) ;;
  *)
    echo "unsupported release target $goos/$goarch (supported: linux/amd64, linux/arm64)" >&2
    exit 2
    ;;
esac

if [[ "${RELEASE_SKIP_UI:-0}" != "1" ]]; then
  "$repo_root/scripts/stage-go-ui.sh"
fi
if [[ "${RELEASE_SKIP_TESTS:-0}" != "1" ]]; then
  go test ./...
fi

output_root="${RELEASE_DIR:-$repo_root/release}"
artifact="tandem_${version}_${goos}_${goarch}"
target_dir="$output_root/$artifact"
rm -rf "$target_dir"
mkdir -p "$target_dir"

ldflags="-s -w -buildid= -X github.com/aiguy110/tandem/internal/buildinfo.Version=$version -X github.com/aiguy110/tandem/internal/buildinfo.Commit=$commit -X github.com/aiguy110/tandem/internal/buildinfo.BuildTime=$build_time"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$target_dir/tandem" ./cmd/tandem

if command -v sha256sum >/dev/null 2>&1; then
  binary_sha="$(sha256sum "$target_dir/tandem" | awk '{print $1}')"
else
  binary_sha="$(shasum -a 256 "$target_dir/tandem" | awk '{print $1}')"
fi

ARTIFACT="$artifact" VERSION="$version" COMMIT="$commit" BUILD_TIME="$build_time" GOOS="$goos" GOARCH="$goarch" BINARY_SHA="$binary_sha" \
  node <<'NODE' > "$target_dir/manifest.json"
const manifest = {
  schemaVersion: 1,
  artifact: process.env.ARTIFACT,
  executable: "tandem",
  version: process.env.VERSION,
  commit: process.env.COMMIT,
  buildTime: process.env.BUILD_TIME,
  target: { os: process.env.GOOS, arch: process.env.GOARCH },
  sha256: process.env.BINARY_SHA,
  embedded: ["React UI", "config.yml.example agent catalog"],
  externalDependencies: [
    { name: "Git", requiredWhen: "creating or managing Git worktree workspaces" },
    { name: "agent CLI", requiredWhen: "launching the corresponding configured Claude, Codex, or Pi agent" },
    { name: "Node.js and Node-based ACP adapter", requiredWhen: "a configured ACP agent uses a Node.js adapter" },
    { name: "@playwright/mcp", requiredWhen: "TANDEM_BROWSER_MCP is enabled for ACP browser tools" },
    { name: "Chromium or Chrome", requiredWhen: "TANDEM_BROWSER_DRIVER=local and a browser session is provisioned" },
    { name: "Steel service", requiredWhen: "TANDEM_BROWSER_DRIVER=steel" }
  ]
};
process.stdout.write(JSON.stringify(manifest, null, 2) + "\n");
NODE

(
  cd "$target_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum tandem manifest.json > SHA256SUMS
  else
    shasum -a 256 tandem manifest.json > SHA256SUMS
  fi
)

echo "$target_dir"
