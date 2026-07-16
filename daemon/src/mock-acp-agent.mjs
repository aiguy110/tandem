// A stand-in ACP agent so the spine can be exercised without a real coding agent.
// Speaks newline-delimited JSON-RPC 2.0 over stdio, like an ACP agent subprocess,
// and — crucially for Phase 3 — is a faithful ACP *client-exerciser*: on cue it
// drives the daemon's fs/* and terminal/* services and asserts the round-trips.
//
// Default behaviour (unchanged, relied on by the Phase 1/2 derisk suites):
//   - steady heartbeat (agent_thought_chunk "tick #N") every 250ms;
//   - on session/prompt: message + plan + tool_call, then a permission request,
//     finishing once answered.
//
// Phase 3 scripted turns, selected by a token in the prompt text so the derisk
// harness can drive them (the agent runs in the worktree cwd, so it builds
// absolute paths from process.cwd()):
//   DERISK_SERVICES  fs write→read round-trip + a terminal (printf chunks, exit 0):
//                    poll terminal/output, wait_for_exit, release.
//   DERISK_ESCAPE    attempt fs/write to ../outside — expects a JSON-RPC error.
//   DERISK_SLOWTERM  a terminal that streams chunks over ~1.5s (reconnect test).
//   DERISK_BIGTERM   a terminal whose output exceeds a tiny outputByteLimit
//                    (truncation contract test).
//   DERISK_CANCEL    a running tool_call + permission request that never self-
//                    completes — resolved only by session/cancel.
//   DERISK_SERVICE_CANCEL  a pending terminal/wait_for_exit request that must
//                    be unblocked when the client cancels the turn.

import process from 'node:process';
import path from 'node:path';
import crypto from 'node:crypto';

process.stdin.on('end', () => process.exit(0));
process.stdout.on('error', (e) => {
  if (e && e.code === 'EPIPE') process.exit(0);
});

let sessionId = 'sess_mock';
let tick = 0;
let inbuf = '';
let pendingPromptId = null;
const PERM_REQ_ID = 1000; // high, to avoid colliding with the client's request ids

// ---- client-request plumbing (agent -> client JSON-RPC calls) ----
let nextCallId = 5000; // distinct id space from PERM_REQ_ID and the client's ids
const pendingCalls = new Map(); // id -> { resolve, reject }

