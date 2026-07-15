// ACP ↔ terminal handoff de-risk: active turns require explicit interruption,
// the resume CLI owns interactive I/O under the same agent id, and CLI exit
// automatically reloads the ACP session without breaking the event stream.

import { makeHarness, open, report, rule, sleep, type Frame } from './testHarness.ts';

const PORT = 7732;

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · ACP/terminal handoff de-risk');
  console.log(rule);

  const h = await makeHarness(PORT);
  const frames: Frame[] = [];
  const ws = await open(PORT, h.token, (frame) => frames.push(frame));
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
  const resumedWhileTerminal = await h.registry.resume('sess_mock');

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
  const session = h.registry.get(agentId)!;

  const checks: [string, boolean, string][] = [
    ['busy ACP turn refuses an implicit takeover', !!busy.error?.startsWith('agent_busy:'), busy.error ?? 'missing error'],
    ['explicit takeover cancels then enters terminal', !takeover.error && modes.includes('terminal'), `modes=${modes.join(' → ')}`],
    ['resume CLI receives the persisted session id', !!ready, 'MOCK_RESUME_READY sess_mock'],
    ['resume lookup during terminal control returns the same live agent', resumedWhileTerminal.id === agentId && h.registry.list().length === 1, `${resumedWhileTerminal.id}, live agents=${h.registry.list().length}`],
    ['terminal input/output round-trips through the agent channel', !!input, 'MOCK_RESUME_INPUT exit'],
    ['CLI exit automatically restores ACP control', session.controlMode === 'transcript' && session.acpSessionId === 'sess_mock', `mode=${session.controlMode} session=${session.acpSessionId}`],
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
