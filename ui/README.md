# Tandem UI — mission control

The React web UI over the Tandem daemon. It is a **projection**: the browser holds no
authoritative state; everything is derived from daemon messages over one WebSocket
(`docs/ws-protocol.md`). Layout is docked rails + focus (D6): a conductor bar, a left agents
rail, a center focus area with pane tabs, a right approvals rail, and a collapsible bottom
inspector.

Stack: React 18 + Vite + TypeScript (strict), a small [zustand](https://github.com/pmndrs/zustand)
store, `marked` for markdown, and the terminal engines `ghostty-web` (default) + `@xterm/xterm`
(fallback). No Tailwind — hand-rolled CSS with custom properties, matching `design/index.html`.

## Develop

```bash
cd ui
npm install
npm run dev            # Vite dev server (default :5178)
```

The dev server does **not** proxy the daemon. Point the UI at a running daemon and pass the
token in the URL fragment:

```bash
# in another terminal — start the native daemon (prints a bootstrap URL + token)
cd .. && ./start-dev-server.sh

# then open the dev server with the daemon's WS origin + token, e.g.
#   http://localhost:5178/?#t=<token>
# and set the WS origin (same-origin is assumed by default):
#   window.__TANDEM_WS__ = 'ws://127.0.0.1:7717'   (in devtools), or
#   VITE_TANDEM_WS=ws://127.0.0.1:7717 npm run dev
```

In production (served by the daemon) the WS is **same-origin**, so no override is needed —
just open the daemon's bootstrap URL.

### Auth (D15)

On first load the UI reads the `#t=<token>` fragment, stores it in `localStorage`, strips it
from the visible URL, and presents it as `?token=` on the WS connect. A rejected token
(close code `4401`) or a missing token shows a paste-token screen. Reconnects reuse the
stored token with exponential backoff; per-agent `lastSeq` drives gapless `sinceSeq` replay.

### Terminal engine

`ghostty-web` is the default (its `ghostty-vt.wasm` is copied into `public/` by the
`copy-wasm` prebuild/predev script and served as a static asset). If the WASM fails to load,
the renderer transparently falls back to `@xterm/xterm`. Force an engine with
`?term=xterm` / `?term=ghostty`, `localStorage['tandem.termEngine']`, or `VITE_TERM_ENGINE`.

### Keymap (D10)

Scope-aware single-key + chord bindings, rebindable via `localStorage['tandem.keybindings']`
(a `{ commandId: binding }` JSON map; delete the key to reset to defaults). Bare keys never
fire while typing (text-input scope). Defaults: `c` quick-spawn, `C` sibling, `⌘/Ctrl+K`
command palette, `g a` go-to-agent, `j`/`k` next/prev, `1`–`4` panes, `a`/`d` approve/deny
top approval, `w` take/release the shared-browser wheel, `⌫` close, `Esc` interrupt (while
working). Every palette row shows its binding.

### Browser pane (Phase 5)

The Browser pane renders the focused agent's shared-browser CDP screencast into a `<canvas>`
(scale-to-fit, letterboxed) and forwards mouse/key/wheel back over `browser_input` while the
user holds the wheel. Frames arrive on the `browser` channel and are fanned out via
`terminal/browserHub.ts` (kept out of the reactive store, like `ptyHub`). The tab enables
only once `browser_state` reports the browser `active` (it spins up lazily on the agent's
first browser use). A control-owner badge + grab/release button (also `w`) flip the token;
agent-initiated `takeover_request` events show a Browser-pane banner and an attention card in
the approvals rail. Only the focused, browser-viewing client subscribes the channel, so only
that agent streams (bandwidth rule).

## Build & embed in the daemon

```bash
cd ui
npm run build          # tsc --noEmit + vite build → ui/dist (includes ghostty-vt.wasm)
```

Stage the build, compile the native daemon, and open its bootstrap URL:

```bash
cd ..
./scripts/stage-go-ui.sh
go build -o tandem ./cmd/tandem
./tandem
# → open the printed http://127.0.0.1:7717/#t=<token>
```

The Go daemon embeds `internal/ui/dist` and serves it on the same port as the WS
(`internal/httpserver`), with correct MIME types including `application/wasm` for the
terminal engine. Set `TANDEM_UI_DIR=ui/dist` to override the embedded files while developing.

## Layout of the source

```
src/
  wire.ts                 wire types mirrored by the Go daemon protocol types
  store.ts                zustand store: the projection of daemon messages + actions
  ws/client.ts            durable WS client (token auth, backoff reconnect, sinceSeq resub)
  useGlobalKeys.ts        scope-aware global key handler
  markdown.ts             marked wrapper for message chunks
  fuzzy.ts                tiny fuzzy matcher for the palettes
  history.ts              History palette ranking + repo grouping (name > repo > transcript)
  commands/
    keymap.ts             bindings, scopes, chord matcher, localStorage persistence
    registry.ts           command registry (stable IDs) shared by keymap + palette
  terminal/
    TerminalRenderer.ts   engine-agnostic interface + ghostty/xterm adapters
    ptyHub.ts             non-reactive raw_pty buffer + fan-out (rehydrate on focus)
  components/
    ConductorBar, AgentsRail, ApprovalsRail, Inspector, FocusArea,
    ConnectionBanner, TokenScreen, SpawnPalette, CommandPalette, HistoryPalette
    panes/ TranscriptPane, TerminalPane, DiffPane, BrowserPane
```

`DiffPane` and `BrowserPane` are Phase-5 placeholders — the daemon's `merge_back` /
`browser_control` still return error acks. See the header comment in `BrowserPane.tsx` for
the exact plug-in points for the browser screencast, `browser_frame` handling, and the
control-owner wheel.
