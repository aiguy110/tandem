# Script automation framework proposal

## Goal

Give agents a durable TypeScript automation surface inspired by the Hermes coding
harness. An agent can write, save, run, and evaluate scripts which use the same
Tandem tools it can invoke directly. Scripts make multi-step automation concise
without hiding the outcome from the calling agent: the caller receives the
script's stdout, stderr, and exit code, rather than every internal tool call.

Tandem also schedules saved scripts with cron-style schedules. A non-zero exit
can launch a configured agent to investigate or act on the failure.

## Principles

1. **Daemon-owned and durable.** Definitions, runs, schedules, and recovery
   relationships live in Tandem's SQLite store. A UI reconnect or daemon restart
   must not lose a job definition or make a running recovery agent ambiguous.
2. **One tool catalog.** MCP and scripts are separate transports over one daemon
   `ToolRegistry`; a tool's schema, availability, authorization, approval rules,
   and implementation are defined once.
3. **No approval bypass.** A script call is authorized and approved exactly as
   the equivalent direct tool call would be.
4. **Bounded agent visibility.** The agent invoking a script receives only the
   script result (stdout, stderr, exit code, duration, and run ID). Tandem keeps
   the individual tool-call audit trail available to the user and logs.
5. **Explicit capability context.** A script never silently borrows a live
   agent's workspace or browser. Its workspace and browser identity are selected
   in its execution context.

## Architecture

```text
Agent direct calls ─┐
                    ├─ ToolRegistry ── approvals / audit / workspace / browser
TypeScript scripts ─┘
                         │
                  ScriptRunner (short-lived Node process)
                         │
           stdout / stderr / exit code → invoking agent or scheduler
                         │
             Scheduler → optional recovery-agent launch on non-zero exit
```

`ToolRegistry` is a Go daemon service. A tool declaration includes its name,
JSON schema, availability predicate, approval classification, and invocation
handler. Existing MCP servers become adapters over it. The initial refactor
targets the browser and Tandem-control tools currently wired in
`internal/browser/mcp.go` and `internal/controlmcp`.

For every script invocation, the daemon starts a short-lived Node runner and
generates a temporary ESM module plus `.d.ts` declarations. The module exposes
only the tools available to the invocation's capability context. Tool calls are
made over authenticated local IPC back to the daemon; they do not give the child
process daemon credentials or direct access to internal services.

## TypeScript surface

The generated module is imported as `tandem:runtime`:

```ts
import { tools, log } from "tandem:runtime";

const messages = await tools.playwright.browser_find({ text: "Polar Ice Air" });
if (messages.length === 0) {
  log("No reply yet");
  process.exit(0);
}

log("Reply found");
process.exit(1);
```

The exact generated `tools` type is specific to the invocation. This keeps
documentation, TypeScript completion, and runtime availability aligned.

Two equivalent entry points are exposed to agents:

```ts
scripts.evaluate({ source, context, timeoutMs? });
scripts.run({ path, args?, context, timeoutMs? });
```

`evaluate` writes ephemeral source for one run. `run` executes a saved `.ts`
file. Both use the same runner and return stdout, stderr, exit code, duration,
and a durable run ID.

## Execution context

Every invocation specifies an `ExecutionContext`. Its initial fields are:

```ts
type ExecutionContext = {
  workspace: { kind: "existing"; cwd: string };
  browser?: { profileId: string };
  toolScope?: string[];
};
```

The browser field is required for browser-backed automation. It refers to a
durable automation browser profile, not an arbitrary active agent browser. This
is necessary for a watcher such as Gmail: scheduled work cannot depend on an
agent session that might be closed when the schedule fires.

## Scheduling and recovery

```ts
scripts.schedule({
  scriptPath,
  cron,
  timezone?,
  context,
  onNonZero?: {
    profile?: string,
    prompt: string,
    workspace: WorkspaceSpec,
  },
  concurrency?: "skip" | "queue",
  skipWhileRecoveryAgentRunning?: boolean,
});
```

`concurrency` and `skipWhileRecoveryAgentRunning` intentionally cover distinct
forms of overlap:

- `concurrency` controls concurrent **script executions**. Its default is
  `skip`: if a previous invocation is still running when the next cron tick
  occurs, Tandem records that occurrence as skipped and does not run another
  copy.
- `skipWhileRecoveryAgentRunning` controls later ticks after a script has
  already completed non-zero and launched a **recovery agent**. Its default is
  `true`: no further invocation fires while that recovery agent is `working` or
  `blocked`.

This prevents a quick polling script from spawning a new agent on every tick
after it identifies the same condition. When the recovery agent reaches a
terminal state, the next normal schedule occurrence is eligible to run.

The scheduler does not replay missed occurrences after downtime; it calculates
the next future cron occurrence. It records all starts, finishes, skips, and
recovery-agent links so the UI can explain why a job did or did not fire.

## Persistence

Add these SQLite tables:

- `automation_jobs`: path, cron expression, timezone, execution context,
  concurrency and recovery policy, and enabled state.
- `automation_runs`: scheduled time, start/end timestamps, outcome, bounded
  stdout/stderr, exit code, and skip reason.
- `automation_recoveries`: job/run to spawned-agent mapping and lifecycle state.
- `automation_tool_calls`: user-visible audit references for individual tool
  calls made inside a run.

Output retention should be bounded by byte and age limits; the run record keeps
a truncation marker rather than silently dropping data.

## Initial delivery sequence

1. Introduce `ToolRegistry` and adapt the existing Tandem-control tool to it.
2. Add the isolated TypeScript runner, generated bindings, `evaluate`, and
   `run`, with run/audit persistence and tests.
3. Add durable cron scheduling, skip semantics, and job/run UI visibility.
4. Add non-zero recovery-agent launching through the existing profile and
   workspace spawn path.
5. Add durable automation browser profiles, then implement the Gmail reply
   watcher as the first end-to-end example.

## Open decisions

- Which direct agent tools are safe and useful in v1 script contexts, beyond
  Tandem-control and browser tools?
- Should queued occurrences coalesce to one pending run or preserve every missed
  tick when `concurrency: "queue"` is selected?
- What retention limits should stdout, stderr, and tool-call audit records use?
- How should an automation browser profile be created, authenticated, paused,
  and explicitly shared with a user?
