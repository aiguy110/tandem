// Live shared-browser broker: serves the viewer, streams the screencast over a
// WebSocket, forwards the human's input, and runs a demo "agent loop" that keeps
// touching the page (paused while the human holds the wheel). For eyeballing +
// the screenshot proof.
//
// Run: npm run broker   (then open http://localhost:5178/index.html)

import { createServer } from 'node:http';
import { readFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WebSocketServer } from 'ws';
import { launchChrome, SharedBrowser } from './sharedBrowser.ts';

const CDP_PORT = 9334;
const HTTP_PORT = 5178;
const WS_PORT = 7788;
const pubDir = new URL('../public/', import.meta.url).pathname;

function servePublic(port: number) {
  return createServer((req, res) => {
    const p = req.url === '/' ? '/index.html' : req.url!.split('?')[0];
    try {
      const body = readFileSync(join(pubDir, p));
      res.writeHead(200, { 'content-type': p.endsWith('.html') ? 'text/html' : 'text/plain' });
      res.end(body);
    } catch {
      res.writeHead(404);
      res.end('not found');
    }
  }).listen(port);
}

async function main() {
  servePublic(HTTP_PORT);
  const proc = await launchChrome(CDP_PORT, mkdtempSync(join(tmpdir(), 'tandem-chrome-')));
  const sb = new SharedBrowser(`http://localhost:${CDP_PORT}`);
  await sb.connect();
  await sb.agentDo('nav', (p) => p.goto(`http://localhost:${HTTP_PORT}/target.html`, { waitUntil: 'load' }));

  const wss = new WebSocketServer({ port: WS_PORT });
  const broadcast = (o: unknown) => wss.clients.forEach((c) => c.readyState === 1 && c.send(JSON.stringify(o)));

  await sb.startScreencast((b64) => broadcast({ t: 'frame', data: b64 }));

  wss.on('connection', (ws) => {
    ws.send(JSON.stringify({ t: 'owner', owner: sb.controlOwner }));
    ws.on('message', (raw) => {
      let m: any;
      try {
        m = JSON.parse(raw.toString());
      } catch {
        return;
      }
      if (m.t === 'grab') {
        sb.grab();
        broadcast({ t: 'owner', owner: 'user' });
      } else if (m.t === 'release') {
        sb.release();
        broadcast({ t: 'owner', owner: 'agent' });
      } else if (m.t === 'input' && sb.controlOwner === 'user') {
        if (m.kind === 'click') void sb.userClick(m.x, m.y);
        else if (m.kind === 'move') void sb.userMove(m.x, m.y);
        else if (m.kind === 'text') void sb.userType(m.text);
      }
    });
  });

  // demo agent loop — keeps touching the page, but only when it owns the wheel
  setInterval(() => {
    if (sb.controlOwner !== 'agent') return;
    void sb.agentDo('demo', (p) => p.locator('#input').fill('agent @ ' + new Date().toLocaleTimeString()));
  }, 1500);

  // demo agent-initiated takeover request after 9s
  setTimeout(() => broadcast({ t: 'takeover', reason: 'Log in to continue — the agent needs you' }), 9000);

  console.log(`shared-browser broker live:`);
  console.log(`  viewer     http://localhost:${HTTP_PORT}/index.html`);
  console.log(`  ws frames  ws://localhost:${WS_PORT}`);
  console.log(`  chrome pid ${proc.pid} (CDP :${CDP_PORT})`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
