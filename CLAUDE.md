# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Workflow

- After implementing a feature or bug fix, verify it works (run tests, typecheck, or exercise the change). Once verified, commit the changes with git automatically — do not wait for the user to ask separately. Only skip the auto-commit if the user has said otherwise for that specific change.
- The running app is a `systemd --user` service (`tandem.service`) built from `/home/josiah/Projects/tandem` on branch `master` — not from whatever worktree/branch you happen to be working in. A commit on a feature branch/worktree is invisible to the live app, and a browser refresh will keep showing the old build no matter what, until master itself has the change *and* the service has rebuilt. So: once a change is committed and verified, automatically (a) merge it into `master` in `/home/josiah/Projects/tandem`, and (b) run `./redeploy.sh` from there to rebuild + restart `tandem.service`. Do this without waiting to be asked, the same way you auto-commit — treat "merged to master and redeployed" as part of "done," not a separate follow-up step. Only skip it if the user says otherwise for that specific change, or the merge has conflicts you can't resolve confidently (then stop and ask).

## Commands

```bash
./start-dev-server.sh          # builds the UI, installs daemon deps, starts the daemon at 127.0.0.1:7717
./redeploy.sh                  # rebuild + restart the running app (calls: systemctl --user restart tandem.service)
```

The app runs under a `systemd --user` unit (`deploy/tandem.service`, installed via `deploy/install.sh`) whose `ExecStart` is `start-dev-server.sh` itself — so a restart always rebuilds first. `redeploy.sh` is meant to be run from inside the app (e.g. its own terminal pane) after the app has edited its own code, so it can redeploy itself. Logs: `journalctl --user -u tandem -f`.

Manual equivalent:

```bash
# UI (daemon serves the compiled dist)
cd ui && npm install && npm run build   # tsc --noEmit && vite build
cd ui && npm run typecheck              # tsc --noEmit only
cd ui && npm run dev                    # vite dev server

# Daemon (compiles better-sqlite3 native module)
cd daemon && npm install
TANDEM_UI_DIR=../ui/dist npm run daemon # start the daemon

# Daemon tests — nine de-risk suites, each against a throwaway TANDEM_HOME
cd daemon && npm test                   # = derisk:all, runs all suites below in sequence
npm run derisk             # durability + ACP approval loop (single agent)
npm run derisk:multi       # two agents: independent seq streams, queues, isolated teardown
npm run derisk:restart     # persist → stop → restart → restore from SQLite + session/load
npm run derisk:auth        # no/bad token → 4401; valid token → connects
npm run derisk:workspace   # git worktrees: provision, isolation, dirty-block, restart-recreate, list_dirs, collisions
npm run derisk:services    # ACP client services: fs round-trip, path-escape reject, terminal buffering, cancel
npm run derisk:browser     # shared browser: laziness, broker CDP proxy, screencast, grab/hold/release, takeover MCP
npm run derisk:integration # full-slice smoke: discover → spawn → prompt → approval → 2nd agent → reconnect replay → dirty-close teardown
npm run derisk:resume      # resumable-session catalog + live/closed/external resume paths

# Opt-in (NOT part of `npm test`): needs a self-hosted Steel (Docker). Skips+passes if unreachable.
STEEL_BASE_URL=http://localhost:3000 npm run derisk:steel # SteelDriver + broker against a live Steel (see docs/browser.md › Self-hosting Steel)

npm run acp:live           # drive the real @agentclientprotocol/claude-agent-acp (needs an authenticated `claude` CLI)
npm run pty-smoke          # exercise the pty adapter (needs node-pty)
```

There is no single-test runner — each `derisk:*` script is a standalone, self-contained scenario (`daemon/src/derisk*.ts`); run the one relevant to the area you're changing.

### Config (env)

| Env | Default | Meaning |
|---|---|---|
| `TANDEM_HOME` | `~/.tandem` | Root for `tandem.db`, `token`, `worktrees/` |
| `TANDEM_PORT` / `TANDEM_BIND` | `7717` / `127.0.0.1` | HTTP + WS listen address |
| `TANDEM_UI_DIR` | — | Static UI dist to serve (else a placeholder page) |
| `TANDEM_PROJECT_ROOTS` | `~/Projects` | Directories scanned for repos in the spawn palette |
| `TANDEM_ACP_CMD` | — | JSON array overriding how **every** ACP agent launches, regardless of `SpawnSpec.agent` (tests point it at the mock) |
| `TANDEM_ACP_CMD_CLAUDE` / `_CODEX` / `_PI` | bundled `claude-agent-acp` / `codex-acp` / `pi-acp` | Per-agent launch override, selected by `SpawnSpec.agent` |
| `TANDEM_BROWSER_DRIVER` | `local` | `local` (bundled Chromium) or `steel` (needs `STEEL_BASE_URL`) |
| `TANDEM_BROWSER_MCP` | `on` | `off` skips registering Playwright + Tandem-control MCP at `session/new` |

## Architecture

Tandem is a browser-based orchestration layer for terminal coding agents (Claude Code, Gemini CLI, …), run as a self-hosted daemon with a React UI as a thin, reconnectable view.

```
Browser (React app)
   │  durable WebSocket (reconnect + replay)
   ▼
Self-hosted Daemon
   ├── Session/Agent registry (source of truth)
   ├── Agent Supervisor
   │     └── per-agent AgentAdapter
   │           ├── AcpAdapter (structured, primary)
   │           └── PtyAdapter (raw pty / user shell)
   ├── Workspace mgr (git worktree per agent)
   ├── Approvals / Interrupt bus
   └── Browser broker
         └── Steel session (CDP)
               ├── agent = Playwright/CDP client
               └── user  = CDP screencast + input
                     └── control-owner token
```

**Design invariants** (see `docs/decisions.md` for full rationale):

