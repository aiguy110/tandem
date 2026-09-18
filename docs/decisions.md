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

**Choice:** The browser is one tab of the focused agent, alongside Chat / Terminal /
Diff.

**Why:** Keeps everything about an agent in one place and keeps v1 tight. Detachable /
first-class browser surfaces can come later.

## D8 — Workspaces: one git worktree + branch per agent (host dirs, no sandbox)

**Choice:** Agents run in host working directories. Spawning into a git repo creates an
isolated `git worktree` + branch (`tandem/<agent>`, based on current HEAD, checked out under
`~/.tandem/worktrees/<repo>/<agent>/`); non-git dirs are used as-is. No sandboxing yet.

**Why:** Worktrees give clobber-free parallel agents on one repo for almost free — they're
just different host directories, no container machinery. Central placement keeps the real
repo clean and makes worktrees easy to enumerate/GC.

**Teardown:** closing an agent removes the worktree checkout but **keeps the branch**
(work never lost); uncommitted changes block the close with a warning. **Merge-back is an
explicit human action** (`agent.mergeBack` → review diff → merge or PR), never automatic.

**Deferred:** real sandboxing (containers/per-agent isolation) is future work.

## D9 — Spawn UX: dir-first quick-spawn + command palette

**Choice:** A dedicated quick-spawn hotkey opens a fuzzy **directory-first** palette
(`Enter` = spawn idle, `⇥ task` = spawn + dispatch, `⌘Enter` = advanced). A separate command
palette (`⌘K`) covers all commands + jump-to-agent and shows every keybinding inline.

**Why:** Spawn is the most-repeated action; dir-first with defaults makes the common case a
few keystrokes, while progressive disclosure keeps full control reachable. The palette is
the discoverability layer that also teaches the hotkeys.

## D10 — Hotkeys: single-key + chords, rebindable

**Choice:** Single-key and chord bindings (`c`, `g a`), active when not typing in a field,
resolved by scope (`global` / `agent-focused` / `modal-open` / `text-input`), fully
customizable via a `keybindings.json`.

**Why:** Fewest keystrokes for a keyboard-heavy conductor workflow; mirrors the
`keybindings.json` model the user already uses in Claude Code. Scope-awareness avoids
bare-key collisions with text input.

## D11 — Agents are persistent; sessions restored on restart

**Choice:** Agents are durable. The daemon persists each agent's `SpawnSpec` + ACP
`sessionId` + worktree/branch, and on restart restores them — resuming via `session/load`
where the agent supports `loadSession`, else a fresh session with history re-rendered from
the event log. Project roots are configurable via `TANDEM_PROJECT_ROOTS` (default
`~/Projects`); after a spawn, focus jumps to the newly launched agent.

**Why:** An agent is a long-lived unit of work with a workspace and history; a daemon
restart (crash, upgrade) shouldn't lose it.

## D12 — Terminal renderer: ghostty-web default, xterm.js fallback

**Choice:** Render terminals against the xterm.js API behind a `TerminalRenderer` interface.
Default engine is `ghostty-web` (Ghostty's `libghostty-vt` in WASM, xterm-API-compatible,
purpose-built for parallel agentic dev); `@xterm/xterm` is a drop-in fallback via config.

**Why:** Ghostty's VT engine is best-in-class and fits our category, while xterm-API
compatibility makes the choice reversible (≈one-line swap) and keeps a proven fallback. The
interface keeps the emulator out of the rest of the UI.

**Constraints:** only the focused terminal renders live (WebGL context limits); reconnect
replays the distinct `raw_pty` and `shell_pty` buffers; resize propagates via the matching
agent-PTY or user-shell WS message. See
[`terminal.md`](terminal.md).

## D13 — Shared browser: per-agent Steel, Playwright MCP, Tandem-mediated token

**Choice:** Each agent gets its own Steel (self-hosted) browser session. The agent drives it
via a **Playwright MCP** server (snapshot mode) registered in ACP `mcpServers`, pointed at a
**Tandem browser-broker** URL that provisions the Steel session **lazily on first use** — so
every agent has browser tools but no browser spins up until needed. The user views/controls
the same browser via Steel's CDP screencast in the Browser pane.

**Control:** a per-browser control-owner token (`agent` / `user`). Since the Playwright MCP
is Tandem-mediated, grabbing the wheel **hard-pauses** the agent's browser tool calls (async
hold, no errors) until release. Agent-initiated handoff uses a dedicated **Tandem-control MCP**
tool `browser.request_takeover(reason)`, which surfaces in the attention rail as
`blocked · needs you` and resolves when the human hands back.

