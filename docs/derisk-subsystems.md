# De-risk suite ownership

This map assigns each executable black-box suite to the subsystem whose compatibility it
primarily protects during the Go migration. Cross-cutting coverage is noted explicitly.

| Script | Owning subsystem | Compatibility exercised |
| --- | --- | --- |
| `derisk` | event log and ACP adapter | dropped-client replay, monotonic sequence stream, permissions |
| `derisk:multi` | registry | independent agents, queues, streams, teardown |
| `derisk:restart` | persistence and restore | SQLite cold replay, ACP session load, sequence continuation |
| `derisk:deferred-restart` | process lifecycle | active-turn drain and shutdown request idempotence |
| `derisk:auth` | HTTP/WebSocket server | bearer-token rejection and acceptance |
| `derisk:workspace` | workspace manager | refs, feature lineage, worktrees, discovery, collision and restore |
| `derisk:services` | ACP client services | contained filesystem, daemon terminals, buffering and cancellation |
| `derisk:browser` | browser broker | local CDP, screencast, control pause/queue, input and takeover MCP |
| `derisk:integration` | cross-subsystem integration | discover/spawn/prompt/approval/reconnect/dirty teardown slice |
| `derisk:resume` | session catalog and registry | live, closed, and external resume paths |
| `derisk:handoff` | adapter lifecycle | ACP-to-terminal transfer and automatic ACP reload |
| `derisk:images` | asset store and prompt boundary | upload authorization, durable blocks, ACP image bytes/order |
| `derisk:config` | configuration and launch resolution | catalog/profile overlay and durable direct-terminal launch |
| `derisk:steel` | Steel browser driver | remote session provisioning, shared CDP and release; opt-in |
| `derisk:parity` | implementation boundary | normalized Node/Go HTTP+WS behavior, bidirectional restore, and resource leak checks |

`derisk:all` owns no subsystem; it is the required sequential composition of every
non-opt-in suite above. `derisk:steel` stays opt-in because it requires an external service.

The process-level launchers take `TANDEM_DAEMON_CMD` as a JSON command array. The CI parity
gate builds the native daemon, keeps the deeper Node white-box suite, then drives Node and Go
as opaque child processes with separate homes and ports. `derisk:parity` compares only stable
observable facts (event kinds and protocol results), not IDs, PIDs, timestamps, ports, or
temporary paths. It also starts each runtime on the other's database to cover both forward
migration and rollback, and fails if child process groups, ports, worktrees, homes, or project
roots remain after teardown. The live Steel suite remains separate and opt-in.
