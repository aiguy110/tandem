# ACP notes — pinned facts

Reference for wiring the [`AcpAdapter`](../daemon/src/acpAdapter.ts). Pinned against:

- **`@agentclientprotocol/sdk` 1.2.1** (protocol version **`1`**) — the schema is at
  `node_modules/@agentclientprotocol/sdk/schema/schema.json`.
- **`@agentclientprotocol/claude-agent-acp` 0.59.0** — the real Claude agent used for the
  live test (`npm run acp:live`). Bin: `claude-agent-acp` → `dist/index.js`.

> Note: this is **not** `@zed-industries/claude-code-acp` (a different, older adapter on
> SDK 0.14). The `@agentclientprotocol/*` packages are the ones to target.

## Wire transport

- **JSON-RPC 2.0** over the agent subprocess's **stdio**.
- **Framing: newline-delimited JSON** — each message is `JSON.stringify(msg) + "\n"`, and
  the reader splits on `\n`. (Confirmed in the SDK's `stream.js`. There is a separate HTTP
  adapter that uses `Content-Length`, but stdio agents use ndjson.)

## Handshake

1. Client → `initialize` `{ protocolVersion: 1, clientCapabilities: { fs: { readTextFile,
   writeTextFile }, terminal } }`. Agent → `{ protocolVersion, agentCapabilities:
   { loadSession, promptCapabilities, mcpCapabilities, … }, authMethods }`.
2. Client → `session/new` `{ cwd, mcpServers: [], additionalDirectories? }` → `{ sessionId }`.
   (`session/load` resumes, gated by `agentCapabilities.loadSession`.)
3. Client → `session/prompt` `{ sessionId, prompt: ContentBlock[] }` → `{ stopReason }`.

As of **Phase 3** we advertise **`fs: { readTextFile: true, writeTextFile: true }, terminal:
true`** and service `fs/*` + `terminal/*` ourselves (WorkspaceFs + TerminalHost). `terminal`
is a **plain boolean** in `ClientCapabilities` (not an object) — "the Client supports all
`terminal/*` methods". See the fs/terminal sections below for the pinned request/response
shapes.

> **Real-agent note (pinned):** `@agentclientprotocol/claude-agent-acp` 0.59.0 issues **no**
> `fs/*` or `terminal/*` client requests at all — its dist contains no `terminal/create` or
> `fs/read_text_file` call sites; it does its own file I/O and runs shell commands with an
> internal tool. It *accepts* our advertised fs/terminal capabilities (initialize succeeds,
> turns complete, output is correct) but never delegates to the client. So the fs/terminal
> servicing path is exercised end-to-end by the **mock agent** (`derisk:services`), while
> `acp:live` only proves the capability handshake holds against the real agent.

## Client methods we service (fs + terminals) — Phase 3

The daemon is a real ACP **client**: the agent sends these as JSON-RPC **requests** and we
answer. All carry `sessionId`. Pinned against the SDK schema
(`x-side: client`); shapes below are exactly what `AcpAdapter.serviceRequest` speaks.

### fs

| method | request | response |
|---|---|---|
| `fs/read_text_file` | `{ sessionId, path, line?, limit? }` — `path` **absolute**; `line` 1-based start, `limit` max lines (both optional, nullable) | `{ content: string }` |
| `fs/write_text_file` | `{ sessionId, path, content }` — `path` **absolute** | `{}` (empty; `_meta?` only) |

Both are the **sandbox choke point**: `WorkspaceFs` resolves the path against the agent's
workspace cwd and rejects any escape (`../` or symlink) with JSON-RPC **`-32602`**
(invalid params) before touching disk.

### terminals

