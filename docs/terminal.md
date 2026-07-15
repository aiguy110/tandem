# Terminal pane

Renders raw pty streams (the `PtyAdapter` / the user's escape-hatch shell) and ACP
client-owned terminal output. Built against the **xterm.js API** behind a thin
`TerminalRenderer` interface so the emulator engine is swappable.

## Engine: ghostty-web (default), xterm.js (fallback)

- **Default — [`ghostty-web`](https://github.com/coder/ghostty-web):** Ghostty's
  `libghostty-vt` engine compiled to WASM, **xterm.js-API-compatible**, originally built for
  Mux (isolated parallel agentic development) — our exact category. Best-in-class VT fidelity.
- **Fallback — [`@xterm/xterm`](https://www.npmjs.com/package/@xterm/xterm):** the proven
  baseline (powers VS Code). Selected by config/build flag.
- Because ghostty-web mirrors the xterm API, switching engines is ≈a one-line import change.

## `TerminalRenderer` interface

The rest of the UI depends only on this, never on the concrete engine:

```ts
interface TerminalRenderer {
  write(bytes: Uint8Array): void;            // feed pty / terminal output
  onData(cb: (data: string) => void): void;  // user keystrokes
  resize(cols: number, rows: number): void;
  readonly cols: number;
  readonly rows: number;
  focus(): void;
  dispose(): void;
}
```

## Data flow

- **Output:** daemon `pty` channel (base64 `raw_pty` frames) → `write()`.
- **Input:** `onData` → `{ t: 'input', bytesB64 }`.
- **Resize:** fit-to-container → `{ t: 'resize', cols, rows }` → `PtyAdapter.resize()`
  (node-pty).
- **Reconnect:** replay the daemon's buffered `raw_pty` bytes into `write()` — no serialize
  addon needed; reuses the durability spine.

## ACP ↔ CLI handoff

Opening Terminal is initially view-only: a shroud requires the user to confirm **Take
control** before an idle ACP agent swaps to its resumable CLI. The handoff preserves the
Tandem agent id, workspace, ACP session id, and monotonic event log. For a mid-turn agent
the Terminal pane instead requires the stronger **Interrupt & take over** confirmation:
the daemon sends `session/cancel`, waits up to four seconds for the prompt to settle, disposes
ACP, and launches the CLI with its session id. Transcript is shrouded while the CLI owns the
session. Normal CLI exit—or **return to Transcript now**—disposes the PTY and reloads the same
session through ACP automatically.

CLI templates come from each agent's `terminal.resumeArgs` entry in `config.yml.example`
or the `$TANDEM_HOME/config.yml` overlay. They can also be overridden with the legacy
`TANDEM_RESUME_CMD_<AGENT>` environment variable. Tandem prefers `node-pty`; when its native
module is unavailable, util-linux `script` supplies the required pseudoterminal.

## Multi-agent rendering constraint

Browsers cap WebGL contexts (~16 per page). Only the **focused** terminal renders live;
background terminals pause and rehydrate from the daemon buffer on focus. Applies to both
engines.

## Two content kinds, one renderer

1. **Raw pty** (`PtyAdapter` / user shell) — full VT emulation.
2. **ACP `terminal_output`** (client-owned tool terminals) — same renderer, one instance
   per terminal.

## Spike

[`spike/terminal/`](../spike/terminal/) — a Vite app that renders a live shell via
ghostty-web, driven by the daemon's real `pty` channel (`npm run daemon -- --pty`).

**Findings (verified, ghostty-web 0.4.0):** passed end-to-end against the live pty:

- `init()` loads `ghostty-vt.wasm` cleanly under Vite (served from `public/`).
- The exported `FitAddon` sizes the terminal to its container (measured 93×31); the new
  `resize` message reaches `PtyAdapter.resize()`.
- The daemon `pty` channel (base64 `raw_pty`) feeds `term.write()`; SGR colors, bold, and
  `ls --color` render correctly.
- Input round-trips (WS `input` → pty → echoed output → render).
- Subscribing `sinceSeq: 0` replays the pty scrollback snapshot into `write()` — the
  reconnect/replay path renders as expected.
- API is xterm-compatible (`Terminal`, `onData`, `write`, `loadAddon`, `cols`/`rows`), so the
  `@xterm/xterm` fallback is a drop-in. Rendering is canvas-based (`CanvasRenderer`), which
  softens the WebGL-context concern.

**Conclusion:** ghostty-web confirmed as the default engine.
