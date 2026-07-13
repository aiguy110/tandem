// Optional: proves the pty adapter path when node-pty is installed. The daemon
// owns the pty master fd, so the child keeps running independent of any client.
// Skips cleanly if node-pty isn't built.

import { PtyAdapter } from './ptyAdapter.ts';
import { AgentSession } from './session.ts';

async function main() {
  const adapter = new PtyAdapter('shell-1');
  const session = new AgentSession('shell-1', adapter);
  try {
    await session.start({
      cwd: process.cwd(),
      cmd: 'bash',
      args: ['-c', 'for i in 1 2 3 4 5; do echo "pty line $i"; sleep 0.2; done'],
    });
  } catch (e) {
    console.log('⚠  pty smoke skipped:', (e as Error).message);
    process.exit(0);
  }

  console.log(`▸ pty agent spawned · pid ${session.agentPid}`);
  let out = '';
  session.onEvent((le) => {
    if (le.event.kind === 'raw_pty') out += Buffer.from(le.event.data).toString('utf8');
  });

  await new Promise((r) => setTimeout(r, 1600));
  console.log('── captured pty scrollback (owned by daemon) ──');
  console.log(out.trim());
  await session.dispose();
  process.exit(0);
}

main();
