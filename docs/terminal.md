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