function send(o) {
  process.stdout.write(JSON.stringify(o) + '\n');
}
function note(update) {
  send({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
}
// Call a client method (fs/*, terminal/*) and await its response.
function call(method, params) {
  const id = ++nextCallId;
  return new Promise((resolve, reject) => {
    pendingCalls.set(id, { resolve, reject });
    send({ jsonrpc: '2.0', id, method, params: { sessionId, ...params } });
  });
}
function finish(stopReason) {
  if (pendingPromptId != null) {
    send({ jsonrpc: '2.0', id: pendingPromptId, result: { stopReason } });
    pendingPromptId = null;
  }
}

// steady heartbeat — the agent keeps producing regardless of who's attached
setInterval(() => {
  tick++;
  note({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: `tick #${tick}` } });
}, 250);

process.stdin.on('data', (d) => {
  inbuf += d.toString('utf8');
  let i;
  while ((i = inbuf.indexOf('\n')) >= 0) {
    const line = inbuf.slice(0, i);
    inbuf = inbuf.slice(i + 1);
    if (line.trim()) handle(JSON.parse(line));
  }
});

function handle(msg) {
  // response to one of our client-calls (fs/terminal)
  if (msg.id != null && pendingCalls.has(msg.id) && (msg.result !== undefined || msg.error !== undefined)) {
    const p = pendingCalls.get(msg.id);
    pendingCalls.delete(msg.id);
    if (msg.error !== undefined) p.reject(Object.assign(new Error(msg.error.message || 'rpc error'), { rpcError: msg.error }));
    else p.resolve(msg.result);
    return;
  }

  if (msg.method === 'initialize') {
    // Advertised so the client knows what we might exercise (informational).
    send({
      jsonrpc: '2.0',
      id: msg.id,
      result: {
        protocolVersion: 1,
        agentCapabilities: {
          loadSession: true,
          sessionCapabilities: { list: true },
          promptCapabilities: { image: process.env.TANDEM_MOCK_IMAGE_CAPABILITY !== 'false' },
        },
      },
    });
    return;
  }
  if (msg.method === 'session/list') {
    send({
      jsonrpc: '2.0',
      id: msg.id,
      result: {
        sessions: [
          { sessionId: 'sess_mock', cwd: process.cwd(), title: 'Mock current session', updatedAt: '2026-07-15T12:00:00.000Z' },
          { sessionId: 'sess_external', cwd: process.cwd(), title: 'External mock session', updatedAt: '2026-07-15T13:00:00.000Z' },
        ],
      },
    });
    return;
  }
  if (msg.method === 'session/new') {
    send({ jsonrpc: '2.0', id: msg.id, result: {
      sessionId,
      modes: { currentModeId: 'ask', availableModes: [{ id: 'ask', name: 'Ask' }, { id: 'auto', name: 'Automatic' }] },
      configOptions: [
        { id: 'model', name: 'Model', category: 'model', type: 'select', currentValue: 'mock-large', options: [{ value: 'mock-large', name: 'Mock Large' }] },
        { id: 'thought_level', name: 'Effort', category: 'thought_level', type: 'select', currentValue: 'medium', options: [{ value: 'medium', name: 'Medium' }] },
      ],
    } });
    return;
  }
  if (msg.method === 'session/load') {
    if (msg.params?.sessionId) sessionId = msg.params.sessionId;
    process.stderr.write(`MOCK_LOADSESSION ${sessionId}\n`);
    send({ jsonrpc: '2.0', id: msg.id, result: { loaded: true, sessionId } });
    return;
  }
  // Cancellation (notification, no id): resolve the in-flight prompt as cancelled.
  // The client has already answered our permission request as cancelled.
  if (msg.method === 'session/cancel') {
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Cancelled.' } });
    finish('cancelled');
    return;
  }
  if (msg.method === 'session/prompt') {
    pendingPromptId = msg.id;
    const blocks = msg.params?.prompt ?? [];
    const text = blocks.map((b) => b?.text ?? '').join(' ');
    if (text.includes('DERISK_IMAGE')) return void runImagePrompt(blocks);
    if (text.includes('DERISK_UNKNOWN_UPDATE')) {
      note({ sessionUpdate: 'mock_future_optional_update', value: 1 });
      finish('end_turn');
      return;
    }
    if (text.includes('DERISK_MALFORMED_UPDATE')) {
      // Used by both runtime adapter harnesses to verify that an understood
      // variant missing required fields fails the session deterministically.
      note({ sessionUpdate: 'tool_call', status: 'pending' });
      return;
    }
    if (text.includes('DERISK_SERVICES')) return void runServices();
    if (text.includes('DERISK_ESCAPE')) return void runEscape();
    if (text.includes('DERISK_SLOWTERM')) return void runSlowTerm();
    if (text.includes('DERISK_BIGTERM')) return void runBigTerm();
    if (text.includes('DERISK_SERVICE_CANCEL')) return void runServiceCancel();
    if (text.includes('DERISK_CANCEL')) return void runCancel();
    return runDefault();
  }
  // the client's response to our permission request (default + cancel turns)
  if (msg.id === PERM_REQ_ID && msg.result) {
    const outcome = msg.result?.outcome?.outcome;
    if (outcome === 'cancelled') return; // session/cancel will finish the prompt
    const opt = msg.result?.outcome?.optionId;
    if (opt === 'allow') {
      note({ sessionUpdate: 'tool_call_update', toolCallId: 'tc1', status: 'completed' });
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Done — deps reinstalled.' } });
    } else {
      note({ sessionUpdate: 'tool_call_update', toolCallId: 'tc1', status: 'failed' });
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Skipped install.' } });
    }
    finish('end_turn');
  }
}

// Echo a compact, deterministic description of the exact ACP content blocks.
// The image-upload derisk suite uses this to prove that Tandem preserved mixed
// text/image ordering and delivered the original bytes at the ACP boundary.
function runImagePrompt(blocks) {
  const description = blocks.map((block) => {
    if (block?.type === 'text') return `text:${block.text}`;
    if (block?.type === 'image') {
      const bytes = Buffer.from(block.data ?? '', 'base64');
      const digest = crypto.createHash('sha256').update(bytes).digest('hex').slice(0, 16);
      return `image:${block.mimeType}:${bytes.length}:${digest}`;
    }
    return `unknown:${block?.type ?? 'missing'}`;
  }).join('|');
  note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `ACP_PROMPT_BLOCKS ${description}` } });
  finish('end_turn');
}

