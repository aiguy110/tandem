#!/usr/bin/env bash
# Copy Tandem's markdown specs into the Obsidian vault as a Sync-safe snapshot.
# (Symlinks don't propagate through Obsidian Sync; copies do.)
#
# Usage:   ./sync-to-vault.sh
# Override destination with: TANDEM_VAULT_DIR=/path/to/vault/Folder ./sync-to-vault.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="${TANDEM_VAULT_DIR:-$HOME/Documents/my-vault/Tech/Tandem}"

mkdir -p "$DEST/docs" "$DEST/daemon"
cp "$REPO/README.md"        "$DEST/README.md"
cp "$REPO"/docs/*.md        "$DEST/docs/"
cp "$REPO/daemon/README.md" "$DEST/daemon/README.md"

echo "Synced Tandem specs → $DEST"
ls -1 "$DEST"/*.md "$DEST"/docs/*.md "$DEST"/daemon/*.md | sed "s#$DEST/#  #"
