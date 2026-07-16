// Configurable catalog/profile + durable direct-terminal launch regression.
// Runs the configured daemon as an opaque process and observes only WS output.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { WebSocket } from 'ws';
import { parseDaemonCommand, startDaemon, type RunningDaemon } from './processHarness.ts';
import { sleep } from './testHarness.ts';

const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-config-'));
const command = parseDaemonCommand();

function configYaml(): string { return `
defaults:
  agent: custom
  profile: custom-fast
agents:
  custom:
    name: Custom Agent
    acp:
      command: ${JSON.stringify(process.execPath)}
      args: [mock-acp.js]
      env: { ACP_CUSTOM: yes }
    terminal:
      command: ${JSON.stringify(process.execPath)}
      startArgs: ["-e", "console.log(process.env.CUSTOM_ENV + ':' + process.argv.slice(1).join(':'))"]
      resumeArgs: [resume, "{sessionId}"]
      env: { CUSTOM_ENV: configured }
profiles:
  custom-fast:
    agent: custom
    name: Custom Fast
    acpArgs: [--fast]
    terminalArgs: [profile-arg]
`; }

async function socket(d: RunningDaemon): Promise<{ ws: WebSocket; frames: any[] }> {
  const frames: any[] = [];
  const ws = new WebSocket(`ws://127.0.0.1:${d.port}/?token=${d.token}`);
  ws.on('message', (raw) => frames.push(JSON.parse(raw.toString())));
  await new Promise<void>((resolve, reject) => { ws.once('open', resolve); ws.once('error', reject); });
  return { ws, frames };
}

async function waitFor<T>(fn: () => T | undefined, label: string): Promise<T> {
  for (let i = 0; i < 400; i++) { const value = fn(); if (value !== undefined) return value; await sleep(25); }
  throw new Error(`timeout waiting for ${label}`);
}

async function main(): Promise<void> {
  fs.writeFileSync(path.join(home, 'config.yml'), configYaml());
  let daemon: RunningDaemon | undefined;
  try {
    daemon = await startDaemon({ command, home, env: { TANDEM_BROWSER_MCP: 'off' } });
    let client = await socket(daemon);
    client.ws.send(JSON.stringify({ t: 'list_agent_catalog', corrId: 'catalog' }));
    const catalog = await waitFor(() => client.frames.find((f) => f.t === 'agent_catalog')?.catalog, 'catalog');
    if (catalog.defaultAgent !== 'custom' || catalog.defaultProfile !== 'custom-fast') throw new Error('configured defaults were not loaded');
    if (!catalog.agents.some((a: any) => a.id === 'claude') || !catalog.agents.some((a: any) => a.id === 'custom')) throw new Error('built-ins were not merged with custom agents');

    client.ws.send(JSON.stringify({ t: 'spawn_agent', corrId: 'spawn', spec: {
      adapter: 'pty', agent: 'custom', profile: 'custom-fast', workspace: { kind: 'existing', cwd: home },
      terminalArgs: ['spawn-arg', '{cwd}', '{agentId}', '{agentName}'],
    } }));
    const spawned = await waitFor(() => client.frames.find((f) => f.t === 'ack' && f.corrId === 'spawn'), 'spawn');
    if (!spawned.agentId || spawned.error) throw new Error(`spawn failed: ${spawned.error}`);
    client.ws.send(JSON.stringify({ t: 'subscribe', agentId: spawned.agentId, sinceSeq: 0 }));
    const output = await waitFor(() => {
      const text = client.frames.flatMap((f) => f.t === 'snapshot' ? f.transcript ?? [] : f.t === 'event' ? [{ event: f.event }] : [])
        .filter((e: any) => e.event?.kind === 'raw_pty').map((e: any) => Buffer.from(e.event.dataB64, 'base64').toString()).join('');
      return text.includes('configured:') ? text : undefined;
    }, 'terminal output');
    if (!output.includes(`configured:profile-arg:spawn-arg:${home}:${spawned.agentId}:`)) throw new Error(`profile args/placeholders missing: ${output}`);
    client.ws.close();

    // Restarting from the same home must replay the resolved launch rather than
    // re-resolving mutable catalog/profile input.
    await daemon.stop(); daemon = undefined;
    fs.writeFileSync(path.join(home, 'config.yml'), configYaml().replace('terminalArgs: [profile-arg]', 'terminalArgs: [changed-after-spawn]'));
    daemon = await startDaemon({ command, home, env: { TANDEM_BROWSER_MCP: 'off' } });
    client = await socket(daemon);
    client.ws.send(JSON.stringify({ t: 'subscribe', agentId: spawned.agentId, sinceSeq: 0 }));
    const replay = await waitFor(() => {
      const text = client.frames.flatMap((f) => f.t === 'snapshot' ? f.transcript ?? [] : []).filter((e: any) => e.event?.kind === 'raw_pty')
        .map((e: any) => Buffer.from(e.event.dataB64, 'base64').toString()).join('');
      return text.includes('configured:') ? text : undefined;
    }, 'persisted terminal replay');
    if (!replay.includes('configured:profile-arg:spawn-arg:')) throw new Error(`resolved launch was not durable: ${replay}`);
    client.ws.close();
    await daemon.stop(); daemon = undefined;

    fs.writeFileSync(path.join(home, 'config.yml'), 'agents:\n  broken:\n    terminal:\n      command: node\n      startArgs: --not-an-array\n');
    let invalid = '';
    try { daemon = await startDaemon({ command, home, env: { TANDEM_BROWSER_MCP: 'off' }, readyTimeoutMs: 2_000 }); }
    catch (error) { invalid = String(error); }
    if (!invalid.includes('agents.broken.terminal.startArgs must be an array of strings')) throw new Error(`invalid config was not rejected clearly: ${invalid}`);
    console.log('PASS configurable agents, profiles, terminal args/env, durable resolution, and invalid-config rejection');
  } finally {
    await daemon?.stop();
    fs.rmSync(home, { recursive: true, force: true });
  }
}
main().catch((error) => { console.error(error); process.exit(1); });
