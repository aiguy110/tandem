// Tandem-control MCP server (stdio) — the ONLY tool the agent gets beyond
// Playwright MCP: `browser_request_takeover(reason)`. It is spawned by the ACP
// AGENT (which is the MCP client), so it reaches the daemon over localhost HTTP,
// authed with the bearer token injected via the mcpServer `env` block at
// session/new. The call BLOCKS until the human takes the wheel and hands it back.
//
// Transport: MCP stdio = newline-delimited JSON-RPC 2.0 (one message per line).
// Kept dependency-free and thin on purpose (hand-rolled, mirrors acpAdapter).

import readline from 'node:readline';

const CONTROL_URL = process.env.TANDEM_CONTROL_URL; // e.g. http://127.0.0.1:7717
const TOKEN = process.env.TANDEM_TOKEN ?? '';
const AGENT_ID = process.env.TANDEM_AGENT_ID ?? '';
const POLL_MS = 1000;

function send(msg) {
  process.stdout.write(JSON.stringify(msg) + '\n');
}
function result(id, res) {
  send({ jsonrpc: '2.0', id, result: res });
}
function error(id, code, message) {
  send({ jsonrpc: '2.0', id, error: { code, message } });
}

const TOOL = {
  name: 'browser_request_takeover',
  description:
    'Ask the human to take control of the shared browser to complete a step you cannot (e.g. log in, solve a CAPTCHA, approve a dialog). BLOCKS until the human takes the wheel and hands control back, then returns so you can continue.',
  inputSchema: {
    type: 'object',
    properties: {
      reason: { type: 'string', description: 'Short reason shown to the human, e.g. "log in to continue".' },
    },
    required: ['reason'],
  },
};

async function requestTakeover(reason) {
  if (!CONTROL_URL) throw new Error('TANDEM_CONTROL_URL not set');
  const base = CONTROL_URL.replace(/\/$/, '');
  const q = `token=${encodeURIComponent(TOKEN)}&agentId=${encodeURIComponent(AGENT_ID)}`;
  const reg = await fetch(`${base}/internal/browser/takeover?${q}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ reason }),
  });
  if (!reg.ok) throw new Error(`takeover register failed (${reg.status})`);
  const { reqId } = await reg.json();
  // Poll until the human releases the wheel.
  for (;;) {
    await new Promise((r) => setTimeout(r, POLL_MS));
    const st = await fetch(`${base}/internal/browser/takeover?token=${encodeURIComponent(TOKEN)}&reqId=${encodeURIComponent(reqId)}`);
    if (!st.ok) continue;
    const body = await st.json();
    if (body.resolved) return body;
  }
}

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', async (line) => {
  const s = line.trim();
  if (!s) return;
  let msg;
  try {
    msg = JSON.parse(s);
  } catch {
    return;
  }
  const { id, method, params } = msg;
  try {
    switch (method) {
      case 'initialize':
        return result(id, {
          protocolVersion: params?.protocolVersion ?? '2025-06-18',
          capabilities: { tools: {} },
          serverInfo: { name: 'tandem-control', version: '0.1.0' },
        });
      case 'notifications/initialized':
        return; // notification, no response
      case 'ping':
        return result(id, {});
      case 'tools/list':
        return result(id, { tools: [TOOL] });
      case 'tools/call': {
        if (params?.name !== TOOL.name) return error(id, -32602, `unknown tool: ${params?.name}`);
        const reason = String(params?.arguments?.reason ?? 'the agent needs you');
        await requestTakeover(reason);
        return result(id, {
          content: [{ type: 'text', text: 'The human took the wheel, completed the step, and handed control back. You may continue.' }],
        });
      }
      default:
        if (id !== undefined) return error(id, -32601, `method not found: ${method}`);
    }
  } catch (e) {
    if (id !== undefined) error(id, -32603, e?.message ?? 'tandem-control error');
  }
});
