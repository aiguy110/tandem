# Go Backend Migration Plan

## Purpose

Migrate Tandem's daemon from Node.js/TypeScript to Go while keeping the React UI,
browser-to-daemon protocol, persisted data, agent behavior, and deployment semantics
compatible throughout the transition.

The migration is intentionally divided into phases that a competent coding agent can
complete and verify in one focused session. A phase should normally produce one reviewable
commit. If a phase uncovers a required wire or schema change, that change must be documented
and isolated rather than folded into unrelated porting work.

The desired end state is:

- a native `tandem` daemon executable built with Go;
- the existing React application embedded in that executable;
- no Node.js dependency for Tandem's own HTTP, WebSocket, persistence, registry, terminal,
  workspace, or browser-broker implementation;
- ACP agents and third-party MCP servers remaining ordinary managed subprocesses;
- `@playwright/mcp` remaining an external agent-owned process initially;
- direct Chrome DevTools Protocol (CDP) use inside the Go daemon instead of
  `playwright-core`;
- the existing SQLite database, `$TANDEM_HOME` layout, `config.yml`, and WebSocket protocol
  remaining compatible across rollback to the Node daemon.
- the feature-aware workspace lineage implemented by the Node daemon—explicit create/attach
  intent, canonical source ref, immutable source commit, integration target, and
  feature-relative status—remaining intact in Go.

## Non-goals

The migration does not initially:

- rewrite the React UI;
- redesign Tandem's WebSocket protocol;
- replace ACP with a Go-specific agent integration;
- reimplement Playwright MCP in Go;
- bundle agent implementations into the Tandem executable;
- complete the planned ACP registry or managed-install stages;
- change the worktree model or branch naming rules;
- introduce multi-tenant hosting;
- require the Node and Go daemons to write the same database concurrently.

## Target architecture

```text
Browser (existing React UI, embedded in tandem)
  | HTTP + authenticated/replayable WebSocket
  v
Go tandem daemon
  |-- config, token, SQLite store, event log
  |-- session and agent registry
  |-- ACP adapter (JSON-RPC over child stdio)
  |-- PTY adapter and daemon-owned terminal services
  |-- workspace and asset services
  `-- browser broker
       |-- local Chromium or Steel driver
       |-- gated CDP proxy for the agent side
       `-- direct daemon CDP client for screencast and user input

ACP agent process
  | owns MCP client lifecycle
  |-- external @playwright/mcp --cdp-endpoint <broker URL>
  `-- tandem mcp-control (same Go executable, internal subcommand)
