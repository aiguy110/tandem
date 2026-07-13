# Design decisions

ADR-style record of the choices made during initial architecture, with rationale and the
alternatives considered.

## D1 — Deployment topology: self-hosted daemon (v1)

**Choice:** The user runs one daemon on their machine or their server; the browser UI
connects over localhost / Tailscale.

**Why:** Coding agents run arbitrary commands and the shared browser holds real
credentials. A self-hosted single-tenant daemon sidesteps the enormous multi-tenant
sandboxing burden (container escape, egress filtering, secret isolation) while still being
a complete product.

**Alternatives:** Multi-tenant cloud SaaS (deferred — a separate, much larger infra +
security effort); local-first desktop app (a viable packaging of the same daemon later).

## D2 — Agent integration: hybrid, structured-first

**Choice:** Talk to agents through structured/headless protocols where available, with a
raw pty + xterm.js path as escape hatch and fallback.

**Why:** Structured events (messages, tool calls, permission requests) make a legible
orchestration UI possible. Raw pty alone forces terminal-scraping to answer "is the agent
done / blocked / erroring?" A raw terminal view is still valuable for power users and for
TUI-only agents.

**Alternatives:** Raw pty only (familiar UX, poor orchestration); structured only (cleanest
UI, but excludes TUI-only agents and hides the native experience).

## D3 — Adopt ACP as the primary adapter implementation

**Choice:** Use Zed's Agent Client Protocol as the reference `AgentAdapter` implementation
(`AcpAdapter`), behind Tandem's own internal interface. Keep `PtyAdapter` for TUI-only
agents and the user shell.

**Why:** ACP standardizes exactly the structured-first model D2 calls for, and its
client-owns-terminals / client-owns-fs design places buffering and mediation on the daemon
side — the correct side of the durability boundary. It gets Claude Code + Gemini CLI +
anything ACP-speaking through one code path, plus a ready-made permission model that maps
1:1 to the approvals queue.

**Caveats:** ACP is young/evolving (treat as one adapter, not the whole interface); its
terminal buffers truncate irreversibly past `outputByteLimit` (the daemon keeps its own
scrollback); ACP does not cover the shared browser.

## D4 — Shared browser stack: CDP screencast, Steel-based

**Choice:** Agent and user are both CDP clients on one browser; the user sees/controls via
CDP screencast. Use Steel (self-hostable) to provide sessions + CDP + live view.

**Why:** CDP screencast is lighter than VNC, needs no X server, and shares the exact CDP
session the agent uses — making joint control natural. Steel collapses the
chromium/CDP/live-view plumbing into one self-hostable component.

**Alternatives:** Self-built CDP screencast (more control, more plumbing); VNC/desktop
(heavier, but needed for OS dialogs / extensions / file pickers — kept as a future
fallback only).

**Related:** A **control-owner token** per browser session arbitrates agent-vs-human input
to avoid races.

## D5 — Orchestration model: human-as-conductor

**Choice:** The user manually spawns, assigns, and approves across a legible multi-agent
view. The app surfaces state and control; humans do the coordinating.

**Why:** The differentiator vs. raw terminals is legibility and control, not autonomy. A
supervisor/conductor *agent* adds magic and failure modes better deferred.

**Alternatives:** Supervisor/conductor agent (later); "just a nice multi-terminal" (too
undifferentiated).

## D6 — Home-view layout: docked rails + focus

**Choice:** Fixed left agent rail, center focus area with pane tabs, right approvals rail,
contextual bottom inspector, top conductor bar.

**Why:** Dense, legible, predictable; good for many agents and fast switching. A free
spatial canvas is more work and can get messy; can be added later as a second lens.

## D7 — Shared browser in UI: pane inside focus mode

**Choice:** The browser is one tab of the focused agent, alongside Transcript / Terminal /
Diff.

**Why:** Keeps everything about an agent in one place and keeps v1 tight. Detachable /
first-class browser surfaces can come later.
