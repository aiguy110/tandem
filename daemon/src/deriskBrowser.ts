// Shared-browser de-risk (Phase 5, D13). Runs the REAL daemon (makeHarness) with
// the LocalChromiumDriver broker, and a scripted CDP client standing in for the
// agent side — connecting through the broker's gated CDP proxy exactly as
// Playwright MCP would (chromium.connectOverCDP of the broker's --cdp-endpoint).
//
// Asserts a–h from the phase VALIDATION list:
//   a) laziness: spawning an agent creates NO browser; first use provisions one.
//   b) agent drives the page via the broker (connectOverCDP through the proxy).
//   c) screencast frames flow over the daemon WS to a browser-channel subscriber.
//   d) grab → the next agent action is HELD (not executed, page unchanged).
//   e) user input via browser_input reaches the page while owner=user.
//   f) release → the held agent action completes.
//   g) takeover: the real Tandem-control MCP (stdio) blocks, surfaces the
//      attention event over the WS, and resolves on release (status → working).
//   h) close_agent tears the browser down (process gone).
//
// Run: npm run derisk:browser

import http from 'node:http';
import fs from 'node:fs';
import { spawn, type ChildProcess } from 'node:child_process';
import { chromium, type Browser, type Page } from 'playwright-core';
import { WebSocket } from 'ws';
import { makeHarness, open, sleep, rule, report, type Frame } from './testHarness.ts';

const PORT = 7738;
const targetHtml = fs.readFileSync(new URL('./browser/target.html', import.meta.url), 'utf8');
const controlMcpPath = new URL('./browser/controlMcp.mjs', import.meta.url).pathname;

function serveTarget(): Promise<{ url: string; close: () => void }> {
  return new Promise((resolve) => {
    const srv = http.createServer((req, res) => {
      res.writeHead(200, { 'content-type': 'text/html' });
      res.end(targetHtml);
    });
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      const port = typeof addr === 'object' && addr ? addr.port : 0;
      resolve({ url: `http://127.0.0.1:${port}/target.html`, close: () => srv.close() });
    });
  });
}

