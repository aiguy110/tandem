# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Workflow

- After implementing a feature or bug fix, verify it works (run tests, typecheck, or exercise the change). Once verified, commit the changes with git automatically — do not wait for the user to ask separately. Only skip the auto-commit if the user has said otherwise for that specific change.
- The running app is a `systemd --user` service (`tandem.service`) built from `/home/josiah/Projects/tandem` on branch `master` — not from whatever worktree/branch you happen to be working in. A commit on a feature branch/worktree is invisible to the live app, and a browser refresh will keep showing the old build no matter what, until master itself has the change *and* the service has rebuilt. So: once a change is committed and verified, automatically (a) merge it into `master` in `/home/josiah/Projects/tandem`, and (b) run `./redeploy.sh` from there to rebuild + restart `tandem.service`. Do this without waiting to be asked, the same way you auto-commit — treat "merged to master and redeployed" as part of "done," not a separate follow-up step. Only skip it if the user says otherwise for that specific change, or the merge has conflicts you can't resolve confidently (then stop and ask).
- Cutting a release: only push the tag (`git tag vX.Y.Z && git push origin vX.Y.Z`) and let `.github/workflows/release.yml` build the binaries and run `gh release create`. Do not run `gh release create` yourself first — the workflow's own `gh release create` fails with "a release with the same tag name already exists" if a release already exists for that tag, so a manual release blocks CI from ever attaching the binary artifacts.

## Commands

```bash
./start-dev-server.sh          # builds UI + native Go daemon, starts at 127.0.0.1:7717
./redeploy.sh                  # requests a deferred rebuild/restart of tandem.service
```

The backend is exclusively the native Go daemon (`cmd/tandem` + `internal/`). The app runs under a `systemd --user` unit (`deploy/tandem.service`, installed via `deploy/install.sh`) whose `ExecStart` is `start-dev-server.sh`; a restart builds the React UI, installs external agent runtime dependencies, embeds the UI in the Go binary, and starts that binary. Logs: `journalctl --user -u tandem -f`.

Manual equivalent:

```bash
# Native daemon and UI
go test ./...                       # primary native test suite
./scripts/stage-go-ui.sh            # typecheck/build UI and stage embedded assets
go build -o tandem ./cmd/tandem
./tandem

# UI development
cd ui && npm install && npm run build   # tsc --noEmit && vite build
cd ui && npm run typecheck              # tsc --noEmit only
cd ui && npm run dev                    # vite dev server

# External ACP bridges and Playwright MCP (no Tandem backend code)
cd runtime && npm install
```

For Go changes, run the relevant package tests or `go test ./...`.

### Config (env)

| Env | Default | Meaning |
|---|---|---|
| `TANDEM_HOME` | `~/.tandem` | Root for `tandem.db`, `token`, `worktrees/` |
| `TANDEM_PORT` / `TANDEM_BIND` | `7717` / `127.0.0.1` | HTTP + WS listen address |
| `TANDEM_UI_DIR` | — | Static UI dist to serve (else a placeholder page) |
| `TANDEM_PROJECT_ROOTS` | `~/Projects` | Directories scanned for repos in the spawn palette |
| `TANDEM_NODE_CMD` | `node` | Node launcher for external ACP and browser adapters |
| `TANDEM_ACP_CMD` | — | JSON array overriding how **every** ACP agent launches (primarily compatibility tests) |
| `TANDEM_ACP_CMD_CLAUDE` / `_CODEX` / `_PI` | bundled `claude-agent-acp` / `codex-acp` / `pi-acp` | Per-agent launch override, selected by `SpawnSpec.agent` |
| `TANDEM_RESUME_CMD_CLAUDE` / `_CODEX` / `_PI` | agent-specific CLI command | JSON array or command template for Terminal handoff; `{sessionId}` is substituted |
| `TANDEM_BROWSER_DRIVER` | `local` | `local` (bundled Chromium) or `steel` (needs `STEEL_BASE_URL`) |
| `TANDEM_BROWSER_MCP` | `on` | `off` skips registering Playwright + Tandem-control MCP at `session/new` |

Agent launches are declared by the shipped `config.yml.example`, overlaid by optional
`$TANDEM_HOME/config.yml` definitions and reusable profiles. Claude, Codex, and Pi are
ordinary YAML entries rather than daemon-code special cases; see `docs/spawn-and-workspaces.md`.

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
2. **Every agent is an `AgentAdapter`** implementing the interface in `internal/agentadapter/adapter.go`. Structured-first via ACP where available; raw pty is the fallback and the user's escape-hatch shell.
3. **One CDP browser, two clients, one control token.** Agent (via Playwright/CDP) and human (via a live view) are peers on the same browser session; a control-owner token arbitrates who is driving.
4. **Workspace isolation by default** — a git worktree (or existing dir) per agent so parallel agents don't clobber each other.

### Native daemon (`cmd/`, `internal/`)

- `internal/registry/` + `internal/session/` — agent lifecycle, restore, handoff, and durable session state.
- `internal/wsserver/` + `internal/httpserver/` — browser protocol, auth, subscriptions, and static UI serving.
- `internal/acpadapter/` + `internal/ptyadapter/` — structured ACP and raw terminal adapters.
- `internal/store/` + `internal/eventlog/` — SQLite persistence and replay.
- `internal/workspace/` + `internal/workspacefs/` — isolated worktrees and ACP filesystem sandboxing.
- `internal/terminalhost/` + `internal/browser/` — daemon-owned terminals and shared browser control.

There is no TypeScript backend or rollback daemon in the repository. Backend behavior must be implemented and tested in Go.

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
- `LocalChromiumDriver` remains the default, no-Docker path; `SteelDriver` is opt-in and requires a self-hosted Steel instance.

## Agent catalog TODOs

- **Stage 3 — ACP Registry discovery:** fetch/cache registry manifests, expose available
  agents and versions in settings, and resolve supported distribution types without
  installing during ordinary daemon startup.
- **Stage 4 — managed installs:** install into versioned `$TANDEM_HOME/agents/` locations,
  record exact resolutions in a lockfile and persisted sessions, detect updates explicitly,
  retain versions needed by resumable sessions, and support rollback.
- **Stage 5 — custom Git manifests:** accept pinned tags or commit SHAs only, require an
  explicit registry-compatible build/launch manifest, and never guess or execute an
  arbitrary repository's installation workflow.
