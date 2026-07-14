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

export class WsClient {
  private ws: WebSocket | null = null;
  private token: string | null = null;
  private attempts = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closedByUs = false;
  private outbox: ClientMsg[] = [];

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

    ws.onopen = () => {
      this.attempts = 0;
      this.opts.onState('connected');
      // Flush anything queued while offline, then let the store subscribe.
      const pending = this.outbox;
      this.outbox = [];
      for (const m of pending) this.rawSend(m);
      this.opts.onOpen();
    };

    ws.onmessage = (e) => {
      let msg: ServerMsg;
      try {
        msg = JSON.parse(e.data as string);
      } catch {
        return;
      }
      this.opts.onMessage(msg);
    };

    ws.onclose = (e) => {
      this.ws = null;
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
    else this.outbox.push(m);
  }

  private closeSocket(): void {
    if (this.ws) {
      this.closedByUs = true;
      try {
        this.ws.close();
      } catch {
        /* ignore */
      }
      this.ws = null;
      this.closedByUs = false;
    }
  }

  reconnectTimerRef(): ReturnType<typeof setTimeout> | null {
    return this.reconnectTimer;
  }
  clearReconnectTimer(): void {
    this.reconnectTimer = null;
  }
}

function clearReconnect(c: WsClient): void {
  const t = c.reconnectTimerRef();
  if (t) {
    clearTimeout(t);
    c.clearReconnectTimer();
  }
}
