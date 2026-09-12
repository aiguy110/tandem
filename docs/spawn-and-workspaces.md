# Spawning agents & workspaces

Spawning is the most-repeated action in Tandem, so it's optimized for **minimum
keystrokes**: sensible defaults, progressive disclosure, and heavy keyboard control.
No sandboxing yet — agents run in **working directories on the host** (git worktrees where
possible).

## Resume Session

The **Resume session…** command (default binding `R`) opens a picker over every live or
closed ACP session persisted by Tandem, plus imported vendor transcripts and external
sessions returned by configured ACP adapters that advertise `sessionCapabilities.list`.

Rows are **grouped by source repository**. The daemon attributes each session to a repo in
`ResumeCatalog`: Tandem-owned sessions use their recorded workspace, and everything else is
resolved from its working directory by `workspace.RepoForDir`, which follows a linked
worktree's `gitdir:` pointer back to the repository it was cut from before falling back to
walking up for a `.git` entry. `repoPath` is the grouping identity; `repo` is the label.
Each repo group is ordered by its **best individual match**, so the repo you named leads.

Search ranks by **match quality first, then field**. Quality is exact > prefix > word >
substring > subsequence; field priority is session name > repo > transcript > incidental
metadata (agent, branch, cwd, session id). Quality outranking field is deliberate: strict
field tiering let any title that merely contained the query as a subsequence ("Trim And
Deduplicate Empty Metadata" matches `tandem`) beat the repo the user actually typed. A
transcript hit is a token-prefix FTS match from the daemon (debounced, stale responses
discarded) and grades as `substring`, so it sits below a deliberate name or repo match and
above an incidental subsequence.

Entries are deduplicated by ACP session id, and by agent id for running agents. **Active
sessions appear with an `active` badge and are focused rather than resumed** — Tandem never
spawns a second agent against a transcript that is already open. Choosing a closed Tandem
entry recreates its worktree when necessary and loads the stored session; choosing an
external entry creates a Tandem agent in its reported working directory and captures the
loaded transcript. External discovery is best-effort, and adapters without ACP session
listing are identified in the picker.

## SpawnSpec

An agent is defined by a small spec; almost everything is defaulted or inferred:

```ts
interface SpawnSpec {
  adapter: 'acp' | 'pty';                 // default: acp
  agent?: string;                         // agent definition id; default: defaults.agent or claude
  profile?: string;                       // optional profile id referencing that agent
  acpArgs?: string[];                     // one-off ACP arguments, after profile arguments
  terminalArgs?: string[];                // one-off direct-terminal arguments
  workspace:                              // where it works
    | { kind: 'worktree'; repo: string;
        branchMode?: 'create' | 'attach'; branch?: string;
        source?: { ref: string; commit?: string };
        integration?: { kind: 'local-branch' | 'remote-branch' | 'detached'; ref: string };
        baseRef?: string }                // accepted for legacy clients
    | { kind: 'existing'; cwd: string };  // reuse a dir as-is (non-git, or opt-in)
  name?: string;                          // auto: einstein-1, curie-2… (renamable)
  task?: string;                          // optional initial prompt, dispatched on spawn
  handoffFrom?: string;                   // agent id to continue from (see Hand-off)
  preset?: string;                        // reserved legacy field
}
```

## Agent catalog and profiles

Tandem loads its shipped catalog from the repository's `config.yml.example`, then overlays
an optional `$TANDEM_HOME/config.yml` (normally `~/.tandem/config.yml`). The shipped file
declares Claude, Codex, and Pi using the same schema available to users; there are no
agent-specific definitions in daemon code. User definitions with the same id override their
fields, and environment-variable launch overrides remain supported for backwards
compatibility. Copy `config.yml.example` to `~/.tandem/config.yml` to start from a fully
editable catalog, or keep the home file small and override only selected fields.
The `{node}` placeholder uses the explicitly configurable `TANDEM_NODE_CMD` launcher. This
is important for the native daemon, which is not itself a Node executable; its default is
the `node` executable resolved from `PATH`, `~/.local/bin`, or `~/bin`.

An **agent definition** describes how to start the same tool through ACP and directly in a
terminal. A **profile** gives that definition a reusable set of extra arguments:

```yaml
agents:
  pi:
    name: Pi
    acp:
      command: pi-acp
      args: []
      env:
        PI_ACP_ENABLE_EMBEDDED_CONTEXT: "true"
    terminal:
      command: pi
      startArgs: [--model, openai-codex/gpt-5.4]
      resumeArgs: [--session, "{sessionId}"]
      env: {}

profiles:
  pi-fast:
    agent: pi
    name: Pi Fast
    acpArgs: []
    terminalArgs: [--model, openai-codex/gpt-5.4-mini]
```

Commands and arguments are arrays rather than shell command strings, avoiding shell
quoting and injection surprises. Profile arguments are appended to the selected launch
mode's configured arguments. Direct Terminal launches use `terminal.startArgs`; ACP-to-CLI
handoff uses `terminal.resumeArgs` and is offered only when that template is present.
`{node}`, `{runtimeRoot}`, `{tandemRoot}`, and `{home}` are expanded while loading either
catalog layer. Runtime terminal argument templates additionally substitute `{sessionId}`,
`{cwd}`, `{agentId}`, and `{agentName}`. Environment entries are passed only to the
configured child process.

The spawn palette lists definitions and profiles from the normalized catalog. Selecting a
profile retains its own display name while resolving its referenced agent's ACP or terminal
command. Persisted specs naming `claude`, `codex`, or `pi` resolve through the built-ins
exactly as before.

## Surfaces

Two keyboard surfaces, one for speed and one for discoverability:

1. **Quick-spawn palette** — a dedicated hotkey opens a modal focused on a fuzzy
   **directory** input. Sources, ranked: focused agent's repo → recent dirs → git repos
   under your project roots (`TANDEM_PROJECT_ROOTS`, default `~/Projects`) → pinned
   bookmarks. Each row shows branch, clean/dirty, and whether an agent already occupies
   that dir.
   - `Enter` on a dir → spawn with defaults; **focus jumps to the new agent**.
   - `⇥` then type a task → spawn **and dispatch** in one shot (focus jumps to it too).
   - `⌘Enter` → reveal advanced fields, including a fuzzy Git-ref picker and explicit
     new-branch / continue-branch / existing-checkout workspace modes.
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
| `R` | `agent.resume` | Resume session — past + active, grouped by repo |
| `⌘K` | `palette.open` | Command palette |
| `g a` | `nav.goToAgent` | Jump to agent by name |
| `j` / `k` | `nav.next` / `nav.prev` | Move through the agent rail |
| `1`–`4` | `pane.chat/shell/diff/browser` | Switch Chat / Terminal / Diff / Browser |
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
- **Integration target:** the local/remote feature branch where work is intended to land;
  this is distinct from the agent's private branch.
- **Source commit:** the selected ref is resolved to an immutable OID at spawn time. Both
  ref and OID are persisted, so later branch movement cannot rewrite the fork point.
- **Agent branch:** `tandem/<context>/<agent-name>` (for example
  `tandem/go-backend-migration/einstein-1`), with an expert override.
- **Branch picker:** Advanced Settings fuzzy-finds local branches, remote branches, and
  tags, including checked-out and upstream-divergence metadata. Refresh is read-only and
  never performs an implicit `git fetch`.
- **Explicit intent:** create refuses an existing explicit branch; attach requires an
  existing, currently-unchecked-out local branch.
- **Uncommitted changes:** worktrees start from a committed revision. The picker warns when
  changes in an existing checkout will not be included.
- **Opt-out:** a toggle reuses the existing working tree (`kind: 'existing'`) for quick
  throwaway work.

### Non-git dir → plain cwd

Just `cd` into the directory (`kind: 'existing'`).

### Sharing a worktree

Targeting a dir another open agent already works in is allowed, not refused. The joining
agent **inherits that agent's workspace descriptor** — repo, branch, merge target — so a
shared Tandem worktree keeps behaving like the worktree it is, rather than collapsing into
an anonymous existing dir.

Sharing is never implicit: the spawn form names the other occupants before launching, and
the delete dialog names them again when a worktree is being left behind. The checkout is
torn down with its **last** occupant; until then a close removes the agent and keeps the
worktree and branch (`ClosePreview.cohabitants` is what the warning is built from).

The motivating case is a hand-off: an agent that stops mid-turn usually leaves uncommitted
work in place, and the agent taking over needs to be standing in it.

## Hand-off

A hand-off moves a conversation to a different agent harness without asking any model to
summarize it. `internal/handoff` walks the source agent's durable event log and renders a
**Hand-off Transcript**: every user and assistant message verbatim and in order, each
intervening tool call reduced to one `- <title> [status]` line, private reasoning dropped,
runaway tool loops capped. A short preamble explains that the receiving agent is taking
over an unfinished session and states the working directory and branch.

That text is dispatched as the new agent's first user message (with any task typed at spawn
appended under "New instruction from the user"). It is deliberately not written into the
persisted `SpawnSpec` — the transcript can be large, and the spec is re-marshalled on every
restore.

