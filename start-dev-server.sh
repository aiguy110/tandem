#!/usr/bin/env bash
# Build the UI and start the daemon serving it, per README Quick Start.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

echo "==> Building UI"
(cd ui && npm install && npm run build)

echo "==> Starting daemon"
cd daemon
npm install
TANDEM_UI_DIR=../ui/dist exec npm run daemon
