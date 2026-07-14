# Shared browser

Joint agent + human control of one browser **per agent**. The agent drives via Playwright
MCP over CDP; the user views and controls the same browser via Steel's screencast, arbitrated
by a **control-owner token**. Builds on D4 (CDP-screencast, Steel-based) and D13.

## Topology

```
        ┌───────────── Steel session (self-hosted, Docker) · per agent ──────────┐
        │                        one Chromium, CDP endpoint                        │
        └───────────────────────────────────────────────────────────────────────┘
             ▲ CDP (Playwright)                         ▲ CDP screencast + Input.*
             │                                          │
   Playwright MCP  ──▶ Tandem browser broker            Browser pane (user)
   (--cdp-endpoint = broker URL)   │ lazily provisions   grab / release the wheel
             ▲ MCP tools           │ Steel on first use
             │                     ▼
        the ACP agent        Steel session (created on demand)
```

Both the agent (Playwright over CDP) and the user (screencast + `Input.*` over CDP) are
clients of **one** browser; the token serializes them.

## Agent → browser (Playwright MCP)

- **Every agent has browser tools, provisioned lazily.** Tandem registers a Playwright MCP
  server in the agent's ACP `mcpServers` at `session/new`, pointed at a **stable per-agent
  CDP URL served by a Tandem browser broker**. The broker spins up the Steel session on the
  **first** browser tool call and proxies CDP thereafter — so no Steel session exists until a
  browser is actually used.
- Playwright MCP runs in **snapshot mode** (deterministic accessibility tree) — the agent
  acts on structure, the human on pixels; same page, two lenses.
- Alongside it, Tandem exposes a small **Tandem-control MCP** with
  `browser.request_takeover(reason)` (see Attention).
- Isolation: **one Steel session per agent** (own cookies/auth/state), matching the
  worktree-per-agent model. Agents never collide on one page.

## User → browser

- The Browser pane renders Steel's **screencast** (`Page.startScreencast`) and forwards the
  user's mouse/keyboard back via `Input.dispatchMouseEvent` / `dispatchKeyEvent`.
- Only the **focused** agent's browser streams live (bandwidth); others pause and resume on
  focus — mirrors the terminal focus rule.

## Control token & enforcement (hard-pause)

A per-browser **control owner** — `agent` or `user`. The non-owner is view-only.

- Because the Playwright MCP is **Tandem-mediated**, when `controlOwner = user` Tandem
  **holds the agent's browser tool calls** (async, like a slow tool) until control returns —
  no races, no errors, no agent cooperation required.
- When `controlOwner = agent`, the user's input events are suppressed (view-only), but the
  user can **grab** at any time, which immediately pauses the agent.

## Attention: signaling that a human is needed

Folds into the same attention/approvals rail as permissions. Two directions:

- **User-initiated grab:** *Grab the wheel* → Tandem pauses the agent → user drives →
  *Release* → the agent's next tool call resumes. The agent never had to know.
- **Agent-initiated handoff:** the agent calls `browser.request_takeover("log in to
  continue")`. Tandem (which hosts that MCP) sets the agent's status to
  **`blocked · needs you`**, adds an attention item, and shows a Browser-pane banner
  *"web-1 needs you — log in to continue. [Take the wheel]"*. When the user takes the wheel,
  completes the step, and hands back (release), the agent's blocked tool call **resolves with
  the result** and it continues.

## Lifecycle

- The Steel session is provisioned lazily and **torn down when the agent closes**.
- Browser auth/state is per-agent and **ephemeral in v1** — not restored across daemon
  restart (the agent re-navigates). Persisting browser cookies/state is deferred.

## Spike (verified)

[`spike/browser/`](../spike/browser/) proves the mechanics against a real headless Chrome —
the same CDP surface Steel wraps. `npm run derisk` passes all five checks:

- the agent drives the page via **Playwright over CDP** (`connectOverCDP` — exactly what
  Playwright MCP uses under the hood);
- a CDP **`Page.startScreencast`** streams the same page **concurrently** (12 frames) while
  the agent acts;
- **grabbing the wheel hard-pauses** the agent's actions (held until release);
- **releasing resumes** the paused action;
- the human's input reaches the page via CDP **`Input.*`** (a click flipped page state).

`npm run broker` runs it live: a viewer renders the screencast while a demo agent loop types
into the page — the human watches the agent drive in real time and can grab the wheel.

**Transfer to production:** swap the raw Chrome + `connectOverCDP` URL for a **Steel
session's CDP endpoint**; the Playwright-MCP layer is a thin wrapper over the same
`connectOverCDP` proven here (wiring `--cdp-endpoint` + the lazy broker is a build-phase step).

## Deferred

- **VNC/desktop fallback** for OS-level needs (native file pickers, dialogs, extensions,
  non-browser apps) — only if CDP screencast proves insufficient.
- **Shared profile / cross-agent logins** (log in once, many agents reuse).
- **Persisted browser sessions** across restart.
