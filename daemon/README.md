# tandem-daemon — spine PoC

A minimal, runnable proof of the two riskiest theses in the [v1 build slice](../docs/architecture.md#v1-build-slice):

1. **Durability** — the daemon owns the agent process; a browser socket can die
   (simulated dropped SSH pipe) and reconnect with **gapless seq-replay**, while the
   agent keeps working the entire time. This is the concrete answer to the original
   question: *can a session survive its stdio pipes dying?* — yes, when the daemon, not
   the terminal, owns the process.
2. **ACP path** — a real JSON-RPC-over-stdio [`AcpAdapter`](src/acpAdapter.ts) translates
   `session/update` → normalized events and round-trips `session/request_permission`
   (the approvals loop), exercised against a [mock ACP agent](src/mock-acp-agent.mjs).

Nothing here needs Claude Code installed — the mock agent stands in for any ACP agent.

## Run it

```bash
cd daemon
npm install
npm run derisk        # the automated proof (exits non-zero on any failure)
npm run pty-smoke     # optional: proves the pty adapter (needs node-pty)
npm run daemon        # standalone daemon on ws://localhost:7717 for manual poking
```

`npm run derisk` output (all six checks must pass):

```
✅  agent subprocess survived the disconnect        pid N → N
✅  reconnect replayed from exactly lastSeq+1        first replayed = 7, expected 7
✅  no seq gaps across the disconnect                24 events, seq 1..24
✅  no heartbeat ticks lost during the gap           ticks 1..14 contiguous
✅  ACP permission delivered after reconnect         reqId=perm_1
✅  approval → agent completed the turn              received "Done" message
```

## How it maps to the specs

| Spec | Code |
|---|---|
| `AgentAdapter` interface | [`src/types.ts`](src/types.ts) |
| ACP adapter (client owns fs/terminals) | [`src/acpAdapter.ts`](src/acpAdapter.ts) |
| pty adapter (daemon owns the pty master) | [`src/ptyAdapter.ts`](src/ptyAdapter.ts) |
| daemon owns all state (seq'd log + ring buffer) | [`src/eventLog.ts`](src/eventLog.ts), [`src/session.ts`](src/session.ts) |
| WS subscribe / snapshot / event / replay | [`src/server.ts`](src/server.ts) |

## What this deliberately does NOT do yet

- No auth on the WS, no TLS — bind to localhost only (see the security note in the specs).
- ACP framing here is newline-delimited JSON; **the exact framing and the
  `session/update` content-block shapes must be confirmed against the ACP spec** before
  wiring `claude-code-acp`. (This is the "pin the ACP version" next step.)
- `fs/*` and `terminal/*` requests are acknowledged minimally, not serviced by a real
  workspace manager / `TerminalHost`.
- Single agent; no git-worktree workspace isolation; no shared browser.

These are the next slices, not gaps in the thesis being proved here.
