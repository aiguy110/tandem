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

Back up `TANDEM_HOME` before an operational upgrade. Rollbacks use a previous native release
or Git revision; there is no alternate TypeScript daemon.

Check startup or a deferred restart with `journalctl --user -u tandem -f`. The daemon prints
`TANDEM_READY`, the bound port, and the bootstrap URL after restoration succeeds.
