# Tandem

A browser-based **orchestration layer for terminal coding agents** — a "true copilot"
that lets you use the agents you already know (Claude Code, Gemini CLI, …) but wraps them
in a UI-driven, human-as-conductor control surface. Not an IDE, and not necessarily
coding-focused.

The name *Tandem* captures the product's soul: **human and agent driving together** —
most visibly in the shared, jointly-controlled browser, but throughout the whole
experience.

## What it is

- **Orchestrate multiple agents** from one browser UI, each in its own isolated workspace.
- **Stay in control**: a legible, always-visible approvals queue puts the human in the
  conductor's seat.
- **Joint browser control**: agent (via Playwright/CDP) and user (via a live view) share
  one browser session, with an explicit control-owner token for turn-taking.
- **Survives disconnects by design**: a long-lived daemon owns all state; the browser is a
  pure view that can drop and reconnect without disturbing the agents.

## Quick start

Prerequisites: **Node 22+** for the UI and Node-based agent adapters, **Go 1.24+** for the
native daemon, and `git` on `PATH`. An authenticated agent CLI is required to drive its
corresponding real agent.

```bash
./start-dev-server.sh
```

This builds the UI, embeds it in the Go executable, installs external agent and browser
runtime dependencies, and starts the native daemon on `127.0.0.1:7717`. Equivalent manual
steps:

```bash
./scripts/stage-go-ui.sh
(cd runtime && npm install)                   # external ACP bridges + Playwright MCP
go build -o tandem ./cmd/tandem
./tandem daemon                              # → 127.0.0.1:7717
```

The daemon prints a **bootstrap URL** with an embedded token on first run, e.g.
`http://127.0.0.1:7717/#t=<token>` — open it. The browser reads the token from the URL
fragment once and stores it; later visits to `http://127.0.0.1:7717/` just work. Press `c`
to quick-spawn an agent into a repo under your project roots.

Common configuration:

| Env var | Default | Purpose |
|---|---|---|
| `TANDEM_UI_DIR` | embedded UI | Optional filesystem UI override for frontend development |
| `TANDEM_PROJECT_ROOTS` | `~/Projects` | Directories scanned for repos in the spawn palette |
| `TANDEM_HOME` | `~/.tandem` | Root for `tandem.db`, `token`, and `worktrees/` |
| `TANDEM_PORT` / `TANDEM_BIND` | `7717` / `127.0.0.1` | HTTP + WS listen address |
| `TANDEM_NODE_CMD` | `node` for the native daemon | Node launcher for Node-based ACP adapters |
| `TANDEM_BROWSER_DRIVER` | `local` | `local` (bundled Chromium) or `steel` (needs `STEEL_BASE_URL`; see below) |

