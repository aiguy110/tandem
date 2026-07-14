#!/usr/bin/env bash
# Rebuild and restart the running app. Meant to be invoked from inside the
# app itself (e.g. a terminal pane) after making code changes to it.
#
# The actual rebuild (npm install + vite build for the UI, npm install for
# the daemon) happens in start-dev-server.sh, which is the unit's ExecStart —
# restarting the systemd unit re-runs it.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

echo "==> Restarting tandem.service"
systemctl --user restart tandem.service

echo "==> Restart requested. Tail logs with: journalctl --user -u tandem -f"
