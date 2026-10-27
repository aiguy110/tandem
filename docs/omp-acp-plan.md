# omp ACP integration plan

Plan for closing the ACP gaps between Tandem (client) and Oh My Pi's `omp acp`
(agent). It came out of an audit of Tandem's `internal/acpadapter` against
omp 18.12.1 (`packages/coding-agent/src/modes/acp/`, with the protocol layer in
`@oh-my-pi/pi-utils/src/acp/`). Upstream `main` was checked on 2026-10-26 at
`e0996aad2c`; at that point the newest npm release was 18.13.0.

There are two kinds of work:

- **Tandem-only.** omp already supports the feature and Tandem doesn't use it.
  These items are fixed in this repo.
- **omp changes.** These go on our own omp fork, which Tandem installs through
  its ACP fork tracking (`acpForks`, `tandem acp fork`). Each one is also
  offered upstream, so the fork can be retired once upstream has everything.

## Fork and upstreaming strategy

### Where the fork lives

- **Fork repo:** `https://github.com/aiguy110/oh-my-pi`, forked from
  `can1357/oh-my-pi`. Its `upstream` remote is the canonical repo.
- **Upstream branches:** one per upstream PR, cut from upstream `main`, named
  `acp/<topic>`. Each one carries exactly one logical change, which is what the
  contribution guide requires.
- **The `tandem` branch:** this is the branch Tandem installs. It starts at the
  **release tag** matching `upstreamVersion` (for example `v18.13.0`), not at
  `main`. The workspace resolves `@oh-my-pi/*` siblings through
  `catalog:`/exact versions. The published `dist/cli.js` bundles most of them,
  but `@oh-my-pi/pi-natives` (a Rust addon) and `@oh-my-pi/omptype` are still
  resolved from npm at install time (verified in the 18.12.1 dist). Basing on a
  tag guarantees those versions exist on npm. `tandem` is the release tag plus
  the `acp/*` branches cherry-picked on top, along with any open upstream PRs
  we choose to carry.
- **Fork record:**
  - `tandem acp fork omp --repo https://github.com/aiguy110/oh-my-pi --ref tandem --commit <sha> --upstream-package @oh-my-pi/pi-coding-agent --rebased-onto <ver> --upstream-repo https://github.com/can1357/oh-my-pi --package-dir packages/coding-agent --build-script tandem:build --reason "<one line>" --pr <N>...`
  - Each upstream PR number is added with `--pr` once it exists.
  - When upstream has merged every carried PR, `tandem acp upstream omp`
    retires the fork.

### Upstream contribution rules

These come from `CONTRIBUTING.md` and the repo's `AGENTS.md` on upstream `main`.

**Before a PR**

- Bug fixes and narrowly scoped improvements can go straight to a PR.
- Major features, behavioral or architectural changes, new subsystems, and
  changes spanning several packages need a discussion in their Discord **before
  implementation**. A GitHub issue doesn't count as that discussion.
- **Do not open an issue for work we are about to submit.** Their bot (robomp)
  picks up actionable issues and may start the same fix in parallel. Link
  existing issues from the PR instead.

**AI-assisted work**

- Agents may help, but a human must review every changed file, exercise the
  behavior, and submit the PR themselves. An agent must not publish it.

**PR contents**

- The PR body must include at least one sentence written by Josiah in his own
  words explaining what changed and why. An agent may draft everything else,
  but never that sentence.
- The PR must report a concrete verification scenario and its result:
  reproduce-then-fix for bugs, end-to-end use for features. `bun check` alone is
  not sufficient.
- One logical change per PR, with no drive-by cleanup.
- Add a one-line, user-facing changelog entry to
  `packages/coding-agent/CHANGELOG.md` under `## [Unreleased]`.
- Use conventional subjects (`fix(acp): …`, `feat(acp): …`).
- Contributions are MIT-licensed. No CLA or DCO is required.

**Local gates**

- Run from `packages/coding-agent`: `bun run check`, then `bun run test`.
- Never run `tsc` directly.
- Use the central logger, never `console.*`.
- Model- or provider-specific policy belongs in the KDL catalog, not in
  TypeScript.

**GitHub comments**

