# Script automation framework proposal

## Goal

Give agents a durable TypeScript automation surface, inspired by Hermes, for
repository-owned scripts, scheduled checks, and intentional agent handoffs.
Scripts may use ordinary Node.js APIs and may also call explicitly granted MCP
tools through Tandem. Tandem records each run and exposes its stdout, stderr,
exit code, reports, tool-call audit trail, and any agent it wakes.

Saved scripts live beneath the repository root at:

```text
.tandem/scripts/**/*.ts
```

Adding a file does not activate its schedule. An agent or user must explicitly
preapprove and register it.

## Trust and security model

Scripts are trusted repository code, comparable to a shell script, package
script, or build tool. A script runs as an ordinary program with the operating
system access of the Tandem process. It may use the filesystem, environment,
network, child processes, installed packages, and unrestricted local imports.
Tandem does not sandbox or hash the script or its dependency graph as a security
boundary.

MCP grants control only access to Tandem-brokered tools. They do not restrict
what a script can do through Node.js, a child process, or another locally
available interface. The approval UI must state this distinction explicitly.
Users who require host isolation should run Tandem, or the relevant workspace,
inside a container or virtual machine.

The daemon still resolves saved-script paths beneath `.tandem/scripts`, rejects
path traversal and symlink escapes, and avoids exposing reusable daemon
credentials to child processes. Those are integrity protections for Tandem's
script interface, not an OS sandbox.

## Principles

1. **Repository-owned automation.** Scripts, descriptions, requested MCP tools,
   schedules, browser selection, and default wake behavior are reviewable with
   the repository.
2. **Explicit repository-scoped MCP grants.** A user approves requested MCP
   capabilities for a repository. A script edit does not revoke existing
   grants, but a newly requested tool requires approval.
3. **Ordinary TypeScript.** Scripts have normal host access, imports, package
   resolution, arguments, stdout, stderr, and exit behavior.
4. **Explicit scheduling.** Committing frontmatter never silently creates or
   changes a durable job. Registration and synchronization are deliberate.
5. **Intentional wakeups.** A successful script can request an agent handoff
   using structured context. Script failure is represented separately.
6. **Daemon-owned history.** Jobs, runs, grants, tool calls, browser clones, and
   wake relationships survive UI reconnects and daemon restarts.

## Architecture

```text
Agent direct MCP calls ─┐
                       ├─ ToolRegistry ─ grants / schemas / audit / browser
TypeScript MCP calls ──┘
                              ▲
                              │ short-lived authenticated local RPC
                    ScriptRunner (ordinary Node process)
                       │       │             │
                 stdout/err  report()   process exit
                       └───────┴─────────────┘
                                  │
                     invoking agent or Scheduler
                                  │
                      optional agent wake / recovery
```

`ToolRegistry` is a Go daemon service. A tool declaration includes its name,
JSON schema, availability predicate, authorization rules, and invocation
handler. Direct MCP calls and script-originated MCP calls share the same
registry, validation, implementations, and audit path.

For every invocation, Tandem starts a short-lived Node.js process using its
bundled TypeScript loader. The child inherits the normal process environment,
uses the repository root as its working directory, and receives a per-run local
RPC endpoint plus a short-lived token restricted to that repository, run, and
approved MCP tool set. The token is not a reusable daemon credential.

## Script frontmatter

A saved script may begin with YAML in a leading `@tandem` documentation block:

```ts
/**
 * @tandem
 * name: gmail-reply-watcher
 * description: Check whether a reply of interest has arrived.
 * tools:
 *   - name: playwright.browser_find
 *     reason: Search the authenticated inbox for a matching reply.
 *   - name: playwright.browser_snapshot
 *     reason: Capture the matching message for the awakened agent.
 * browser:
 *   snapshot: gmail-authenticated
 * schedule:
 *   cron: "every 5m"
 *   timezone: America/New_York
 *   concurrency: skip
 * wake:
 *   agentProfile: inbox-triage
 *   prompt: Review the detected message and decide what to do next.
 */
```

The fields are:

