// Browser broker (docs/browser.md, D13). Per-agent LAZY provisioning: nothing
// spins up until first browser use. The broker serves a stable per-agent CDP
// endpoint URL; Playwright MCP is pointed at it with --cdp-endpoint. On the first
// CDP connection the broker provisions via the driver and proxies CDP thereafter.
//
// The control token's HARD-PAUSE is enforced AT THE CDP PROXY LAYER: while
// controlOwner=user, CDP command frames from the agent's connection are QUEUED
// (not forwarded, not errored), so the agent's Playwright call simply awaits its
// response — an async hold, no agent cooperation. On release the queue flushes in
// order. The daemon's own screencast/input runs on a SEPARATE CDP connection
// (SharedBrowser) straight to the browser, so it is never gated.
//
// Design choice (documented in browser.md): a transparent CDP WebSocket proxy —
// rather than fronting the MCP process — because Playwright MCP talks CDP
// directly, so this is the only layer that both preserves laziness (provision on
// first connect) AND can hold the agent's actions without its cooperation.

import http from 'node:http';
import { WebSocketServer, WebSocket, type RawData } from 'ws';
import { EventEmitter } from 'node:events';
import { SharedBrowser, type BrowserInputEvent, type ScreencastFrame } from './sharedBrowser.ts';
import type { BrowserDriver } from './driver.ts';

export type ControlOwner = 'agent' | 'user';

export interface BrowserStateSnapshot {
  active: boolean;
  controlOwner: ControlOwner;
}

interface AgentBrowser {
  agentId: string;
  provisioned: boolean;
  provisioning?: Promise<void>;
  realCdpHttp?: string; // http://host:port CDP endpoint from the driver
  realBrowserWsUrl?: string; // browser-level webSocketDebuggerUrl (rewritten to clients)
  shared?: SharedBrowser;
  controlOwner: ControlOwner;
  // Live agent-side CDP proxy connections (usually one — the MCP).
  proxies: Set<ProxyLink>;
  frameListeners: number;
  takeoverWaiters: Array<() => void>;
}

interface HeldFrame {
  data: RawData;
  binary: boolean;
}

interface ProxyLink {
  agentSock: WebSocket;
  upstream?: WebSocket;
  // Frames from agent held while owner=user (awaiting release), in order.
  held: HeldFrame[];
}

export class BrowserBroker extends EventEmitter {
  private server: http.Server;
  private wss: WebSocketServer;
  private agents = new Map<string, AgentBrowser>();
  private port = 0;

  constructor(private driver: BrowserDriver, private opts: { host?: string } = {}) {
    super();
    this.server = http.createServer((req, res) => this.onHttp(req, res));
    this.wss = new WebSocketServer({ noServer: true });
    this.server.on('upgrade', (req, socket, head) => this.onUpgrade(req, socket, head));
  }

  /** Start the broker's internal HTTP/WS server on an ephemeral localhost port. */
  start(): Promise<void> {
    return new Promise((resolve) => {
      this.server.listen(0, this.opts.host ?? '127.0.0.1', () => {
        const addr = this.server.address();
        this.port = typeof addr === 'object' && addr ? addr.port : 0;
        resolve();
      });
    });
  }

  /** The stable per-agent CDP endpoint URL handed to Playwright MCP. Calling this
   *  does NOT provision — the browser stays cold until the first CDP hit. */
  endpointFor(agentId: string): string {
    return `http://127.0.0.1:${this.port}/cdp/${encodeURIComponent(agentId)}`;
  }

  driverKind(): string {
    return this.driver.kind;
  }
  isProvisioned(agentId: string): boolean {
    return this.driver.isProvisioned(agentId);
  }
  browserPid(agentId: string): number | undefined {
    return this.driver.pid(agentId);
  }

  private rec(agentId: string): AgentBrowser {
    let a = this.agents.get(agentId);
    if (!a) {
      a = { agentId, provisioned: false, controlOwner: 'agent', proxies: new Set(), frameListeners: 0, takeoverWaiters: [] };
      this.agents.set(agentId, a);
    }
    return a;
  }

  state(agentId: string): BrowserStateSnapshot {
    const a = this.agents.get(agentId);
    return { active: !!a?.provisioned, controlOwner: a?.controlOwner ?? 'agent' };
  }

  private emitState(agentId: string): void {
    this.emit(`state:${agentId}`, this.state(agentId));
  }

  // ---- lazy provisioning ----------------------------------------------------