- Comments and reviews on their tracker (for example on an existing PR we want
  to carry) are shown to Josiah for confirmation before posting.

## Phase 0: prerequisites

### 0.1 Update the managed omp to ≥ 18.12.6 (fixes steering)

`_session/steering` and `_meta.steering.supported` landed upstream in
`0a4c236fe2`, first released in v18.12.6. The installed runtime is 18.12.1;
Tandem's pin `@oh-my-pi/pi-coding-agent@^18.11.0` already allows 18.13.0.

- **Tandem:** install the update through the normal ACP update flow, and raise
  the pin floor in `internal/runtimeinstall/runtime.go` to `^18.13.0` so a fresh
  install never lands on a version without steering.
- **Compatibility, checked against `main`:**
  - omp returns `injected` or `promptRequired`, and never `startedNewTurn`.
  - Tandem's `idleBehavior: "promptRequired"` matches this.
  - omp also accepts an omp-specific `_meta.steering.queue: "followUp"`. We
    don't need it now.
- **Verify:**
  - Start an omp session.
  - While a turn is running, deliver an agent message (or steer from the UI).
  - Expect `agent message steered into active turn` in the journal and the text
    injected mid-turn, not queued.

### 0.2 Teach ACP fork tracking to build a monorepo package

`runtimeinstall.buildFork` assumes the package sits at the clone root. It runs
`bun install`, then `bun run build` (or `prepare`), then `bun pm pack` there.
For omp that does the wrong thing at every step:

- The root is the private `omp` workspace.
- The root `build` script is `bun run --workspaces --if-present build`, which
  builds every package, natives included.
- `packages/coding-agent`'s own `build` makes a standalone binary, not
  `dist/cli.js`.
- `dist/cli.js` is produced by that package's `prepack`
  (`gen:tool-views && gen:bundle`).

Changes:

- **New `ACPFork` fields:**
  - `packageDir`: path inside the repo, for example `packages/coding-agent`.
    Empty keeps today's behavior.
  - `buildScript`: script name to run before packing. Empty keeps today's
    `build`/`prepare` choice; `none` runs no script.
  - For omp, `buildScript` is `tandem:build`, a fork-only script
    (`scripts/tandem-fetch-natives.ts`). `bun pm pack` does run the package's
    `prepack` (`gen:tool-views && gen:bundle`), but the bundle step loads
    `pi-natives`, and a source checkout has no compiled addon. The fork carries
    no Rust changes, so `tandem:build` downloads the published
    `@oh-my-pi/pi-natives-<platform>-<arch>` addon for the workspace version,
    checks its npm integrity hash, and drops it into `packages/natives/native/`.
    Never upstream this commit; keep it on top of `tandem` across rebases.
- **`buildFork`:**
  - Run `bun install` at the clone root, so workspaces resolve.
  - Run the build script and `bun pm pack` in `packageDir`.
- **Plumbing:** wire both fields through `tandem acp fork` (`--package-dir`,
  `--build-script`), `ForkStatus`, `config.yml.example`, and the fork
  validation tests.
- **Verify:**
  - Point an `acpForks.omp` record at an unmodified release tag.
  - Spawn an omp session.
  - Confirm the distribution
    `agents/omp/<ver>+fork.<sha>/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js`
    exists and the session runs.
  - Confirm `tandem acp status` shows the fork.

### 0.3 Create the omp fork

- Fork `can1357/oh-my-pi` to `aiguy110/oh-my-pi`. This is outward-facing, so
  confirm with Josiah before creating it.
- Create `tandem` from the current release tag.
- Record the fork only once it carries at least one change Tandem needs. An
  unmodified fork is just slower installs.

### Status (2026-10-27)

- Fork created. `tandem` = `v18.13.2` plus the carried PRs below, pinned in
  `acpForks.omp` at `74f142636d7e` with `--package-dir packages/coding-agent
  --build-script tandem:build`.
- **Carried:** #12716 (mid-turn `usage_update`), #14472 (per-command
  `allow_always`), #14797 (MCP routes on resume), #15043 (subagent sessions).
  `CHANGELOG.md` hunks were dropped from the picks (3-way merges put them in
  released sections) and re-added under `[Unreleased]` in one fork commit.
