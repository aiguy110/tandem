# Browser ↔ Daemon WebSocket protocol

Multiplexed by `agentId` + channel, with a **monotonic `seq` per agent** so reconnect can
replay. JSON envelopes; binary frames for `raw_pty` and browser screencast. The browser
speaks Tandem's **normalized shapes** (see [`agent-adapter.md`](agent-adapter.md)) — the
daemon has already translated ACP away, so the UI is adapter-agnostic.

## Messages

```ts
// ---- Browser → Daemon ----
type ClientMsg =
  | { t: 'subscribe';   agentId: string; channels: Channel[]; sinceSeq?: number }
  | { t: 'unsubscribe'; agentId: string; channels: Channel[] }
  | { t: 'prompt';      agentId: string; text: string }
  | { t: 'input';       agentId: string; bytesB64: string }            // → adapter.sendInput
  | { t: 'resize';      agentId: string; cols: number; rows: number }  // → adapter.resize (pty)
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string }
  | { t: 'interrupt';   agentId: string }
  | { t: 'spawn_agent'; spec: SpawnSpec }                               // see spawn-and-workspaces.md
  | { t: 'close_agent'; agentId: string; force?: boolean }             // teardown: keep branch, drop checkout
  | { t: 'merge_back';  agentId: string; mode: 'merge'|'pr' }          // explicit, human-initiated
  | { t: 'browser_control'; agentId: string; action: 'grab'|'release' }; // control-owner token

type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

// ---- Daemon → Browser ----
type ServerMsg =
  | { t: 'snapshot'; agentId: string; seq: number;                      // reconstruct on reconnect
                     transcript: AgentEvent[]; terminals: TermSnapshot[];
                     status: AgentStatus; pendingApprovals: Approval[];
                     browser?: { sessionId: string; controlOwner: 'agent'|'user' } }
  | { t: 'event';    agentId: string; seq: number; event: AgentEvent }  // live tail (monotonic)
  | { t: 'pty_frame'; agentId: string; /* binary payload follows */ }
  | { t: 'browser_frame'; agentId: string; /* binary screencast */ }
  | { t: 'ack';      corrId: string };
```

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
2. If the daemon still holds events past that `seq`, it replays them as `event` frames.
3. If the gap is too large — or terminal output was truncated past `outputByteLimit` — the
   daemon sends a fresh `snapshot` instead.

Either way the UI is made whole again; the agents never noticed the disconnect. This is the
concrete payoff of the "daemon owns all state" invariant.

## Channel notes

- **`transcript`** — normalized `AgentEvent`s (message/thought chunks, tool calls, plans,
  permission requests). Kept always-on for subscribed agents.
- **`status`** — kept always-on even for *unfocused* agents so the left rail and approvals
  queue stay live.
- **`pty`** — binary `raw_pty` frames for xterm.js (TUI-only agents, user shell).
- **`terminals`** — client-owned terminal output for structured agents.
- **`browser`** — CDP screencast frames + control-owner state, plus `takeover_request`
  attention items (agent called `browser.request_takeover`); `browser_control` grab/release
  accepts the wheel / hands back. See [`browser.md`](browser.md).
