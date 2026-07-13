// A stand-in ACP agent so the spine can be exercised without a real coding agent.
// Speaks newline-delimited JSON-RPC 2.0 over stdio, like an ACP agent subprocess.
//
// Behaviour:
//   - emits a steady heartbeat (agent_thought_chunk "tick #N") every 250ms, so a
//     client can disconnect/reconnect and we can prove no events were lost.
//   - on session/prompt: streams a message + plan + tool_call, then asks permission
//     (a JSON-RPC *request* from agent -> client) and finishes once answered.

import process from 'node:process';

const sessionId = 'sess_mock';
let tick = 0;
let inbuf = '';
let pendingPromptId = null;
const PERM_REQ_ID = 1000; // high, to avoid colliding with the client's request ids

function send(o) {
  process.stdout.write(JSON.stringify(o) + '\n');
}
function note(update) {
  send({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
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
  if (msg.method === 'initialize') {
    send({ jsonrpc: '2.0', id: msg.id, result: { protocolVersion: 1, agentCapabilities: { loadSession: true } } });
    return;
  }
  if (msg.method === 'session/new') {
    send({ jsonrpc: '2.0', id: msg.id, result: { sessionId } });
    return;
  }
  if (msg.method === 'session/prompt') {
    pendingPromptId = msg.id;
    note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Working on it. ' } });
    note({
      sessionUpdate: 'plan',
      entries: [
        { content: 'Reinstall dependencies', status: 'in_progress' },
        { content: 'Run tests', status: 'pending' },
      ],
    });
    note({ sessionUpdate: 'tool_call', toolCallId: 'tc1', title: 'terminal: rm -rf node_modules && npm ci', status: 'pending' });
    // ask the client for permission (agent-initiated JSON-RPC request)
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
    return;
  }
  // the client's response to our permission request
  if (msg.id === PERM_REQ_ID && msg.result) {
    const opt = msg.result?.outcome?.optionId;
    if (opt === 'allow') {
      note({ sessionUpdate: 'tool_call_update', toolCallId: 'tc1', status: 'done' });
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Done — deps reinstalled.' } });
    } else {
      note({ sessionUpdate: 'tool_call_update', toolCallId: 'tc1', status: 'error' });
      note({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Skipped install.' } });
    }
    if (pendingPromptId != null) {
      send({ jsonrpc: '2.0', id: pendingPromptId, result: { stopReason: 'end_turn' } });
      pendingPromptId = null;
    }
  }
}