- **Not carried:** #10652. 18.13.2 already keeps a failed client MCP server in
  the session and reports it as a chunk instead of rejecting `session/new`.
- **Fork-only fixes on top of the picks:**
  - #12716's second commit (`order prompt handlers`) chains event handlers, so
    the `agent_end` handler's async-delivery drain settled the turn before the
    follow-up events queued behind it ran. `acp-agent.test.ts` "keeps async
    delivery follow-up updates inside the owning ACP prompt" failed. Fix: wait
    for idle and settle off the chain, after the chain drains. Report on #12716
    (after confirmation).
  - #14472 makes `allow_always` per-command; 18.13.2's pattern-policy test
    still expected tool-wide grants, so its expectation was updated.
  - `tandem:build` (natives fetch, see 0.2). Never upstream.
- **Checks run:** `check:types` clean; ACP test files 362/363 pass. The one
  failure, `acp-lazy-startup` "honors explicit host-defaulted and todo
  settings", fails identically on clean `v18.13.2`. Tandem's real
  `installFork` built the fork, `omp --version` ran from it, and an ACP
  handshake advertised `_meta.steering.supported` and created a session.

## Phase 1: correctness fixes

### 1.1 `usage_update` with `size: 0` kills the session

- **Problem:**
  - omp sends `size: contextUsage.contextWindow`
    (`acp-agent.ts`, `#emitEndOfTurnUpdates`), which is `0` when the model's context
    window is unknown (`session-stats.ts:281-282`). Typical case: a custom or
    local model without `contextWindow`.
  - Tandem rejects `size <= 0` as a malformed `session/update`
    (`adapter.go:1369`), and malformed updates are fatal (`adapter.go:813`).
  - Result: the session dies at the end of a turn. This is still present on
    upstream `main`.
- **Tandem:**
  - In `handleUpdate`, treat a `usage_update` whose `size <= 0` (or `used < 0`)
    as a non-fatal `OptionalUpdateError` diagnostic and drop it.
  - A meter we can't scale doesn't justify killing the turn. Keep the rejection
    of unparseable JSON fatal.
  - Add a regression test in `adapter_test.go`: a mock agent emits
    `size: 0`; the session stays alive and no `usage` event is pushed.
- **omp (`acp/usage-unknown-window`):** in `#emitEndOfTurnUpdates`, skip the update
  when `contextWindow <= 0`. ACP's `UsageUpdate.size` is the window size, so `0`
  is not a meaningful value.
- **Upstream:**
  - Small bug fix, so a direct PR.
  - Reference #12667 / PR #12716 (per-message `usage_update`). The guard must
    cover the extracted builder in #12716 too, so rebase onto it if it lands
    first.
- **Verify:**
  - Configure an omp model with no `contextWindow`.
  - Before the fix the session errors at turn end; after it, the turn ends
    cleanly with no usage meter.

### 1.2 One failing MCP server rejects `session/new`

- **Problem:** omp's `#configureMcpServers` throws if any server fails, so
  `session/new`, `session/load` and `session/fork` all fail. Tandem always sends
  its own Playwright and Tandem-control servers plus the user's `mcpServers`, so
  one broken user server makes omp unspawnable.
- **omp:**
  - Not needed on the 18.13.x base: a failed client-declared server is kept
    and reported as a chunk, so `session/new` succeeds. #10652 conflicts with
    that change and isn't carried.
- **Tandem:**
  - Read `agentCapabilities.mcpCapabilities`, and omit `http`/`sse` servers the
    agent doesn't advertise, with a warning log naming the skipped server.
  - Today they are sent unconditionally. omp advertises both, so this protects
    other agents rather than omp.
- **Verify:** add a `mcpServers` entry with a bad command. The omp session still
  spawns, and Tandem's browser tools are present.

### 1.3 Restoring a saved mode or config the agent no longer offers

- **Problem:** `registry.go:1237`/`:1249` re-applies the saved mode and config
  options after start, and treats rejection as an error. omp only offers `plan`
  mode when plan mode is enabled in settings, and a saved `model` value
  (`provider/id`) can disappear from the catalog.
- **Tandem:** when re-applying, skip values that aren't in the agent's current
  `modes`/`configOptions`, and log the skipped id at warn. Keep explicit user
  changes strict.
