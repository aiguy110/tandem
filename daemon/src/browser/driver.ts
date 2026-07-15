// BrowserDriver — the one seam the broker provisions browsers through (D13
// amendment). Two implementations:
//
//   * LocalChromiumDriver — launches Playwright's bundled headless Chromium per
//     agent, with a per-agent user-data dir under TANDEM_HOME. Used where no
//     Steel/Docker is available and by every automated test. Exposes a real CDP
//     endpoint (--remote-debugging-port) — the SAME surface Steel wraps.
//
//   * SteelDriver — provisions a session via Steel's REST API (POST /v1/sessions
//     → CDP ws url; POST .../release to release). The production path, verified
//     end-to-end against a self-hosted Steel by `npm run derisk:steel` (see
//     docs/browser.md › "Self-hosting Steel").
//
// Selection is by config (TANDEM_BROWSER_DRIVER=local|steel).

import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { chromium } from 'playwright-core';

export interface ProvisionResult {
  /** An HTTP CDP endpoint (http://host:port) whose /json/version yields the
   *  browser-level webSocketDebuggerUrl. Both Playwright connectOverCDP and the
   *  daemon's own screencast connection speak to this. */
  cdpUrl: string;
}

export interface BrowserDriver {
  readonly kind: string;
  provision(agentId: string): Promise<ProvisionResult>;
  teardown(agentId: string): Promise<void>;
  /** True while a browser is live for this agent (laziness assertions/tests). */
  isProvisioned(agentId: string): boolean;
  /** The OS pid of the browser process, when the driver owns one (local only). */
  pid(agentId: string): number | undefined;
}

// ---- a free localhost TCP port ----
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      const port = typeof addr === 'object' && addr ? addr.port : 0;
      srv.close(() => resolve(port));
    });
  });
}

// ============================ LocalChromiumDriver ============================

interface LocalHandle {
  proc: ChildProcess;
  port: number;
  cdpUrl: string;
  userDataDir: string;
}

export class LocalChromiumDriver implements BrowserDriver {
  readonly kind = 'local';
  private handles = new Map<string, LocalHandle>();

  constructor(private opts: { userDataRoot: string; executablePath?: string }) {}

  isProvisioned(agentId: string): boolean {
    return this.handles.has(agentId);
  }
  pid(agentId: string): number | undefined {
    return this.handles.get(agentId)?.proc.pid;
  }

  async provision(agentId: string): Promise<ProvisionResult> {
    const existing = this.handles.get(agentId);
    if (existing) return { cdpUrl: existing.cdpUrl };

    const port = await freePort();
    const userDataDir = path.join(this.opts.userDataRoot, agentId);
    fs.mkdirSync(userDataDir, { recursive: true });
    const exe = this.opts.executablePath ?? chromium.executablePath();

    const proc = spawn(
      exe,
      [
        '--headless=new',
        '--no-sandbox',
        '--disable-gpu',
        '--disable-dev-shm-usage',
        `--remote-debugging-port=${port}`,
        `--user-data-dir=${userDataDir}`,
        '--no-first-run',
        '--no-default-browser-check',
        '--window-size=1280,800',
        'about:blank',
      ],
      { stdio: 'ignore' },
    );
    const cdpUrl = `http://127.0.0.1:${port}`;

    // Wait for the CDP HTTP endpoint to answer.
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      if (proc.exitCode !== null) throw new Error(`chromium exited (code ${proc.exitCode}) during launch`);
      try {
        const r = await fetch(`${cdpUrl}/json/version`);
        if (r.ok) {
          this.handles.set(agentId, { proc, port, cdpUrl, userDataDir });
          return { cdpUrl };
        }
      } catch {
        /* not up yet */
      }
      await new Promise((r) => setTimeout(r, 150));
    }
    proc.kill('SIGKILL');
    throw new Error(`chromium CDP did not come up on port ${port} for agent ${agentId}`);
  }

  async teardown(agentId: string): Promise<void> {
    const h = this.handles.get(agentId);
    if (!h) return;
    this.handles.delete(agentId);
    await killAndWait(h.proc);
    // Best-effort user-data cleanup (ephemeral in v1).
    fs.rm(h.userDataDir, { recursive: true, force: true }, () => {});
  }
}

function killAndWait(proc: ChildProcess): Promise<void> {
  return new Promise((resolve) => {
    if (proc.exitCode !== null || proc.signalCode !== null) return resolve();
    const done = () => resolve();
    proc.once('exit', done);
    proc.kill('SIGTERM');
    setTimeout(() => {
      if (proc.exitCode === null && proc.signalCode === null) proc.kill('SIGKILL');
      // Give SIGKILL a moment, then resolve regardless.
      setTimeout(done, 300);
    }, 1500);
  });
}