**Why:** Per-agent isolation matches the worktree model; Playwright MCP over CDP is the
supported way to attach an agent to an existing shared browser; mediating the MCP lets Tandem
enforce the token without trusting the agent; a dedicated takeover tool is a cleaner fit than
overloading ACP permissions. See [`browser.md`](browser.md).

**Deferred:** VNC/desktop fallback, shared cross-agent profiles, persisted browser state.

**Amendment (v1 build):** the browser broker exposes one `BrowserDriver` interface with two
implementations — `SteelDriver` (Steel's sessions API at `STEEL_BASE_URL`, the specced
production path) and `LocalChromiumDriver` (Playwright-launched headless Chromium, used
where no Steel/Docker is available and by the automated tests; same CDP surface).

## D14 — Persistence: SQLite under `~/.tandem`

**Choice:** `better-sqlite3` database at `~/.tandem/tandem.db` holding the agent registry
(`SpawnSpec` + ACP `sessionId` + worktree/branch) and per-agent event logs (seq-keyed).

**Why:** D11 (durable agents) needs a store that survives restarts, appends fast, and can
serve `sinceSeq` range queries for replay. SQLite is atomic, queryable, single-file, and
handles growing logs; WAL mode keeps appends cheap.

**Alternatives:** JSONL + registry.json (zero native deps, human-readable, but manual
compaction and racy multi-file updates).

## D15 — WS auth: localhost bind + bearer token, URL-fragment bootstrap

**Choice:** The daemon binds `127.0.0.1` by default and serves the UI over the same HTTP
port as the WS. On first run it generates a token into `~/.tandem/token` and prints a
bootstrap URL (`http://host/#t=<token>`). The UI reads the fragment once, stores the token
in `localStorage`, and presents it on every WS connect; the daemon rejects unauthenticated
sockets.

**Why:** Remote access is fronted by `tailscale serve` (TLS + network identity); the token
is defense-in-depth. URL fragments are never sent in HTTP requests, so the token stays out
of proxy/serve logs.

## D16 — Configurable agent catalog with launch profiles

**Choice:** Agent implementations are data, not adapter subclasses. Tandem loads the
checked-in `config.yml.example` catalog and overlays `agents` and `profiles` from
`$TANDEM_HOME/config.yml`. Even the shipped Claude, Codex, and Pi definitions live in YAML.
Each agent may declare an ACP command and/or a direct-terminal
command, including argument arrays and scoped environment variables. Profiles reference an
agent and append reusable ACP or terminal arguments.

**Why:** `AcpAdapter` and `PtyAdapter` are already protocol-generic. Keeping launch details
in a catalog makes locally installed and custom agents available without daemon code
changes, while profiles support several model or behavior configurations of one tool.

**Compatibility and safety:** The shipped catalog preserves existing ids and
environment-variable overrides. Launch commands use argv arrays, not shell strings. Direct-terminal start and
ACP-session resume are separate templates because some agents cannot resume an ACP session
in their native CLI.

**Managed distributions:** Built-in npm ACP bridges install side-by-side under the Tandem
runtime root. A lockfile selects the default for new sessions, while every session persists
its exact distribution in `ResolvedLaunch`. Update notifications can therefore install and
roll back defaults without silently changing the executable beneath a durable session.

**Deferred:** ACP Registry manifest discovery and custom Git sources. Custom sources must
still be pinned and provide registry-compatible build and launch metadata.

## D17 — Go ACP client: generated schema types, internal transport

**Choice:** Generate Go wire types from the official schema shipped by the exactly pinned
`@agentclientprotocol/sdk` 1.2.1 package and keep a small internal newline-JSON-RPC
subprocess transport. The schema package version and its SHA-256 are recorded in
`internal/acp/schema.lock`; ACP's negotiated wire protocol remains separately pinned at 1.

**Why:** The available community Go SDKs do not yet demonstrate the complete ACP client
role Tandem needs, especially agent-initiated client-service requests for `fs/*`,
`terminal/*`, and permissions. Generating types avoids a broad hand-maintained schema copy,
while the internal transport is small enough to test exhaustively and keeps session/event
mapping behind Tandem's adapter boundary.

**Regeneration:** Install the daemon's locked npm dependencies (`npm ci` in `daemon`), then
run `go generate ./internal/acp`. Generation consumes the SDK's `schema/schema.json`; the
checked-in generated header exposes both the source package version and schema digest.
