# Architecture

Tandem is a browser-based orchestration layer over terminal coding agents. The production
daemon is a native Go executable with the React application embedded. This document
covers the system decomposition, the durability model, each subsystem, and the proposed
v1 build slice.

## The durability model (why a daemon)

The founding constraint: an interactive terminal agent is normally tied to the pty of the
terminal that launched it. When that terminal dies (e.g. an SSH drop), the agent receives
`SIGHUP` and its stdio hits EOF — it dies. tmux survives this only because tmux, not your
SSH session, owns the pty.

Tandem's answer is to **become the thing that owns the pty**:

- Each agent runs as a child of a long-lived **daemon**, which allocates and holds the pty
  master fd (for pty-based agents) or speaks a structured protocol over the child's stdio
  (for ACP agents).
- The browser connects to the daemon over a WebSocket and is a **pure view**.
- Because the daemon is the source of truth, the browser WS can drop and reconnect freely;
  the agents never notice. This is tmux-like persistence without tmux, plus a clean
  programmatic API.

> An optional future hardening is to run agents under `tmux -CC` control mode so sessions
> survive a daemon *crash* too. Deferred — first make the daemon robust with a
> scrollback/replay buffer.

### Validated (PoC)

Go integration tests hard-kill a client socket mid-stream (no close handshake —
a real dropped pipe), waits with no client attached, then reconnects from the client's
last `seq`. It asserts, and passes, that:

- the agent subprocess PID is unchanged across the disconnect (it never saw SIGHUP);
- not a single heartbeat event was lost during the gap;
- replay resumes from exactly `lastSeq + 1` with a gapless merged stream;
- the ACP `session/request_permission` loop is delivered *after* reconnect and answered
  over the new socket.

The native pty path is likewise covered by Go process and adapter tests: the daemon owns the
pty master fd and captures the child's scrollback. So both adapter paths sit on the correct
side of the durability boundary.

## Runtime layout

`cmd/tandem` is the production entrypoint. Packages under `internal/` own configuration,
SQLite, registry/session state, ACP and PTY adapters, HTTP/WebSocket serving, and direct CDP
browser control. `scripts/stage-go-ui.sh` builds `ui/` into `internal/ui/dist`; `go build`
then embeds that directory so production does not need a loose `ui/dist` tree.

Node remains an external runtime dependency only for configured ACP bridges and
`@playwright/mcp`; their locked dependencies live under `runtime/`. No Tandem backend code
runs under Node. See [deployment](deployment.md).

## Subsystems

Five separable pieces; keeping them decoupled is most of the battle.

1. **Agent Supervisor** — spawns/monitors agent processes, owns their ptys or ACP stdio,
   exposes a per-agent normalized event stream. See [`agent-adapter.md`](agent-adapter.md).
2. **Transport / session layer** — durable WebSocket between browser and daemon;
   subscribe / snapshot / replay. See [`ws-protocol.md`](ws-protocol.md).
3. **Orchestration layer** — cross-agent coordination and the human-as-conductor model
   (approvals queue, task assignment, status). The product's value-add.
4. **Shared browser service** — joint agent+human browser control via CDP screencast.
5. **Web UI** — the React front-end. See [`ui.md`](ui.md).

## Agent integration (structured-first, ACP primary)

Agents are reached through a normalized `AgentAdapter` interface with two implementations:

- **`AcpAdapter`** — speaks Zed's **Agent Client Protocol** (JSON-RPC 2.0 over the agent
  subprocess's stdio). The daemon is the ACP *client*. This is the primary path for Claude
  Code (via `claude-code-acp`), Gemini CLI, and anything ACP-speaking.
- **`PtyAdapter`** — a raw pty child (native Unix PTY). Used for TUI-only agents and for the
  user's escape-hatch shell. No structured events.

### Why ACP fits

ACP independently arrived at Tandem's core invariant. In ACP the **client owns terminals
and filesystem access** while the agent merely requests them. That places terminal output
buffers and fs mediation on the daemon side — exactly the correct side of the durability
boundary. Concretely:

- `session/update` notifications → normalized transcript events.
- `session/request_permission` → the approvals queue (direct 1:1 with human-as-conductor).
- `terminal/*` → client-owned terminals; the daemon owns and buffers output, which
  **persists even after `terminal/release`**, so re-capture on reconnect is native.
