# Legacy Node daemon and compatibility harness

The TypeScript implementation remains available for immediate rollback while the native Go
daemon under `cmd/` and `internal/` is the production default. This directory also owns the
black-box de-risk harness used to keep both implementations compatible.

## What's real now

- **Multi-agent registry** (`src/registry.ts`) — owns many `AgentSession`s keyed by
  agentId, auto-names them (`web-1`, `api-2`, …), and implements the full `SpawnSpec`
  (`adapter: 'acp'|'pty'`, workspace `worktree|existing`, optional `task` dispatched as the
  first prompt).
- **Workspace manager** (D8, `src/workspace.ts`) — `workspace.kind:'worktree'` provisions a
  real `git worktree` (branch `tandem/<agent>`, based on `baseRef`/current HEAD) under
  `$TANDEM_HOME/worktrees/<repo>/<agent>/`; `kind:'existing'` validates the dir and refuses a
  collision with a live agent already occupying it. Close removes the checkout but **keeps
  the branch**; uncommitted changes block the close unless `force`. A missing worktree dir on
  restore is recreated from its (never-deleted) branch — see `WorkspaceManager.reattach`.
  Respawning with an existing `tandem/<name>` branch re-checks it out instead of erroring.
  Repo discovery for the spawn palette (`listRepos`) scans `TANDEM_PROJECT_ROOTS`.
- **Full WS protocol** (`src/server.ts`, [`../docs/ws-protocol.md`](../docs/ws-protocol.md))
  — `subscribe`/`unsubscribe` (per-agent `channels` + `sinceSeq`), `prompt`, `input`,
  `resize`, `permission_response`, `interrupt`, `spawn_agent`, `close_agent`, `list_dirs`.
  Multiple concurrent clients; multiplexed multi-agent subscriptions on one socket.
- **SQLite persistence** (D14, `src/db.ts`) — `better-sqlite3` at `$TANDEM_HOME/tandem.db`
  (WAL). `agents` (incl. the resolved `cwd`) + `events` tables; the event log writes through
  to SQLite while keeping the in-memory ring for hot replay, and falls back to SQLite for
  older ranges.
- **Restore on restart** (D11, `registry.restoreAll`) — non-closed agents are re-spawned
  (workspace reattached first); agents advertising `loadSession` resume via `session/load`
  with the persisted `sessionId`, else start fresh (history still served from the event
  log). A single agent's restore failure marks it `error` — it never crashes the daemon.
- **Auth + HTTP on one port** (D15) — first run writes a random bearer token to
  `$TANDEM_HOME/token` (mode `0600`) and prints a bootstrap URL. WS connections must present
  `?token=<t>` or are closed `4401`. Static UI served from `TANDEM_UI_DIR` if set, else a
  placeholder page.
- **`ClientServices` — daemon as full ACP client** (Phase 3,
  [`../docs/agent-adapter.md`](../docs/agent-adapter.md)). The ACP adapter now advertises
  `fs: { readTextFile: true, writeTextFile: true }, terminal: true` and services the agent's
  requests:
  - **WorkspaceFs** (`src/workspaceFs.ts`) — the **sandbox choke point** for
    `fs/read_text_file` (with optional `line`/`limit`) and `fs/write_text_file`. Paths must
    resolve inside the agent's workspace cwd (realpath containment; `../` and symlink escapes
    rejected with JSON-RPC `-32602`, no disk touched).
  - **TerminalHost** (`src/terminalHost.ts`) — daemon-owned execution for `terminal/create`
    / `output` / `wait_for_exit` / `kill` / `release`. Prefers `node-pty`, degrades to
    `child_process` pipes (or `TANDEM_NO_PTY=1`). Dual buffer: the ACP `output` view honors
    the per-terminal `outputByteLimit` (truncate-from-start, UTF-8-safe, `truncated:true`),
    while the daemon keeps a **larger independent scrollback**. Buffers **persist past
    `terminal/release`**; every chunk is emitted as a normalized `terminal_output` event on
    the `terminals` channel (persisted + replayable like everything else).
- **Interrupt / cancellation** — `interrupt` → `AgentSession.interrupt()` → ACP
  `session/cancel`: pending permission requests resolve `cancelled`, unfinished tool calls
  are marked `cancelled` in the transcript, and the in-flight `session/prompt` resolves with
  stopReason `cancelled` without wedging the adapter (follow-up prompts work).