const alive = (pid?: number): boolean => {
  if (!pid) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · shared-browser de-risk (Phase 5 — broker + gate + MCP)');
  console.log(rule);

  const target = await serveTarget();
  const target2 = target.url + '?after=1';
  const h = await makeHarness(PORT); // browser MCP off; we drive the broker directly
  const checks: [string, boolean, string][] = [];

  // A WS client subscribed to the agent's browser (+ status) channels.
  const frames: Frame[] = [];
  const ws = await open(h.port, h.token, (f) => frames.push(f));
  const browserFrames = () => frames.filter((f) => f.t === 'browser_frame');
  const states = () => frames.filter((f) => f.t === 'browser_state');

  // ============ (a) laziness ============
  const agent = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'existing', cwd: process.cwd() }, name: 'web-1' });
  const id = agent.id;
  ws.send(JSON.stringify({ t: 'subscribe', agentId: id })); // all channels incl. browser + status
  await sleep(150);
  const coldAfterSpawn = !h.broker.isProvisioned(id) && h.broker.browserPid(id) === undefined;
  const stateColdSent = states().some((s) => s.active === false);

  // ============ (b) agent drives via the broker ============
  const brokerUrl = h.broker.endpointFor(id);
  const agentBrowser: Browser = await chromium.connectOverCDP(brokerUrl); // == Playwright MCP path
  const agentCtx = agentBrowser.contexts()[0] ?? (await agentBrowser.newContext());
  const agentPage: Page = agentCtx.pages()[0] ?? (await agentCtx.newPage());
  await agentPage.goto(target.url, { waitUntil: 'load' });
  const title = await agentPage.locator('#title').innerText();
  const agentDrives = /shared browser/i.test(title);

  const provisionedOnce = h.broker.isProvisioned(id) && alive(h.broker.browserPid(id));
  const browserPid = h.broker.browserPid(id);
  // second connect must NOT spawn a second browser
  const agentBrowser2 = await chromium.connectOverCDP(brokerUrl);
  await agentBrowser2.close();
  const stillOne = h.broker.browserPid(id) === browserPid;
  checks.push(['(a) laziness: no browser at spawn; first use provisions exactly one', coldAfterSpawn && provisionedOnce && stillOne, `coldAtSpawn=${coldAfterSpawn} provisioned=${provisionedOnce} stillOnePid=${stillOne} (pid ${browserPid})`]);
  checks.push(['(a) browser_state active:false sent before first use', stateColdSent, `saw ${states().length} state msgs`]);
  checks.push(['(b) agent drives the page via the broker (connectOverCDP proxy)', agentDrives, `#title="${title}"`]);

  // ============ (c) screencast frames over the daemon WS ============
  frames.length = 0;
  // Nudge the WS to (re)subscribe browser now that the browser is active, so the
  // server starts the screencast for this focused subscriber.
  ws.send(JSON.stringify({ t: 'subscribe', agentId: id }));
  await sleep(1500); // the target's live counter animates → frames flow
  const fr = browserFrames();
  const plausible = fr.length >= 3 && fr.every((f) => (f.meta?.deviceWidth ?? 0) > 0 && (f.meta?.deviceHeight ?? 0) > 0);
  const activeState = states().some((s) => s.active === true);
  checks.push(['(c) screencast frames flow over the WS (plausible dims)', plausible, `${fr.length} frames, dims ${fr[0]?.meta?.deviceWidth}x${fr[0]?.meta?.deviceHeight}`]);
  checks.push(['(c) browser_state active:true announced once provisioned', activeState, `states active=${states().map((s) => s.active).join(',')}`]);

  // ============ (d) grab hard-pauses the agent ============
  const shared = await h.broker.sharedBrowser(id); // daemon's UNGATED view/control page
  ws.send(JSON.stringify({ t: 'browser_control', agentId: id, action: 'grab' }));
  await sleep(100);
  let navDone: boolean = false;
  const heldNav = agentPage.goto(target2, { waitUntil: 'load' }).then(() => {
    navDone = true;
  }).catch(() => {});
  await sleep(800);
  const urlWhileHeld = shared.page!.url();
  const heldWhileUserOwns = !navDone && !/after=1/.test(urlWhileHeld);
  checks.push(['(d) grab holds the next agent action (page unchanged)', heldWhileUserOwns, `navDone=${navDone} sharedUrl=${urlWhileHeld}`]);

  // ============ (e) user input reaches the page (owner=user) ============
  const box = await shared.page!.locator('#btn').boundingBox();
  ws.send(JSON.stringify({ t: 'browser_input', agentId: id, event: { kind: 'click', x: box!.x + box!.width / 2, y: box!.y + box!.height / 2 } }));
  await sleep(400);
  const status = await shared.page!.locator('#status').innerText();
  const userInputWorks = /CLICKED BY USER/.test(status);
  checks.push(['(e) user input via browser_input reaches the page', userInputWorks, `#status="${status}"`]);

  // ============ (f) release resumes the held agent action ============
  ws.send(JSON.stringify({ t: 'browser_control', agentId: id, action: 'release' }));
  await heldNav;
  await sleep(200);
  const resumed = navDone && /after=1/.test(shared.page!.url());
  checks.push(['(f) release resumes the held agent action', resumed, `navDone=${navDone} url=${shared.page!.url()}`]);

  // ============ (g) agent-initiated takeover via the real control MCP ============
  frames.length = 0;
  const takeoverResult = await runTakeover(h.port, h.token, id, ws, agent);
  checks.push(['(g) takeover_request surfaces over the WS + control MCP blocks then resolves', takeoverResult.ok, takeoverResult.detail]);

  // ============ (h) close_agent tears the browser down ============
  const pidBeforeClose = h.broker.browserPid(id);
  await agentBrowser.close().catch(() => {});
  await h.registry.close(id, true);
  await sleep(500);
  const gone = !h.broker.isProvisioned(id) && !alive(pidBeforeClose);
  checks.push(['(h) close_agent tears the browser down (process gone)', gone, `pid ${pidBeforeClose} alive=${alive(pidBeforeClose)} provisioned=${h.broker.isProvisioned(id)}`]);

  const pass = report(checks);
  ws.close();
  await h.stop();
  target.close();
  process.exit(pass ? 0 : 1);
}