- `fs/read_text_file` / `fs/write_text_file` → routed through the daemon's workspace
  manager, our sandbox choke point.
- `session/load` (behind the `loadSession` capability) → session resume after an agent
  subprocess restart.

**Caveat:** ACP terminal buffers are bounded by `outputByteLimit` and truncate from the
beginning irreversibly (`truncated` flag, cannot re-fetch). Since the daemon is the client,
the mitigation is ours: set a generous limit and/or keep a larger scrollback in the daemon.

ACP is treated as **one adapter implementation behind our own interface**, not a
replacement for it — TUI-only agents and future protocols still slot in via `PtyAdapter`
or new adapters.

## Shared browser service

Goal: a browser that **both** the agent (via Playwright/CDP) and the user (via a live view)
can control jointly.

- Agent and human are **two CDP clients on one browser** — this is what makes joint control
  natural.
- **User view/control is CDP screencast**, not VNC: `Page.startScreencast` streams frames;
  input is forwarded back via `Input.dispatchMouseEvent` / `dispatchKeyEvent`. Lighter than
  VNC, needs no X server, and shares the exact CDP session the agent uses.
- **Steel** (self-hostable) provides managed sessions + CDP endpoint + a live viewer,
  collapsing several layers. VNC/Xvfb is kept only as a future fallback for desktop-level
  needs (OS dialogs, extensions, file pickers, non-browser apps).
- **Control-owner token**: agent and human driving simultaneously causes input races. Each
  browser session has an explicit control owner; the human can "grab the wheel" (pausing the
  agent) and later "release" it.

This subsystem is orthogonal to ACP — ACP says nothing about browser control.

## Orchestration model — human-as-conductor

The differentiator vs. raw terminals is **legibility and control**, not autonomy. The app
surfaces multi-agent state and intervention points; humans do the coordinating.

Core primitives:

- **Agent/Session** — a running agent with a status: `idle | working | blocked | error`.
- **Workspace** — a filesystem context, isolated as a git worktree (or dir) per agent.
- **Task** — a unit of work assigned to an agent.
- **Approval / Interrupt** — the human-in-the-loop gate; the conductor's inbox.
- **Artifact / Diff** — reviewable results (file diffs, command output, browser recordings).

## Security notes

- v1 is **single-tenant self-hosted**, which sidesteps the large multi-tenant sandboxing
  burden. Even so, the daemon can run arbitrary commands and the shared browser holds real
  credentials — so the WS must be authenticated and the daemon bound carefully (localhost /
  Tailscale, not a public interface).
- Multi-tenant cloud (per-user containers/Firecracker, egress filtering, secret isolation)
  is a separate, later product decision.

## v1 build slice

Thinnest end-to-end spine; each step is independently demoable, riskiest theses first.
**All five steps are now built and validated** — see [`../README.md`](../README.md#status)
for the suite-by-suite breakdown and recorded deviations.

1. **Daemon + one agent.** ✓ **Built + validated.** Spawn a Claude Code agent (via
   `AcpAdapter`, `PtyAdapter` fallback), expose its normalized event stream + scrollback over
   the WS with reconnect/replay, persisted to SQLite and restored on restart.
   *Survive-the-dropped-pipe thesis proven (`derisk`, `derisk:restart`).*
2. **UI spine.** ✓ **Built.** `ui/` — left rail + focus with Transcript + Terminal panes,
   durable WS client, quick-spawn + command palettes, rebindable keymap.
3. **Approvals.** ✓ **Built + validated.** `session/request_permission` →
   `permission_request` events → the always-on right-rail queue → response over the WS.
   *Human-as-conductor proven (`derisk`, `derisk:services` cancellation).*
4. **Second agent + workspace isolation.** ✓ **Built + validated.** Git worktree + branch
   per agent; two agents run without clobbering; rails show both statuses; teardown keeps the
   branch. (`derisk:multi`, `derisk:workspace`, `derisk:integration`.)
5. **Shared browser.** ✓ **Built + validated.** Per-agent browser via a Tandem broker
   (`BrowserDriver`: Steel — specced — or local Chromium — tested), Playwright MCP + a
   Tandem-control MCP, CDP screencast in the Browser pane, control-owner token with
   hard-pause. *Joint control proven (`derisk:browser`, 10 checks).*
