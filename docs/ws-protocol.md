# Browser ↔ Daemon WebSocket protocol

Multiplexed by `agentId` + channel, with a **monotonic `seq` per agent** so reconnect can
replay. JSON envelopes; binary frames for `raw_pty` and browser screencast. The browser
speaks Tandem's **normalized shapes** (see [`agent-adapter.md`](agent-adapter.md)) — the
daemon has already translated ACP away, so the UI is adapter-agnostic.

## Auth handshake (D15)

The daemon serves HTTP **and** WS on one port (default `127.0.0.1:7717`). A WS connection
**must present the bearer token as a query param**: `ws://<host>/?token=<token>`. This is the
chosen mechanism (over a first-message `auth` frame) so the gate runs before any protocol
state exists. An unauthenticated or wrong-token socket is **accepted then immediately closed
with code `4401`** (so the client sees a clean, distinguishable close rather than a raw
transport error). The token is generated on first run into `$TANDEM_HOME/token` (mode
`0600`); the daemon prints a bootstrap URL `http://<host>:<port>/#t=<token>` whose fragment
the UI reads once and stores. Static UI assets are served from `TANDEM_UI_DIR` if set, else a
placeholder page.

### Transcript image assets

Image bytes are uploaded before prompting. Both routes require
`Authorization: Bearer <token>`:

```text
POST /api/agents/:agentId/assets
Content-Type: image/png | image/jpeg | image/gif | image/webp
X-File-Name: <percent-encoded original name>

201 { "asset": { "assetId": "<sha256>", "mimeType": "image/png", "name": "shot.png", "size": 1234 } }

GET /api/agents/:agentId/assets/:assetId
```

Assets are content-addressed under `$TANDEM_HOME/assets`; SQLite records their
per-agent ownership. Physical deduplication never grants cross-agent access.
Bytes are signature-sniffed, declared MIME must agree, malformed or excessive
decoded dimensions are rejected, and SVG is not accepted. Limits are 10 MiB per
image, four images and 20 MiB total image data per prompt. Assets are retained
with durable transcript/session history; there is currently no eager GC.

### Implementation status (Phase 2)

Implemented and de-risked: `subscribe`/`unsubscribe` (per-agent `channels` + `sinceSeq`),
`prompt`, `input`, `resize`, `permission_response`, `interrupt`, `spawn_agent`,
`close_agent` (now backed by the real WorkspaceManager — see below), `list_dirs` (Phase 2,
repo discovery for the quick-spawn palette), and — Phase 5 — the browser channel:
`browser_control` (grab/release, real), `browser_input`, `browser_frame`, `browser_state`
(see below). `merge_back` still returns an **error `ack`** (`"merge_back not implemented
yet"`) pending the diff/merge UI. Multiple concurrent clients and multiplexed multi-agent
subscriptions on one socket are supported.

### `list_sessions` / `resume_session`

`list_sessions` returns a catalog combining Tandem-owned ACP sessions with external
sessions exposed by adapters that advertise ACP `sessionCapabilities.list`. Entries are
deduplicated by session id, with the richer Tandem record taking precedence. The response
also reports each adapter's enumeration support so the UI can identify a partial catalog.

`resume_session` focuses an already-live session, restores a closed Tandem session
(reattaching its worktree when necessary), or imports an external session. `agent` and
`cwd` are required only for an external session. Raw PTY agents have no resumable-session
concept and external enumeration remains optional in ACP.

`enter_terminal {agentId, interrupt?}` and `leave_terminal {agentId}` drive the ACP/CLI
handoff. An active ACP turn rejects an implicit handoff with `agent_busy`; `interrupt:true`
performs graceful cancellation first. `control_state` events and the authoritative
`AgentSummary.controlMode` / `snapshot.controlMode` expose `transcript | switching | terminal`
to every client. CLI exit automatically performs `leave_terminal` daemon-side.

### `spawn_agent` / `close_agent` (Phase 2: real workspaces)

`spawn_agent`'s `workspace.kind:'worktree'` now provisions a real `git worktree` (branch
`tandem/<name>`, based on `baseRef` or the repo's current HEAD) under
`$TANDEM_HOME/worktrees/<repo>/<agent>/`; `kind:'existing'` validates the dir exists and
isn't already occupied by a live agent. A failed provision (missing dir, git error, or a
`kind:'existing'` collision) rejects the `spawn_agent` call — no agent is registered — with
a structured error message (see below).

`close_agent { force? }` removes the worktree checkout but **keeps the branch**. If the
worktree has uncommitted changes and `force` isn't set, the close is **refused** — the agent
keeps running, no `agent_closed` is broadcast — with a `dirty_worktree` error; `force:true`
overrides and removes the checkout anyway. `kind:'existing'` workspaces just detach (no-op).

