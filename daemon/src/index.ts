// Standalone daemon: one agent + the WS server, for manual poking with a client.
//   npm run daemon           # ACP agent (mock)
//   npm run daemon -- --pty  # pty agent (a bash shell; needs node-pty)

import { AcpAdapter } from './acpAdapter.ts';
import { PtyAdapter } from './ptyAdapter.ts';
import { AgentSession } from './session.ts';
import { startServer } from './server.ts';
import type { AgentAdapter } from './types.ts';

const usePty = process.argv.includes('--pty');
const PORT = 7717;
const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;

async function main() {
  const adapter: AgentAdapter = usePty
    ? new PtyAdapter('web-1')
    : new AcpAdapter('web-1', { cmd: process.execPath, args: [mockPath] });

  const session = new AgentSession('web-1', adapter);
  await session.start({ cwd: process.cwd(), cmd: 'bash' });
  startServer(session, PORT);

  console.log(`tandem daemon · agent web-1 (${usePty ? 'pty' : 'acp'}) · ws://localhost:${PORT} · agent pid ${session.agentPid}`);
  console.log('subscribe with: {"t":"subscribe","agentId":"web-1","sinceSeq":0}');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
