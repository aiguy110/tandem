// Deferred-restart de-risk: a restart request reports active turns, remains
// idempotent, and asks the daemon to shut down only after the last turn ends.

import { makeHarness, report, sleep } from './testHarness.ts';

const PORT = 7761;

async function request(port: number, token: string): Promise<[number, number]> {
  const response = await fetch(`http://127.0.0.1:${port}/internal/shutdown-after-turns?token=${token}`, { method: 'POST' });
  if (!response.ok) throw new Error(`shutdown request failed: ${response.status}`);
  const [active, already] = (await response.text()).trim().split(/\s+/).map(Number);
  return [active, already];
}

async function main() {
  let shutdownCalls = 0;
  const h = await makeHarness(PORT, { onShutdownRequested: () => shutdownCalls++ });
  const checks: [string, boolean, string][] = [];

  const agent = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'existing', cwd: h.home } });
  const turn = agent.prompt('DERISK_SLOWTERM');
  await sleep(150);

  const first = await request(PORT, h.token);
  const second = await request(PORT, h.token);
  checks.push(['first request reports one active turn and sets the flag', first[0] === 1 && first[1] === 0, `response=${first.join(' ')}`]);
  checks.push(['repeat request reports the flag was already set', second[0] === 1 && second[1] === 1, `response=${second.join(' ')}`]);

  await sleep(300);
  checks.push(['daemon stays up while the turn is active', shutdownCalls === 0, `shutdownCalls=${shutdownCalls}`]);
  await turn;
  await sleep(400);
  checks.push(['shutdown is requested once after the turn finishes', shutdownCalls === 1, `shutdownCalls=${shutdownCalls}`]);

  const pass = report(checks);
  await h.stop();
  process.exit(pass ? 0 : 1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