- `name` and `description`: display metadata.
- `tools`: MCP tool names plus script-authored reasons shown during approval.
- `browser.snapshot`: a named immutable browser-state snapshot to clone per run.
- `schedule`: an optional cron expression, timezone, and concurrency policy.
- `wake.agentProfile`: the default profile for intentional wakeups and failure
  recovery.
- `wake.prompt`: the base prompt supplied to a woken agent.

Reasons are explanatory, untrusted script text. The approval UI separately
shows Tandem's normalized description of each capability. Frontmatter does not
describe or restrict general host permissions because the script already has
ordinary programmatic access.

## TypeScript runtime

The generated `tandem:runtime` module exposes typed MCP tools and `report()`:

```ts
import { tools, report } from "tandem:runtime";

const matches = await tools.playwright.browser_find({
  text: "Polar Ice Air",
});

if (matches.length === 0) {
  report({ wakeAgent: false });
} else {
  report({
    wakeAgent: true,
    context: {
      matchCount: matches.length,
      summary: "A reply from Polar Ice Air was found.",
    },
  });
}
```

`report()` accepts either of these shapes:

```ts
type ScriptReport =
  | { wakeAgent: false; context?: JsonValue }
  | {
      wakeAgent: true;
      agentProfile?: string;
      context?: JsonValue;
    };
```

For `wakeAgent: true`, `agentProfile` overrides `wake.agentProfile` for that
report only. If it is omitted, Tandem uses the frontmatter default. Tandem
validates the selected profile before spawning. A wake request with neither an
override nor a frontmatter default fails as an invalid wake request; it does not
silently choose an arbitrary agent.

```ts
report({
  wakeAgent: true,
  agentProfile: "security-review",
  context: {
    finding: "Unexpected OAuth scope appeared",
    severity: "high",
  },
});
```

The last successfully emitted report is authoritative. If no report is
emitted, the default is `wakeAgent: false`. Ordinary stdout is never parsed as
the public report protocol, even if the internal transport uses a reserved
record format.

When an agent is awakened, it receives the configured prompt, structured report
context, repository and script path, run ID, stdout, stderr, and the run's live
browser clone when one exists.

## Run outcomes and wake semantics

Intentional handoff and process failure are distinct:

| Process result | Report | Meaning |
|---|---|---|
| Exit `0` | absent or `wakeAgent: false` | Successful quiet completion |
| Exit `0` | `wakeAgent: true` | Successful condition-of-interest handoff |
| Non-zero | any | Script failure and recovery path |

A non-zero exit never becomes an intentional report wake, even if the script
emitted `wakeAgent: true` first. If the script has `wake.agentProfile`, Tandem
starts that default profile in recovery mode with the exit code, stdout, stderr,
run metadata, and configured prompt. The per-report profile override does not
apply to a failed process because its report was not successfully completed. If
there is no default wake profile, Tandem records and surfaces the failure
without spawning an agent.

Runner startup failures, invalid reports, MCP authorization errors,
cancellation, and daemon shutdown are recorded separately from an intentional
non-zero script exit where possible. The proposal adds no configurable time,
memory, output, or tool-call limits in the first implementation. Explicit
cancellation and unambiguous terminal run states are still required.

## Repository-scoped MCP grants

An approval belongs to Tandem's stable repository record, not a worktree,
remote URL, individual script hash, or agent session. The canonical repository
identity should be backed by its Git common directory so worktrees share the
same grants and changing a remote does not silently change identity.

When a script requests tools the repository has not been granted, Tandem shows
one combined approval with:

- repository identity and script path;
- every newly requested MCP tool;
- each script-authored reason;
- Tandem's normalized capability description; and
- the requested browser snapshot, when applicable.

Approval adds those capabilities to the repository grant set. Any script in the
same repository may then use them. Editing code or removing tool declarations
does not invalidate existing grants. Adding a newly required tool blocks the
run until that capability is approved. Tandem may retain the source hash for
audit and display “changed since reviewed,” but the hash is not the permission
identity.

