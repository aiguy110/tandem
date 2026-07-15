// Browser <-> daemon HTTP + WebSocket, on one port (D15). Implements the full
// ClientMsg/ServerMsg envelope from docs/ws-protocol.md across MANY agents and
// MANY concurrent clients. The daemon has already translated ACP away, so only
// normalized shapes cross this boundary.
//
// Auth (D15): a WS connection must present the bearer token as `?token=<t>`.
// Unauthenticated sockets are accepted then immediately closed with code 4401.
// (base64-in-JSON framing for raw_pty is kept from the PoC; real binary framing
// is a future optimization — noted in docs/ws-protocol.md.)

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { WebSocketServer, WebSocket } from 'ws';
import type { AgentRegistry } from './registry.ts';
import type { AgentSession } from './session.ts';
import type { BrowserBroker } from './browser/broker.ts';
import type { AgentEvent, Channel, ClientMsg, ServerMsg, WireEvent } from './types.ts';
import { AssetTooLargeError, MAX_ASSET_BYTES, UnsupportedAssetError } from './assetStore.ts';

const ALL_CHANNELS: Channel[] = ['transcript', 'pty', 'terminals', 'browser', 'status'];

function channelOf(ev: AgentEvent): Channel {
  switch (ev.kind) {
    case 'raw_pty':
      return 'pty';
    case 'terminal_output':
      return 'terminals';
    case 'status':
      return 'status';
    // takeover_request rides the always-on transcript channel (like
    // permission_request) so the attention rail surfaces it even when the Browser
    // pane isn't open. The Browser-pane banner reads the same store state.
    default:
      return 'transcript';
  }
}

// raw_pty carries bytes; JSON can't, so base64 it on the wire.
function serialize(ev: AgentEvent): WireEvent {
  if (ev.kind === 'raw_pty') return { kind: 'raw_pty', dataB64: Buffer.from(ev.data).toString('base64') };
  return ev;
}

interface Sub {
  channels: Set<Channel>;
  unsub: () => void;
}

export interface Server {
  http: http.Server;
  close(): Promise<void>;
}

