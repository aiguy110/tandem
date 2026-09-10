#!/usr/bin/env bash
# Helpers shared by local build entrypoints. This file is sourced, not run.

tandem_development_version() {
  local tag commit
  tag="$(git describe --tags --abbrev=0)"
  if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "latest tag must be a vX.Y.Z release tag (got $tag)" >&2
    return 1
  fi
  commit="$(git rev-parse --short=8 HEAD)"
  printf '%s.%s\n' "$tag" "$commit"
}