```

There is still one browser with two CDP connections. The agent-owned Playwright MCP
connection passes through Tandem's gated proxy. Tandem's daemon-owned CDP connection goes
directly to the browser so screencast and human input continue while agent commands are
held.

## Migration invariants

Every phase must preserve these constraints:

1. **The daemon owns durable state.** The UI must remain reconstructable after reconnect
   from SQLite plus live daemon state.
2. **The browser remains adapter-agnostic.** ACP details do not leak into the React store or
   WebSocket message shapes.
3. **Sequence numbers remain monotonic per agent.** Reconnect replay cannot duplicate, skip,
   or renumber persisted events.
4. **Existing data remains rollback-safe.** Until final cleanup, a database last written by
   Go must still open correctly under the Node daemon.
5. **One writer at a time.** Tests and deployment must never run Node and Go daemons against
   the same `TANDEM_HOME` concurrently.
6. **Workspace containment remains centralized.** All ACP filesystem access continues
   through one realpath-aware sandbox boundary.
7. **The human control token remains a hard pause.** Agent CDP command frames are queued,
   not rejected, while the user controls the browser.
8. **Subprocess ownership stays explicit.** Tandem owns agents, PTYs, and service processes;
   the ACP agent owns the MCP processes declared during `session/new`.
9. **No production cutover on partial parity.** The Node service remains the default until
   the complete black-box suite passes against the Go binary.

## Rules for session-sized phases

Each phase below is scoped so one agent session can finish it. The implementing agent must:

1. read this plan and the current subsystem documentation before editing;
2. inspect the Node implementation being replaced rather than porting from memory;
3. add or extend tests in the same session;
4. run the phase-specific verification plus `go test ./...` once a Go module exists;
5. keep Node behavior working unless the phase explicitly performs the final cutover;
6. update this document when implementation reality changes the remaining plan;
7. commit only after verification passes.

A phase is not complete when it merely compiles. Its acceptance criteria must pass from a
clean checkout without relying on a developer's existing `$TANDEM_HOME`.

## Compatibility strategy

### Contract sources of truth

- `docs/ws-protocol.md` defines browser-facing messages.
- `docs/agent-adapter.md` defines normalized agent behavior.
- `docs/acp-notes.md` records pinned ACP behavior.
- `daemon/src/db.ts` and its migrations define the persisted schema at migration start.
- `config.yml.example` defines the shipped catalog and substitution behavior.
- the `derisk:*` suites define executable end-to-end behavior.

### Testing layers

The migration should use four layers rather than depending only on Go unit tests:

1. Go package tests for deterministic subsystem behavior.
2. Golden contract fixtures shared by Node and Go.
3. Existing black-box de-risk suites parameterized to launch either daemon.
4. A final service-level smoke test against the built release artifact.

### Rollback model

Before final cutover, switching implementations means stopping the current daemon and
starting the other one against the same `$TANDEM_HOME`. No separate migration command
should be necessary. New schema changes must remain additive until the Node daemon is
retired.

## Phase 0 - Record the migration baseline

**Goal:** Turn current Node behavior into an explicit compatibility target before Go code
can drift from it.

**Work:**

- Add a migration contract test directory containing canonical JSON fixtures for client
  messages, server messages, normalized events, agent records, and `config.yml` examples.
- Add a small Node fixture generator that serializes representative values through the
  current implementation.
- Record the current SQLite schema, indexes, pragma values, and a small seeded database as
  test inputs. Generate the database during tests rather than committing WAL files.
- Record exit codes and observable startup output such as `TANDEM_READY`.
- Include feature-workspace fixtures for canonical Git refs, explicit create/attach modes,
  immutable source commits, integration targets, and legacy `baseRef` records.
- Add a document mapping every `derisk:*` suite to its owning subsystem.

**Acceptance:**

- Fixture generation is deterministic.
- Existing daemon tests still pass.
- A fresh test verifies the seeded database can be opened and replayed by Node.

**Out of scope:** Go code, protocol improvements, and schema cleanup.

## Phase 1 - Create the Go module and CI foundation

**Goal:** Establish a buildable Go daemon skeleton without claiming feature parity.

**Work:**

- Add `go.mod`, `cmd/tandem/main.go`, and a conventional `internal/` package layout.
- Add build metadata fields for version, commit, and build time using linker flags.
- Implement `tandem version` and a placeholder `tandem daemon` command that reports that
  the Go daemon is not yet production-ready.
- Add Go formatting, vet, unit-test, and build jobs to CI.
- Add a development script that builds the React UI and stages it where a Go package can
  embed it later; do not change the running service.

**Acceptance:**

- `go test ./...`, `go vet ./...`, and `go build ./cmd/tandem` pass.
- `tandem version` reports injected and fallback development metadata correctly.
- Existing npm builds and tests remain unchanged.

**Out of scope:** HTTP serving, SQLite, agents, and deployment artifacts.

## Phase 2 - Port configuration, token management, and the agent catalog

**Goal:** Make Go resolve the same runtime configuration as Node.

**Work:**

- Port all `TANDEM_*` defaults and environment parsing.
- Port `$TANDEM_HOME` directory derivation and permission-safe token creation.
- Embed `config.yml.example` as the built-in catalog.
- Overlay `$TANDEM_HOME/config.yml` with the same precedence, validation, profiles, legacy
  environment overrides, and placeholder expansion.
- Replace the Node-specific `{node}` assumption with an explicit configured Node launcher;
  preserve the current behavior while Node-based adapters remain in use.
- Add a test-only `tandem debug config` JSON view that redacts secrets and makes parity
  comparisons deterministic.

**Acceptance:**

- Table-driven Go tests cover valid configuration, malformed YAML, missing commands,
  profile errors, environment overrides, executable resolution, and token permissions.
- Golden tests show Node and Go produce equivalent normalized configurations.

**Out of scope:** Starting any configured process.

## Phase 3 - Port the SQLite store and schema compatibility

**Goal:** Read and write Tandem's existing database without CGo or format changes.

**Work:**

- Introduce a CGo-free SQLite driver behind a narrow store package.
- Recreate current tables, indexes, additive migrations, WAL mode, and synchronous mode.
- Port agent CRUD, session lookup, close/reopen behavior, asset metadata, and agent-asset
  associations.
- Keep timestamp units, null handling, JSON encoding, row ordering, and status strings
  byte-compatible where observable.
- Add a schema-version mechanism only if it is additive and the Node daemon safely ignores
  it.

**Acceptance:**

- Go opens a database created and populated by Node.
- Node opens the same database after Go has updated every supported record type.
- Tests cover fresh creation, old `cwd` migration, WAL checkpointing, and malformed rows.

**Out of scope:** In-memory event buffering and registry logic.

## Phase 4 - Port the durable event log

**Goal:** Reproduce hot replay, cold replay, and sequence allocation independently of
sessions and WebSockets.

**Work:**

- Implement normalized event encoding, including base64 handling for raw PTY bytes.
- Implement per-agent monotonic sequence allocation.
- Implement the in-memory ring buffer and SQLite cold-replay fallback.
- Implement replay coverage detection and snapshot fallback signals.
- Define locking so concurrent producers cannot reuse or reorder sequence numbers.

**Acceptance:**

- Tests cover append, ring eviction, restart with an empty ring, cold replay, independent
  agent sequences, and concurrent append.
- A Node-produced event history replays identically through Go.
- A Go-produced event history replays identically through Node.

**Out of scope:** Network subscribers and live agent processes.

## Phase 5 - Port workspace management and repository discovery

**Goal:** Match worktree isolation and lifecycle behavior before agents can be spawned.

**Work:**

- Port existing-directory validation and live-directory collision detection.
- Port worktree/branch naming, provisioning, restoration, missing-directory recreation,
  dirty-close refusal, forced close, and branch retention.
- Port read-only local/remote/tag discovery, checked-out-worktree metadata, explicit
  create-versus-attach errors, immutable source-commit resolution, integration targets,
  context-qualified agent branches, and feature-relative ahead/behind/diverged state.
- Preserve transactional cleanup when adapter startup fails after provisioning.
- Port project-root scanning, depth limits, skip directories, repository metadata, and
  collision reporting.
- Centralize Git command execution with context cancellation and captured diagnostics.

**Acceptance:**

- Port the `derisk:workspace` cases into black-box tests runnable against the Go workspace
  package.
- Verify no test touches a real developer repository or home directory.
- Verify Node can restore a worktree record created through Go.

**Out of scope:** Starting agents in provisioned workspaces.

## Phase 6 - Port filesystem sandboxing and asset storage

**Goal:** Establish the daemon-owned filesystem boundary used by ACP and image prompts.

**Work:**

- Port realpath-based containment for reads and writes, including symlink escapes,
  nonexistent write targets, and platform path differences.
- Preserve ACP invalid-parameter error mapping at the adapter boundary.
- Port content-addressed image storage, MIME signature validation, decoded-dimension checks,
  upload limits, per-agent authorization, and physical deduplication.
- Preserve the `$TANDEM_HOME/assets` layout and SQLite metadata associations.

**Acceptance:**

- Round-trip, traversal, symlink, race-resistant parent validation, malformed image, size,
  dimension, deduplication, and cross-agent access tests pass.
- Existing stored assets remain readable.

**Out of scope:** HTTP routes and ACP request dispatch.

**Implementation note:** The Go filesystem boundary uses the descriptor-backed `os.Root`
API, so the native daemon requires Go 1.24 or newer.

## Phase 7 - Build the process and PTY primitives

**Goal:** Provide cancellable subprocess and pseudoterminal foundations without agent
semantics.

**Work:**

- Add a subprocess abstraction with explicit argv, cwd, environment, stdin/stdout/stderr,
  process-group termination, exit observation, and bounded shutdown.
- Add a Unix PTY implementation for Linux and macOS with resize and binary I/O.
- Define Windows behavior explicitly: implement it in this phase only if a supported PTY
  library is selected; otherwise return a clear unsupported error and keep Windows out of
  the initial release matrix.
- Ensure child cleanup is idempotent and does not kill unrelated reused PIDs.

**Acceptance:**

- Tests cover normal exit, cancellation, forced kill, environment/cwd, resize, binary data,
  repeated disposal, and parent shutdown.
- Tests leave no child processes behind.

**Out of scope:** ACP framing and persisted terminal scrollback.

## Phase 8 - Port the daemon-owned TerminalHost

**Goal:** Reproduce ACP terminal service behavior and durable Tandem scrollback.

**Work:**

- Port terminal create, output, wait-for-exit, kill, and release operations.
- Preserve the distinction between ACP-visible `outputByteLimit` truncation and Tandem's
  larger independent scrollback.
- Emit normalized `terminal_output` events through the event-log interface.
- Keep released-terminal scrollback available for replay.
- Add bounded memory and cleanup policies matching current behavior.

**Acceptance:**

- Equivalent cases from `derisk:services` pass against the Go package.
- Tests cover truncation-from-start, output offsets, release-before-exit, kill, and replay
  after release.

**Out of scope:** Handling terminal JSON-RPC requests from a live ACP agent.

## Phase 9 - Implement ACP JSON-RPC transport and generated types

**Goal:** Establish a protocol-correct ACP connection without session behavior.

**Work:**

- Pin the official ACP schema artifact and record its version separately from ACP wire
  protocol version 1.
- Decide in a short ADR whether to use the community Go SDK or an internal schema-generated
  client. Prefer generated types plus a small internal transport unless the community SDK
  demonstrates complete client-service coverage.
- Implement newline-delimited JSON-RPC over child stdio, request IDs, concurrent pending
  calls, notifications, inbound client-service requests, cancellation, stderr forwarding,
  malformed-message handling, and process exit propagation.
- Do not hand-maintain broad copies of schema types when generation is practical.

**Acceptance:**

- Transport tests cover out-of-order responses, concurrent requests, inbound requests,
  notifications, malformed lines, unknown IDs, cancellation, EOF, and child failure.
- Generated code is reproducible from a pinned input.

**Out of scope:** Mapping ACP session updates to Tandem events.

## Phase 10 - Port the ACP adapter session lifecycle

**Goal:** Run a mock ACP agent through initialize, new/load, prompt, update, permission, and
cancel flows.

**Work:**

- Port capability negotiation and `session/new`/`session/load` behavior.
- Port prompt blocks, image capability checks, and inline image resolution.
- Map all supported `session/update` variants into normalized Tandem events.
- Port permission request routing and response correlation.
- Port prompt serialization, cancellation, status transitions, and session ID capture.
- Adapt the existing mock ACP agent so both Node and Go harnesses can launch it.

**Acceptance:**

- The core `derisk`, approval-loop, image, interrupt, and load-session adapter cases pass
  without HTTP or WebSocket involvement.
- Unknown optional updates are tolerated and logged; malformed required messages fail the
  session predictably.

**Out of scope:** Filesystem and terminal client-service request dispatch.

## Phase 11 - Connect ACP client services

**Goal:** Complete Tandem's role as an ACP client by exposing its filesystem and terminal
services to live agents.

**Work:**

- Route `fs/read_text_file` and `fs/write_text_file` through the workspace sandbox.
- Route `terminal/create`, `output`, `wait_for_exit`, `kill`, and `release` through the Go
  TerminalHost.
- Preserve JSON-RPC error codes and validation behavior.
- Ensure every request is scoped to the requesting agent's workspace and terminal set.
- Wire permission requests into the same event and pending-approval abstractions that the
  future registry will consume.

**Acceptance:**

- The relevant `derisk:services` scenarios pass against a live Go ACP adapter and mock
  agent.
- Escape attempts touch no files and create no processes.
- Cancellation unblocks pending client-service operations.

**Out of scope:** Multi-agent registry and browser MCP declarations.

## Phase 12 - Port the raw PTY adapter

**Goal:** Support terminal-only agents and the user's escape-hatch shell.

**Work:**

- Implement spawn, raw byte events, input, resize, exit/error status, and disposal using
  the Phase 7 PTY primitive.
- Preserve the normalized `raw_pty` event representation and binary-safe behavior.
- Keep structured capabilities disabled and reject unsupported operations clearly.

**Acceptance:**

- Port the PTY smoke scenario and run it on every supported release OS.
- Tests cover UTF-8 split across reads, arbitrary binary bytes, resize, shell exit, and
  forced cleanup.

**Out of scope:** Browser-facing binary framing changes.

## Phase 13 - Port session and multi-agent registry behavior

**Goal:** Make the Go process the authoritative in-memory owner of agents and sessions.

**Work:**

- Port agent IDs and auto-name counter seeding across closed records.
- Port adapter selection, workspace ownership, status, prompt serialization, approvals,
  interrupt, close, and disposal.
- Port multi-agent isolation and independent event sequences.
- Port startup restoration so one failed agent becomes `error` without crashing the daemon.
- Preserve database writes at each lifecycle transition.

**Acceptance:**

- Package-level equivalents of `derisk:multi`, `derisk:restart`, and deferred-restart
  behavior pass.
- Tests cover partial restore failure, repeated close, dirty worktree refusal, and shutdown
  with active turns.

**Out of scope:** Network protocol and terminal handoff/resume catalog.

## Phase 14 - Implement authenticated HTTP and embedded UI serving

**Goal:** Make the Go executable serve the unchanged React application and asset API.

**Work:**

- Embed the production UI build into the Go executable with a reproducible build step.
- Implement SPA fallback, content types including WASM/fonts, and current no-cache policy.
- Implement the placeholder page for development builds without embedded UI.
- Implement bearer-token asset upload/download routes and their current limits.
- Preserve bootstrap URL behavior and avoid leaking the token in HTTP requests or logs.

**Acceptance:**

- UI build plus `go build` produces one executable containing the frontend.
- Static asset, SPA route, content-type, auth, upload, download, and path traversal tests
  pass.
- A browser can load the existing UI from the Go daemon.

**Out of scope:** WebSocket upgrades and live agent control.

## Phase 15 - Implement WebSocket subscriptions and replay

**Goal:** Support authenticated connection, multiplexed subscriptions, snapshots, live
events, and reconnect replay.

**Work:**

- Implement pre-protocol token validation and close code 4401 behavior.
- Implement subscribe/unsubscribe by agent and channel.
- Implement snapshot versus replay selection using the Phase 4 event log.
- Implement live fan-out to multiple clients without allowing slow clients to block agent
  event ingestion.
- Implement `list_agents`, `list_dirs`, and `list_agent_catalog` read operations.
- Preserve `corrId`, JSON shapes, ordering, and connection cleanup.

**Acceptance:**

- `derisk:auth` and dropped-pipe replay scenarios pass against the Go daemon.
- Tests cover multiple clients, overlapping channel sets, stale checkpoints, slow clients,
  reconnect, and daemon restart.

**Out of scope:** Mutating prompt/spawn/close commands and browser frames.

## Phase 16 - Implement core WebSocket commands

**Goal:** Make the unchanged React UI able to create and operate ordinary ACP and PTY
agents.

**Work:**

- Implement prompt, input, resize, permission response, interrupt, spawn, close,
  `get_spawn_options`, and the existing `merge_back` error response.
- Preserve validation, structured error prefixes, acknowledgements, and broadcasts.
- Route image prompt blocks through the existing asset and capability checks.
- Ensure disconnecting a browser never disposes its agents.

**Acceptance:**

- Core `derisk`, multi-agent, workspace, service, image, and integration scenarios pass
  against the Go daemon with browser support disabled.
- The React transcript, approvals rail, terminal pane, spawn palette, and agent rail work
  without UI code changes.

**Out of scope:** Resume/handoff and shared-browser commands.

## Phase 17 - Port session discovery, resume, and terminal handoff

**Goal:** Complete the non-browser session lifecycle exposed by the current daemon.

**Work:**

- Port live/Tandem-owned/external session catalog assembly and deduplication.
- Port restore of closed Tandem sessions and import of external sessions.
- Port ACP-to-CLI handoff, busy rejection, interrupt-first behavior, absolute resume CLI
  resolution, control-mode broadcasts, and automatic ACP reload after CLI exit.
- Preserve behavior when an adapter lacks list/load support.

**Acceptance:**

- `derisk:resume`, `derisk:handoff`, and `derisk:config` pass against Go.
- Tests cover live focus, closed restore, external import, unsupported enumeration, busy
  handoff, interrupted handoff, CLI failure, and reload failure.

**Out of scope:** Browser subsystem.

## Phase 18 - Port browser provisioning and the gated CDP proxy

**Goal:** Reproduce lazy browser lifecycle and the agent-side control gate without using
Playwright inside Go.

**Work:**

- Port the `BrowserDriver` seam and Steel REST lifecycle.
- Port local Chromium launch using explicit executable discovery/configuration instead of
  `chromium.executablePath()`.
- Port CDP endpoint resolution, HTTP metadata proxying, WebSocket proxying, URL rewriting,
  payload limits, and cleanup.
- Port per-agent lazy provisioning.
- Port grab/release so outbound agent CDP command frames queue and flush in order while
  browser events can continue in the reverse direction.

**Acceptance:**

- Broker tests connect a raw CDP test client through the proxy and verify cold start,
  pass-through, hard pause, ordered release, disconnect cleanup, and independent agents.
- Steel behavior is tested with an HTTP fake; live Steel remains an opt-in test.

**Out of scope:** Screencast, human input, and MCP registration.

## Phase 19 - Add the daemon-side Go CDP client

**Goal:** Replace Tandem's internal `playwright-core` connection with direct CDP for the
human browser pane.

**Work:**

- Connect directly to the browser endpoint and attach to a suitable page target.
- Create a page target when none exists.
- Implement `Page.startScreencast`, frame acknowledgement, stop, and reconnect behavior.
- Implement mouse, wheel, key, and text input using `Input.*` commands.
- Keep this connection direct and separate from the agent's gated proxy connection.
- Port the development-only navigation hook without introducing high-level Playwright as a
  Go daemon dependency.

**Acceptance:**

- Tests verify frame delivery, acknowledgement, repeated start/stop without listener leaks,
  coordinate/input mapping, navigation recovery, and connection cleanup.
- During user control, screencast and input continue while an agent-side command remains
  held.

**Out of scope:** Accessibility snapshots, selectors, or agent-facing browser tools.

## Phase 20 - Port MCP wiring and Tandem control MCP

**Goal:** Restore the agent-facing browser tool configuration while keeping Playwright MCP
external.

**Work:**

- Generate the `@playwright/mcp` stdio declaration with the broker's per-agent CDP URL.
- Stop deriving Node from the Tandem daemon executable; resolve it from the configured
  adapter/tool runtime.
- Implement `tandem mcp-control` as a hidden/internal Go subcommand speaking MCP over stdio.
- Have that subcommand call the daemon's authenticated takeover endpoint and block until
  user release, matching the existing control MCP.
- Register both MCP servers during ACP `session/new` only when browser MCP is enabled.
- Document that the ACP agent owns these MCP subprocesses even though Tandem supplies their
  declarations.

**Acceptance:**

- Existing MCP wiring assertions pass with equivalent command, args, and environment.
- The complete `derisk:browser` suite passes against Go.
- The agent can use Playwright MCP, the user can grab control, and a takeover request
  resolves only after release.

**Out of scope:** Rewriting `@playwright/mcp` or managing its installation.

## Phase 21 - Parameterize and run the full parity suite

**Goal:** Make implementation parity an automated gate rather than a manual claim.

**Work:**

- Refactor the de-risk launcher so every black-box suite accepts a daemon command and does
  not assume `tsx`, Node source paths, or `process.execPath`.
- Run the same suites against Node and Go in CI using separate throwaway homes and ports.
- Add cross-runtime restart cases: Node writes then Go restores, and Go writes then Node
  restores.
- Compare normalized observable output rather than timestamps, PIDs, or temporary paths.
- Keep the opt-in live Steel suite separate.

**Acceptance:**

- All standard `derisk:*` suites pass against both implementations in CI.
- Cross-runtime restart and rollback cases pass.
- The job fails on leaked processes, worktrees, ports, or temporary homes.

**Out of scope:** Changing the production service.

## Phase 22 - Add release builds and the GitHub workflow

**Goal:** Publish installable native Tandem executables without changing production yet.

**Work:**

- Add a reproducible release command that builds the UI, stages embedded assets, runs Go
  tests, and compiles `tandem` with version metadata.
- Add a GitHub Actions matrix for the explicitly supported OS/architecture combinations.
- Start with Linux amd64 and arm64; add macOS amd64/arm64 when PTY and Chromium discovery
  tests pass there. Do not advertise Windows until Phase 7's PTY decision supports it.
- Generate checksums and a release manifest describing required external tools such as Git,
  agent CLIs, Node-based ACP adapters, Playwright MCP, Chromium, or Steel.
- Smoke-test each artifact on its target runner by starting with a throwaway home, checking
  `TANDEM_READY`, loading the embedded UI, and shutting down cleanly.

**Acceptance:**

- CI artifacts run without a Go toolchain or source checkout.
- Embedded UI and catalog are present.
- Checksums are stable for identical toolchain inputs where the platform permits.
- Artifact smoke tests pass before upload.

**Out of scope:** Automatic publication on every commit and production cutover.

**Implementation note:** The initial workflow publishes Linux amd64 and arm64 only. macOS
remains unadvertised until native runners exercise real PTY and Chromium integration tests
on both architectures, as required above.

## Phase 23 - Cut the development and production entrypoints over to Go

**Goal:** Make the Go daemon the default while retaining an immediate rollback path.

**Work:**

- Update local start scripts, `redeploy.sh`, and the systemd unit to build/run the Go
  daemon and embedded UI.
- Preserve environment variables and shutdown/restart semantics.
- Keep a documented command or service override for starting the Node daemon against the
  same home after the Go process has stopped.
- Update architecture, deployment, browser, terminal, and contributor documentation.
- Run a real upgrade from a copy of an existing Tandem home, then perform one rollback to
  Node and one forward restart to Go.

**Acceptance:**

- Full suites pass before and after the entrypoint change.
- `./redeploy.sh` rebuilds and restarts the Go service successfully.
- Existing agents restore, the UI reconnects, and browser control works after deployment.
- The rehearsed rollback does not lose events, sessions, assets, or worktree associations.

**Out of scope:** Deleting the Node implementation.

## Phase 24 - Retire the Node daemon after a stabilization window

**Goal:** Remove dual-maintenance cost only after the Go daemon has proven stable.

**Entry condition:** The Go daemon has been the production default through an agreed
stabilization window with no unresolved data-compatibility or lifecycle regressions.

**Work:**

- Remove Node daemon source and Node-only daemon dependencies.
- Retain or relocate shared black-box fixtures needed to test the Go daemon.
- Remove cross-runtime CI jobs only after preserving at least one historical Node database
  compatibility fixture.
- Update developer commands, systemd deployment, documentation, and repository layout.
- Decide separately whether Node-based ACP adapters and Playwright MCP become managed
  installs; do not silently bundle that separate project into cleanup.

**Acceptance:**

- UI build, Go tests, full de-risk suite, release matrix, install smoke test, and redeploy
  all pass from a clean checkout.
- A historical Node-created database still passes the Go compatibility test.
- Repository search finds no stale Node-daemon launch path or documentation.

## Phase dependency summary

```text
0 baseline
`-- 1 Go foundation
    |-- 2 config
    |-- 3 SQLite -- 4 event log
    |-- 5 workspaces
    |-- 6 filesystem/assets
    `-- 7 process/PTY -- 8 TerminalHost