Surfaces: the spawn form's "Continue from session" picker (default: none), and **Hand off…**
in the right-click menu on a rail agent card, which opens that form with the session already
chosen and its worktree pre-selected. The two things a parent session offers — its
transcript and its worktree — are independent: the transcript is a checkbox, the worktree a
Git-workspace mode, and either can be taken without the other.

## Lifecycle

- **Status:** `idle | working | blocked | error` (see the rail + approvals queue).
- **Close / teardown (default):** remove the worktree checkout to reclaim disk, but
  **keep the branch and its commits** — work is never lost. Uncommitted changes block the
  close with a warning (force to override). Reused-existing-tree agents just detach.
- **Merge-back (explicit action, never automatic):** `agent.mergeBack` reviews the agent's
  diff and either merges its branch into the base or opens a PR. The human conductor
  decides — nothing merges on its own.
- **Respawn:** a closed agent's branch can be re-checked-out into a fresh worktree.
- **Feature-relative status:** dirty/ahead/behind/diverged/merged state and close previews
  compare with the persisted integration target, not the source checkout's mutable HEAD.
- **Sibling spawn:** the ordinary command starts from the integration target; the dependent
  sibling command starts from the focused agent's commits while retaining the same target.

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
- **Managed agent distribution** — ACP Registry discovery, versioned installs and rollback,
  and custom pinned Git manifests are separate lifecycle work; the current catalog launches
  commands already installed on the host.

See [`decisions.md`](decisions.md) D8–D10 for the rationale.

## Implementation status

The native workspace package (`internal/workspace/`) implements these git-worktree
mechanics, wired through the Go registry — see
[`ws-protocol.md`](ws-protocol.md) for the wire-level `spawn_agent`/`close_agent`/`list_dirs`
details. Legacy `branch`/`baseRef` records remain supported. New spawns persist canonical
`source.ref`, immutable `source.commit`, `integration`, and explicit `branchMode`. Only a
legacy request may infer attach from an explicitly named existing branch. Auto-generated
collisions receive a suffix; explicit conflicts return `branch_exists`, `branch_missing`,
or `branch_checked_out`. Adapter-start failure rolls back its new DB row, worktree, and
newly-created branch.

Merge-back remains future UI/daemon work; the daemon side (`merge_back`) still returns an
error `ack`.
