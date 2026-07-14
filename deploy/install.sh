#!/usr/bin/env bash
# Install the tandem systemd --user unit and enable it to start on boot.
# Requires `loginctl enable-linger $USER` for the unit to run without a login
# session (check with `loginctl show-user $USER | grep Linger`).
set -euo pipefail

unit_src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/tandem.service"
unit_dst="$HOME/.config/systemd/user/tandem.service"

mkdir -p "$(dirname "$unit_dst")"
ln -sf "$unit_src" "$unit_dst"

systemctl --user daemon-reload
systemctl --user enable --now tandem.service

echo "==> tandem.service installed and started"
echo "    status: systemctl --user status tandem.service"
echo "    logs:   journalctl --user -u tandem -f"