9 ACP transport
`-- 10 ACP lifecycle
    `-- 11 ACP services (uses 6 and 8)

12 PTY adapter (uses 7)
13 registry (uses 2-5 and 10-12)
|-- 14 HTTP/UI (uses 2, 3, and 6)
`-- 15 WS replay (uses 4 and 13)
    `-- 16 WS commands
        `-- 17 resume/handoff

18 CDP broker
`-- 19 daemon CDP client
    `-- 20 MCP wiring

21 full parity
`-- 22 release workflow
    `-- 23 production cutover
        `-- 24 Node retirement
```

Some early package work can proceed in parallel across separate worktrees, but phases that
change shared contracts should merge in the order shown. The default execution model is
still one phase per agent session and one verified commit per phase.

## Decisions that must remain explicit

The following are deliberate decision points, not details for an implementation agent to
guess:

1. **Go ACP implementation:** community SDK versus pinned schema-generated internal client.
   Resolve in Phase 9 with an ADR.
2. **SQLite driver:** it must support the existing database and release targets without
   silently introducing CGo requirements.
3. **WebSocket library:** select one with maintained server support, explicit limits, and
   cancellation; hide it behind Tandem's transport package.
4. **PTY platform matrix:** do not claim Windows support until ConPTY behavior is tested.
5. **Chromium source:** system executable, managed browser download, or Steel-only
   deployment must be configurable and documented.
6. **External Node tools:** a native Tandem binary does not make Node-based ACP adapters or
   Playwright MCP native. Managed installation is a follow-on product capability.
7. **Schema evolution:** additive compatibility is mandatory until Phase 24.

## Definition of migration complete

The migration is complete when:

- the React UI runs unchanged against the Go daemon;
- the full black-box suite passes against the released executable;
- reconnect/replay, daemon restart, session resume, terminal handoff, approvals, images,
  worktrees, multi-agent operation, and browser takeover match existing behavior;
- a Node-created home upgrades to Go and can roll back without data loss;
- release artifacts contain the UI and require no Go toolchain;
- production runs the Go daemon through the normal redeploy path;
- the remaining external Node requirements are accurately reported as agent/MCP runtime
  dependencies rather than Tandem daemon dependencies.
