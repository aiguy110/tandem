#!/usr/bin/env bash
# Rebuild and restart the running app. Meant to be invoked from inside the
# app itself (e.g. a terminal pane) after making code changes to it.
#
# The actual rebuild (React UI staging, Node-based adapter dependencies, and
# the native Go binary) happens in start-dev-server.sh, which is the unit's
# ExecStart. Restarting the systemd unit embeds the fresh UI and runs Go.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

token_path="${TANDEM_HOME:-$HOME/.tandem}/token"
host="${TANDEM_BIND:-127.0.0.1}"
port="${TANDEM_PORT:-7717}"
[[ "$host" == "0.0.0.0" ]] && host="127.0.0.1"

# Pick up Restart=always (and any future unit changes) before asking the daemon
# to exit cleanly, otherwise systemd may still be using its previous policy.
systemctl --user daemon-reload

if [[ -r "$token_path" ]]; then
  token="$(<"$token_path")"
  if response="$(curl --fail --silent --show-error --max-time 5 \
    --request POST "http://${host}:${port}/internal/shutdown-after-turns?token=${token}")"; then
    read -r active_turns already_requested <<<"$response"
    if [[ "$already_requested" == "1" ]]; then
      echo "==> Deferred restart was already requested."
    else
      echo "==> Deferred restart requested."
    fi
    echo "==> ${active_turns} agent(s) currently mid-turn; tandem.service will automatically restart once all agents are finished."
    echo "==> If you are an AI agent, the count above likely includes you. You must first end your turn for the restart to proceed."
    exit 0
  fi
fi

# Compatibility path for an older daemon that does not expose the deferred
# shutdown endpoint yet, or for a service that is not currently reachable.
echo "==> Daemon unavailable or does not support deferred restart; restarting tandem.service now."
systemctl --user restart tandem.service

echo "==> Restart requested. Tail logs with: journalctl --user -u tandem -f"
