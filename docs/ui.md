# UI structure

Tandem is explicitly **not** an IDE. The metaphor is **mission control**: the user conducts
a set of semi-autonomous agents, and the UI's job is legibility + intervention. Layout is
**docked rails + focus** (D6), with the shared browser as a **pane inside focus mode** (D7).

## Regions

```
┌───────────────────────────────────────────────────────┐
│ Conductor bar:  [+ Agent] [Assign…]   ⌘K palette   ◐   │
├──────────┬───────────────────────────────┬────────────┤
│ Agents   │  FOCUS: <agent name>          │ Approvals  │
│ ──────   │  [Transcript][Term][Diff][Web]│ ────────   │
│ ● web-1  │                               │ ⚠ web-1    │
│ ● api-2  │   (selected pane)             │  run `rm…` │
│ ○ docs-3 │                               │  [✓] [✗]   │
│ ⚠ ui-4   │                               │ ⚠ ui-4     │
│          │                               │  edit x.ts │
├──────────┴───────────────────────────────┴────────────┤
│ Inspector (contextual): workspace files · task · logs  │
└───────────────────────────────────────────────────────┘
```

- **Left rail — Agents.** The orchestra; one row per agent with a status dot
  (`idle · working · ⚠ blocked · error`), name, and workspace. `⚠ blocked` and `error`
  float to the top. Click = focus. Always visible.
- **Center — Focus.** The selected agent, with pane tabs:
  - **Transcript** (default) — the structured event stream, cleanly rendered.
  - **Terminal** — xterm.js on the pty. The "drop to terminal" escape hatch.
  - **Diff** — the workspace's uncommitted diff, reviewable/stageable.
  - **Browser** — the shared Steel screencast + a visible **control-owner indicator** and a
    "grab/release wheel" button (grabbing pauses the agent). Shown only when a browser is
    attached.
- **Right rail — Approvals.** The conductor's inbox: every `blocked-on-approval` across
  *all* agents, most-urgent first, with inline approve / deny / edit. Clicking an item
  focuses that agent and the relevant pane. The single most important element in a
  human-as-conductor model.
- **Top — Conductor bar.** `+ Agent`, `Assign task`, `⌘K` palette (jump / spawn /
  broadcast), theme toggle.
- **Bottom — Inspector.** Contextual to focus: workspace file tree, current task, raw logs.
  Collapsible.

## Front-end state model

The browser holds **no authoritative state** — everything is a projection of daemon state
over the WebSocket.

```ts
type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';

interface AgentView {
  id: string;
  name: string;
  workspace: { repo: string; branch: string; dirty: boolean };
  status: AgentStatus;
  transcript: AgentEvent[];   // structured events, appended
  terminalBuffer: RingBuffer; // pty scrollback for xterm.js
  browser?: { sessionId: string; controlOwner: 'agent' | 'user' };
  pendingApprovals: Approval[];
}
```

## Key client behaviors

- **One WS, multiplexed** by `agentId` + channel. Subscribe on focus; keep `status` and
  approvals streams **always-on** so the rails stay live for unfocused agents.
- **Reconnect = resnapshot + replay.** On reopen, request each subscribed agent's snapshot
  (last N events, scrollback tail, current browser frame), then resume the live tail. See
  [`ws-protocol.md`](ws-protocol.md#reconnect--replay).
- **Optimistic control token.** On "grab wheel," immediately reflect `controlOwner: 'user'`
  and disable agent input; confirm/rollback on daemon ack.

## Later (deferred)

- A **canvas/grid overview** mode as a second lens over the docked rails.
- **Detachable, first-class browser surfaces** (pop-out, shared across agents,
  side-by-side with a transcript).
