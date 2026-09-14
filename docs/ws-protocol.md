# Browser ↔ Daemon WebSocket protocol

Multiplexed by `sessionId` + channel, with a **monotonic `seq` per session** so reconnect can
replay. JSON envelopes; binary frames for `raw_pty` and browser screencast. The browser
speaks Tandem's **normalized shapes** (see [`agent-adapter.md`](agent-adapter.md)) — the
daemon has already translated ACP away, so the UI is adapter-agnostic.

`sessionId` is Tandem's dock-session identity. `externalSessionId` is the
upstream harness session used for resume. During the rolling rename, the daemon
accepts `agentId` as an input alias and emits it alongside `sessionId` for
older browser and federation peers.

## Auth handshake (D15)

The daemon serves HTTP **and** WS on one port (default `127.0.0.1:7717`). A WS connection
**must present the bearer token as a query param**: `ws://<host>/?token=<token>`. This is the
chosen mechanism (over a first-message `auth` frame) so the gate runs before any protocol
state exists. An unauthenticated or wrong-token socket is **accepted then immediately closed
with code `4401`** (so the client sees a clean, distinguishable close rather than a raw
transport error). The token is generated on first run into `$TANDEM_HOME/token` (mode
`0600`); the daemon prints a bootstrap URL `http://<host>:<port>/#t=<token>` whose fragment
the UI reads once and stores. The native daemon serves its embedded UI by default;
`TANDEM_UI_DIR` optionally overrides those assets for frontend development. A build made
with the `tandem_dev` tag has no embedded assets and shows a placeholder without an override.

### Transcript attachments

Files are uploaded before prompting. Both routes require
`Authorization: Bearer <token>`:

```text
POST /api/agents/:sessionId/assets
Content-Type: <file MIME type>
X-File-Name: <percent-encoded original name>
X-Store-Image-Asset: true (when the client needs an image prompt asset)

201 { "upload": { "path": "report.pdf", "name": "report.pdf", "size": 1234 }, "asset": { "assetId": "<sha256>", "mimeType": "image/png", "name": "shot.png", "size": 1234 } }

GET /api/agents/:sessionId/assets/:assetId
```

Images are stored outside the repository as content-addressed assets under
`$TANDEM_HOME/assets` by default. Set `{"uploads":{"directory":".tandem/uploads"}}`
in `.tandem/settings.json` to also retain uploaded images in a repository-relative
directory. Other file uploads are saved to the repository root by default. Tandem
avoids overwriting an existing filename by adding ` (1)`, ` (2)`, and so on. Agents
are told the resulting paths with the prompt and are asked to gitignore a dedicated
upload directory unless the user wants uploaded files committed. SQLite records their
per-agent ownership. Physical deduplication never grants cross-agent access.
Bytes are signature-sniffed, declared MIME must agree, malformed or excessive
decoded dimensions are rejected, and SVG is not accepted. Limits are 10 MiB per
image, four images and 20 MiB total image data per prompt. Assets are retained
with durable transcript/session history; there is currently no eager GC.

### Implementation status (Phase 2)

Implemented and de-risked: `subscribe`/`unsubscribe` (per-agent `channels` + `sinceSeq`),
`prompt`, `input`, `resize`, `permission_response`, `interrupt`, `set_mode`,
`set_config_option`, `spawn_agent`, `rename_agent`,
`close_agent` (now backed by the real WorkspaceManager — see below), `list_dirs` (Phase 2,
repo discovery for the quick-spawn palette), and — Phase 5 — the browser channel:
`browser_control` (grab/release, real), `browser_input`, `browser_frame`, `browser_state`
(see below). `merge_back` still returns an **error `ack`** (`"merge_back not implemented
yet"`) pending the diff/merge UI. Multiple concurrent clients and multiplexed multi-agent
subscriptions on one socket are supported.

### Host-scoped federation