// ---- default turn (Phase 1/2 approval flow, unchanged) ----
function runDefault() {
  note({ sessionUpdate: 'usage_update', used: 12300, size: 1000000, cost: { amount: 0.045, currency: 'USD' } });
  note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Working on it. ' } });
  note({
    sessionUpdate: 'plan',
    entries: [
      { content: 'Reinstall dependencies', priority: 'high', status: 'in_progress' },
      { content: 'Run tests', priority: 'medium', status: 'pending' },
    ],
  });
  note({ sessionUpdate: 'tool_call', toolCallId: 'tc1', title: 'terminal: rm -rf node_modules && npm ci', status: 'pending' });
  send({
    jsonrpc: '2.0',
    id: PERM_REQ_ID,
    method: 'session/request_permission',
    params: {
      sessionId,
      toolCall: { toolCallId: 'tc1', title: 'rm -rf node_modules && npm ci' },
      options: [
        { optionId: 'allow', name: 'Allow', kind: 'allow_once' },
        { optionId: 'reject', name: 'Reject', kind: 'reject_once' },
      ],
    },
  });
}

// ---- DERISK_SERVICES: fs round-trip + terminal lifecycle ----
async function runServices() {
  try {
    const file = path.join(process.cwd(), 'tandem-roundtrip.txt');
    const body = 'hello from the agent\nline two\n';
    await call('fs/write_text_file', { path: file, content: body });
    const read = await call('fs/read_text_file', { path: file });
    const roundtrip = read?.content === body;
    note({
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: roundtrip ? 'FS_ROUNDTRIP_OK ' : 'FS_ROUNDTRIP_MISMATCH ' },
    });

    // A short terminal command that prints a few chunks and exits 0.
    const { terminalId } = await call('terminal/create', {
      command: 'sh',
      args: ['-c', 'printf "chunk-a\\n"; printf "chunk-b\\n"; printf "chunk-c\\n"; exit 0'],
    });
    note({ sessionUpdate: 'tool_call', toolCallId: terminalId, title: 'terminal: printf chunks', status: 'in_progress' });
    const exit = await call('terminal/wait_for_exit', { terminalId });
    const out = await call('terminal/output', { terminalId });
    note({
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: `TERM_EXIT_${exit?.exitCode} OUTLEN_${(out?.output ?? '').length} ` },
    });
    note({ sessionUpdate: 'tool_call_update', toolCallId: terminalId, status: 'completed' });
    await call('terminal/release', { terminalId });
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'SERVICES_DONE' } });
    finish('end_turn');
  } catch (e) {
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `SERVICES_ERROR ${e.message}` } });
    finish('end_turn');
  }
}

// ---- DERISK_ESCAPE: a path-traversal write must be rejected ----
async function runEscape() {
  const escape = path.join(process.cwd(), '..', 'tandem-escape.txt');
  try {
    await call('fs/write_text_file', { path: escape, content: 'should never land' });
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'ESCAPE_UNEXPECTEDLY_ALLOWED' } });
  } catch (e) {
    const code = e.rpcError?.code;
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `ESCAPE_REJECTED code=${code}` } });
  }
  finish('end_turn');
}

