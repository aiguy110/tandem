// Resume-session de-risk: catalog union/dedupe plus all three resume paths
// (live focus, closed Tandem row restore, and external ACP import).

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { open, report, rule, sleep, mockPath, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7731;

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · resume-session de-risk');
  console.log(rule);

  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-resume-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'off',
  } });
  const h = { port: daemon.port, token: daemon.token, home, stop: async () => { await daemon.stop(); fs.rmSync(home, { recursive: true, force: true }); } };
  const frames: Frame[] = [];
  const ws = await open(h.port, h.token, (frame) => frames.push(frame));
  const send = (value: unknown) => ws.send(JSON.stringify(value));
  const waitFor = async (predicate: (frame: Frame) => boolean, timeoutMs = 8000): Promise<Frame> => {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const found = frames.find(predicate);
      if (found) return found;
      await sleep(25);
    }
    throw new Error('timed out waiting for resume test frame');
  };

  send({ t: 'spawn_agent', corrId: 'spawn', spec: { adapter: 'acp', agent: 'claude', workspace: { kind: 'existing', cwd: process.cwd() } } });
  const spawned = await waitFor((f) => f.t === 'ack' && f.corrId === 'spawn');
  const agentId = spawned.agentId!;

  send({ t: 'list_sessions', corrId: 'catalog-1' });
  const firstCatalog = (await waitFor((f) => f.t === 'sessions' && f.corrId === 'catalog-1')).catalog;
  const mockEntries = firstCatalog.sessions.filter((s: any) => s.sessionId === 'sess_mock');
  const external = firstCatalog.sessions.find((s: any) => s.sessionId === 'sess_external');

  send({ t: 'resume_session', corrId: 'live', sessionId: 'sess_mock' });
  const live = await waitFor((f) => f.t === 'ack' && f.corrId === 'live');

  send({ t: 'close_agent', corrId: 'close', agentId });
  await waitFor((f) => f.t === 'ack' && f.corrId === 'close');
  send({ t: 'resume_session', corrId: 'closed', sessionId: 'sess_mock' });
  const closed = await waitFor((f) => f.t === 'ack' && f.corrId === 'closed');

  // Use another existing cwd so the imported external session doesn't collide
  // with the reopened Tandem agent's directory.
  send({ t: 'resume_session', corrId: 'external', sessionId: 'sess_external', agent: 'claude', cwd: h.home });
  const imported = await waitFor((f) => f.t === 'ack' && f.corrId === 'external');
  send({ t: 'list_agents', corrId: 'agents-after-resume' });
  const agents = (await waitFor((f) => f.t === 'agents' && f.corrId === 'agents-after-resume')).agents ?? [];
  send({ t: 'list_sessions', corrId: 'catalog-2' });
  const secondCatalog = (await waitFor((f) => f.t === 'sessions' && f.corrId === 'catalog-2')).catalog;

  const checks: [string, boolean, string][] = [
    ['catalog dedupes ACP discovery against Tandem rows', mockEntries.length === 1 && mockEntries[0]?.source === 'tandem', `${mockEntries.length} sess_mock row(s), source=${mockEntries[0]?.source}`],
    ['catalog includes externally discovered sessions', external?.source === 'external' && external?.agent === 'claude', `${external?.sessionId ?? 'missing'} via ${external?.agent ?? '—'}`],
    ['adapter enumeration capability is reported', firstCatalog.adapters.some((a: any) => a.agent === 'claude' && a.supportsList), JSON.stringify(firstCatalog.adapters)],
    ['resuming a live session returns the existing agent', live.agentId === agentId && !live.error, `${live.agentId} === ${agentId}`],
    ['resuming a closed Tandem session restores its agent id', closed.agentId === agentId && !closed.error && agents.some((a: any) => a.id === agentId), `${closed.agentId} restored`],
    ['resuming an external session imports a new agent', !!imported.agentId && imported.agentId !== agentId && !imported.error && agents.some((a: any) => a.id === imported.agentId) && secondCatalog.sessions.some((s: any) => s.sessionId === 'sess_external' && s.source === 'tandem'), `${imported.agentId ?? 'missing'} bound to sess_external`],
  ];

  const pass = report(checks);
  ws.close();
  await h.stop();
  process.exit(pass ? 0 : 1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
