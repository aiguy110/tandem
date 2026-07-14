// Terminal spike: render the daemon's real pty channel via ghostty-web.
// Proves: WASM init under Vite, fit/resize, feeding raw_pty bytes, input round-trip.
//
// Run the daemon first:  cd ../../daemon && npm run daemon -- --pty
// Then:                  npm run dev   (this app on :5177)

import { init, Terminal, FitAddon } from 'ghostty-web';

const AGENT = 'web-1';
const WS_URL = 'ws://localhost:7717';

const statusEl = document.getElementById('status')!;
const termEl = document.getElementById('term')!;
const setStatus = (s: string, color = '#4cc6e8') => {
  statusEl.textContent = s;
  statusEl.style.color = color;
};

const enc = new TextEncoder();
const bytesToB64 = (u: Uint8Array) => btoa(String.fromCharCode(...u));
const b64ToBytes = (b64: string) => Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));

async function main() {
  await init(); // loads ghostty-vt.wasm (served from /public)

  const term = new Terminal({ fontSize: 13, cursorBlink: true } as any);
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(termEl);
  fit.fit();

  const ws = new WebSocket(WS_URL);
  let ready = false;

  const sendResize = () => ready && ws.send(JSON.stringify({ t: 'resize', agentId: AGENT, cols: term.cols, rows: term.rows }));

  ws.onopen = () => {
    ready = true;
    setStatus(`connected · ${term.cols}×${term.rows}`, '#4fbf87');
    ws.send(JSON.stringify({ t: 'subscribe', agentId: AGENT, channels: ['pty'], sinceSeq: 0 }));
    sendResize();
    // demo output so a screenshot shows real rendering (colors + a directory listing)
    setTimeout(() => {
      const cmd = "printf '\\033[1;36mghostty-web\\033[0m in Tandem — \\033[32mVT OK\\033[0m\\n'; ls --color=always -1 | head\n";
      ws.send(JSON.stringify({ t: 'input', agentId: AGENT, bytesB64: bytesToB64(enc.encode(cmd)) }));
    }, 700);
  };
  ws.onclose = () => setStatus('ws closed', '#f2b441');
  ws.onerror = () => setStatus('ws error — is the daemon running? (npm run daemon -- --pty)', '#ff6b6b');

  const handle = (ev: any) => {
    if (ev?.kind === 'raw_pty' && ev.dataB64) term.write(b64ToBytes(ev.dataB64));
  };
  ws.onmessage = (e) => {
    const f = JSON.parse(e.data as string);
    if (f.t === 'snapshot') f.events.forEach((x: any) => handle(x.event));
    else if (f.t === 'event') handle(f.event);
  };

  // user keystrokes -> daemon -> pty
  term.onData((d: string) => ready && ws.send(JSON.stringify({ t: 'input', agentId: AGENT, bytesB64: bytesToB64(enc.encode(d)) })));

  window.addEventListener('resize', () => {
    fit.fit();
    sendResize();
  });

  // expose for the automated check
  (window as any).__tandemSpike = {
    get cols() { return term.cols; },
    get rows() { return term.rows; },
    get wsState() { return ws.readyState; },
  };
}

main().catch((e) => setStatus('init failed: ' + (e?.message ?? e), '#ff6b6b'));