export function startServer(
  registry: AgentRegistry,
  opts: { host: string; port: number; token: string; uiDir?: string; bootstrapUrl: string; broker?: BrowserBroker; onShutdownRequested?: () => void },
): Promise<Server> {
  const broker = opts.broker;
  // ---- one connection's per-agent subscriptions ----
  const connections = new Set<Conn>();

  // Pending agent-initiated takeovers, keyed by reqId (the control MCP polls the
  // internal HTTP endpoint below). A takeover resolves when the user releases the
  // wheel (broker.release resolves broker.requestTakeover).
  const takeovers = new Map<string, { agentId: string; resolved: boolean }>();
  let takeoverSeq = 0;
  let shutdownRequested = false;
  let shutdownPoll: NodeJS.Timeout | undefined;

  const requestShutdownWhenIdle = (): { activeTurns: number; alreadyRequested: boolean } => {
    const alreadyRequested = shutdownRequested;
    shutdownRequested = true;
    const activeTurns = registry.activeTurnCount();
    if (!shutdownPoll) {
      shutdownPoll = setInterval(() => {
        if (registry.activeTurnCount() !== 0) return;
        clearInterval(shutdownPoll);
        shutdownPoll = undefined;
        opts.onShutdownRequested?.();
      }, 250);
    }
    return { activeTurns, alreadyRequested };
  };

  class Conn {
    subs = new Map<string, Sub>();
    constructor(readonly ws: WebSocket) {}
    send(m: ServerMsg): void {
      if (this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
    }
    dispose(): void {
      for (const s of this.subs.values()) s.unsub();
      this.subs.clear();
    }
  }

  const httpServer = http.createServer((req, res) => {
    const url = new URL(req.url ?? '/', 'http://localhost');
    if (url.pathname.startsWith('/api/agents/')) {
      handleAsset(req, res, url).catch((e) => {
        if (!res.headersSent) res.writeHead(500, { 'content-type': 'application/json' }).end(JSON.stringify({ error: (e as Error).message }));
      });
      return;
    }
    if (url.pathname.startsWith('/internal/')) {
      handleInternal(req, res, url).catch((e) => {
        res.writeHead(500, { 'content-type': 'text/plain' }).end((e as Error).message);
      });
      return;
    }
    serveStatic(req, res, opts);
  });

  async function handleAsset(req: http.IncomingMessage, res: http.ServerResponse, url: URL): Promise<void> {
    const auth = req.headers.authorization;
    if (auth !== `Bearer ${opts.token}`) {
      res.writeHead(401, { 'content-type': 'application/json' }).end(JSON.stringify({ error: 'unauthorized' }));
      return;
    }
    const match = /^\/api\/agents\/([^/]+)\/assets(?:\/([a-f0-9]{64}))?$/.exec(url.pathname);
    if (!match) return void res.writeHead(404).end('not found');
    const agentId = decodeURIComponent(match[1]);
    const assetId = match[2];
    if (!registry.get(agentId)) return void res.writeHead(404).end('not found');

    if (req.method === 'POST' && !assetId) {
      let body: Buffer;
      try {
        body = await readBuffer(req, MAX_ASSET_BYTES);
      } catch (error) {
        const status = error instanceof AssetTooLargeError ? 413 : 400;
        res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify({ error: (error as Error).message }));
        return;
      }
      let rawName = Array.isArray(req.headers['x-file-name']) ? req.headers['x-file-name'][0] : req.headers['x-file-name'];
      try {
        if (rawName) rawName = decodeURIComponent(rawName);
      } catch {
        // Keep the literal header when it is not valid percent encoding.
      }
      const name = (rawName || 'image').replace(/[\x00-\x1f\x7f/\\]/g, '_').slice(0, 255);
      try {
        const stored = registry.assets.put(agentId, body, req.headers['content-type']);
        res.writeHead(201, { 'content-type': 'application/json' }).end(JSON.stringify({ asset: { ...stored, name } }));
      } catch (error) {
        const status = error instanceof AssetTooLargeError ? 413 : error instanceof UnsupportedAssetError ? 415 : 500;
        res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify({ error: (error as Error).message }));
      }
      return;
    }

    if (req.method === 'GET' && assetId) {
      try {
        const asset = registry.assets.get(agentId, assetId);
        res.writeHead(200, {
          'content-type': asset.mimeType,
          'content-length': asset.size,
          'cache-control': 'private, max-age=31536000, immutable',
          'x-content-type-options': 'nosniff',
        }).end(asset.data);
      } catch {
        res.writeHead(404).end('not found');
      }
      return;
    }
    res.writeHead(404).end('not found');
  }

  // Internal HTTP surface for daemon-spawned helper processes (the Tandem-control
  // MCP). Authed with the same bearer token; localhost-only via the daemon bind.
  async function handleInternal(req: http.IncomingMessage, res: http.ServerResponse, url: URL): Promise<void> {
    if (url.searchParams.get('token') !== opts.token) {
      res.writeHead(401).end('unauthorized');
      return;
    }
    const json = (code: number, body: unknown): void => {
      res.writeHead(code, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    };

    if (url.pathname === '/internal/shutdown-after-turns' && req.method === 'POST') {
      const result = requestShutdownWhenIdle();
      res.writeHead(200, { 'content-type': 'text/plain' }).end(`${result.activeTurns} ${result.alreadyRequested ? 1 : 0}\n`);
      return;
    }

    if (url.pathname === '/internal/browser/takeover' && req.method === 'POST') {
      const agentId = url.searchParams.get('agentId') ?? '';
      const session = registry.get(agentId);
      if (!session || !broker) return json(404, { error: 'no such agent or browser disabled' });
      const body = await readBody(req);
      const reason = (safeJson(body)?.reason as string) ?? 'the agent needs you';
      const reqId = `tk_${++takeoverSeq}`;
      takeovers.set(reqId, { agentId, resolved: false });
      // Normalized event: raises the attention item on the browser channel and
      // flips the agent to blocked.
      session.pushEvent({ kind: 'takeover_request', reqId, reason });
      session.pushEvent({ kind: 'status', status: 'blocked' });
      // Block (async) until the user hands the wheel back, then unblock the agent.
      void broker.requestTakeover(agentId).then(() => {
        const t = takeovers.get(reqId);
        if (t) t.resolved = true;
        session.pushEvent({ kind: 'status', status: 'working' });
      });
      return json(200, { reqId });
    }

    // DEV-ONLY: force-provision + navigate an agent's browser (UI e2e helper).
    // Gated by TANDEM_DEV_BROWSER=1 so it never exists in a normal daemon.
    if (url.pathname === '/internal/browser/devnav' && req.method === 'POST' && process.env.TANDEM_DEV_BROWSER === '1') {
      const agentId = url.searchParams.get('agentId') ?? '';
      if (!registry.get(agentId) || !broker) return json(404, { error: 'no such agent or browser disabled' });
      const navUrl = (safeJson(await readBody(req))?.url as string) ?? 'about:blank';
      await broker.devNavigate(agentId, navUrl);
      return json(200, { ok: true });
    }

    if (url.pathname === '/internal/browser/takeover' && req.method === 'GET') {
      const reqId = url.searchParams.get('reqId') ?? '';
      const t = takeovers.get(reqId);
      if (!t) return json(404, { error: 'no such takeover' });
      return json(200, { resolved: t.resolved });
    }

    res.writeHead(404).end('not found');
  }

  const wss = new WebSocketServer({ server: httpServer });
  wss.on('connection', (ws, req) => {
    // ---- auth gate (D15) ----
    const url = new URL(req.url ?? '/', 'http://localhost');
    if (url.searchParams.get('token') !== opts.token) {
      ws.close(4401, 'unauthorized');
      return;
    }

    const conn = new Conn(ws);
    connections.add(conn);

    ws.on('message', (raw) => {
      let m: ClientMsg;
      try {
        m = JSON.parse(raw.toString());
      } catch {
        return;
      }
      handle(conn, m).catch((e) => conn.send({ t: 'ack', corrId: (m as any).corrId, error: (e as Error).message }));
    });

    ws.on('close', () => {
      conn.dispose();
      connections.delete(conn);
    });
  });

  async function handle(conn: Conn, m: ClientMsg): Promise<void> {
    switch (m.t) {
      case 'subscribe': {
        const session = registry.get(m.agentId);
        if (!session) return conn.send({ t: 'ack', corrId: m.corrId, error: `no such agent: ${m.agentId}` });
        subscribe(conn, session, m.channels, m.sinceSeq ?? 0);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      case 'unsubscribe': {
        const s = conn.subs.get(m.agentId);
        if (s) {
          s.unsub();
          conn.subs.delete(m.agentId);
        }
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      case 'prompt':
        requireSession(m.agentId).prompt(m.blocks ?? m.text ?? '').catch(() => {});
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'input':
        requireSession(m.agentId).sendInput(Buffer.from(m.bytesB64 ?? '', 'base64'));
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'resize':
        requireSession(m.agentId).resize(m.cols, m.rows);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'permission_response':
        requireSession(m.agentId).respondPermission(m.reqId, m.optionId);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'interrupt':
        requireSession(m.agentId).interrupt();
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'set_mode':
        await requireSession(m.agentId).setMode(m.modeId);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'set_config_option':
        await requireSession(m.agentId).setConfigOption(m.configId, m.value);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      case 'spawn_agent': {
        const session = await registry.spawn(m.spec);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: session.id });
        break;
      }
      case 'get_spawn_options': {
        try {
          const options = await registry.spawnOptions(m.agent, m.cwd);
          conn.send({ t: 'spawn_options', corrId: m.corrId, options });
        } catch (error) {
          conn.send({ t: 'spawn_options', corrId: m.corrId, error: (error as Error).message });
        }
        break;
      }
      case 'get_close_preview': {
        try {
          const preview = await registry.closePreview(m.agentId);
          conn.send({ t: 'close_preview', corrId: m.corrId, preview, error: preview ? undefined : 'no such agent' });
        } catch (error) {
          conn.send({ t: 'close_preview', corrId: m.corrId, error: (error as Error).message });
        }
        break;
      }
      case 'close_agent': {
        let ok = false;
        try {
          ok = await registry.close(m.agentId, m.force, m.deleteWorktree);
        } catch (e) {
          // Teardown refused (e.g. dirty_worktree) — the agent is untouched
          // and still running; surface the structured reason, don't broadcast
          // agent_closed.
          conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId, error: (e as Error).message });
          break;
        }
        // Tell every subscriber (across all connections) the agent is gone, and
        // drop their subscriptions — teardown affects only the target agent.
        for (const c of connections) {
          const s = c.subs.get(m.agentId);
          if (s) {
            s.unsub();
            c.subs.delete(m.agentId);
            c.send({ t: 'agent_closed', agentId: m.agentId });
          }
        }
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId, error: ok ? undefined : 'no such agent' });
        break;
      }
      case 'list_dirs': {
        const dirs = await registry.listDirs();
        conn.send({ t: 'dirs', corrId: m.corrId, dirs });
        break;
      }
      case 'list_agents': {
        conn.send({ t: 'agents', corrId: m.corrId, agents: await registry.summaries() });
        break;
      }
      case 'list_sessions': {
        conn.send({ t: 'sessions', corrId: m.corrId, catalog: await registry.resumeCatalog() });
        break;
      }
      case 'resume_session': {
        const session = await registry.resume(m.sessionId, { agent: m.agent, cwd: m.cwd });
        // A resumed Tandem agent may be new to some clients — let every connection
        // refresh its rail, then ack the initiator with the (possibly new) agentId.
        const agents = await registry.summaries();
        for (const c of connections) c.send({ t: 'agents', agents });
        conn.send({ t: 'ack', corrId: m.corrId, agentId: session.id });
        break;
      }
      case 'enter_terminal': {
        await registry.enterTerminal(m.agentId, !!m.interrupt);
        const agents = await registry.summaries();
        for (const c of connections) c.send({ t: 'agents', agents });
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      case 'leave_terminal': {
        await registry.leaveTerminal(m.agentId);
        const agents = await registry.summaries();
        for (const c of connections) c.send({ t: 'agents', agents });
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      case 'browser_control': {
        if (!broker) return conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId, error: 'browser subsystem disabled' });
        if (!registry.get(m.agentId)) return conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId, error: `no such agent: ${m.agentId}` });
        if (m.action === 'grab') broker.grab(m.agentId);
        else broker.release(m.agentId); // also resolves any blocked takeover_request
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      case 'browser_input': {
        // Forwarded to the page ONLY while the user owns the wheel (view-only otherwise).
        if (broker && broker.controlOwner(m.agentId) === 'user') void broker.dispatchUserInput(m.agentId, m.event);
        conn.send({ t: 'ack', corrId: m.corrId, agentId: m.agentId });
        break;
      }
      // Stub for Phase 3 — honest error ack rather than a silent no-op.
      case 'merge_back':
        conn.send({ t: 'ack', corrId: m.corrId, agentId: (m as any).agentId, error: `${m.t} not implemented yet` });
        break;
    }
  }

  function requireSession(agentId: string): AgentSession {
    const s = registry.get(agentId);
    if (!s) throw new Error(`no such agent: ${agentId}`);
    return s;
  }

  function subscribe(conn: Conn, session: AgentSession, channels: Channel[] | undefined, sinceSeq: number): void {
    const chans = new Set<Channel>(channels?.length ? channels : ALL_CHANNELS);
    const wants = (ev: AgentEvent) => chans.has(channelOf(ev));

    // Replace any prior subscription to this agent on this connection.
    conn.subs.get(session.id)?.unsub();

    const log = session.log;
    if (sinceSeq > 0 && !log.hasGap(sinceSeq) && sinceSeq <= log.head) {
      // Gapless replay from the client's checkpoint (ring or SQLite backstop).
      for (const le of log.since(sinceSeq)) if (wants(le.event)) conn.send({ t: 'event', agentId: session.id, seq: le.seq, event: serialize(le.event) });
    } else {
      // Fresh (or gap too large): full snapshot reconstructed from the persisted
      // log — this is how a restored agent's pre-restart history reaches a client.
      const transcript = session.log
        .fullHistory()
        .filter((le) => wants(le.event))
        .map((le) => ({ seq: le.seq, event: serialize(le.event) }));
      conn.send({ t: 'snapshot', agentId: session.id, seq: log.head, transcript, status: session.status, controlMode: session.controlMode, pendingApprovals: session.pendingApprovals() });
    }

    const eventUnsub = session.onEvent((le) => {
      if (wants(le.event)) conn.send({ t: 'event', agentId: session.id, seq: le.seq, event: serialize(le.event) });
    });

    // Browser channel: stream screencast frames + control-owner state for THIS
    // agent (the focus rule: only a browser-subscribed, i.e. focused, client
    // streams). Subscribing does NOT provision — the browser stays cold until the
    // agent's first use; `active:false` state is sent until then.
    let browserUnsub: () => void = () => {};
    if (broker && chans.has('browser')) {
      const st = broker.state(session.id);
      conn.send({ t: 'browser_state', agentId: session.id, active: st.active, controlOwner: st.controlOwner });
      const offFrame = broker.addFrameListener(session.id, (f) => conn.send({ t: 'browser_frame', agentId: session.id, dataB64: f.dataB64, meta: f.meta }));
      const stateHandler = (s: { active: boolean; controlOwner: 'agent' | 'user' }) => conn.send({ t: 'browser_state', agentId: session.id, active: s.active, controlOwner: s.controlOwner });
      broker.on(`state:${session.id}`, stateHandler);
      browserUnsub = () => {
        offFrame();
        broker.off(`state:${session.id}`, stateHandler);
      };
    }

    const unsub = () => {
      eventUnsub();
      browserUnsub();
    };
    conn.subs.set(session.id, { channels: chans, unsub });
  }

  // Poll worktree git state (dirty/unmerged/synced) for the rail's status dot —
  // it changes from outside the event stream (the agent committing, the user
  // merging elsewhere), so nothing else would tell connected clients to refresh.
  // Skipped entirely with no connections, since it's a `status --porcelain`
  // (+ maybe `rev-list`) shell-out per live agent.
  const gitStatePoll = setInterval(() => {
    if (connections.size === 0) return;
    registry
      .summaries()
      .then((agents) => {
        for (const c of connections) c.send({ t: 'agents', agents });
      })
      .catch(() => {});
  }, 5000);

  return new Promise((resolve) => {
    httpServer.listen(opts.port, opts.host, () => {
      resolve({
        http: httpServer,
        close: () =>
          new Promise<void>((res) => {
            clearInterval(gitStatePoll);
            if (shutdownPoll) clearInterval(shutdownPoll);
            for (const c of connections) c.ws.terminate();
            wss.close(() => httpServer.close(() => res()));
          }),
      });
    });
  });
}

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve) => {
    let buf = '';
    req.on('data', (c) => (buf += c));
    req.on('end', () => resolve(buf));
    req.on('error', () => resolve(buf));
  });
}
function readBuffer(req: http.IncomingMessage, limit: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const declared = Number(req.headers['content-length'] ?? 0);
    if (declared > limit) {
      req.resume();
      reject(new AssetTooLargeError(`image exceeds ${limit} byte limit`));
      return;
    }
    const chunks: Buffer[] = [];
    let size = 0;
    let done = false;
    req.on('data', (chunk: Buffer) => {
      if (done) return;
      size += chunk.length;
      if (size > limit) {
        done = true;
        reject(new AssetTooLargeError(`image exceeds ${limit} byte limit`));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (!done) resolve(Buffer.concat(chunks, size));
    });
    req.on('error', (error) => {
      if (!done) reject(error);
    });
  });
}
function safeJson(s: string): Record<string, unknown> | undefined {
  try {
    return JSON.parse(s);
  } catch {
    return undefined;
  }
}

