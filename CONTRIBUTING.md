# Contributing

The backend is Go; the React UI is Node-based.
Use Go 1.24+, Node 22+, and Git. Start the complete application with
`./start-dev-server.sh`, which embeds the current UI and runs `./tandem`.

Before submitting backend changes, run `go test ./...`, `go vet ./...`, and the UI build.

Node is required for the React build and for configured ACP bridges and Playwright MCP under
`runtime/`, but backend behavior belongs under `cmd/` and `internal/`. See
[deployment](docs/deployment.md).
