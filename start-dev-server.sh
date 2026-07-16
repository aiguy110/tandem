#!/usr/bin/env bash
# Build the native daemon with its embedded UI and start it, per README Quick Start.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

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

./scripts/stage-go-ui.sh

echo "==> Installing external agent and browser runtimes"
(cd runtime && npm install)

echo "==> Building native daemon"
"$go_cmd" build -o tandem ./cmd/tandem

echo "==> Starting native daemon with embedded UI"
exec ./tandem daemon