  /** Provision on demand (idempotent). Used by the CDP proxy on first connect and
   *  by tests that stand in for the agent side. */
  async ensureProvisioned(agentId: string): Promise<void> {
    const a = this.rec(agentId);
    if (a.provisioned) return;
    if (a.provisioning) return a.provisioning;
    a.provisioning = (async () => {
      const { cdpUrl } = await this.driver.provision(agentId);
      a.realCdpHttp = cdpUrl;
      a.realBrowserWsUrl = await resolveBrowserWs(cdpUrl);
      // Bring up the daemon's own view/control connection (separate from the agent).
      const shared = new SharedBrowser(cdpUrl);
      await shared.connect();
      a.shared = shared;
      a.provisioned = true;
      // If a viewer is already waiting, begin the screencast now.
      if (a.frameListeners > 0) await this.startCast(a);
      this.emitState(agentId);
    })();
    try {
      await a.provisioning;
    } finally {
      a.provisioning = undefined;
    }
  }

  /** Direct page access for the daemon (screencast connection). Provisions if
   *  needed — used by tests standing in for the agent to drive the page. */
  async sharedBrowser(agentId: string): Promise<SharedBrowser> {
    await this.ensureProvisioned(agentId);
    return this.agents.get(agentId)!.shared!;
  }

  /** DEV-ONLY (gated by the caller): provision the agent's browser and navigate
   *  it, standing in for the agent's first real browser use. Used by the UI e2e
   *  where the mock agent has no Playwright MCP. */
  async devNavigate(agentId: string, url: string): Promise<void> {
    const shared = await this.sharedBrowser(agentId);
    await shared.page?.goto(url, { waitUntil: 'load' }).catch(() => {});
  }

  // ---- control token --------------------------------------------------------

  grab(agentId: string): void {
    const a = this.rec(agentId);
    if (a.controlOwner === 'user') return;
    a.controlOwner = 'user';
    this.emitState(agentId);
  }

  release(agentId: string): void {
    const a = this.rec(agentId);
    if (a.controlOwner === 'agent') {
      // Still resolve any takeover waiters even if grab never flipped the token.
      this.resolveTakeovers(a);
      return;
    }
    a.controlOwner = 'agent';
    // Flush every held CDP frame, in order, per proxy link.
    for (const link of a.proxies) {
      const held = link.held;
      link.held = [];
      for (const f of held) link.upstream?.send(f.data as any, { binary: f.binary });
    }
    this.resolveTakeovers(a);
    this.emitState(agentId);
  }

  controlOwner(agentId: string): ControlOwner {
    return this.agents.get(agentId)?.controlOwner ?? 'agent';
  }

  // ---- agent-initiated takeover (Tandem-control MCP) ------------------------

  /** Block until the user releases the wheel. Resolves the agent's held tool call
   *  (browser.request_takeover). */
  requestTakeover(agentId: string): Promise<void> {
    const a = this.rec(agentId);
    return new Promise<void>((resolve) => a.takeoverWaiters.push(resolve));
  }
  private resolveTakeovers(a: AgentBrowser): void {
    const w = a.takeoverWaiters;
    a.takeoverWaiters = [];
    for (const f of w) f();
  }
  hasPendingTakeover(agentId: string): boolean {
    return (this.agents.get(agentId)?.takeoverWaiters.length ?? 0) > 0;
  }

  // ---- user input (only when owner=user; server enforces) -------------------

  async dispatchUserInput(agentId: string, event: BrowserInputEvent): Promise<void> {
    const a = this.agents.get(agentId);
    if (!a?.shared) return;
    await a.shared.dispatch(event).catch(() => {});
  }

  // ---- screencast fan-out (refcounted; only the focused agent subscribes) ---

  addFrameListener(agentId: string, cb: (f: ScreencastFrame) => void): () => void {
    const a = this.rec(agentId);
    a.frameListeners++;
    const handler = (f: ScreencastFrame) => cb(f);
    this.on(`frame:${agentId}`, handler);
    // Ensure the cast is running whenever a listener is present (idempotent — the
    // SharedBrowser guards against double-start). Robust against refcount skew
    // across reconnects (a stale count must never leave a live viewer frozen).
    if (a.provisioned) void this.startCast(a);
    return () => {
      this.off(`frame:${agentId}`, handler);
      a.frameListeners = Math.max(0, a.frameListeners - 1);
      if (a.frameListeners === 0 && a.shared) void a.shared.stopScreencast().catch(() => {});
    };
  }

  private async startCast(a: AgentBrowser): Promise<void> {
    if (!a.shared || a.shared.isCasting()) return;
    await a.shared.startScreencast((f) => this.emit(`frame:${a.agentId}`, f)).catch((e) => {
      if (process.env.TANDEM_BROKER_DEBUG) console.error('[broker] startScreencast error', (e as Error).message);
    });
  }

  // ---- CDP HTTP proxy (connectOverCDP bootstrap) ----------------------------

