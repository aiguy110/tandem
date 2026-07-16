# Contributing

The production daemon is Go; the React UI and black-box migration harness remain Node-based.
Use Go 1.24+, Node 22+, and Git. Start the complete application with
`./start-dev-server.sh`, which embeds the current UI and runs `./tandem daemon`.

Before submitting daemon changes, run `go test ./...`, `go vet ./...`, and the UI build.
After building `./tandem`, run the cross-runtime gate from `daemon/` with
`TANDEM_GO_DAEMON_CMD='["../tandem","daemon"]' npm run derisk:matrix`; focused `derisk:*`
scripts are useful while iterating. The matrix uses temporary Tandem homes and runs every
standard suite against both Go and the rollback Node daemon.

Keep persisted schema changes additive while rollback support exists. Never start the two
implementations against the same `TANDEM_HOME` concurrently. Node is still required for
configured Node-based ACP adapters and Playwright MCP, but new daemon behavior belongs under
`cmd/` and `internal/`, not `daemon/src/`. See [deployment and rollback](docs/deployment.md)
and the [de-risk ownership map](docs/derisk-subsystems.md).