The browser may receive a host catalog and may qualify repository, agent-catalog,
spawn-option, and spawn requests with a host identity. Omitting the host retains local,
single-daemon behavior. The catalog is flattened for selection while `parentId`, `route`,
and `depth` retain its rooted topology; descendant IDs are opaque route-scoped addresses.
Remote agent summaries carry their leaf host identity and use a master-side namespaced
agent ID (`fed~<hostId>~<sessionId>`; the older base64 form is still accepted so an open
tab survives an upgrade). Subsequent ordinary agent commands use that ID and are routed
one hop at a time. Live transcript, terminal, approval, lifecycle, and browser-channel
traffic is proxied without exposing ACP or CDP details to the UI.

Slave registration is a daemon-to-daemon protocol, not browser bearer-token
authentication. First contact remains pending until the master accepts its system
notification; later connections authenticate with the durable registration credential.
See [`federation.md`](federation.md). Host-scoped `list_sessions`, `search_sessions`, and
`resume_session` execute against the selected host's own history index and annotate their
results with that host identity.

### `list_sessions` / `search_sessions` / `resume_session`

`list_sessions` returns a catalog combining Tandem-owned sessions, external sessions
exposed by ACP adapters, and transcript-imported history. Entries are deduplicated by
`(agent, session id)`, with live Tandem, closed Tandem, ACP, then history precedence.
Each entry carries `repo` / `repoPath` — the source repository the Resume palette groups
by, taken from a Tandem session's workspace or resolved from the working directory
otherwise. The response also reports each adapter's enumeration support.

`search_sessions` performs literal token-prefix FTS over the normalized local transcript
index. Its correlated `session_search` response groups up to `maxHitsPerSession` excerpts
under each resumable session, including structured highlight ranges and adjacent-entry
context. User input is tokenized server-side and is never passed through as raw FTS
`MATCH` syntax.

`resume_session` focuses an already-live session, restores a closed Tandem session
(reattaching its worktree when necessary), or imports an external session. `agent` and
`cwd` are required only for an external session. Raw PTY agents have no resumable-session
concept and external enumeration remains optional in ACP.

`enter_terminal {sessionId, interrupt?}` and `leave_terminal {sessionId}` drive the ACP/CLI
handoff. An active ACP turn rejects an implicit handoff with `agent_busy`; `interrupt:true`
performs graceful cancellation first. `control_state` events and the authoritative
`SessionSummary.controlMode` / `snapshot.controlMode` expose `transcript | switching | terminal`
to every client. CLI exit automatically performs `leave_terminal` daemon-side.

`shell_open`, `shell_input`, `shell_resize`, and `shell_close` drive the independent user
shell shown in the Terminal tab. `shell_open` lazily starts `$SHELL` in the agent worktree
and only resizes when it is already running. Output and exit are durable `shell_pty` and
`shell_exit` events, separate from the agent CLI's `raw_pty` stream.

### `spawn_agent` / `close_agent` (Phase 2: real workspaces)

`spawn_agent`'s `workspace.kind:'worktree'` provisions a real `git worktree` (by default a
context-qualified `tandem/<feature>/<name>` branch) under
`$TANDEM_HOME/worktrees/<repo>/<agent>/`; `kind:'existing'` validates only that the dir
exists. A failed provision (missing dir, git error) rejects the `spawn_agent` call — no
agent is registered — with a structured error message (see below).

Naming a directory another open agent already works in is allowed and **shares** it: if
that occupant holds a Tandem worktree, the new agent inherits its whole workspace
descriptor (repo, branch, integration target) instead of degrading to an anonymous
existing dir. The UI warns and names the other occupants; see spawn-and-workspaces.md's
"Sharing a worktree".

`spawn_agent`'s `spec.handoffFrom` is an agent id whose transcript is rendered into the new
agent's first user message (`internal/handoff`), at the detail `spec.handoffMode` selects
(`full`, the default, or `brief`). The rendering is mechanical — no model is invoked — and
the result is dispatched as a prompt rather than persisted onto the spec. An unknown or
transcript-less source rejects the spawn before anything is provisioned.

