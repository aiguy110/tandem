# Deployment

Production runs the native Go daemon under the user unit in `deploy/tandem.service`.
`start-dev-server.sh` is the unit entrypoint: it builds and stages the React UI, installs
the Node-based ACP and Playwright adapters, builds `./tandem`, and replaces itself with
`./tandem`. The resulting process serves the embedded UI; `TANDEM_UI_DIR` remains an
optional development override.

Install the unit with `deploy/install.sh`. After changing Tandem, run:

```bash
./redeploy.sh
```

The script reloads the unit and asks the authenticated daemon to shut down after active
turns finish. `Restart=always` then starts the new native build. If the daemon is unavailable
or too old for deferred shutdown, the script requests an immediate systemd restart. SIGINT,
SIGTERM, clean deferred exit, bind/port configuration, `TANDEM_HOME`, browser settings, agent
launcher settings, and all other inherited environment variables retain their existing
semantics.

## Login-shell environment

A supervisor such as systemd starts the daemon with a bare environment, so the PATH entries
and exports in the operator's shell rc files would otherwise never reach agents. Unless it was
started from a terminal (which already passes that environment down), the daemon runs `$SHELL`
(falling back to `/etc/passwd`) once at startup as a login, interactive shell, captures the
environment that shell ends up with, and merges it into its own before anything is spawned:

- Variables the daemon already has keep their values, so `Environment=` lines in the unit and
  every `TANDEM_*` setting win. `TANDEM_*` and shell bookkeeping (`PWD`, `SHLVL`, `TERM`, …)
  are never imported.
- `PATH` is merged: the shell's entries first, then any daemon entries the shell lacks. The
  unit's `Environment=PATH=` is therefore the floor agents get if resolution fails.
- Everything else the shell exports is imported, including credentials such as
  `GITHUB_TOKEN`; agents see what a terminal would. The log records variable names only.

While resolving, the shell sees `TANDEM_RESOLVING_ENVIRONMENT=1` (and VS Code's equivalent
`VSCODE_RESOLVING_ENVIRONMENT=1`) so rc files can skip slow or session-hijacking setup:
`[[ -n $TANDEM_RESOLVING_ENVIRONMENT ]] || exec tmux`. Resolution times out after
`TANDEM_SHELL_ENV_TIMEOUT` (default `10s`); on failure the daemon logs a warning and continues
with its own environment. Set `TANDEM_SHELL_ENV=off` to disable it, or `force` to resolve even
from a terminal. Changes to rc files take effect at the next daemon restart. Look for
`imported login shell environment` in `journalctl --user -u tandem` to see what was added.

Back up `TANDEM_HOME` before an operational upgrade. Rollbacks use a previous native release
or Git revision; there is no alternate TypeScript daemon.

Check startup or a deferred restart with `journalctl --user -u tandem -f`. The daemon prints
`TANDEM_READY`, the bound port, and the bootstrap URL after restoration succeeds.

Release builds check GitHub for a newer release when the daemon starts and every five
minutes thereafter. An available release appears as a daemon-owned notification in every
connected UI. Installing uses the same verified, atomic replacement path as `tandem update`.
After replacement, the running daemon asks the new binary whether `settings.configVersion`
needs review. Compatible updates offer a deferred restart; configuration changes instead
offer to start the configured default ACP agent in `$TANDEM_HOME`. Unversioned development
builds (`dev`) and installations with `TANDEM_NO_UPDATE_CHECK` set do not make update-check
requests. A source build stamped from its nearest release, such as `v0.8.0.f1817c0c`, checks
for and can install newer releases.