**Structured errors:** today `ack.error` is still a plain string (no wire shape change), but
WorkspaceManager errors are conventionally prefixed `"<code>: <detail>"` so a client can
`error.split(':')[0]` to branch on the reason. Codes in use: `no_such_dir`, `dir_occupied`
(the UI's cue to offer "open a worktree instead" / "attach to the existing agent" — see
spawn-and-workspaces.md's Collision section), `dirty_worktree`, `worktree_exists`.

### `list_dirs` (Phase 2: spawn-palette repo discovery)

```ts
{ t: 'list_dirs'; corrId?: string }                    // → Browser
{ t: 'dirs'; corrId?: string; dirs: RepoInfo[] }        // → Daemon

interface RepoInfo { path: string; name: string; currentBranch: string; dirty: boolean; hasLiveAgent: boolean }
```

Scans `TANDEM_PROJECT_ROOTS` (depth configurable via `TANDEM_DIR_SCAN_DEPTH`, default 1) for
git repos, skipping `node_modules`/`dist`/etc. `hasLiveAgent` is true if any live agent's
worktree or existing-dir workspace is tied to that repo.

### `list_agents` (Phase 4: rail discovery)

```ts
{ t: 'list_agents'; corrId?: string }                   // → Browser
{ t: 'agents'; corrId?: string; agents: AgentSummary[] } // → Daemon

interface AgentSummary {
  id: string; name: string;
  workspace: { kind: 'worktree'|'existing'; repo: string; repoPath: string; branch: string; cwd: string };
  status: AgentStatus; pendingApprovals: number;
}
```

Added for the UI's left rail. The `snapshot` carries an agent's `status` + `pendingApprovals`
but neither its **name** nor **workspace** — a freshly loaded or reconnecting client (and,
critically, one attaching after a **daemon restart** that restored agents) has no other way to
learn which agents exist or how to label them. The UI calls `list_agents` on every (re)connect,
reconciles the rail, then `subscribe`s to each agent with its tracked `sinceSeq`. `repo` is a
display basename; `repoPath` is the source-repo path used for "sibling" spawns.

## Messages

All client messages accept an optional `corrId` echoed back on the matching `ack`.

```ts
// ---- Browser → Daemon ----
type ClientMsg =
  | { t: 'subscribe';   agentId: string; channels?: Channel[]; sinceSeq?: number } // channels omitted = all
  | { t: 'unsubscribe'; agentId: string; channels?: Channel[] }
  | { t: 'prompt';      agentId: string; text?: string; blocks?: PromptBlock[] }
  | { t: 'input';       agentId: string; bytesB64: string }            // → adapter.sendInput
  | { t: 'resize';      agentId: string; cols: number; rows: number }  // → adapter.resize (pty)
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string }
  | { t: 'interrupt';   agentId: string }                              // → session/cancel; pending perms → cancelled
  | { t: 'spawn_agent'; spec: SpawnSpec }                               // see spawn-and-workspaces.md
  | { t: 'close_agent'; agentId: string; force?: boolean }             // teardown: keep branch, drop checkout
  | { t: 'merge_back';  agentId: string; mode: 'merge'|'pr' }          // error ack for now (no diff/merge UI yet)
  | { t: 'browser_control'; agentId: string; action: 'grab'|'release' } // Phase 5: flips the control-owner token
  | { t: 'browser_input'; agentId: string; event: BrowserInputWire }   // Phase 5: user mouse/key/wheel (owner=user only)
  | { t: 'list_dirs' }                                                  // Phase 2: repo discovery, see below
  | { t: 'list_agents' }                                                // Phase 4: rail discovery, see below
  | { t: 'list_sessions' }                                              // resumable-session catalog
  | { t: 'resume_session'; sessionId: string; agent?: string; cwd?: string }
  | { t: 'enter_terminal'; agentId: string; interrupt?: boolean }
  | { t: 'leave_terminal'; agentId: string };

type PromptBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; assetId: string; mimeType: string; name?: string };

// Phase 5: the normalized user-input event carried by browser_input — mapped to CDP
// Input.dispatchMouseEvent / dispatchKeyEvent / insertText daemon-side. x/y are in the
// browser's device coordinates (the UI maps canvas → device using browser_frame.meta).
interface BrowserInputWire {
  kind: 'mousemove'|'mousedown'|'mouseup'|'click'|'wheel'|'keydown'|'keyup'|'text';
  x?: number; y?: number; button?: 'left'|'middle'|'right'; buttons?: number;
  clickCount?: number; deltaX?: number; deltaY?: number;
  key?: string; code?: string; keyCode?: number; text?: string;
}
// (+ optional corrId on every variant)

type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

// ---- Daemon → Browser ----
// NOTE (Phase 1 deviation): `snapshot.transcript` carries `{ seq, event }[]`, not bare
// AgentEvent[], so a reconnecting client can checkpoint per event. `terminals` / `browser`
// snapshot fields and the dedicated `pty_frame`/`browser_frame` binary frames are Phase 2/3;
// raw_pty currently rides inside a normal `event` as `{ kind:'raw_pty', dataB64 }` (base64 in
// JSON — a future binary-framing optimization). `ack` gained `agentId`/`error`; `agent_closed`
// is emitted to every subscriber when an agent is torn down.
type ServerMsg =
  | { t: 'snapshot'; agentId: string; seq: number;                     // reconstruct on reconnect
                     transcript: { seq: number; event: WireEvent }[];
                     status: AgentStatus; pendingApprovals: Approval[] }
  | { t: 'event';    agentId: string; seq: number; event: WireEvent }  // live tail (monotonic)
  | { t: 'ack';      corrId?: string; agentId?: string; error?: string }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents';   agents: AgentSummary[] }                           // Phase 4: reply to list_agents
  | { t: 'dirs';     dirs: RepoInfo[] }                                 // reply to list_dirs
  | { t: 'sessions'; catalog: ResumeCatalog }                           // reply to list_sessions
  // Phase 5 browser channel (only sent to subscribers of that agent's 'browser' channel):
  | { t: 'browser_frame'; agentId: string; dataB64: string;             // CDP screencast JPEG
      meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean;             // lifecycle + wheel
      controlOwner: 'agent'|'user' };

// WireEvent = AgentEvent, except raw_pty's bytes become { kind:'raw_pty', dataB64: string }.
```

Legacy `text` prompts normalize to one text block. Block order is preserved into
ACP `session/prompt`; only at that boundary does the daemon resolve an owned asset
to ACP's inline `{type:'image', mimeType, data:<base64>}` block. The ACP
`promptCapabilities.image` value is persisted as a
`{kind:'prompt_capabilities', image:boolean}` event. An image prompt is rejected
before logging a user message or starting a turn when that capability is false.

## End-to-end mapping of ACP

- **`session/update`** → daemon emits `{ t: 'event', event: AgentEvent }` on the agent's
  `transcript` channel.
- **`session/request_permission`** → daemon emits a `permission_request` **event** *and*
  records it in that agent's `pendingApprovals`, so it appears in the always-on global
  approvals rail even for unfocused agents. The browser answers with `permission_response`;
  the daemon calls `adapter.respondPermission`.
- **`terminal/*`** output → `terminal_output` events on the `terminals` channel, buffered by
  the daemon's `TerminalHost`.

## Reconnect / replay

Each agent's event stream carries a monotonic `seq`. The browser tracks the last `seq` it
saw per subscribed agent.

1. On reconnect the browser re-`subscribe`s with `sinceSeq`.
2. If the daemon still holds events past that `seq`, it replays them as `event` frames. The
   in-memory ring is the hot path; older ranges are served from **SQLite** (D14), so replay
   stays gapless for any range the DB still retains — including **history from before a daemon
   restart**, which lives only in SQLite (the ring starts empty in the new process).
3. Only if even SQLite can't cover the checkpoint (events pruned) — or terminal output was
   truncated past `outputByteLimit` — the daemon sends a fresh `snapshot` instead.

Either way the UI is made whole again; the agents never noticed the disconnect. This is the
concrete payoff of the "daemon owns all state" invariant.

## Channel notes

- **`transcript`** — normalized `AgentEvent`s (message/thought chunks, tool calls, plans,
  permission requests). Kept always-on for subscribed agents.
- **`status`** — kept always-on even for *unfocused* agents so the left rail and approvals
  queue stay live.
- **`pty`** — binary `raw_pty` frames for xterm.js (TUI-only agents, user shell).
- **`terminals`** — client-owned terminal output for structured agents.
- **`browser`** (Phase 5, implemented) — `browser_frame` CDP screencast frames (JSON+base64;
  binary framing is a future optimization, same as raw_pty) and `browser_state`
  (active + controlOwner). Subscribing to this channel is what starts the screencast — the
  UI subscribes it **only for the focused agent with the Browser pane open** (bandwidth
  rule); the daemon stops the cast when the last browser-channel subscriber unsubscribes.
  Subscribing does **not** provision a browser; `active:false` is reported until the agent's
  first browser use. `browser_control` grab/release flips the control-owner token
  (release also resolves a pending takeover); `browser_input` forwards user input while
  owner=user. **`takeover_request` events ride the `transcript` channel** (like
  `permission_request`) so the attention rail sees them even when no Browser pane is open.
  See [`browser.md`](browser.md).