// ---- DERISK_SLOWTERM: stream chunks over time (reconnect test) ----
async function runSlowTerm() {
  try {
    const { terminalId } = await call('terminal/create', {
      command: 'sh',
      args: ['-c', 'for i in 1 2 3 4 5 6; do printf "slow-%s\\n" "$i"; sleep 0.2; done; exit 0'],
    });
    note({ sessionUpdate: 'tool_call', toolCallId: terminalId, title: 'terminal: slow stream', status: 'in_progress' });
    const exit = await call('terminal/wait_for_exit', { terminalId });
    note({ sessionUpdate: 'tool_call_update', toolCallId: terminalId, status: 'completed' });
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `SLOWTERM_DONE_${exit?.exitCode}` } });
    finish('end_turn');
  } catch (e) {
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `SLOWTERM_ERROR ${e.message}` } });
    finish('end_turn');
  }
}

// ---- DERISK_BIGTERM: output exceeds a tiny outputByteLimit ----
async function runBigTerm() {
  try {
    const { terminalId } = await call('terminal/create', {
      // ~2600 bytes of output, far over the 64-byte ACP limit below.
      command: 'sh',
      args: ['-c', 'i=0; while [ $i -lt 100 ]; do printf "0123456789ABCDEF0123456\\n"; i=$((i+1)); done; exit 0'],
      outputByteLimit: 64,
    });
    note({ sessionUpdate: 'tool_call', toolCallId: terminalId, title: 'terminal: big output', status: 'in_progress' });
    await call('terminal/wait_for_exit', { terminalId });
    const out = await call('terminal/output', { terminalId });
    note({
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: `BIGTERM id=${terminalId} acpLen=${(out?.output ?? '').length} truncated=${out?.truncated}` },
    });
    note({ sessionUpdate: 'tool_call_update', toolCallId: terminalId, status: 'completed' });
    finish('end_turn');
  } catch (e) {
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `BIGTERM_ERROR ${e.message}` } });
    finish('end_turn');
  }
}

// ---- DERISK_SERVICE_CANCEL: cancellation unblocks an in-flight client call ----
async function runServiceCancel() {
  try {
    const { terminalId } = await call('terminal/create', {
      command: 'sh', args: ['-c', 'sleep 60'],
    });
    note({ sessionUpdate: 'tool_call', toolCallId: terminalId, title: 'terminal: pending wait', status: 'in_progress' });
    try {
      await call('terminal/wait_for_exit', { terminalId });
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'SERVICE_WAIT_UNEXPECTEDLY_COMPLETED' } });
    } catch (e) {
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `SERVICE_WAIT_CANCELLED code=${e.rpcError?.code}` } });
    } finally {
      await call('terminal/release', { terminalId });
    }
  } catch (e) {
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `SERVICE_CANCEL_ERROR ${e.message}` } });
  }
}

// ---- DERISK_CANCEL: a turn that only ends via session/cancel ----
function runCancel() {
  note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Starting a long operation. ' } });
  note({ sessionUpdate: 'tool_call', toolCallId: 'tc_cancel', title: 'terminal: long running', status: 'in_progress' });
  send({
    jsonrpc: '2.0',
    id: PERM_REQ_ID,
    method: 'session/request_permission',
    params: {
      sessionId,
      toolCall: { toolCallId: 'tc_cancel', title: 'rm -rf / (needs approval)' },
      options: [
        { optionId: 'allow', name: 'Allow', kind: 'allow_once' },
        { optionId: 'reject', name: 'Reject', kind: 'reject_once' },
      ],
    },
  });
  // Intentionally never self-finishes: the harness interrupts, the client answers
  // the permission 'cancelled', and session/cancel resolves the prompt.
}