The runner token and generated tool bindings contain only the intersection of
the script's requested tools and the repository's grants. Undeclared or
ungranted MCP calls fail even when the repository has granted that capability
for a different requested context. Tool calls pass through `ToolRegistry` for
schema validation, availability checks, browser binding, audit records, and
tool-specific safeguards.

## Agent-facing API

Agents receive three script operations:

```ts
scripts.run({
  path: ".tandem/scripts/check-inbox.ts",
  args?: string[],
});

scripts.evaluate({
  source: string,
  args?: string[],
  requestedTools?: Array<{
    name: string;
    reason: string;
  }>,
  browserSnapshot?: string,
});

scripts.preapprove({
  path: ".tandem/scripts/check-inbox.ts",
  registerSchedule?: boolean,
});
```

`run` resolves a repository-relative saved script, validates its frontmatter,
checks grants, and executes it. Arguments are passed through `process.argv`.

`evaluate` executes ephemeral TypeScript with the same trusted host access. It
does not have saved frontmatter, so it declares requested MCP tools and browser
snapshot in the invocation. It returns its report to the calling agent but does
not register a schedule. A future extension may add explicit wake configuration
if ephemeral wakeups prove useful.

`preapprove` parses and validates a saved script, creates or returns one user
approval request for missing repository MCP grants, and optionally registers
the declared schedule after approval. The calling agent can request approval
but cannot grant it. The result distinguishes `approved`, `pending`, and
`rejected` and includes an approval ID when applicable.

If `run` needs approval, it creates or reuses a pending approval and returns
without starting the script. A scheduled occurrence needing approval is
recorded as `awaiting_approval`; it does not repeatedly create requests and does
not run retroactively when approved.

## Scheduling

Schedules are explicit durable database records. `registerSchedule: true`
imports the frontmatter schedule after validation and approval. Merely creating,
editing, or committing a script never starts a job.

At each tick Tandem rereads the current script and frontmatter. Code changes
take effect on the next run. Newly requested MCP tools require approval.
Schedule, browser snapshot, or wake-configuration changes are shown as pending
and require an explicit synchronization operation before the durable job
changes; the last registered job specification remains authoritative until
then.

`concurrency` controls overlapping script executions and initially supports:

- `skip` (default): record and skip a tick while the prior run is active.
- `queue`: retain one pending occurrence to run after the active run finishes.

After a successful wake or a failure recovery, later ticks are skipped while
the linked agent is in a nonterminal state. This prevents a polling script from
creating a new agent for the same condition on every tick. The scheduler does
not replay missed occurrences after daemon downtime; it computes the next
future cron occurrence.

## Immutable browser snapshots

`browser.snapshot` names a copied, immutable browser state. For each run:

1. Tandem creates a writable browser profile from the snapshot.
2. Approved browser MCP tools operate on that run-specific clone.
3. An awakened agent inherits the live clone when one exists.
4. A clone with no awakened agent is discarded after the run.
5. The source snapshot is never modified.

Concurrent runs never share a writable clone. Snapshot selection is part of the
registered job specification and the approval display, while MCP browser access
still requires the relevant repository tool grants.

## Persistence

Add these SQLite tables or equivalent store records:

- `repository_tool_grants`: repository, MCP tool capability, approval actor and
  timestamps, source approval request, and revocation state.
- `automation_jobs`: repository, script path, registered schedule, timezone,
  concurrency, browser snapshot, wake specification, enabled state, and parsed
  source metadata used to detect pending synchronization.
- `automation_runs`: job or direct invocation, scheduled/start/end timestamps,
  source hash for audit, process and runner outcomes, stdout, stderr, exit code,
  report payload, browser clone, and skip/approval reason.
- `automation_wakeups`: run-to-agent mapping, selected profile, intentional or
  failure reason, supplied context, and durable lifecycle state.
- `automation_tool_calls`: run-linked audit references for individual MCP calls.

Suggested run outcomes are `succeeded_quiet`, `succeeded_wake_requested`,
`failed`, `runner_failed`, `awaiting_approval`, `skipped_concurrency`,
`skipped_agent_active`, and `cancelled`. On daemon restart, runs whose child
process cannot be proven alive transition to an explicit interrupted/runner
failure state; existing wake-agent links are restored from durable agent state.

