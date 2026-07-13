# AgentAdapter interface

One normalized event vocabulary that every agent is reached through, with two
implementations behind it: `AcpAdapter` (structured, primary) and `PtyAdapter` (raw pty,
fallback + user shell). The event vocabulary is ACP-shaped, since ACP is the reference.

The daemon translates ACP (or pty bytes) into these normalized types, so **the browser
never speaks ACP** — see [`ws-protocol.md`](ws-protocol.md).

## Normalized event stream

```ts
type AgentEvent =
  | { kind: 'message_chunk';  text: string }                         // assistant prose
  | { kind: 'thought_chunk';  text: string }                         // reasoning, if exposed
  | { kind: 'tool_call';      id: string; title: string;
                              status: 'pending'|'running'|'done'|'error'; content: ToolContent[] }
  | { kind: 'tool_call_update'; id: string; status?: string; content?: ToolContent[] }
  | { kind: 'plan';           entries: PlanEntry[] }
  | { kind: 'terminal_output';   termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; options: PermOption[] }
  | { kind: 'status';         status: 'idle'|'working'|'blocked'|'error' }
  | { kind: 'error';          message: string }
  | { kind: 'raw_pty';        data: Uint8Array };                    // pty adapter / user shell only
```

## The interface

```ts
interface AgentAdapter {
  readonly id: string;
  readonly capabilities: { structured: boolean; terminals: boolean; loadSession: boolean; fs: boolean };

  spawn(opts: SpawnOpts, services: ClientServices): Promise<void>;
  prompt(input: PromptInput): Promise<void>;              // one user turn
  sendInput(bytes: Uint8Array): void;                     // raw path: user shell / TUI-only agent
  respondPermission(reqId: string, optionId: string): void;
  interrupt(): void;                                      // cancel current turn
  loadSession?(sessionId: string): Promise<void>;         // only if capabilities.loadSession
  dispose(): Promise<void>;

  events: AsyncIterable<AgentEvent>;                       // the normalized stream
}
```

## ClientServices

ACP makes the **client** own terminals and filesystem access, so an adapter is not a pure
outbound pipe — it needs handles back into the daemon. These are the daemon-owned services
an adapter drives on the agent's behalf:

```ts
interface ClientServices {
  fs:          { readTextFile(path: string): Promise<string>;
                 writeTextFile(path: string, text: string): Promise<void> };
  terminals:   TerminalHost;       // create/output/waitForExit/kill/release — OWNS the buffers
  permissions: PermissionRouter;   // pushes permission_request → global approvals queue
}
```

`TerminalHost` is where terminal output lives and is buffered (beyond ACP's
`outputByteLimit` if desired), which is what makes terminal re-capture on reconnect native.

## Implementations

### AcpAdapter (primary)

Speaks Agent Client Protocol (JSON-RPC 2.0) over the agent subprocess's stdio. The daemon
is the ACP client.

- **Handshake:** fork the agent (`claude-code-acp`, `gemini`, …), run `initialize`, then
  `session/new` (or `session/load` when resuming).
- **`session/update`** → map 1:1 to `AgentEvent` (`message_chunk`, `thought_chunk`,
  `tool_call`, `tool_call_update`, `plan`).
- **`session/request_permission`** → emit `permission_request`; resolve via
  `respondPermission()` → send the ACP response.
- **`terminal/*`** → serviced by `ClientServices.terminals`; the daemon owns and buffers
  output. Output persists past `terminal/release`.
- **`fs/read_text_file` / `fs/write_text_file`** → serviced by `ClientServices.fs`; the
  daemon's sandbox choke point.
- **`session/load`** → `loadSession()` when the agent advertises the `loadSession`
  capability.

### PtyAdapter (fallback + user shell)

A raw pty child via `node-pty`. Used for TUI-only agents and the user's escape-hatch shell.

- Events are only `{ kind: 'raw_pty', data }`.
- `status` is `'working'` heuristically or unknown; no structured `tool_call`s, no
  structured approvals.
- `sendInput()` writes bytes to the pty master; the browser renders via xterm.js.

## Consequence for the design

Adopting ACP demotes the raw-pty path: `AcpAdapter` becomes the main channel for Claude
Code / Gemini CLI / anything ACP-speaking, while `PtyAdapter` covers (a) the user's shell
and (b) legacy TUI-only agents. Terminal buffers live in `TerminalHost` inside the daemon —
the correct side of the durability line.
