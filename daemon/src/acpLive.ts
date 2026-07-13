// Live integration test: drives the REAL agent
// (@agentclientprotocol/claude-agent-acp) through AcpAdapter to confirm the
// adapter's framing and update parsing match a real ACP agent, not just the mock.
//
// Requires the local `claude` CLI to be authenticated (this uses the same auth).
// Run: npm run acp:live

import { AcpAdapter } from './acpAdapter.ts';
import { AgentSession } from './session.ts';

const rule = '─'.repeat(64);
// Resolve the real agent's entry point directly (avoids PATH/symlink surprises).
const agentEntry = new URL('../node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js', import.meta.url).pathname;

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · live ACP test — @agentclientprotocol/claude-agent-acp');
  console.log(rule);

  const adapter = new AcpAdapter('claude-1', { cmd: process.execPath, args: [agentEntry] });
  const session = new AgentSession('claude-1', adapter);

  const kinds: Record<string, number> = {};
  let text = '';
  session.onEvent((le) => {
    const k = le.event.kind;
    kinds[k] = (kinds[k] ?? 0) + 1;
    if (k === 'message_chunk') text += (le.event as any).text;
    if (k === 'tool_call') console.log('  · tool_call:', (le.event as any).title);
    if (k === 'permission_request') {
      const ev = le.event as any;
      console.log('  ⚠ permission requested:', ev.title, '→ auto-allow');
      session.respondPermission(ev.reqId, ev.options?.[0]?.optionId ?? 'allow');
    }
  });

  console.log('\n  ▸ spawning real agent:\n    ' + agentEntry);
  const guard = setTimeout(() => {
    console.error('\n  ✖ timed out after 60s — is `claude` authenticated? (try `claude` once interactively)');
    process.exit(2);
  }, 60000);

  await session.start({ cwd: process.cwd() });
  console.log(`  ▸ initialize + session/new OK · agent pid ${session.agentPid}`);

  console.log('  ▸ prompt: "Reply with exactly the word: pong…"');
  await session.prompt('Reply with exactly the word: pong. Do not use any tools or read any files.');
  clearTimeout(guard);

  await new Promise((r) => setTimeout(r, 300));
  console.log('\n' + rule);
  console.log('  normalized events seen:', JSON.stringify(kinds));
  console.log('  assistant text:', JSON.stringify(text.trim().slice(0, 200)));
  console.log(rule);

  const ok = (kinds['message_chunk'] ?? 0) > 0 && /pong/i.test(text);
  console.log(
    ok
      ? '  ✅ LIVE ACP OK — the real agent drove AcpAdapter end-to-end\n     (ndjson framing + session/update parsing confirmed against a real agent)'
      : '  ⚠ ran, but no clean "pong" — inspect the events above',
  );
  console.log(rule + '\n');

  await session.dispose();
  process.exit(ok ? 0 : 1);
}

main().catch((e) => {
  console.error('\n  ✖ live test error:', e?.message ?? e);
  process.exit(1);
});
