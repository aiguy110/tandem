// ACP ↔ terminal handoff de-risk: active turns require explicit interruption,
// the resume CLI owns interactive I/O under the same agent id, and CLI exit
// automatically reloads the ACP session without breaking the event stream.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { open, report, rule, sleep, mockPath, mockResumeCliPath, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7732;

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · ACP/terminal handoff de-risk');
  console.log(rule);

  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-handoff-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]),
    TANDEM_RESUME_CMD_CLAUDE: JSON.stringify([process.execPath, mockResumeCliPath, '{sessionId}']), TANDEM_BROWSER_MCP: 'off',
  } });
  const h = { port: daemon.port, token: daemon.token, stop: async () => { await daemon.stop(); fs.rmSync(home, { recursive: true, force: true }); } };
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
    throw new Error('timed out waiting for handoff test frame');
  };

  send({ t: 'spawn_agent', corrId: 'spawn', spec: { adapter: 'acp', agent: 'claude', workspace: { kind: 'existing', cwd: process.cwd() } } });
  const agentId = (await waitFor((f) => f.t === 'ack' && f.corrId === 'spawn')).agentId!;
  send({ t: 'subscribe', agentId, sinceSeq: 0 });
  await waitFor((f) => f.t === 'snapshot' && f.agentId === agentId);

  send({ t: 'prompt', agentId, text: 'DERISK_CANCEL' });
  await waitFor((f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'permission_request');

  send({ t: 'enter_terminal', corrId: 'busy', agentId });
  const busy = await waitFor((f) => f.t === 'ack' && f.corrId === 'busy');
  send({ t: 'enter_terminal', corrId: 'takeover', agentId, interrupt: true });
  const takeover = await waitFor((f) => f.t === 'ack' && f.corrId === 'takeover');
  const ready = await waitFor(
    (f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'raw_pty' && Buffer.from(f.event.dataB64, 'base64').toString().includes('MOCK_RESUME_READY sess_mock'),
  );
  send({ t: 'resume_session', corrId: 'resume-terminal', sessionId: 'sess_mock' });
  const resumedWhileTerminal = await waitFor((f) => f.t === 'ack' && f.corrId === 'resume-terminal');
  send({ t: 'list_agents', corrId: 'agents-terminal' });
  const liveAgents = (await waitFor((f) => f.t === 'agents' && f.corrId === 'agents-terminal')).agents ?? [];

  send({ t: 'input', agentId, bytesB64: Buffer.from('exit\n').toString('base64') });
  const input = await waitFor(
    (f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'raw_pty' && Buffer.from(f.event.dataB64, 'base64').toString().includes('MOCK_RESUME_INPUT exit'),
  );
  await waitFor((f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'control_state' && f.event.mode === 'transcript');
  await sleep(100);

  const modes = frames
    .filter((f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'control_state')
    .map((f) => f.event.mode);
  const events = frames.filter((f) => f.t === 'event' && f.agentId === agentId);
  const seqs = events.map((f) => f.seq!).filter(Boolean);
  const gapless = seqs.every((seq, i) => i === 0 || seq === seqs[i - 1] + 1);
  const restoredControl = frames.some((f) => f.t === 'event' && f.agentId === agentId && f.event?.kind === 'control_state' && f.event.mode === 'transcript');

  const checks: [string, boolean, string][] = [
    ['configured resume CLI launches successfully', !!ready, 'mock resume CLI produced readiness output'],
    ['busy ACP turn refuses an implicit takeover', !!busy.error?.startsWith('agent_busy:'), busy.error ?? 'missing error'],
    ['explicit takeover cancels then enters terminal', !takeover.error && modes.includes('terminal'), `modes=${modes.join(' → ')}`],
    ['resume CLI receives the persisted session id', !!ready, 'MOCK_RESUME_READY sess_mock'],
    ['resume lookup during terminal control returns the same live agent', resumedWhileTerminal.agentId === agentId && liveAgents.length === 1, `${resumedWhileTerminal.agentId}, live agents=${liveAgents.length}`],
    ['terminal input/output round-trips through the agent channel', !!input, 'MOCK_RESUME_INPUT exit'],
    ['CLI exit automatically restores ACP control', restoredControl, `mode=${modes.at(-1)}`],
    ['one monotonic event stream spans both adapters', gapless, `${seqs.length} events, seq ${seqs[0]}..${seqs.at(-1)}`],
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