// Spawn the real Tandem-control MCP over stdio, drive a browser_request_takeover
// tool call, and prove it blocks until release while the WS sees the attention
// event and the agent goes blocked → working.
async function runTakeover(
  port: number,
  token: string,
  agentId: string,
  ws: WebSocket,
  agent: { status: string },
): Promise<{ ok: boolean; detail: string }> {
  const mcp: ChildProcess = spawn(process.execPath, [controlMcpPath], {
    stdio: ['pipe', 'pipe', 'inherit'],
    env: { ...process.env, TANDEM_CONTROL_URL: `http://127.0.0.1:${port}`, TANDEM_TOKEN: token, TANDEM_AGENT_ID: agentId },
  });
  const responses = new Map<number, any>();
  let buf = '';
  mcp.stdout!.on('data', (d) => {
    buf += d.toString();
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line.trim()) continue;
      try {
        const m = JSON.parse(line);
        if (m.id !== undefined) responses.set(m.id, m);
      } catch {
        /* ignore */
      }
    }
  });
  const send = (o: unknown) => mcp.stdin!.write(JSON.stringify(o) + '\n');
  const waitFor = async (rid: number, ms = 4000) => {
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      if (responses.has(rid)) return responses.get(rid);
      await sleep(30);
    }
    return undefined;
  };

  // MCP handshake.
  send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'derisk', version: '0' } } });
  const initRes = await waitFor(1);
  send({ jsonrpc: '2.0', method: 'notifications/initialized' });
  send({ jsonrpc: '2.0', id: 2, method: 'tools/list' });
  const listRes = await waitFor(2);
  const hasTool = !!listRes?.result?.tools?.some((t: any) => t.name === 'browser_request_takeover');

  // Fire the blocking tool call.
  const seenTakeover: Frame[] = [];
  const collect = (f: Frame) => {
    if (f.t === 'event' && f.event?.kind === 'takeover_request') seenTakeover.push(f);
  };
  ws.on('message', (raw) => collect(JSON.parse(raw.toString())));
  send({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'browser_request_takeover', arguments: { reason: 'log in to continue' } } });

  // Wait for the attention event + blocked status; the tool call must still be pending.
  let surfaced = false;
  for (let i = 0; i < 60 && !surfaced; i++) {
    await sleep(50);
    surfaced = seenTakeover.length > 0;
  }
  await sleep(200);
  const blockedWhileWaiting = agent.status === 'blocked';
  const stillPending = !responses.has(3);

  // Hand the wheel back → the blocked tool call resolves, agent returns to working.
  ws.send(JSON.stringify({ t: 'browser_control', agentId, action: 'grab' }));
  await sleep(50);
  ws.send(JSON.stringify({ t: 'browser_control', agentId, action: 'release' }));
  const callRes = await waitFor(3, 6000);
  await sleep(200);
  const resolvedText = /handed control back/i.test(callRes?.result?.content?.[0]?.text ?? '');
  const backToWorking = agent.status === 'working' || agent.status === 'idle';

  mcp.kill();
  const ok = !!initRes && hasTool && surfaced && blockedWhileWaiting && stillPending && resolvedText && backToWorking;
  return {
    ok,
    detail: `init=${!!initRes} tool=${hasTool} surfaced=${surfaced} blocked=${blockedWhileWaiting} pendingWhileBlocked=${stillPending} resolved=${resolvedText} status→${agent.status}`,
  };
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