  private async onHttp(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    const m = /^\/cdp\/([^/]+)(\/.*)?$/.exec(req.url ?? '');
    if (!m) {
      res.writeHead(404).end();
      return;
    }
    const agentId = decodeURIComponent(m[1]);
    const rest = m[2] ?? '/';
    try {
      // First CDP hit provisions the browser (laziness invariant).
      await this.ensureProvisioned(agentId);
      const a = this.agents.get(agentId)!;
      const upstream = await fetch(`${a.realCdpHttp}${rest === '/' ? '/json/version' : rest}`);
      const text = await upstream.text();
      // Rewrite any browser-level ws debugger URL to point back at THIS proxy, so
      // Playwright connects through us (where the gate lives).
      const rewritten = text.replace(/"webSocketDebuggerUrl":\s*"ws:\/\/[^"]+"/g, `"webSocketDebuggerUrl": "ws://127.0.0.1:${this.port}/cdp/${encodeURIComponent(agentId)}/devtools/browser"`);
      res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json' });
      res.end(rewritten);
    } catch (e) {
      res.writeHead(502, { 'content-type': 'text/plain' }).end(`broker: ${(e as Error).message}`);
    }
  }

  // ---- CDP WebSocket proxy (the gated channel) ------------------------------

  private onUpgrade(req: http.IncomingMessage, socket: any, head: Buffer): void {
    const m = /^\/cdp\/([^/]+)(\/.*)?$/.exec(req.url ?? '');
    if (!m) {
      socket.destroy();
      return;
    }
    const agentId = decodeURIComponent(m[1]);
    this.wss.handleUpgrade(req, socket, head, (ws) => this.bridgeCdp(agentId, ws).catch(() => ws.close()));
  }

  private async bridgeCdp(agentId: string, agentSock: WebSocket): Promise<void> {
    await this.ensureProvisioned(agentId);
    const a = this.agents.get(agentId)!;
    const link: ProxyLink = { agentSock, held: [] };
    a.proxies.add(link);

    const upstream = new WebSocket(a.realBrowserWsUrl!, { perMessageDeflate: false, maxPayload: 256 * 1024 * 1024 });
    link.upstream = upstream;
    const upQueue: HeldFrame[] = []; // agent frames arriving before upstream opens

    upstream.on('open', () => {
      for (const f of upQueue.splice(0)) this.forwardAgentFrame(a, link, f);
    });
    // browser -> agent : always forwarded (preserve text/binary framing — CDP is text).
    upstream.on('message', (data, isBinary) => {
      if (agentSock.readyState === WebSocket.OPEN) agentSock.send(data as any, { binary: isBinary });
    });
    upstream.on('close', (code, reason) => {
      if (process.env.TANDEM_BROKER_DEBUG) console.error('[broker] upstream close', code, reason?.toString(), 'url', a.realBrowserWsUrl);
      agentSock.close();
    });
    upstream.on('error', (err) => {
      if (process.env.TANDEM_BROKER_DEBUG) console.error('[broker] upstream error', (err as Error).message);
      agentSock.close();
    });

    // agent -> browser : GATED while owner=user.
    agentSock.on('message', (data, isBinary) => {
      const frame: HeldFrame = { data, binary: isBinary };
      if (upstream.readyState !== WebSocket.OPEN) {
        upQueue.push(frame);
        return;
      }
      this.forwardAgentFrame(a, link, frame);
    });
    agentSock.on('close', () => {
      a.proxies.delete(link);
      upstream.close();
    });
    agentSock.on('error', () => upstream.close());
  }

  private forwardAgentFrame(a: AgentBrowser, link: ProxyLink, frame: HeldFrame): void {
    if (a.controlOwner === 'user') {
      link.held.push(frame); // HARD-PAUSE: hold until release() flushes
      return;
    }
    link.upstream?.send(frame.data as any, { binary: frame.binary });
  }

  // ---- teardown -------------------------------------------------------------

  async teardown(agentId: string): Promise<void> {
    const a = this.agents.get(agentId);
    if (!a) return;
    this.agents.delete(agentId);
    this.resolveTakeovers(a);
    for (const link of a.proxies) {
      link.agentSock.close();
      link.upstream?.close();
    }
    await a.shared?.close().catch(() => {});
    await this.driver.teardown(agentId).catch(() => {});
    this.emit(`state:${agentId}`, { active: false, controlOwner: 'agent' });
  }

  async stop(): Promise<void> {
    for (const id of [...this.agents.keys()]) await this.teardown(id);
    await new Promise<void>((res) => this.wss.close(() => res()));
    await new Promise<void>((res) => this.server.close(() => res()));
  }
}

// connectOverCDP needs the browser-level webSocketDebuggerUrl; fetch it from the
// driver's HTTP CDP endpoint. If the driver already handed us a ws:// URL, use it.
async function resolveBrowserWs(cdpUrl: string): Promise<string> {
  if (cdpUrl.startsWith('ws://') || cdpUrl.startsWith('wss://')) return cdpUrl;
  const r = await fetch(`${cdpUrl.replace(/\/$/, '')}/json/version`);
  const body = (await r.json()) as { webSocketDebuggerUrl?: string };
  if (!body.webSocketDebuggerUrl) throw new Error(`no webSocketDebuggerUrl at ${cdpUrl}/json/version`);
  return body.webSocketDebuggerUrl;
}