- **Verify:** resume a session saved in `plan` mode with plan mode disabled. It
  resumes in `default` with a warning, and doesn't error.

## Phase 2: protocol behavior Tandem expects from omp

### 2.1 Fork at a message (`_meta.jetbrains.air.fork.messageId`)

- **Problem:** Tandem sends `_meta.jetbrains.air.fork {version:1, messageId}` on
  `session/fork` (`adapter.go:337-340`). omp ignores request `_meta` and always
  forks the whole session, so "fork from here" silently includes later history.
- **Tandem (this lands first):**
  - Add a catalog capability, `acp.meta.forkAtMessage: true`, to the Claude and
    Codex entries in `config.yml.example`.
  - Have the adapter refuse a message-scoped fork for agents that don't declare
    it, with a clear error, instead of silently forking everything.
  - Emit it in `aside_capabilities` (or a sibling event) so the UI hides "fork
    from here" for agents without it.
- **omp (`acp/fork-at-message`):**
  - In `unstable_forkSession`, when `_meta.jetbrains.air.fork.messageId` is
    present, resolve the ACP `messageId` to its session entry and fork the
    session tree at that entry. omp already branches sessions (see `/tree`,
    `session.fork()`).
  - The ACP `messageId` values omp emits on `agent_message_chunk` and
    `user_message_chunk` (`A:1595-1637`) need a stable mapping to entry ids.
  - Return an error for an unknown `messageId`.
- **Upstream:** this is a JetBrains `_meta` convention that the Claude and Codex
  bridges also honor. It is narrowly scoped, but it is a behavioral addition to
  fork, so float it on their Discord first. If they would rather see a
  standardized field, follow that and keep the `_meta` read only for
  compatibility.
- **Verify:**
  - Fork from the second of four user messages.
  - The fork's `session/load` replay ends at that message.
  - Then flip `forkAtMessage` on for omp in the catalog.

### 2.2 Compaction lifecycle (`compaction_update`, `compaction_summary_chunk`)

- **Problem:** Tandem advertises `clientCapabilities.session.compaction` and
  renders `compaction_update`/`compaction_summary_chunk`
  (`docs/acp-notes.md:119-127`). omp never emits either, so automatic and manual
  compaction are invisible except as a drop in usage.
- **omp (`acp/compaction-updates`):**
  - In `acp-event-mapper.ts`, map `auto_compaction_start`/`auto_compaction_end`
    (and the `/compact` builtin) to `compaction_update`, using the trigger and
    pre/post tokens.
  - Stream the summary as `compaction_summary_chunk`, but only when the client
    advertised `session.compaction`.
  - Clients that didn't advertise it see no change.
- **Upstream:** issue #16969 asks for exactly this signal (forwarding
  auto-compaction). Link it from the PR rather than opening a new issue. The
  wire shape follows the unstable ACP compaction RFD that claude-agent-acp
  implements; cite it in the PR.
- **Tandem:** no change. Re-check the `contextCompaction` `_meta` that Tandem
  keeps against what omp sends.
- **Verify:**
  - Run `/compact` in an omp session in Tandem.
  - A compaction row with pre/post tokens and the summary appears.
  - Repeat with a forced auto-compaction (low threshold).

### 2.3 Subagent nesting

- **Problem:** Tandem nests subagent output via `parentToolCallIdPath` into
  update `_meta`, which only Claude provides. omp sends no `_meta`, so `task`
  subagents render flat or not at all.