`close_agent { force?, deinitSubmodules? }` removes the worktree checkout but **keeps the branch**. If the
worktree has uncommitted changes and `force` isn't set, the close is **refused** — the agent
keeps running, no `agent_closed` is broadcast — with a `dirty_worktree` error; `force:true`
overrides and removes the checkout anyway. `kind:'existing'` workspaces just detach (no-op). A worktree
shared with other open agents is also kept: it is removed with its last occupant.
`get_close_preview` returns `cohabitants`, the names of the other open agents in the same
directory, so the UI can say so before the close.
If Git refuses removal because initialized submodules are present, Tandem returns
`submodules_block_worktree_removal`. A subsequent `force:true, deinitSubmodules:true` request
explicitly runs `git submodule deinit -f --all` before retrying; callers must show the
submodule preflight because this can discard uncommitted submodule changes.

**Structured errors:** today `ack.error` is still a plain string (no wire shape change), but
WorkspaceManager errors are conventionally prefixed `"<code>: <detail>"` so a client can
`error.split(':')[0]` to branch on the reason. Codes in use: `no_such_dir`, `dirty_worktree`, `worktree_exists`,
`ref_not_found`, `invalid_branch`, `branch_exists`, `branch_missing`, and
`branch_checked_out`.

### `list_dirs` (Phase 2: spawn-palette repo discovery)

```ts
{ t: 'list_dirs'; corrId?: string }                    // → Browser
{ t: 'dirs'; corrId?: string; dirs: RepoInfo[] }        // → Daemon

interface RepoInfo { path: string; name: string; currentBranch: string; dirty: boolean; hasLiveAgent: boolean }
```

Scans `TANDEM_PROJECT_ROOTS` (depth configurable via `TANDEM_DIR_SCAN_DEPTH`, default 1) for
git repos, skipping `node_modules`/`dist`/etc. `hasLiveAgent` is true if any live agent's
worktree or existing-dir workspace is tied to that repo.

### `list_git_refs` (feature-branch context picker)

```ts
{ t: 'list_git_refs'; repo: string; corrId?: string }
// →
{ t: 'git_refs'; corrId?: string; refs?: GitRefInfo[]; error?: string }
```

This read-only request returns canonical local/remote/tag refs, immutable commit IDs,
current/default markers, upstream ahead/behind counts, and the path of any worktree currently
checking out a local branch. It never runs `git fetch`. The Advanced spawn UI fuzzy-filters
the result and sends explicit `branchMode`, `source`, and `integration` workspace fields.

New worktrees resolve `source.ref` to `source.commit`, create the private branch from that
OID, and use `integration.ref` for rail state and close previews. Legacy `baseRef` requests
remain accepted.

### `list_agents` (Phase 4: rail discovery)

```ts
{ t: 'list_agents'; corrId?: string }                   // → Browser
{ t: 'agents'; corrId?: string; sessions: SessionSummary[] } // → Daemon

interface SessionSummary {
  id: string; name: string;
  workspace: { kind: 'worktree'|'existing'; repo: string; repoPath: string; branch: string; cwd: string;
               gitState?: 'dirty'|'ahead'|'behind'|'diverged'|'merged'|'synced'|'target_missing';
               ahead?: number; behind?: number; targetRef?: string; startCommit?: string };
  status: SessionStatus; pendingApprovals: number;
}
```

Added for the UI's left rail. The `snapshot` carries a session's `status` + `pendingApprovals`
but neither its **name** nor **workspace** — a freshly loaded or reconnecting client (and,
critically, one attaching after a **daemon restart** that restored agents) has no other way to
learn which agents exist or how to label them. The UI calls `list_agents` on every (re)connect,
reconciles the rail, then `subscribe`s to each agent with its tracked `sinceSeq`. `repo` is a
display basename; `repoPath` is the source-repo path used for "sibling" spawns.

### `list_agent_catalog` (configurable agents and profiles)

```ts
{ t: 'list_agent_catalog'; corrId?: string }
// →
{ t: 'agent_catalog'; corrId?: string; catalog: AgentCatalog }
```

