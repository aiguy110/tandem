// Public-boundary browser suite. The deeper Node broker/CDP assertions remain
// in deriskBrowser.ts; this suite is the implementation-parity gate.
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { WebSocket } from 'ws';
import { mockPath, report, rule, sleep } from './testHarness.ts';
import { assertPortReleased, parseDaemonCommand, startDaemon } from './processHarness.ts';

const controlMcpPath = new URL('./browser/controlMcp.mjs', import.meta.url).pathname;

async function target(): Promise<{ url: string; clicked: () => boolean; close(): void }> {
  let clicked = false;
  const server = http.createServer((req, res) => {
    if (req.url === '/clicked') { clicked = true; res.end('ok'); return; }
    res.setHeader('content-type', 'text/html');
    res.end(`<!doctype html><body tabindex="0" onkeydown="fetch('/clicked')"><script>document.body.focus();setInterval(()=>document.title=String(Date.now()),100)</script>`);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address() as any;
  return { url: `http://127.0.0.1:${address.port}/`, clicked: () => clicked, close: () => server.close() };
}

async function main(): Promise<void> {
  console.log(`\n${rule}\n  TANDEM · process browser parity\n${rule}`);
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-browser-process-'));
  const page = await target();
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || 17738), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'on', TANDEM_DEV_BROWSER: '1',
  } });
  const frames: any[] = [];
  const ws = new WebSocket(`ws://127.0.0.1:${daemon.port}/?token=${daemon.token}`);
  ws.on('message', (raw) => frames.push(JSON.parse(raw.toString())));
  await new Promise<void>((resolve, reject) => { ws.once('open', resolve); ws.once('error', reject); });
  const send = (value: any) => ws.send(JSON.stringify(value));
  const wait = async (predicate: () => boolean, ms = 6_000) => { const end = Date.now() + ms; while (Date.now() < end) { if (predicate()) return true; await sleep(30); } return false; };
  const checks: [string, boolean, string][] = [];
  try {
    send({ t: 'spawn_agent', corrId: 'spawn', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd: home }, name: 'web-1' } });
    await wait(() => frames.some((f) => f.corrId === 'spawn'));
    const id = frames.find((f) => f.corrId === 'spawn')?.agentId;
    if (!id) throw new Error('spawn failed');
    send({ t: 'subscribe', agentId: id });
    const cold = await wait(() => frames.some((f) => f.t === 'browser_state' && f.agentId === id && f.active === false));
    checks.push(['browser is lazy and initially inactive', cold, `coldState=${cold}`]);

    const nav = await fetch(`http://127.0.0.1:${daemon.port}/internal/browser/devnav?agentId=${encodeURIComponent(id)}&token=${daemon.token}`, {
      method: 'POST', headers: { 'content-type': 'application/json', authorization: `Bearer ${daemon.token}` }, body: JSON.stringify({ url: page.url }),
    });
    const active = await wait(() => frames.some((f) => f.t === 'browser_state' && f.agentId === id && f.active === true));
    const screencast = await wait(() => frames.filter((f) => f.t === 'browser_frame' && f.agentId === id).length >= 2);
    checks.push(['dev navigation provisions the shared browser', nav.ok && active, `status=${nav.status} active=${active}`]);
    checks.push(['screencast frames cross the daemon WS', screencast, `frames=${frames.filter((f) => f.t === 'browser_frame').length}`]);

    send({ t: 'browser_control', corrId: 'grab', agentId: id, action: 'grab' });
    const grabbed = await wait(() => frames.some((f) => f.t === 'browser_state' && f.agentId === id && f.controlOwner === 'user'));
    send({ t: 'browser_input', corrId: 'key', agentId: id, event: { kind: 'keydown', key: 'Enter', code: 'Enter', keyCode: 13 } });
    const inputAccepted = await wait(() => frames.some((f) => f.t === 'ack' && f.corrId === 'key' && !f.error), 3_000);
    checks.push(['grab transfers control and accepts user input', grabbed && inputAccepted, `grabbed=${grabbed} inputAccepted=${inputAccepted}`]);
    send({ t: 'browser_control', corrId: 'release', agentId: id, action: 'release' });
    const released = await wait(() => frames.some((f) => f.t === 'browser_state' && f.agentId === id && f.controlOwner === 'agent'));
    checks.push(['release returns control to the agent', released, `released=${released}`]);

    const mcp = spawn(process.execPath, [controlMcpPath], { stdio: ['pipe', 'pipe', 'ignore'], env: {
      ...process.env, TANDEM_CONTROL_URL: `http://127.0.0.1:${daemon.port}`, TANDEM_TOKEN: daemon.token, TANDEM_AGENT_ID: id,
    } });
    let output = '';
    mcp.stdout!.on('data', (chunk) => { output += chunk.toString(); });
    const line = (value: any) => mcp.stdin!.write(JSON.stringify(value) + '\n');
    line({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'derisk', version: '0' } } });
    line({ jsonrpc: '2.0', method: 'notifications/initialized' });
    line({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'browser_request_takeover', arguments: { reason: 'parity' } } });
    const surfaced = await wait(() => frames.some((f) => f.t === 'event' && f.agentId === id && f.event?.kind === 'takeover_request'));
    const pending = !output.split('\n').some((v) => v.includes('"id":2'));
    send({ t: 'browser_control', agentId: id, action: 'grab' });
    send({ t: 'browser_control', agentId: id, action: 'release' });
    const resolved = await wait(() => output.split('\n').some((v) => v.includes('"id":2') && v.includes('handed control back')));
    mcp.kill();
    checks.push(['takeover MCP surfaces, blocks, and resolves on release', surfaced && pending && resolved, `surfaced=${surfaced} pending=${pending} resolved=${resolved}`]);

    send({ t: 'close_agent', corrId: 'close', agentId: id, force: true });
    const closed = await wait(() => frames.some((f) => f.t === 'ack' && f.corrId === 'close' && !f.error));
    checks.push(['close_agent succeeds after browser use', closed, `closed=${closed}`]);
    if (!report(checks)) process.exitCode = 1;
  } finally {
    ws.close(); page.close(); const port = daemon.port; await daemon.stop(); await assertPortReleased(port);
    fs.rmSync(home, { recursive: true, force: true });
  }
}
main().catch((error) => { console.error(error); process.exit(1); });
