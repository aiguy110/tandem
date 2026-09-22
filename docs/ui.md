# UI structure

Tandem is explicitly **not** an IDE. The metaphor is **mission control**: the user conducts
a set of semi-autonomous sessions, and the UI's job is legibility + intervention. Layout is
**docked rails + focus** (D6), with the shared browser as a **pane inside focus mode** (D7).

## Regions

```
┌───────────────────────────────────────────────────────┐
│ Conductor bar:  [+ Agent] [Assign…]   ⌘K palette   ◐   │
├──────────┬───────────────────────────────┬────────────┤
│ Agents   │  FOCUS: <agent name>          │ Approvals  │
│ ──────   │  [Chat][Terminal][Diff][Web]  │ ────────   │
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
  (`idle · working · ⚠ blocked · error`), name, and workspace. The order stays user-arranged,
  even for blocked and error sessions. Click = focus; drag a row to reorder; the pencil edits the display name without renaming the
  stable agent id, worktree, or branch. Always visible.
- **Center — Focus.** The selected agent, with pane tabs:
  - **Chat** (default) — structured ACP transcript or the agent's resumable CLI, selected
    whose selected tab becomes an ACP/CLI switch, with guarded confirmations around
    active-process interruption.
  - **Terminal** — the user's independent default shell in the agent worktree, lazily
    started and preserved across pane switches and reconnects.
  - **Diff** — the workspace's uncommitted diff, reviewable/stageable.
  - **Browser** — the shared Steel screencast + a visible **control-owner indicator** and a
    "grab/release wheel" button (grabbing pauses the agent). Shown only when a browser is
    attached.
- **Right rail — Approvals.** The conductor's inbox: every `blocked-on-approval` across
  *all* sessions, most-urgent first, with inline approve / deny / edit. Clicking an item
  focuses that agent and the relevant pane. The single most important element in a
  human-as-conductor model.
- **Top — Conductor bar.** `+ Agent` (dir-first quick-spawn), `Assign task`, `⌘K` command
  palette (jump / spawn / run commands, with keybindings shown inline), and `Aa` for the
  Appearance modal — theme plus the app-wide and terminal font sizes, which can be locked to
  one ratio or adjusted separately (`src/appearance.ts`; the app size drives `--ui-scale`,
  which every `--fs-*` token in `styles.css` is expressed against). Spawn
  mechanics and the rebindable single-key + chord keymap live in
  [`spawn-and-workspaces.md`](spawn-and-workspaces.md).
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
  cliBuffer: RingBuffer;      // agent CLI raw_pty scrollback
  shellBuffer: RingBuffer;    // user shell shell_pty scrollback
  browser?: { sessionId: string; controlOwner: 'agent' | 'user' };
  pendingApprovals: Approval[];
}
```

## Key client behaviors

- **One WS, multiplexed** by `agentId` + channel. Subscribe on focus; keep `status` and
  approvals streams **always-on** so the rails stay live for unfocused sessions.
- **Reconnect = resnapshot + replay.** On reopen, request each subscribed agent's snapshot
  (last N events, scrollback tail, current browser frame), then resume the live tail. See
  [`ws-protocol.md`](ws-protocol.md#reconnect--replay).
- **Optimistic control token.** On "grab wheel," immediately reflect `controlOwner: 'user'`
  and disable agent input; confirm/rollback on daemon ack.
- **Animated popups and transcript arrivals.** `src/transitions.ts` holds `usePresence` /
  `useValuePresence`, which keep a dismissed popup mounted for the length of its exit
  animation (React would otherwise unmount it before the keyframes paint) and hand back a
  `closing` flag; `useUpdateFlash` re-triggers a flash whenever a value changes. Modals and
  palettes, pick boxes, context menus, the details popover, and the annotation popover all
  animate in and out; user prompts and tool cards fade up as they arrive and flash on
  change. Durations live in the `--motion-*` custom properties in `styles.css`, and
  `--motion-out` must stay in sync with `EXIT_MS`.

## Implementation status (Phase 4)

The React UI in [`ui/`](../ui/) (Vite + TypeScript strict) implements this doc for real: the
docked-rails layout (D6), a zustand store that is a pure projection of daemon messages, the
durable token-auth WS client with backoff reconnect + `sinceSeq` replay (D15), the transcript
renderer (merged prose/markdown, dimmed thoughts, collapsed tool cards with status chips,
plans, per-terminal mini-terminals, inline permission cards, error banners), the always-on
global approvals rail, the Chat ACP/CLI switch and independent Terminal shell
(`ghostty-web` default via WASM, `@xterm/xterm` fallback — D12), the dir-first quick-spawn
palette (D9), and the scope-aware rebindable
command palette + keymap (D10). See [`ui/README.md`](../ui/README.md).

Small deviations from the sketch above, all driven by what the wire actually carries:

- **`AgentView.transcript`** is stored as the raw seq-tagged `WireEvent[]`; the rendered
  message/tool/plan/terminal items are derived per render (the store stays a thin projection).
- **PTY buffers** live outside the reactive store: `ptyHub` holds the agent CLI and
  `shellHub` holds the user's Terminal shell, so byte streams never trigger React renders
  and cannot mix during replay.
- The rail needs each agent's **name + workspace**, which no `snapshot` carries, so the client
  discovers sessions via a **`list_agents`** message (see `ws-protocol.md`) on every
  (re)connect — this is what makes the rail correct after a daemon restart.
- **Diff** and **Browser** panes are placeholders (their daemon verbs still return error acks).

## Later (deferred)

- A **canvas/grid overview** mode as a second lens over the docked rails.
- **Detachable, first-class browser surfaces** (pop-out, shared across agents,
  side-by-side with a transcript).
