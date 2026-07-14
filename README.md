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
| Orchestration model | Human-as-conductor |
| Home-view layout | Docked rails + focus |
| Shared browser in UI | Pane inside focus mode |
| Workspaces | Git worktree + branch per agent (host dirs, no sandbox yet) |
| Spawn UX | Dir-first quick-spawn + command palette |
| Hotkeys | Single-key + chords, rebindable |

## Docs

- [`docs/architecture.md`](docs/architecture.md) — full architecture and subsystems
- [`docs/decisions.md`](docs/decisions.md) — the design decisions and their rationale
- [`docs/agent-adapter.md`](docs/agent-adapter.md) — the `AgentAdapter` interface + ACP/pty impls
- [`docs/ws-protocol.md`](docs/ws-protocol.md) — browser ↔ daemon WebSocket protocol
- [`docs/ui.md`](docs/ui.md) — UI structure and state model
- [`docs/spawn-and-workspaces.md`](docs/spawn-and-workspaces.md) — spawn mechanics, keymap, worktrees
- [`docs/acp-notes.md`](docs/acp-notes.md) — pinned ACP protocol facts

## Status

Specs + validated spine. What exists:

- **`docs/`** — the architecture and locked decisions (below).
- **`design/`** — an interactive wireframe of the docked-rails UI.
- **`daemon/`** — a runnable PoC that **de-risks the durability + ACP spine**: the daemon
  owns the agent process, a client socket can die (simulated dropped SSH pipe) and
  reconnect with gapless seq-replay, and the ACP `request_permission` loop round-trips
  end-to-end. `cd daemon && npm install && npm run derisk` — all checks pass. See
  [`daemon/README.md`](daemon/README.md).

See [`docs/architecture.md`](docs/architecture.md#v1-build-slice) for the full v1 build slice.