- **Shared browser** (Phase 5, D13, [`../docs/browser.md`](../docs/browser.md)) —
  `src/browser/`: a `BrowserDriver` seam (`LocalChromiumDriver` real + tested;
  `SteelDriver` specced, untested — no Docker here) behind a **broker** that serves a stable
  per-agent CDP URL and provisions **lazily on the first CDP connection** (no Chromium
  until first browser use). The control-owner token is enforced **at the CDP proxy**: while
  the user holds the wheel, the agent's CDP frames are queued (held, not errored) and flush
  on release. Playwright MCP + a Tandem-control MCP (`browser_request_takeover`, blocks
  until release) are registered in ACP `mcpServers` at `session/new`
  (`TANDEM_BROWSER_MCP=off` to disable). Screencast frames + control state stream over the
  WS `browser` channel; `browser_input` forwards user input; browsers tear down with
  `close_agent`.

## What's still stubbed (post-Phase 5)

- **`merge_back`** — accepted but returns an **error `ack`**; no diff review / merge-or-PR UI
  yet (the WorkspaceManager it needs — branches, base refs — is now real).
- **Binary framing** for `raw_pty` and `browser_frame` — still base64-in-JSON (noted as a
  future optimization).

### Real-vs-stubbed at a glance

| Capability | Status | Notes |
|---|---|---|
| ACP transcript (`session/update` → normalized events) | **real** | message/thought/tool_call/plan |
| ACP permissions (`session/request_permission`) | **real** | → approvals queue → response |
| `fs/read_text_file` / `fs/write_text_file` | **real** | WorkspaceFs sandbox, `line`/`limit`, escape → `-32602` |
| `terminal/create·output·wait_for_exit·kill·release` | **real** | TerminalHost, node-pty↔child_process, dual buffer |
| `session/cancel` cancellation semantics | **real** | perms + tool calls cancelled; stopReason `cancelled` |
| `session/load` restore | **real** | resumes when the agent advertises `loadSession` |
| Real agent drives fs/terminal | **n/a** | claude-agent-acp 0.59.0 never issues `fs/*`·`terminal/*` (does own I/O); mock exercises the path |
| Shared browser: lazy broker + CDP-proxy hard-pause gate | **real** | derisk:browser a–h |
| `browser_control` / `browser_input` / screencast over WS | **real** | browser channel, focus rule |
| Playwright MCP + Tandem-control MCP at `session/new` | **real** | `TANDEM_BROWSER_MCP=off` to disable |
| `SteelDriver` | **specced** | untested (no Docker); LocalChromiumDriver is the tested path |
| `merge_back` | **stub** | error ack; awaits diff/merge UI |
| `raw_pty` / `browser_frame` binary framing | **stub** | base64-in-JSON for now |

## Run the rollback implementation

Stop the Go process first; the implementations must never write the same `TANDEM_HOME`
concurrently. See [`docs/deployment.md`](../docs/deployment.md) for the complete switch.

```bash
cd daemon
npm install               # builds better-sqlite3 (native)
npm run daemon            # start the daemon (127.0.0.1:7717); prints the bootstrap URL

# Tests / de-risk (each exits non-zero on failure; all use a temp TANDEM_HOME):
npm test                  # = derisk:all — runs all seven below in sequence
npm run derisk            # durability + ACP approval loop (single agent)
npm run derisk:multi      # two agents: independent seq streams, queues, isolated teardown
npm run derisk:restart    # persist → stop process → restart → restore from SQLite + session/load
npm run derisk:auth       # no/bad token → 4401; valid token → connects
npm run derisk:workspace  # git worktrees: provision, isolation, dirty-block, restart-recreate, list_dirs, collisions
npm run derisk:services   # ACP client services: fs round-trip, path-escape reject, terminal buffering, outputByteLimit, reconnect, cancel
npm run derisk:browser    # shared browser: laziness, broker CDP proxy, screencast over WS, grab/hold/release, user input, takeover MCP, teardown

npm run acp:live          # drive the REAL @agentclientprotocol/claude-agent-acp (needs authed `claude`); registers browser MCP unless TANDEM_BROWSER_MCP=off
npm run pty-smoke         # optional: proves the pty adapter (needs node-pty)
```