- **Direction:**
  - Don't invent an omp `_meta` convention. Upstream PR #15043 implements the
    ACP **Subagent Sessions** RFD (agentclientprotocol/agent-client-protocol#1992).
  - Clients that advertise `clientCapabilities.subagents` get `subagent_update`,
    plus each child streaming under its own session id
    `<root>/<agentId>`.
- **omp:** carry #15043 on `tandem` while it's open. Review and test it, and
  report findings on the PR (after confirmation).
- **Tandem:** implement the Subagent Sessions client side:
  - advertise `subagents`;
  - track child sessions from `subagent_update`;
  - route child `session/update`s into the parent transcript as a nested group
    keyed by child session id.
  - This generalizes the Claude `parentToolCallIdPath` rendering, which stays
    for Claude. This is a sizeable Tandem feature, so plan it separately in
    `docs/` before implementing.
- **Verify:** an omp turn that spawns two `task` subagents shows two nested
  groups with their own tool calls and end states.

## Phase 3: omp features Tandem should use (Tandem-only)

### 3.1 Elicitation (`elicitation.form`)

- **Problem:** Tandem doesn't advertise `elicitation.form`, so omp:
  - creates sessions with `interactivePrompts: false`, which disables the `ask`
    tool path;
  - auto-approves plan proposals;
  - returns empty for extension `select`/`confirm`/`input`;
  - never installs the usage-fallback confirmer.

  Upstream #11686 describes the same failure from the omp side.
- **Tandem:**
  - Advertise `clientCapabilities.elicitation: {form: {}}`.
  - Handle the agent→client request `elicitation/create`
    `{sessionId, mode:"form", message, requestedSchema}`.
  - Emit an approval-style event carrying the schema.
  - Render a form in the approvals rail. The schema is flat: string, enum,
    boolean and number fields.
  - Reply `{action:"accept", content}`, `{action:"decline"}` or
    `{action:"cancel"}`.
  - `Interrupt` cancels pending elicitations the way it cancels permissions.
  - Persist the prompt and its outcome in the event log, so it can be replayed
    on reconnect.
  - Document it in `docs/ws-protocol.md` and `docs/acp-notes.md`.
- **Verify:**
  - In omp plan mode, a plan proposal shows "Approve and execute / Refine plan"
    in Tandem. Refine keeps plan mode.
  - An `ask` tool call shows a form and the answer reaches the model.

### 3.2 `session/list` paging and cwd scoping

- **Problem:** the probe calls `session/list` with `{}` and ignores
  `nextCursor` (`probe.go:56`). omp pages at 50, so the resume palette only
  sees the newest 50.
- **Tandem:**
  - Follow `nextCursor` until it's exhausted.
  - Bound it with a page cap and a context deadline, and log the page count.
  - Pass `cwd` when the caller is scoped to a project.
- **Verify:** with more than 50 omp sessions, the palette lists older sessions.

### 3.3 `session/resume` instead of load-and-discard

- **Problem:** on restore, Tandem calls `session/load` and suppresses the replay
  (`adapter.go:267-273`, `:366`). Tandem keeps its own event log, so the replay
  is wasted work.
- **Tandem:**
  - When `sessionCapabilities.resume` is present, restore with `session/resume`
    `{sessionId, cwd, mcpServers}` and skip the replay barrier.
  - Keep `session/load` for agents without resume, and for adopting forks and
    asides. Claude needs `load` to make a fork promptable.
- **Verify:** restart the daemon with a long omp session. It restores with no
  replay traffic, and the next prompt keeps the conversation context.

### 3.4 `session_info_update` titles

- **Problem:** omp sends `{title, updatedAt}` at bootstrap, at turn end and on
  rename. Tandem ignores it (`adapter.go:1388`).
- **Tandem:**
  - Emit a `session_info` event.
  - Use `title` as the session's display name unless the user has named the
    session explicitly; a user name wins.
- **Verify:** renaming an omp session (or omp's own auto-title after the first
  turn) updates the rail title.

### 3.5 Embedded context in prompts

- **Problem:** omp advertises `promptCapabilities.embeddedContext` and accepts
  `resource` and `resource_link`. Tandem only sends `text` and `image`.
- **Tandem:**
  - Gate on `embeddedContext`.
  - Send attached workspace files as `resource` `{uri: file://…, text}`, with a
    size cap and a binary guard.
  - Send mentioned paths as `resource_link`.
  - Extend `resolvePrompt` and the composer's attachment model.
- **Verify:** attach a source file to a prompt. omp quotes its contents without
  a read tool call.

### 3.6 Close main sessions

- **Tandem:** when a Tandem session is stopped or deleted and the agent
  advertises `sessionCapabilities.close`, send `session/close` before tearing
  down the process. Make it best effort with a short timeout, as the aside path
  already does.
- **Verify:** the omp process logs disposing the session on stop.

### 3.7 Per-turn token usage from `PromptResponse.usage`

- **Tandem:** parse `usage` (input, output, total, cached read/write tokens)
  from the `session/prompt` response and attach it to the turn-end event.
- **Verify:** the turn-end event carries token counts for an omp turn.

### 3.8 Tool-call detail: `rawOutput`, `locations`, permission option `kind`

- **Tandem:**
  - Forward `rawOutput` and `locations` from `tool_call` and `tool_call_update`.
    Locations make "open file" possible from a tool row.
  - Keep each permission option's `kind`, so the UI can style
    `allow_always`/`reject_always` distinctly and doesn't depend on option
    names.
- **Verify:** an omp edit shows the file location, and the permission prompt
  shows the always-allow option styled as persistent.

### 3.9 Auth methods and terminal auth

- **Tandem:**
  - Read `authMethods`.
  - Advertise `clientCapabilities.auth.terminal`.
  - When a prompt or `session/new` fails with an auth-required error, offer
    "Sign in in terminal". That opens a Tandem terminal running the agent's
    launch command with the method's `args` (for omp, `--acp-terminal-auth`).
  - Never call `authenticate` with a method the agent didn't list.
- **Verify:** an omp install with no credentials surfaces the sign-in action,
  and completing it in the terminal lets the next prompt succeed.

### 3.10 omp extension methods

omp exposes `_omp/usage`, `_omp/sessions/listAll`, `_omp/projects/list`,
`_omp/chats/byCwd`, `_omp/extensions` (and `/toggle`) and `speech.models.list`.

- **Decision:** don't build on these now. They are undocumented and
  omp-specific, and every one overlaps an ACP-standard path Tandem should use
  first (`session/list`, `usage_update`).
- **Revisit:** `_omp/usage` (provider account limits) once the usage UI has
  somewhere to show account-level quota.

## Phase 4: docs cleanup

Fix the mismatches the audit found:

- `docs/acp-notes.md:107-108` says `current_mode_update`,
  `available_commands_update` and `config_option_update` are ignored. They are
  consumed; only `session_info_update` is ignored, and 3.4 changes that.
- `docs/acp-notes.md:26` documents `additionalDirectories`, which Tandem never
  sends.
- `docs/agent-adapter.md:75` pins SDK 1.2.1, while `acp-notes.md` pins 1.4.0.

Document the omp fork record and the monorepo fork fields in
`docs/spawn-and-workspaces.md`.

## Order and tracking

| # | Item | Where | Upstream |
|---|---|---|---|
| 0.1 | Update omp ≥ 18.12.6, raise pin | Tandem | already upstream (fork base 18.13.2 has it) |
| 0.2 | Monorepo fork builds | Tandem | — (done) |
| 0.3 | Create `aiguy110/oh-my-pi` | GitHub | — (done) |
| 1.1 | `usage_update size 0` | Tandem + omp | direct PR |
| 1.2 | MCP partial failure | Tandem | fixed in 18.13.x |
| 1.3 | Lenient restore of mode/config | Tandem | — |
| 2.1 | Fork at message | Tandem gate, then omp | Discord, then PR |
| 2.2 | Compaction updates | omp | PR, links #16969 |
| 2.3 | Subagent sessions | omp (carry) + Tandem | PR #15043 (theirs) |
| 3.1 | Elicitation | Tandem | — |
| 3.2 | `session/list` paging | Tandem | — |
| 3.3 | `session/resume` | Tandem | — |
| 3.4 | Session titles | Tandem | — |
| 3.5 | Embedded context | Tandem | — |
| 3.6 | Close main sessions | Tandem | — |
| 3.7 | Per-turn usage | Tandem | — |
| 3.8 | Tool-call detail | Tandem | — |
| 3.9 | Terminal auth | Tandem | — |
| 3.10 | omp ext methods | — | deferred |
| 4 | Docs cleanup | Tandem | — |

Phase 1's Tandem halves and Phase 3 don't depend on the fork, and can ship as
soon as they're ready. Each fork change moves into the `acpForks.omp` record
(`--pr N`) when its upstream PR opens. When a carried PR merges and its release
is published, rebase `tandem` onto that tag and drop the commit. When nothing
is left, run `tandem acp upstream omp`.
