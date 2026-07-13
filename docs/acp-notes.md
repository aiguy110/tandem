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

We currently advertise **`fs:false, terminal:false`** so the agent does its own I/O; flip
these on once the daemon has a real workspace-fs + `TerminalHost` to service `fs/*` and
`terminal/*` requests.

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
