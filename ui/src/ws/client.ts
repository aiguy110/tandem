// Durable WebSocket client for the Tandem daemon.
//
// - Token auth (D15): the token is read once from the URL fragment (#t=<token>),
//   persisted to localStorage, and appended as ?token= on the WS URL. A 4401
//   close means the token was rejected → surface the paste-token screen.
// - Auto-reconnect with exponential backoff + jitter.
// - The client is transport only: it hands raw ServerMsgs to `onMessage` and
//   reports transport state via `onState`. All state projection lives in the
//   store, which also decides what to (re)subscribe on (re)connect.

import type { ClientMsg, ServerMsg } from '../wire';

export type ConnState = 'connecting' | 'connected' | 'reconnecting' | 'need-token' | 'rejected';

const TOKEN_KEY = 'tandem.token';

// Resolve the token: URL fragment (#t=…) wins and is persisted, else localStorage.
export function resolveToken(): string | null {
  const frag = window.location.hash;
  const m = frag.match(/[#&]t=([^&]+)/);
  if (m) {
    const tok = decodeURIComponent(m[1]);
    localStorage.setItem(TOKEN_KEY, tok);
    // Strip the token out of the visible URL so it doesn't linger in the bar.
    history.replaceState(null, '', window.location.pathname + window.location.search);
    return tok;
  }
  return localStorage.getItem(TOKEN_KEY);
}

export function storeToken(tok: string): void {
  localStorage.setItem(TOKEN_KEY, tok.trim());
}
export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}
export function storedToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

// The WS origin: a runtime override (window.__TANDEM_WS__ or VITE_TANDEM_WS at
// build time) else same-origin (the daemon serves UI + WS on one port, D15).
function wsBase(): string {
  const override =
    (globalThis as { __TANDEM_WS__?: string }).__TANDEM_WS__ ??
    (import.meta.env.VITE_TANDEM_WS as string | undefined);
  if (override) return override;
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}`;
}

export interface WsClientOpts {
  onMessage: (msg: ServerMsg) => void;
  onState: (state: ConnState) => void;
  // Called every time a socket opens, so the store can (re)subscribe with sinceSeq.
  onOpen: () => void;
}

// How long wake()'s liveness probe waits for its pong before deciding
// the socket is dead. Short enough to feel instant, long enough for a phone
// re-acquiring its radio on unlock.
const WAKE_PROBE_TIMEOUT_MS = 3000;

export class WsClient {
  private ws: WebSocket | null = null;
  private token: string | null = null;
  private attempts = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closedByUs = false;
  private outbox: ClientMsg[] = [];
  private probeTimer: ReturnType<typeof setTimeout> | null = null;
  private probeId: string | null = null;
  private probeSequence = 0;
  private snapshots = new Map<string, string[]>();

  constructor(private opts: WsClientOpts) {}

  start(token: string | null): void {
    this.token = token;
    this.closedByUs = false;
    if (!token) {
      this.opts.onState('need-token');
      return;
    }
    this.connect();
  }

  setToken(token: string): void {
    clearReconnect(this);
    this.token = token;
    storeToken(token);
    this.attempts = 0;
    this.closeSocket();
    this.connect();
  }

  private connect(): void {
    if (!this.token) {
      this.opts.onState('need-token');
      return;
    }
    this.opts.onState(this.attempts === 0 ? 'connecting' : 'reconnecting');
    const url = `${wsBase()}/?token=${encodeURIComponent(this.token)}`;
    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    this.snapshots.clear();

    ws.onopen = () => {
      if (this.ws !== ws) return;
      this.attempts = 0;
      this.opts.onState('connected');
      // Flush anything queued while offline, then let the store subscribe.
      const pending = this.outbox;
      this.outbox = [];
      for (const m of pending) this.rawSend(m);
      this.opts.onOpen();
    };

    ws.onmessage = (e) => {
      if (this.ws !== ws) return;
      let msg: ServerMsg;
      try {
        msg = JSON.parse(e.data as string);
      } catch {
        return;
      }
      if (msg.t === 'pong') {
        if (msg.corrId === this.probeId) {
          if (this.probeTimer) clearTimeout(this.probeTimer);
          this.probeTimer = null;
          this.probeId = null;
        }
        return;
      }
      if (msg.t === 'snapshot_start') {
        this.snapshots.set(`${msg.sessionId}/${msg.replayId}`, []);
        return;
      }
      if (msg.t === 'snapshot_chunk') {
        this.snapshots.get(`${msg.sessionId}/${msg.replayId}`)?.push(msg.data);
        return;
      }
      if (msg.t === 'snapshot_end') {
        const key = `${msg.sessionId}/${msg.replayId}`;
        const chunks = this.snapshots.get(key);
        this.snapshots.delete(key);
        if (chunks) {
          try {
            const snapshot = JSON.parse(chunks.join('')) as Extract<ServerMsg, { t: 'snapshot' }>;
            // Federation rewrites the envelope; the opaque JSON chunks still
            // contain the child's local ID. Use the routed envelope's ID.
            snapshot.sessionId = msg.sessionId;
            this.opts.onMessage(snapshot);
          } catch { this.forceReconnect(); }
        }
        return;
      }
      this.opts.onMessage(msg);
    };

    ws.onclose = (e) => {
      if (this.ws !== ws) return;
      console.info('Tandem WebSocket closed', { code: e.code, reason: e.reason, clean: e.wasClean });
      this.ws = null;
      this.closeSocket(); // Clear this generation's pending probe and snapshot fragments.
      if (this.closedByUs) return;
      if (e.code === 4401) {
        // Token rejected — no amount of reconnecting helps.
        this.opts.onState('rejected');
        return;
      }
      this.scheduleReconnect();
    };

    ws.onerror = () => {
      // onclose will follow; nothing to do here (avoids double-scheduling).
    };
  }

  private scheduleReconnect(): void {
    this.opts.onState('reconnecting');
    this.attempts++;
    const base = Math.min(15000, 300 * 2 ** Math.min(this.attempts, 6));
    const delay = base / 2 + Math.random() * (base / 2);
    clearReconnect(this);
    this.reconnectTimer = setTimeout(() => this.connect(), delay);
  }

  private rawSend(m: ClientMsg): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }

  // Public send: queues while offline so intent isn't lost across a blip.
  send(m: ClientMsg): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.rawSend(m);
    else if (m.t !== 'browser_input') this.outbox.push(m);
  }

  private closeSocket(): void {
    if (this.probeTimer) {
      clearTimeout(this.probeTimer);
      this.probeTimer = null;
    }
    this.probeId = null;
    this.snapshots.clear();
    if (this.ws) {
      this.closedByUs = true;
      const retiring = this.ws;
      this.ws = null;
      retiring.onopen = retiring.onmessage = retiring.onclose = retiring.onerror = null;
      try {
        retiring.close();
      } catch {
        /* ignore */
      }
      this.closedByUs = false;
    }
  }

  reconnectTimerRef(): ReturnType<typeof setTimeout> | null {
    return this.reconnectTimer;
  }
  clearReconnectTimer(): void {
    this.reconnectTimer = null;
  }

  // Called on resume-from-hidden (visibilitychange -> visible, pageshow) to
  // recover promptly from a socket the mobile OS silently killed while the
  // page was frozen.
  //
  // readyState alone can't answer "is this dead": a half-open socket keeps
  // reporting OPEN until some future write notices. But unconditionally
  // reconnecting is worse — every ordinary tab switch would drop a healthy
  // connection and force a full replay. So probe instead: ask the daemon for
  // a dedicated ping it always answers, and reconnect if its pong never comes
  // back. Healthy sockets survive; dead ones are replaced in ~3s instead of
  // waiting out scheduleReconnect's backoff, which stays as-is for genuine
  // network failures.
  wake(): void {
    if (this.closedByUs) return; // stop()/not started yet
    if (!this.token) return; // need-token state — nothing to reconnect
    const ws = this.ws;
    if (ws && ws.readyState === WebSocket.CONNECTING) return; // already mid-connect
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      this.forceReconnect();
      return;
    }
    if (this.probeTimer) return; // a probe is already outstanding
    const probeId = `wake-${++this.probeSequence}`;
    this.probeId = probeId;
    this.rawSend({ t: 'ping', corrId: probeId });
    this.probeTimer = setTimeout(() => {
      this.probeTimer = null;
      if (this.probeId === probeId) this.forceReconnect();
    }, WAKE_PROBE_TIMEOUT_MS);
  }

  private forceReconnect(): void {
    console.info('Tandem WebSocket recovery requested', { readyState: this.ws?.readyState, probePending: this.probeId !== null });
    clearReconnect(this);
    this.attempts = 0;
    this.closeSocket();
    this.connect();
  }
}

function clearReconnect(c: WsClient): void {
  const t = c.reconnectTimerRef();
  if (t) {
    clearTimeout(t);
    c.clearReconnectTimer();
  }
}