**Self-hosting Steel (optional):** for the shared browser you can back agents with
[Steel](https://github.com/steel-dev/steel-browser) instead of local Chromium. Run it with
Docker and point Tandem at it:

```bash
docker run -d --name steel --shm-size=2g -p 3000:3000 -p 9223:9223 \
  -e CHROME_HEADLESS=false -e DISPLAY=:10 \
  --entrypoint /bin/sh ghcr.io/steel-dev/steel-browser:latest \
  -c 'Xvfb :10 -screen 0 1920x1080x24 -nolisten tcp & exec /app/api/entrypoint.sh'
TANDEM_BROWSER_DRIVER=steel STEEL_BASE_URL=http://localhost:3000 ./start-dev-server.sh
```

Setup notes and troubleshooting (bundled-Chromium launch crashes, `CHROME_EXECUTABLE_PATH`)
are in [`docs/browser.md`](docs/browser.md) › **Self-hosting Steel**.

**Remote access:** the daemon binds localhost by default; front it with `tailscale serve`
(TLS + network identity) rather than exposing the port. The bearer token is a second layer.

**Validate the build:** `go test ./...` runs the backend test suite. Build the UI with
`npm run build` from `ui/`.

Deployment and deferred restart are documented in [`docs/deployment.md`](docs/deployment.md).

## Architecture at a glance

```
Browser (React app)
   │  durable WebSocket (reconnect + replay)
   ▼
Self-hosted Daemon  ───────────────────────────────┐
   ├── Session/Agent registry (source of truth)     │
   ├── Agent Supervisor                              │
   │     └── per-agent AgentAdapter                  │
   │           ├── AcpAdapter (structured, primary)  │
   │           └── PtyAdapter (raw pty / user shell) │
   ├── Workspace mgr (git worktree per agent)        │
   ├── Approvals / Interrupt bus                      │
   └── Browser broker                                │
         └── Steel session (CDP) ───────────────────┘
               ├── agent = Playwright/CDP client
               └── user  = CDP screencast + input
                     └── control-owner token
```

## Design invariants

1. **The daemon owns all state.** Everything the browser shows is reconstructable from
   daemon state on reconnect (transcripts, terminal scrollback, browser frames).
2. **Every agent is an `AgentAdapter`** implementing one interface. Structured-first
   (ACP) where available; raw pty as the fallback and the user's escape-hatch shell.
3. **One CDP browser, two clients, one control token.** Agent and human are peers on the
   same browser session; the token arbitrates who is driving.
4. **Workspace isolation by default** — a git worktree (or dir) per agent so parallel
   agents don't clobber each other.

## Locked decisions

| Decision | Choice |
|---|---|
| Deployment topology | Self-hosted daemon (v1) |
| Agent integration | Hybrid, structured-first (ACP primary, pty fallback) |
| Shared browser stack | CDP screencast, Steel-based |
| Browser control | Per-agent Steel; agent via Playwright MCP (lazy); hard-pause token; MCP takeover |
| Orchestration model | Human-as-conductor |
| Home-view layout | Docked rails + focus |
| Shared browser in UI | Pane inside focus mode |
| Terminal engine | ghostty-web (xterm.js fallback) |
| Workspaces | Git worktree + branch per agent (host dirs, no sandbox yet) |
| Spawn UX | Dir-first quick-spawn + command palette |
| Hotkeys | Single-key + chords, rebindable |
| Persistence | SQLite (`~/.tandem/tandem.db`) — registry + event logs |
| WS auth | Localhost bind + bearer token, URL-fragment bootstrap (Tailscale for remote) |

## Docs

- [`docs/architecture.md`](docs/architecture.md) — full architecture and subsystems
- [`docs/decisions.md`](docs/decisions.md) — the design decisions and their rationale
- [`docs/agent-adapter.md`](docs/agent-adapter.md) — the `AgentAdapter` interface + ACP/pty impls
- [`docs/ws-protocol.md`](docs/ws-protocol.md) — browser ↔ daemon WebSocket protocol
- [`docs/ui.md`](docs/ui.md) — UI structure and state model
- [`docs/spawn-and-workspaces.md`](docs/spawn-and-workspaces.md) — spawn mechanics, keymap, worktrees
- [`docs/terminal.md`](docs/terminal.md) — terminal pane (ghostty-web) + spike findings
- [`docs/browser.md`](docs/browser.md) — shared browser (Playwright MCP + Steel, control token)
- [`docs/acp-notes.md`](docs/acp-notes.md) — pinned ACP protocol facts

## Status

**The full v1 build slice is implemented and validated.** All five build-slice steps are
built end-to-end, each proven by an automated de-risk suite. What exists:

- **`docs/`** — the architecture and locked decisions (below).
- **`design/`** — an interactive wireframe of the docked-rails UI.
- **`cmd/` + `internal/`** — the production native daemon: multi-agent registry, **SQLite
  persistence** (`~/.tandem/tandem.db`) with restore-on-restart, the full browser↔daemon
  **WS protocol** with **bearer-token auth**, **git-worktree workspaces**, ACP
  **fs/terminal servicing**, and the **shared-browser broker**. `go test ./...` covers its
  packages and native integration tests cover the full backend.
- **`runtime/`** — locked external ACP bridge and Playwright MCP dependencies. It contains
  no Tandem backend implementation.
- **`ui/`** — the React "mission control" front-end: durable WS client (reconnect + replay),
  docked rails + focus, streaming transcript, always-on approvals rail, quick-spawn +
  command palettes with a rebindable keymap, ghostty-web terminal, and the shared-browser
  pane (screencast + grab/release wheel). `scripts/stage-go-ui.sh` builds it and the Go
  daemon embeds the resulting assets; `TANDEM_UI_DIR` can override them during development.
- **`spike/`** — the original de-risk spikes (terminal + shared browser) the production code
  was ported from.

**Validation.** Go package and integration tests cover durability, multi-agent lifecycle,
restart restore, authentication, worktrees, ACP services, browser control, and WebSocket
replay. The UI is typechecked and built independently.

### Deviations from the specs (recorded during the build)

- **Persistence + auth** were unspecified and are now decided: SQLite (D14) and a
  localhost-bind + bearer-token with URL-fragment bootstrap (D15), fronted by `tailscale
  serve` for remote. See [`docs/decisions.md`](docs/decisions.md).
- **`BrowserDriver` interface** with `SteelDriver` (self-hosted Steel over its REST + CDP API)
  and `LocalChromiumDriver` (Playwright-launched, the
  default). Steel is the intended production driver per D4/D13; the local driver is a drop-in
  behind one interface. See D13's amendment.
- **`takeover_request`** rides the always-on `transcript` channel, not the `browser` channel,
  so the attention rail surfaces it even when the Browser pane is closed.
- The **real `claude-agent-acp`** accepts our advertised `fs`/`terminal` capabilities and MCP
  registrations, but does its own file/command I/O and did not delegate to the client during
  spot checks — so the fs/terminal *servicing* path is exercised by the mock agent in
  `internal/acpadapter/testdata/`.

See [`docs/architecture.md`](docs/architecture.md#v1-build-slice) for the build slice.