The catalog is the daemon-normalized view of built-in plus `$TANDEM_HOME/config.yml`
agent definitions and profiles. The spawn palette uses its ACP/Terminal capability flags
instead of hard-coding agent names. Launch commands and environment values stay daemon-side.

## Messages

All client messages accept an optional `corrId` echoed back on the matching `ack`.

```ts
// ---- Browser → Daemon ----
type ClientMsg =
  | { t: 'subscribe';   sessionId: string; channels?: Channel[]; sinceSeq?: number } // channels omitted = all
  | { t: 'unsubscribe'; sessionId: string; channels?: Channel[] }
  | { t: 'prompt';      sessionId: string; text?: string; blocks?: PromptBlock[] }
  | { t: 'steer';       sessionId: string; text?: string; blocks?: PromptBlock[] }
  | { t: 'remove_queued_prompt'; sessionId: string; promptId: string }
  | { t: 'clear_prompt_queue'; sessionId: string }
  | { t: 'interrupt_and_clear_queue'; sessionId: string }
  | { t: 'input';       sessionId: string; bytesB64: string }            // → adapter.sendInput
  | { t: 'resize';      sessionId: string; cols: number; rows: number }  // → adapter.resize (pty)
  | { t: 'permission_response'; sessionId: string; reqId: string; optionId: string }
  | { t: 'interrupt';   sessionId: string }                              // → session/cancel; pending perms → cancelled
  | { t: 'set_mode'; sessionId: string; modeId: string }                 // ACP session/set_mode
  | { t: 'set_config_option'; sessionId: string; configId: string; value: string | boolean }
  | { t: 'spawn_agent'; spec: SpawnSpec }                               // see spawn-and-workspaces.md
  | { t: 'rename_agent'; sessionId: string; name: string }                // display name only; stable id/worktree unchanged
  | { t: 'get_spawn_options'; agent: string; profile?: string; acpArgs?: string[]; cwd: string }
  | { t: 'close_agent'; sessionId: string; force?: boolean }             // teardown: keep branch, drop checkout
  | { t: 'merge_back';  sessionId: string; mode: 'merge'|'pr' }          // error ack for now (no diff/merge UI yet)
  | { t: 'browser_control'; sessionId: string; action: 'grab'|'release' } // Phase 5: flips the control-owner token
  | { t: 'restart_browser'; sessionId: string; snapshotId?: string } // starts/replaces the browser; omitted snapshotId means fresh state
  | { t: 'list_system_notifications' }
  | { t: 'system_notification_action'; notificationId: string; action: string }
  | { t: 'browser_input'; sessionId: string; event: BrowserInputWire }   // Phase 5: user mouse/key/wheel (owner=user only)
  | { t: 'list_dirs' }                                                  // Phase 2: repo discovery, see below
  | { t: 'list_agents' }                                                // Phase 4: rail discovery, see below
  | { t: 'list_agent_catalog' }                                         // configured definitions + profiles
  | { t: 'list_sessions' }                                              // resumable-session catalog
  | { t: 'search_sessions'; query: string; limit?: number; maxHitsPerSession?: number }
  | { t: 'resume_session'; externalSessionId: string; source: 'tandem'|'acp'|'history'; agent: string; cwd?: string }
  | { t: 'enter_terminal'; sessionId: string; interrupt?: boolean }
  | { t: 'leave_terminal'; sessionId: string }
  | { t: 'shell_open'; sessionId: string; cols: number; rows: number }
  | { t: 'shell_input'; sessionId: string; bytesB64: string }
  | { t: 'shell_resize'; sessionId: string; cols: number; rows: number }
  | { t: 'shell_close'; sessionId: string }
  | { t: 'set_audio_position'; sessionId: string; seq: number; positionMs: number } // seq: 0 clears (see below)
  | { t: 'render_message_audio'; sessionId: string; seq: number };       // federation-only, see below

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
  key?: string; code?: string; keyCode?: number; autoRepeat?: boolean; text?: string;
}
// (+ optional corrId on every variant)

type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

// Successful set_mode/set_config_option changes are persisted in the agent spec and
// reapplied after daemon restart, ACP session reload, and Transcript/Terminal handoff.

// ---- Daemon → Browser ----
// NOTE (Phase 1 deviation): `snapshot.transcript` carries `{ seq, event }[]`, not bare
// AgentEvent[], so a reconnecting client can checkpoint per event. `terminals` / `browser`
// snapshot fields and the dedicated `pty_frame`/`browser_frame` binary frames are Phase 2/3;
// raw_pty and shell_pty currently ride inside normal `event` messages with `dataB64`
// (base64 in JSON — a future binary-framing optimization). `ack` gained
// `sessionId`/`error`; `agent_closed` is emitted to every subscriber when an agent is torn down.
type ServerMsg =
  | { t: 'snapshot'; sessionId: string; seq: number;                     // reconstruct on reconnect
                     transcript: { seq: number; event: WireEvent }[];
                     status: SessionStatus; controlMode: ControlMode;
                     pendingApprovals: Approval[];
                     audioReadySeqs?: number[];                        // deprecated bare form, kept for compat
                     audioReady?: { seq: number; durationMs: number }[]; // durationMs 0 = unknown
                     audioPosition?: { seq: number; positionMs: number; updatedAt: number } } // omitted = nothing stored
  | { t: 'audio_position'; sessionId: string; seq: number; positionMs: number; updatedAt: number } // cross-device sync only; never sent back to the sender
  | { t: 'event';    sessionId: string; seq: number; event: WireEvent }  // live tail (monotonic)
  | { t: 'ack';      corrId?: string; sessionId?: string; error?: string }
  | { t: 'agent_closed'; sessionId: string }
  | { t: 'agents';   sessions: SessionSummary[] }                         // Phase 4: reply to list_agents
  | { t: 'agent_catalog'; catalog: AgentCatalog }                       // reply to list_agent_catalog
  | { t: 'system_notifications'; notifications: SystemNotification[] }  // daemon-owned operational notifications
  | { t: 'dirs';     dirs: RepoInfo[] }                                 // reply to list_dirs
  | { t: 'spawn_options'; options?: SpawnOptions; error?: string }      // reply to get_spawn_options
  | { t: 'sessions'; catalog: ResumeCatalog }                           // reply to list_sessions
  | { t: 'session_search'; query?: string; results?: SessionSearchResult[]; error?: string }
  // Phase 5 browser channel (only sent to subscribers of that agent's 'browser' channel):
  | { t: 'browser_frame'; sessionId: string; dataB64: string;             // CDP screencast JPEG
      meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; sessionId: string; active: boolean;             // lifecycle + wheel
      controlOwner: 'agent'|'user' }
  | { t: 'message_audio'; sessionId: string; seq: number;                 // reply to render_message_audio
      mimeType?: string; data?: string; error?: string };               // data: base64 clip bytes

// WireEvent = AgentEvent plus:
//   { kind:'raw_pty', dataB64: string }   // agent CLI / native PTY agent
//   { kind:'shell_pty', dataB64: string } // independent user worktree shell
//   { kind:'shell_exit', message: string }
```

