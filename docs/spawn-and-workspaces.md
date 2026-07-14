# Spawning agents & workspaces

Spawning is the most-repeated action in Tandem, so it's optimized for **minimum
keystrokes**: sensible defaults, progressive disclosure, and heavy keyboard control.
No sandboxing yet — agents run in **working directories on the host** (git worktrees where
possible).

## SpawnSpec

An agent is defined by a small spec; almost everything is defaulted or inferred:

```ts
interface SpawnSpec {
  adapter: 'acp' | 'pty';                 // default: acp (claude-agent-acp)
  workspace:                              // where it works
    | { kind: 'worktree'; repo: string; branch: string; baseRef: string }
    | { kind: 'existing'; cwd: string };  // reuse a dir as-is (non-git, or opt-in)
  name?: string;                          // auto: web-1, api-2… (renamable)
  task?: string;                          // optional initial prompt, dispatched on spawn
  preset?: string;                        // reserved; single default agent for now
}
```

## Surfaces

Two keyboard surfaces, one for speed and one for discoverability:

1. **Quick-spawn palette** — a dedicated hotkey opens a modal focused on a fuzzy
   **directory** input. Sources, ranked: focused agent's repo → recent dirs → git repos
   under your project roots (`TANDEM_PROJECT_ROOTS`, default `~/Projects`) → pinned
   bookmarks. Each row shows branch, clean/dirty, and whether an agent already occupies
   that dir.
   - `Enter` on a dir → spawn with defaults; **focus jumps to the new agent**.
   - `⇥` then type a task → spawn **and dispatch** in one shot (focus jumps to it too).
   - `⌘Enter` → reveal advanced fields (adapter, model, branch, base ref, name).
2. **Command palette** (`⌘K`) — everything: all commands, jump-to-agent-by-name, spawn,
   assign, approve, kill, merge-back. Every row shows its current keybinding inline, so the
   palette *teaches* the hotkeys. Discoverability layer + "I forgot the key" fallback.

Plus **Duplicate / sibling** — a hotkey that spawns another agent in the *same* repo (new
worktree) instantly, for fanning work out across one codebase.

### The fastest path

```
[spawn]  che ↵                          → agent in ~/Projects/checkout (new worktree), focused
[spawn]  che ⇥ fix the rounding bug ↵   → spawned AND dispatched in one shot
[spawn]  che ⌘↵                         → advanced: adapter · model · branch · base · name
```

## Keymap system

Single-key + chords, scope-aware, fully rebindable — mirroring the `keybindings.json`
model the user already uses in Claude Code.

- **Command registry** with stable IDs (below); the keymap maps keys → command IDs.
- **`keybindings.json`** — user-editable: single keys, chords (`g a`), modifier combos;
  reset-to-defaults; import/export.
- **Scopes** resolve keys by context: `global` / `agent-focused` / `modal-open` /
  `text-input`. A bare `c` creates an agent globally but types "c" in a prompt field.
- **Bindings shown everywhere** — button tooltips and palette rows display their keys.

Illustrative default map (all rebindable):

| Keys | Command ID | Action |
|---|---|---|
| `c` | `agent.spawn` | Open quick-spawn palette |
| `C` | `agent.spawn.sibling` | New agent in the focused repo (new worktree) |
| `⌘K` | `palette.open` | Command palette |
| `g a` | `nav.goToAgent` | Jump to agent by name |
| `j` / `k` | `nav.next` / `nav.prev` | Move through the agent rail |
| `1`–`4` | `pane.transcript/terminal/diff/browser` | Switch focus pane |
| `a` / `d` | `approvals.approveFocused` / `denyFocused` | Act on the top approval |
| `w` | `browser.grabWheel` | Grab / release the shared-browser wheel |
| `⌫` | `agent.close` | Close the focused agent (teardown below) |
| `m` | `agent.mergeBack` | Review diff → merge or open PR |

## Workspaces (host dirs, no sandbox)

Config: **project roots** — `TANDEM_PROJECT_ROOTS` (default `~/Projects`), scanned for
repos — plus **recents** and **bookmarks**.

### Git repo → one worktree per agent

Spawning into a git repo creates an isolated `git worktree` so parallel agents on one repo
never clobber each other. It's still just a different host directory.

- **Location:** central — `~/.tandem/worktrees/<repo>/<agent>/`. Keeps the real repo dir
  uncluttered, avoids sibling sprawl, and makes worktrees easy to enumerate and GC.
- **Branch:** `tandem/<agent-name>` (e.g. `tandem/web-1`), renamable; if spawned with a
  task, offer `tandem/<slug-of-task>`.
- **Base ref:** the repo's **current HEAD** by default; advanced picker for `main`/other.
- **Opt-out:** a toggle reuses the existing working tree (`kind: 'existing'`) for quick
  throwaway work.

### Non-git dir → plain cwd

Just `cd` into the directory (`kind: 'existing'`).

### Collision

Targeting a dir already occupied by a live agent (or a non-git dir with one) → warn and
offer "open a worktree instead" or "attach to the existing agent."

## Lifecycle

- **Status:** `idle | working | blocked | error` (see the rail + approvals queue).
- **Close / teardown (default):** remove the worktree checkout to reclaim disk, but
  **keep the branch and its commits** — work is never lost. Uncommitted changes block the
  close with a warning (force to override). Reused-existing-tree agents just detach.
- **Merge-back (explicit action, never automatic):** `agent.mergeBack` reviews the agent's
  diff and either merges its branch into the base or opens a PR. The human conductor
  decides — nothing merges on its own.
- **Respawn:** a closed agent's branch can be re-checked-out into a fresh worktree.

## Persistence & restore

Agents are **durable declarations**, not ephemeral processes. The daemon persists each live
agent's `SpawnSpec` + ACP `sessionId` (and its worktree/branch). On daemon restart it
**restores** every agent:

- re-attach the worktree and re-spawn the adapter subprocess;
- **resume the ACP session via `session/load`** where the agent advertises the
  `loadSession` capability;
- otherwise start a fresh session and re-render history from the persisted event log.

## Deferred

- **Sandboxing** (containers / per-agent isolation beyond the filesystem) — future; for now
  agents share the host with host-level permissions.
- **Presets** (named agent+model+role+cwd bundles, individually hotkey-bound) — the
  SpawnSpec reserves `preset`; a single default agent ships first.

See [`decisions.md`](decisions.md) D8–D10 for the rationale.

## Implementation status (Phase 2)

The daemon's `WorkspaceManager` (`daemon/src/workspace.ts`) implements this doc's git-worktree
mechanics for real, wired through `AgentRegistry.spawn`/`close`/`restoreOne` — see
[`ws-protocol.md`](ws-protocol.md) for the wire-level `spawn_agent`/`close_agent`/`list_dirs`
details. Two small deviations from the illustrative `SpawnSpec` above:

- `workspace.branch` and `workspace.baseRef` are **optional** on the wire (not plain
  `string`s) — the manager fills the documented defaults (`tandem/<agent-name>`, current
  HEAD) and persists the *resolved* values back into the stored spec, so restore and a later
  respawn are unambiguous.
- If an auto-derived branch name collides with an unrelated existing branch, the manager
  suffixes it (`tandem/web-1-2`) rather than erroring; an **explicit** `branch` that already
  exists is treated as the respawn path (re-checkout, not create) — the cheap version of
  "Respawn" above: point `spec.workspace.branch` at a still-live `tandem/<name>` branch and
  spawn normally.

Everything else — quick-spawn palette UX, keymap, command palette, merge-back — is still
Phase 3+ UI work; the daemon side (`merge_back`) still returns an error `ack`.
