# Chat CLI and Terminal shell

The **Chat** tab owns the agent interface: structured ACP transcript by default, or the
agent's resumable CLI selected with the ACP/CLI switch in the selected Chat tab. The separate **Terminal**
tab is the user's default shell in the agent worktree. Both PTYs render through the same
`TerminalRenderer` abstraction, but their streams and lifecycles are independent.

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

- **Agent CLI output:** base64 `raw_pty` events → `ptyHub` → Chat CLI renderer.
- **Agent CLI input/resize:** `input` / `resize` → the active `PtyAdapter`.
- **User shell output:** base64 `shell_pty` events → `shellHub` → Terminal renderer;
  `shell_exit` drives the explicit restart overlay.
- **User shell input/resize:** `shell_input` / `shell_resize`; `shell_open` lazily starts
  `$SHELL` in the worktree and is idempotent while it is running.
- **Reconnect:** both streams replay from the daemon event log into their distinct hubs.

## ACP ↔ CLI handoff

The ACP/CLI switch lives in Chat. An idle ACP agent can switch directly to its resumable
CLI; a mid-turn agent requires the stronger **Interrupt & take over** confirmation:
the daemon sends `session/cancel`, waits up to four seconds for the prompt to settle, disposes
ACP, and launches the CLI with its session id. Switching back requires confirmation before
killing the live CLI. Normal CLI exit disposes the PTY and reloads the same session through
ACP automatically. The Terminal shell continues independently throughout either handoff.

CLI templates come from each agent's `terminal.resumeArgs` entry in `config.yml.example`
or the `$TANDEM_HOME/config.yml` overlay. They can also be overridden with the legacy
`TANDEM_RESUME_CMD_<AGENT>` environment variable. The production Go daemon uses a real Unix
pseudoterminal directly on Linux and macOS; PTY-backed modes are unsupported on Windows.

## Multi-agent rendering constraint

Browsers cap WebGL contexts (~16 per page). Only the **focused** terminal renders live;
background terminals pause and rehydrate from the daemon buffer on focus. Applies to both
engines.

## Independent PTYs

1. **Agent PTY** — a native `PtyAdapter` agent or an ACP agent's resumable CLI; durable
   event kind `raw_pty`.
2. **User shell PTY** — lazily spawned per session in the worktree; durable event kinds
   `shell_pty` and `shell_exit`.
3. **ACP `terminal_output`** — client-owned tool terminals rendered inside the transcript.

ghostty-web 0.4 may recycle a freed WASM terminal handle with old screen cells intact. A
new renderer clears its viewport and scrollback before its selected hub replays, preventing
one PTY's old cells from appearing in another PTY's view.

Its canvas renderer can also miss the final cursor-only update in a shell's normal
`BS, space, BS` erase echo after scrollback replay. The renderer adapter forces a canvas
redraw for output chunks containing BS, keeping the painted cursor and cells aligned with
Ghostty's already-correct VT buffer.

## Font

Terminals render in **Cascadia Mono NF** — Microsoft's own Nerd Fonts build of Cascadia,
self-hosted as `ui/src/assets/fonts/CascadiaMonoNF.woff2` (SIL OFL) so powerline and Nerd
Font glyphs work offline and inside the single-binary embedded UI. The *Mono* cut is
deliberate: both engines paint one codepoint per cell, so Cascadia Code's ligatures could
never form, and Mono's patched glyphs are single-width and stay inside their cell (the
canvas renderer does not clip `fillText`).

Size comes from the Appearance modal (`ui/src/appearance.ts`), which keeps the app-wide and
terminal font sizes either locked to one ratio or independent; `PtyTerminal` applies a change
to the live renderer and refits so the new grid reaches the pty.

`ui/src/terminal/font.ts` owns the stack and its overrides (`?termFont=` or
`localStorage['tandem.termFont']`, mirroring the engine switch). `createRenderer` awaits
`ensureTermFont()` **before** constructing a terminal: ghostty-web measures cell width and
baseline once from `measureText('M')` in its constructor, and xterm.js does the same in
`open()`, so a terminal built against an unloaded webfont locks in the *fallback's* metrics
and every cell stays misaligned. The `@font-face` uses `font-display: block` for the same
reason. Italics are synthesized; no italic face is bundled.

### Cell metrics

`createRenderer` replaces `CanvasRenderer.prototype.measureFont`. ghostty-web 0.4 sizes a
cell as `Math.ceil(measureText('M').width)` in **CSS** pixels and derives its height from the
cap height of `M` plus 2px, ignoring both the device pixel ratio and the font's line box. At
12px Cascadia the advance is 7.03px, so ceiling to 8 stretches every column by ~14% — visibly
wider than native Ghostty. The replacement rounds the advance to the nearest *device* pixel
(cells still land on whole device pixels, so backgrounds do not seam) and takes height and
baseline from `fontBoundingBoxAscent/Descent`.

User shells are launched with `TERM=xterm-256color` when the daemon environment does not
provide a terminal identity (as is typical under systemd). Without it, interactive shells
such as zsh cannot obtain cursor-left/erase capabilities from terminfo: their line editor
deletes the character internally but may paint only a trailing space.

## Spike

[`spike/terminal/`](../spike/terminal/) — a Vite app that renders a live shell via
ghostty-web, driven by the daemon's real `pty` channel. The current native regression lives
in `internal/ptyadapter` and exercises binary output, input, resize, exit, and cleanup.

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