// =============================== SteelDriver ================================
//
// Maps our two lifecycle calls onto Steel's REST API. Verified against the
// self-hosted OSS `ghcr.io/steel-dev/steel-browser` image (see docs/browser.md
// › "Self-hosting Steel"):
//
//   POST {STEEL_BASE_URL}/v1/sessions              -> { id, websocketUrl, ... }
//   POST {STEEL_BASE_URL}/v1/sessions/{id}/release -> releases the session
//
// The create response's `websocketUrl` is the browser-level CDP endpoint, but
// self-hosted Steel advertises it with a bind-address host (`ws://0.0.0.0:3000/`)
// that isn't dialable as-is — so we normalize the host to STEEL_BASE_URL's host
// (this also makes a remote Steel reachable). The result is a ws:// CDP URL;
// Playwright's connectOverCDP and the broker's proxy both accept ws:// upstreams
// directly (resolveBrowserWs + the onHttp ws branch in broker.ts).
//
// NB Steel's `/json/version` (on its separate CDP/debugger port) advertises a
// PORT-LESS `ws://localhost/devtools/...`, which Playwright dials as :80 and
// fails — so we deliberately go through `websocketUrl`, not that endpoint.
//
// If your Steel build differs, this class is the ONE place to adjust field names.

export class SteelDriver implements BrowserDriver {
  readonly kind = 'steel';
  private sessions = new Map<string, string>(); // agentId -> steel session id
  private profiles = new Map<string, string>(); // agentId -> reusable Steel Cloud profile

  constructor(private opts: { baseUrl: string; apiKey?: string; sessionOptions?: Record<string, unknown> }) {}

  isProvisioned(agentId: string): boolean {
    return this.sessions.has(agentId);
  }
  pid(): number | undefined {
    return undefined; // remote session — no local pid
  }

  private get base(): string {
    return this.opts.baseUrl.replace(/\/$/, '');
  }

  private headers(): Record<string, string> {
    const h: Record<string, string> = { 'content-type': 'application/json' };
    if (this.opts.apiKey) h['steel-api-key'] = this.opts.apiKey;
    return h;
  }

  async provision(agentId: string): Promise<ProvisionResult> {
    const existing = this.sessions.get(agentId);
    if (existing) return { cdpUrl: this.cdpUrlFor(existing) };

    const profileId = this.profiles.get(agentId);
    const sessionOptions = {
      dimensions: { width: 1280, height: 800 },
      // Steel Cloud persists the browser user-data directory when supported.
      // Self-hosted Steel safely ignores fields outside its smaller schema.
      persistProfile: true,
      ...(profileId ? { profileId } : {}),
      ...this.opts.sessionOptions,
    };
    const res = await fetch(`${this.base}/v1/sessions`, {
      method: 'POST',
      headers: this.headers(),
      body: JSON.stringify(sessionOptions),
    });
    if (!res.ok) {
      const detail = await res.text().catch(() => '');
      throw new Error(`steel: create session failed (${res.status} ${res.statusText})${detail ? `: ${detail}` : ''}`);
    }
    const body = (await res.json()) as { id: string; websocketUrl?: string; cdpUrl?: string; profileId?: string };
    this.sessions.set(agentId, body.id);
    if (body.profileId) this.profiles.set(agentId, body.profileId);
    const cdpUrl = this.normalizeWs(body.cdpUrl ?? body.websocketUrl) ?? this.cdpUrlFor(body.id);
    return { cdpUrl };
  }

  // Rewrite a Steel-advertised CDP ws URL so its host is reachable: self-hosted
  // Steel returns `ws://0.0.0.0:3000/` (the container bind address). We swap that
  // host for STEEL_BASE_URL's host, keeping the port + path Steel chose. A URL
  // that already names a routable host is left as-is.
  private normalizeWs(raw?: string): string | undefined {
    if (!raw) return undefined;
    try {
      const u = new URL(raw);
      const bindHosts = new Set(['0.0.0.0', '::', '127.0.0.1', 'localhost']);
      if (bindHosts.has(u.hostname)) u.hostname = new URL(this.base).hostname;
      return u.toString();
    } catch {
      return raw;
    }
  }

  private cdpUrlFor(sessionId: string): string {
    // Fallback shape when Steel doesn't echo a usable websocketUrl.
    return `${this.base}/v1/sessions/${sessionId}`;
  }

  async teardown(agentId: string): Promise<void> {
    const id = this.sessions.get(agentId);
    if (!id) return;
    this.sessions.delete(agentId);
    await fetch(`${this.base}/v1/sessions/${id}/release`, {
      method: 'POST',
      headers: this.headers(),
    }).catch(() => {});
  }
}

// ------------------------------ selection ----------------------------------

export interface BrowserDriverConfig {
  driver: 'local' | 'steel';
  userDataRoot: string;
  steelBaseUrl?: string;
  steelApiKey?: string;
  steelSessionOptions?: Record<string, unknown>;
}

export function makeDriver(cfg: BrowserDriverConfig): BrowserDriver {
  if (cfg.driver === 'steel') {
    if (!cfg.steelBaseUrl) throw new Error('TANDEM_BROWSER_DRIVER=steel requires STEEL_BASE_URL');
    return new SteelDriver({ baseUrl: cfg.steelBaseUrl, apiKey: cfg.steelApiKey, sessionOptions: cfg.steelSessionOptions });
  }
  return new LocalChromiumDriver({ userDataRoot: cfg.userDataRoot });
}