| method | request | response |
|---|---|---|
| `terminal/create` | `{ sessionId, command, args?, env?: {name,value}[], cwd?, outputByteLimit? }` | `{ terminalId }` |
| `terminal/output` | `{ sessionId, terminalId }` | `{ output, truncated, exitStatus: {exitCode,signal}\|null }` |
| `terminal/wait_for_exit` | `{ sessionId, terminalId }` | `{ exitCode: number\|null, signal: string\|null }` |
| `terminal/kill` | `{ sessionId, terminalId }` | `{}` |
| `terminal/release` | `{ sessionId, terminalId }` | `{}` |

**Shape gotchas (pinned, differ subtly):**

- `terminal/output` nests exit under **`exitStatus`** (a `TerminalExitStatus` object, `null`
  while running); `terminal/wait_for_exit` returns **flat** `exitCode`/`signal`. Don't
  conflate them.
- `outputByteLimit` truncates **from the beginning** at a **UTF-8 char boundary**, and the
  agent cannot re-fetch what was dropped (`truncated: true`). The daemon keeps a **separate,
  larger scrollback** independent of this limit (docs/architecture.md caveat), so our
  reconnect/UI view is richer than the agent's.
- The kill schema type is **`KillTerminalRequest`/`KillTerminalResponse`** (method
  `terminal/kill`) — not `KillTerminalCommand*`.
- `terminal/release` frees the **process** but the daemon **keeps the buffer**, so output
  stays retrievable after release (native reconnect re-capture).

## session/update notifications (the transcript stream)

`{ sessionId, update: <SessionUpdate> }`. **Each variant FLATTENS its payload next to the
`sessionUpdate` discriminator** (the schema uses `allOf: [$ref: <Type>]`), so fields sit
directly on the `update` object — they are *not* nested under a `toolCall`/`plan` key.

| `sessionUpdate` | flattened type | key fields |
|---|---|---|
| `agent_message_chunk` | ContentChunk | `content` (ContentBlock), `messageId?` |
| `agent_thought_chunk` | ContentChunk | `content` |
| `user_message_chunk` | ContentChunk | `content` |
| `tool_call` | ToolCall | `toolCallId`, `title`, `kind?`, `status?`, `content?`, `locations?` |
| `tool_call_update` | ToolCallUpdate | `toolCallId` (only required), any of the above |
| `plan` | Plan | `entries: PlanEntry[]` |
| `plan_update` / `plan_removed` | (1.2.x) | incremental plan changes |
| `usage_update` | UsageUpdate | `used`, `size`, `cost?` |
| `current_mode_update`, `available_commands_update`, `config_option_update`, `session_info_update` | — | ignored for now |

- **ContentBlock (text):** `{ type: "text", text }` (other types: image, audio,
  resource_link, resource).
- **ToolCallStatus:** `pending | in_progress | completed | failed`.
- **PlanEntry:** `{ content, priority, status }` — `priority ∈ high|medium|low`,
  `status ∈ pending|in_progress|completed` (all three required).

### Normalized mapping (in `AcpAdapter`)

| ACP | Tandem `AgentEvent` |
|---|---|
| ToolCallStatus `in_progress / completed / failed / pending` | `running / done / error / pending` |
| PlanEntryStatus `completed` | plan entry `done` (others pass through) |

## Permissions

- Agent → **request** `session/request_permission` `{ sessionId, toolCall: ToolCallUpdate,
  options: PermissionOption[] }`. `PermissionOption = { optionId, name, kind }`,
  `kind ∈ allow_once | allow_always | reject_once | reject_always`.
- Client → **response** `{ outcome: { outcome: "selected", optionId } }` or
  `{ outcome: { outcome: "cancelled" } }`.

This maps 1:1 to Tandem's `permission_request` event → approvals queue → `respondPermission`.

## Cancellation

`session/cancel` is a **notification** `{ sessionId }` (no id). The agent halts and answers
the outstanding `session/prompt` with `stopReason: "cancelled"`; the client must resolve any
pending permission requests as `cancelled` and mark unfinished tool calls cancelled.

## stopReason values

`end_turn | max_tokens | max_turn_requests | refusal | cancelled`.
