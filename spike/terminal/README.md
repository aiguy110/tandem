# Terminal spike — ghostty-web

Validates [`ghostty-web`](https://github.com/coder/ghostty-web) (the default terminal
engine, see [`../../docs/terminal.md`](../../docs/terminal.md)) against the daemon's real
`pty` channel. Findings are recorded in `docs/terminal.md` — this passed.

## Run

```bash
# 1) start the daemon in pty mode (spawns a bash shell as agent web-1)
cd ../../daemon && npm run daemon -- --pty        # ws://localhost:7717

# 2) start the spike (copies the wasm into public/ via predev, then serves)
cd ../../spike/terminal && npm install && npm run dev   # http://localhost:5177
```

Open http://localhost:5177 — you get a live shell rendered by ghostty-web, fed by the
daemon over the WebSocket. Type to interact; resizing the window propagates `resize` to the
pty. `src/main.ts` is ~70 lines and shows the whole `pty`-channel ↔ terminal wiring.
