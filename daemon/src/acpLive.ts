// Live integration test: drives the REAL agent
// (@agentclientprotocol/claude-agent-acp) through AcpAdapter to confirm the
// adapter's framing and update parsing match a real ACP agent, not just the mock.
//
// Requires the local `claude` CLI to be authenticated (this uses the same auth).
// Run: npm run acp:live

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AcpAdapter } from './acpAdapter.ts';
import { AgentSession } from './session.ts';
import { MemoryStore } from './eventLog.ts';
import { LocalChromiumDriver } from './browser/driver.ts';
import { BrowserBroker } from './browser/broker.ts';
import { buildBrowserMcpServers } from './browser/mcpWiring.ts';
import type { McpServerSpec, SpawnSpec } from './types.ts';

const rule = '─'.repeat(64);
// Resolve the real agent's entry point directly (avoids PATH/symlink surprises).
const agentEntry = new URL('../node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js', import.meta.url).pathname;

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · live ACP test — @agentclientprotocol/claude-agent-acp');
  console.log(rule);

  const adapter = new AcpAdapter('claude-1', { cmd: process.execPath, args: [agentEntry] });
  const spec: SpawnSpec = { adapter: 'acp', workspace: { kind: 'existing', cwd: process.cwd() }, name: 'claude-1' };
  const session = new AgentSession('claude-1', 'claude-1', spec, adapter, new MemoryStore());

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

  // Phase 5: with TANDEM_BROWSER_MCP=on (default here unless 'off'), register the
  // Playwright MCP + Tandem-control MCP in session/new mcpServers, exactly as the
  // registry does — proving the REAL agent accepts the registration. The broker is
  // lazy: no Chromium spawns unless the model actually calls a browser tool.
  let broker: BrowserBroker | undefined;
  let mcpServers: McpServerSpec[] = [];
  if (process.env.TANDEM_BROWSER_MCP !== 'off') {
    const udRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-acplive-browser-'));
    broker = new BrowserBroker(new LocalChromiumDriver({ userDataRoot: udRoot }));
    await broker.start();
    // No daemon HTTP server here — the control MCP registers fine; its takeover
    // tool would only need the URL if the model actually invoked it.
    mcpServers = buildBrowserMcpServers({ broker, controlUrl: 'http://127.0.0.1:0', token: 'acp-live' }, 'claude-1');
    console.log(`\n  ▸ browser MCP: registering ${mcpServers.length} servers (${mcpServers.map((s) => s.name).join(', ')})`);
  }

  console.log('\n  ▸ spawning real agent:\n    ' + agentEntry);
  const guard = setTimeout(() => {
    console.error('\n  ✖ timed out after 60s — is `claude` authenticated? (try `claude` once interactively)');
    process.exit(2);
  }, 60000);

  await session.start({ cwd: process.cwd(), mcpServers });
  console.log(`  ▸ initialize + session/new OK · agent pid ${session.agentPid}${mcpServers.length ? ' · mcpServers accepted' : ''}`);

  console.log('  ▸ prompt: "Reply with exactly the word: pong…"');
  const stop1 = await session.prompt('Reply with exactly the word: pong. Do not use any tools or read any files.');
  console.log(`  ▸ turn 1 stopReason=${stop1}`);
  clearTimeout(guard);

  // ---- Phase 3: prove the real agent accepts our fs/terminal capabilities and
  // actually drives the daemon-owned TerminalHost. If claude-agent-acp exercises
  // terminals for a shell command, we'll see terminal_output events flow through
  // ClientServices — the daemon serviced a real terminal/* request.
  const termChunks: string[] = [];
  session.onEvent((le) => {
    if (le.event.kind === 'terminal_output') termChunks.push((le.event as any).chunk);
  });
  console.log('\n  ▸ prompt 2: "run `echo tandem-live-hello` in a terminal and tell me its output"');
  const guard2 = setTimeout(() => {
    console.error('\n  ✖ timed out after 90s on the terminal turn');
    process.exit(2);
  }, 90000);
  const stop2 = await session.prompt(
    'Run the shell command `echo tandem-live-hello` in a terminal, then tell me exactly what it printed.',
  );
  clearTimeout(guard2);
  console.log(`  ▸ turn 2 stopReason=${stop2}`);

  await new Promise((r) => setTimeout(r, 300));
  const termOut = termChunks.join('');
  console.log('\n' + rule);
  console.log('  normalized events seen:', JSON.stringify(kinds));
  console.log('  assistant text:', JSON.stringify(text.trim().slice(0, 300)));
  console.log('  terminal_output chunks:', termChunks.length, JSON.stringify(termOut.slice(0, 120)));
  console.log(rule);

  const pongOk = (kinds['message_chunk'] ?? 0) > 0 && /pong/i.test(text);
  // The agent was told to run it in a terminal; success either way if the
  // capability handshake held and it completed. We separately note whether the
  // TerminalHost actually saw the command's output.
  const termUsed = (kinds['terminal_output'] ?? 0) > 0;
  const termSawHello = /tandem-live-hello/.test(termOut) || /tandem-live-hello/.test(text);
  console.log(
    pongOk
      ? '  ✅ LIVE ACP OK — the real agent drove AcpAdapter end-to-end\n     (ndjson framing + session/update parsing confirmed against a real agent)'
      : '  ⚠ ran, but no clean "pong" — inspect the events above',
  );
  console.log(
    termUsed
      ? `  ✅ TERMINAL SERVICED — real agent used terminal/*; TerminalHost captured output (hello seen=${termSawHello})`
      : `  ⚠ agent completed the terminal turn but did not use terminal/* (hello in text=${termSawHello}); fs/terminal capabilities were still advertised & accepted`,
  );
  console.log(rule + '\n');

  if (broker) {
    console.log(`  ▸ browser lazily provisioned during run: ${broker.isProvisioned('claude-1')}`);
    await broker.stop();
  }
  await session.dispose();
  process.exit(pongOk ? 0 : 1);
}

main().catch((e) => {
  console.error('\n  ✖ live test error:', e?.message ?? e);
  process.exit(1);
});