// ---- static file serving (D15): UI dist if present, else a placeholder ----
function serveStatic(req: http.IncomingMessage, res: http.ServerResponse, opts: { uiDir?: string; bootstrapUrl: string }): void {
  const reqPath = new URL(req.url ?? '/', 'http://localhost').pathname;
  if (opts.uiDir) {
    const rel = reqPath === '/' ? 'index.html' : reqPath.replace(/^\/+/, '');
    const root = path.resolve(opts.uiDir);
    const file = path.resolve(root, rel);
    // Contain within uiDir (no path traversal).
    if ((file === root || file.startsWith(root + path.sep)) && fs.existsSync(file) && fs.statSync(file).isFile()) {
      // There is no production build mode yet (D15 always serves a freshly
      // built dist via start-dev-server.sh/redeploy.sh) — always no-cache so a
      // redeploy is reflected on next load instead of a stale cached bundle.
      res.writeHead(200, { 'content-type': contentType(file), 'cache-control': 'no-cache, no-store, must-revalidate' });
      fs.createReadStream(file).pipe(res);
      return;
    }
  }
  res.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-cache, no-store, must-revalidate' });
  res.end(placeholderPage(opts.bootstrapUrl, !!opts.uiDir));
}

function contentType(file: string): string {
  const ext = path.extname(file).toLowerCase();
  return (
    {
      '.html': 'text/html; charset=utf-8',
      '.js': 'text/javascript',
      '.mjs': 'text/javascript',
      '.css': 'text/css',
      '.json': 'application/json',
      '.svg': 'image/svg+xml',
      '.wasm': 'application/wasm', // ghostty-web's libghostty-vt.wasm — must not be octet-stream
      '.woff2': 'font/woff2',
      '.woff': 'font/woff',
      '.ttf': 'font/ttf',
      '.png': 'image/png',
      '.ico': 'image/x-icon',
    }[ext] ?? 'application/octet-stream'
  );
}

function placeholderPage(bootstrapUrl: string, uiConfigured: boolean): string {
  return `<!doctype html><meta charset="utf-8"><title>Tandem daemon</title>
<style>body{font:14px/1.6 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}code{background:#f2f2f2;padding:.1em .3em;border-radius:3px}</style>
<h1>Tandem daemon</h1>
<p>The daemon is running. ${uiConfigured ? 'No <code>index.html</code> was found in the configured UI dir.' : 'No UI is built yet (set <code>TANDEM_UI_DIR</code> to serve one).'}</p>
<p>Connect a client to the WebSocket on this same port, presenting the bearer token:</p>
<p><code>ws://&lt;host&gt;/?token=&lt;token&gt;</code></p>
<p>Bootstrap URL (token in the URL fragment, never sent to the server):<br><code>${bootstrapUrl}</code></p>`;
}