For full cross-runtime validation, build `../tandem` and run
`TANDEM_GO_DAEMON_CMD='["../tandem","daemon"]' npm run derisk:matrix`.

### Config (env)

| Env | Default | Meaning |
|---|---|---|
| `TANDEM_HOME` | `~/.tandem` | Root for `tandem.db`, `token`, `worktrees/` (honored everywhere) |
| `TANDEM_PORT` | `7717` | HTTP + WS port |
| `TANDEM_BIND` | `127.0.0.1` | Bind address |
| `TANDEM_UI_DIR` | — | Static UI dist for this rollback daemon (else a placeholder page) |
| `TANDEM_PROJECT_ROOTS` | `~/Projects` | Scanned for repos (spawn palette `list_dirs`) |
| `TANDEM_DIR_SCAN_DEPTH` | `1` | Directories to descend under each project root when scanning for repos |
| `TANDEM_NODE_CMD` | current Node executable (Node daemon); `node` (Go daemon) | Explicit Node launcher used to expand `{node}` in Node-based adapter definitions |
| `TANDEM_ACP_CMD` | — | JSON array overriding how **every** ACP agent launches, regardless of `SpawnSpec.agent` (tests point it at the mock) |
| `TANDEM_ACP_CMD_CLAUDE` / `_CODEX` / `_PI` | bundled `claude-agent-acp` / `codex-acp` / `pi-acp` | Per-agent launch override (JSON array or `"cmd arg arg"`), selected by `SpawnSpec.agent` |
| `TANDEM_BROWSER_DRIVER` | `local` | `local` (Playwright's bundled Chromium) or `steel` (requires `STEEL_BASE_URL`) |
| `STEEL_BASE_URL` / `STEEL_API_KEY` | — | Steel REST API for `driver=steel` (SteelDriver is specced, untested here) |
| `STEEL_SESSION_OPTIONS` | `{}` | JSON merged into Steel `POST /v1/sessions` (UA, dimensions, profiles, device/stealth options) |
| `TANDEM_BROWSER_MCP` | `on` | `off` skips registering Playwright + Tandem-control MCP at `session/new` |

## How it maps to the specs

| Spec | Code |
|---|---|
| `AgentAdapter` + normalized events + `SpawnSpec` + WS envelopes | [`src/types.ts`](src/types.ts) |
| ACP adapter (client owns fs/terminals; `session/load`; cancel) | [`src/acpAdapter.ts`](src/acpAdapter.ts) |
| ClientServices: workspace fs sandbox (fs/* choke point) | [`src/workspaceFs.ts`](src/workspaceFs.ts) |
| ClientServices: daemon-owned terminals + dual buffer (terminal/*) | [`src/terminalHost.ts`](src/terminalHost.ts) |
| pty adapter (daemon owns the pty master) | [`src/ptyAdapter.ts`](src/ptyAdapter.ts) |
| workspace manager: git worktrees, teardown, restore, repo discovery (D8) | [`src/workspace.ts`](src/workspace.ts) |
| multi-agent registry + restore (D11) | [`src/registry.ts`](src/registry.ts) |
| SQLite persistence (D14) | [`src/db.ts`](src/db.ts) |
| event log: ring + SQLite write-through | [`src/eventLog.ts`](src/eventLog.ts) |
| WS subscribe / snapshot / event / replay + auth (D15) | [`src/server.ts`](src/server.ts) |
| shared browser: driver seam (D13 amendment) | [`src/browser/driver.ts`](src/browser/driver.ts) |
| shared browser: lazy broker + gated CDP proxy | [`src/browser/broker.ts`](src/browser/broker.ts) |
| shared browser: daemon screencast/input CDP connection | [`src/browser/sharedBrowser.ts`](src/browser/sharedBrowser.ts) |
| Tandem-control MCP (`browser_request_takeover`) | [`src/browser/controlMcp.mjs`](src/browser/controlMcp.mjs) |
| MCP registration at `session/new` | [`src/browser/mcpWiring.ts`](src/browser/mcpWiring.ts) |
| paths + token + config | [`src/config.ts`](src/config.ts) |