`snapshot.audioReadySeqs` lists the seqs with a durably cached rendered clip (bytes
still fetched from the authenticated audio route). `audioReady` carries the same seqs
plus each clip's playback duration in milliseconds, computed once server-side from the
rendered bytes (`internal/voice.Duration`) so the UI can space a timeline's tick marks
without downloading audio; `durationMs: 0` means unknown (an unparseable format, or a
clip written before duration computation existed and not yet re-read). Both fields are
emitted together for backward compatibility; new clients should prefer `audioReady`.
The live `audio_state` event carries the same optional `durationMs` for a clip that
just finished rendering.

Browsers fetch clip bytes from the authenticated HTTP audio route
(`POST /api/agents/{id}/messages/{seq}/audio`), not over this socket. The
`render_message_audio` command exists for federation: a federated agent's transcript
lives on the host that owns it, so the master renders the clip there over the tunnel —
which carries protocol JSON only, so the bytes come back base64-encoded in
`message_audio` like `raw_pty` and `browser_frame` — and then serves them from its own
audio route under the namespaced agent ID. A UI needs no federation-specific audio code.

The audio player's playback position is daemon-owned so it survives a session switch or
a closed tab: `set_audio_position` writes one row per agent (last-write-wins, not
per-message) to `store.AudioPosition`, keyed by the section (`seq`, a transcript message
seq) and the offset within it (`positionMs`); `seq: 0` is a deliberate reset ("no active
section" — playlist finished or explicitly cleared) and deletes the stored row rather
than persisting a zero seq. `snapshot.audioPosition` is omitted entirely when nothing is
stored for that agent. The client is expected to throttle `set_audio_position` during
playback (roughly one every 5s, plus on pause/section-change/tab-hide) since every call
is a durable write; the daemon does not itself rate-limit it. Every *other* connection
subscribed to the same agent gets a live `audio_position` broadcast on each write, so a
second device's player can follow along — the sender never gets it echoed back, since it
already knows the position it just wrote and an echo would just fight its own clock.

Legacy `text` prompts normalize to one text block. Block order is preserved into
ACP `session/prompt`; only at that boundary does the daemon resolve an owned asset
to ACP's inline `{type:'image', mimeType, data:<base64>}` block. The ACP
`promptCapabilities.image` value is persisted as a
`{kind:'prompt_capabilities', image:boolean}` event. An image prompt is rejected
before logging a user message or starting a turn when that capability is false.

Context usage is a durable `{kind:'usage', used, size, cost?, updatedAt}` event.
`updatedAt` is daemon-owned (epoch ms): the daemon only advances it when the
agent reports *different* totals, so re-reported identical usage (turn
boundaries, resumes, idle pings) leaves the "updated Xm ago" age alone. Because
the stamp is persisted with the event, the UI rebuilds the usage meter and its
age from the replayed transcript — the same for every client, across reconnects,
page loads, and daemon restarts.

Prompts submitted during an active turn enter a daemon-owned FIFO rather than
overlapping ACP `session/prompt` calls. The prompt acknowledgement includes a
`promptId`, `disposition: 'started'|'queued'`, and a one-based queue `position`.
Queue changes are durable `prompt_queued`, `prompt_started`, and `prompt_removed`
events; snapshots include the authoritative `queuedPrompts` array, and queue-bearing
replays end with a `prompt_queue` state message. A normal `interrupt` cancels only
the active turn and preserves the queue.

Agents may additionally advertise the ACP steering extension through
`initialize` result `_meta.steering.supported`. Tandem persists that negotiation
as `{kind:'steering_capabilities', supported:boolean}`. While a turn is active,
the browser can send `{t:'steer', sessionId, text|blocks}` to inject the content
with `_session/steering`; this bypasses the FIFO. Tandem requests the
`promptRequired` idle behavior so a completion race never creates a detached
turn, and leaves the draft available for an explicit send or queue in that case.

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
- **`pty`** — base64 `raw_pty` events for TUI/CLI agents plus distinct `shell_pty` and
  `shell_exit` events for the user's worktree shell.
- **`terminals`** — client-owned terminal output for structured agents.
- **`browser`** (Phase 5, implemented) — `browser_frame` CDP screencast frames (JSON+base64;
  binary framing is a future optimization, same as raw_pty) and `browser_state`
  (active + controlOwner). Subscribing to this channel is what starts the screencast — the
  UI subscribes it **only for the focused agent with the Browser pane open** (bandwidth
  rule); the daemon stops the cast when the last browser-channel subscriber unsubscribes.
  Subscribing does **not** provision a browser; `active:false` is reported until the agent's
  first browser use. `browser_control` grab/release flips the control-owner token
  (grab dismisses its attention card/banner; release resolves the pending takeover);
  `browser_input` forwards user input while
  owner=user. **`takeover_request` events ride the `transcript` channel** (like
  `permission_request`) so the attention rail sees them even when no Browser pane is open.
  See [`browser.md`](browser.md).