## Phased implementation

### Phase 1: Registry and repository grants

- Introduce the daemon-owned `ToolRegistry` abstraction and adapt the existing
  Tandem-control MCP tools first.
- Add repository identity resolution based on the Git common directory.
- Persist repository-scoped MCP grants and combined approval requests.
- Expose normalized capability descriptions and audit records.
- Verify direct MCP calls continue to use the same handlers and authorization.

Exit criteria: direct calls work through the registry, grants survive restart,
worktrees resolve to the same repository grants, and agents cannot approve
their own requests.

### Phase 2: Trusted TypeScript execution

- Add the short-lived Node/TypeScript runner with ordinary host access.
- Implement repository-safe saved-path resolution, frontmatter parsing,
  `tandem:runtime`, typed bindings, short-lived RPC authorization, and durable
  run/tool-call records.
- Add `scripts.evaluate`, `scripts.run`, and `scripts.preapprove` without
  scheduling.
- Implement `report()` parsing and the three-way run outcome model, initially
  returning intentional wake requests without spawning an agent.

Exit criteria: saved and ephemeral scripts can use Node APIs and granted MCP
tools; ungranted calls stop before execution; stdout, stderr, exits, reports,
and tool calls are auditable across restart.

### Phase 3: Intentional wakeups and failure recovery

- Spawn agents through the existing profile/workspace path.
- Resolve `report().agentProfile` before the frontmatter default for successful
  wake requests.
- Use only the frontmatter default profile for non-zero recovery.
- Pass structured context, prompt, output, run metadata, and repository identity
  to the spawned agent; persist and restore the run-to-agent relationship.

Exit criteria: quiet success, overridden-profile wake, default-profile wake,
non-zero recovery, missing-profile validation, and daemon restart all produce
unambiguous durable state.

### Phase 4: Durable scheduling

- Add cron parsing, timezone handling, registration/synchronization, enable and
  disable operations, skip/queue concurrency, and no-downtime-replay behavior.
- Record awaiting-approval and skipped occurrences.
- Suppress later ticks while a linked wake/recovery agent is nonterminal.
- Add job/run/approval/wakeup visibility to the UI.

Exit criteria: registered schedules survive restart, unregistered frontmatter
never executes, changes require sync, and every eligible or skipped tick is
explainable from persisted records.

### Phase 5: Immutable browser-state snapshots

- Add named immutable browser snapshots and per-run writable cloning.
- Bind browser MCP tools to the clone and transfer a live clone to a woken
  agent.
- Discard unused clones and guarantee the source snapshot is unchanged.
- Implement a browser-backed watcher as the first end-to-end example.

Exit criteria: concurrent runs are isolated, authenticated starting state is
repeatable, wake agents inherit the correct clone, and source snapshots remain
byte-for-byte/logically unchanged.

## Testing strategy

Each phase should include unit, integration, persistence, and negative tests:

- path traversal, symlink escape, malformed frontmatter, missing repository,
  and invalid or absent agent profiles;
- repository identity across worktrees, grant approval/rejection/revocation,
  new-tool approval, and attempts to call undeclared tools;
- normal Node filesystem/network/subprocess behavior to document the trusted
  execution contract;
- RPC token scoping, schema validation, child exit, cancellation, daemon
  restart, stdout/stderr capture, and tool-call audit linkage;
- no report, quiet report, default-profile wake, per-report profile override,
  multiple reports, invalid report, report followed by non-zero exit, and
  non-zero failure with and without a recovery profile;
- cron/timezone boundaries, downtime, concurrency skip/queue, pending approval,
  pending specification sync, and active-agent suppression;
- immutable snapshot cloning, concurrent isolation, discard behavior, and live
  clone transfer to the correct agent.

The end-to-end acceptance test should register a repository script, obtain its
MCP grant, run quietly once, detect a condition and override the default agent
profile on a later run, then exercise non-zero recovery using the frontmatter
default while proving the immutable browser snapshot was not mutated.