1. **The daemon owns all state.** Everything the browser shows must be reconstructable from daemon state on reconnect — transcripts, terminal scrollback, browser frames all replay from the event log.
2. **Every agent is an `AgentAdapter`** implementing one interface (`daemon/src/types.ts`). Structured-first via ACP where available; raw pty is the fallback and the user's escape-hatch shell.
3. **One CDP browser, two clients, one control token.** Agent (via Playwright/CDP) and human (via a live view) are peers on the same browser session; a control-owner token arbitrates who is driving.
4. **Workspace isolation by default** — a git worktree (or existing dir) per agent so parallel agents don't clobber each other.

### Daemon (`daemon/src/`)

- `registry.ts` — multi-agent registry: owns `AgentSession`s keyed by agentId, auto-names them (`web-1`, `api-2`, …), implements `SpawnSpec`, and restores agents on daemon restart (`restoreAll`; a single failed restore marks that agent `error` without crashing the daemon).
- `workspace.ts` — git worktree lifecycle: provisions `tandem/<agent>` branches under `$TANDEM_HOME/worktrees/<repo>/<agent>/`, blocks dirty closes unless `force`, keeps the branch after close, recreates a missing worktree dir from its branch on restore, and does repo discovery for the spawn palette.
- `db.ts` / `eventLog.ts` — SQLite persistence (`better-sqlite3`, WAL) at `$TANDEM_HOME/tandem.db`: `agents` + `events` tables; the event log writes through to SQLite while keeping an in-memory ring buffer for hot replay.
- `server.ts` — the WS protocol (see `docs/ws-protocol.md`): `subscribe`/`unsubscribe` (per-agent channels + `sinceSeq`), `prompt`, `input`, `resize`, `permission_response`, `interrupt`, `spawn_agent`, `close_agent`, `list_dirs`. Multiple concurrent clients multiplex multi-agent subscriptions on one socket. Also owns bearer-token auth (`?token=`, else WS closes `4401`) and static UI serving.
- `acpAdapter.ts` — the ACP agent adapter; the daemon acts as the full ACP client (`fs/*`, `terminal/*`), advertising and servicing those capabilities itself.
- `workspaceFs.ts` — the fs sandbox choke point for `fs/read_text_file` / `fs/write_text_file`: paths must resolve (realpath) inside the agent's workspace cwd; escapes are rejected with JSON-RPC `-32602`, no disk touched.
- `terminalHost.ts` — daemon-owned terminal execution for `terminal/create·output·wait_for_exit·kill·release`; prefers `node-pty`, degrades to `child_process` pipes. Dual buffer: the ACP-visible view honors `outputByteLimit` (truncate-from-start), while the daemon keeps a larger independent scrollback that persists past `terminal/release` and replays as `terminal_output` events.
- `ptyAdapter.ts` — raw pty adapter (daemon owns the pty master directly, no ACP).
- `browser/` — the shared-browser subsystem: `driver.ts` (`BrowserDriver` seam: `LocalChromiumDriver` real/tested, `SteelDriver` specced/untested), `broker.ts` (lazy per-agent CDP provisioning, gated CDP proxy enforcing the control-owner token), `sharedBrowser.ts` (daemon's own screencast/input CDP connection), `controlMcp.mjs` (Tandem-control MCP exposing `browser_request_takeover`), `mcpWiring.ts` (registers Playwright MCP + Tandem-control MCP at `session/new`).
- `deriskAuth.ts` / `deriskWorkspace.ts` / `deriskServices.ts` / `deriskBrowser.ts` / `deriskMulti.ts` / `deriskRestart.ts` / `deriskIntegration.ts` / `deriskResume.ts` / `derisk.ts` — the nine standalone de-risk suites (see Commands above); each is the primary regression test for its named subsystem, run against a throwaway `TANDEM_HOME`. `deriskSteel.ts` is a tenth, opt-in suite (needs a self-hosted Steel; not in `npm test`).

### UI (`ui/src/`)

React "mission control" front-end: a durable WS client with reconnect + replay, docked rails (agents, approvals) + a focus pane, streaming transcript, quick-spawn + command palettes with a rebindable keymap, a ghostty-web terminal, and the shared-browser pane (screencast + grab/release).

- `store.ts` — client-side state, projected from daemon WS events (zustand).
- `wire.ts` / `ws/` — the WebSocket client: connection, auth, subscribe/replay handling.
- `components/` — rail, transcript, approvals, terminal, browser-pane, and modal components.
- `commands/` — command palette + spawn palette definitions.
- `useGlobalKeys.ts` — the rebindable single-key/chord hotkey system.

The daemon serves the built UI (`ui/dist`) via `TANDEM_UI_DIR`; there is no separate UI server in production.

## Docs

- `docs/architecture.md` — full architecture and subsystems
- `docs/decisions.md` — design decisions and rationale
- `docs/agent-adapter.md` — the `AgentAdapter` interface + ACP/pty implementations
- `docs/ws-protocol.md` — browser ↔ daemon WebSocket protocol
- `docs/ui.md` — UI structure and state model
- `docs/spawn-and-workspaces.md` — spawn mechanics, keymap, worktrees
- `docs/terminal.md` — terminal pane (ghostty-web) + spike findings
- `docs/browser.md` — shared browser (Playwright MCP + Steel, control token)
- `docs/acp-notes.md` — pinned ACP protocol facts

## Known stubs

- `merge_back` — accepted over WS but returns an error `ack`; no diff review / merge-or-PR UI yet.
- `raw_pty` / `browser_frame` payloads are base64-in-JSON, not binary framing.
- `SteelDriver` is verified against a self-hosted Steel via `npm run derisk:steel` (opt-in, needs Docker); `LocalChromiumDriver` remains the default, no-Docker path.
