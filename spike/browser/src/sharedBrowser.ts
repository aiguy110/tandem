// The shared-browser mediator. One headless Chrome (a stand-in for a Steel
// session — Steel exposes the same CDP surface). The "agent" drives via the
// Playwright high-level API over CDP (exactly what Playwright MCP does under the
// hood); the "viewer" streams the same page via CDP Page.startScreencast and
// injects the human's input via Input.*. A control-owner token HARD-PAUSES the
// agent's actions while the human holds the wheel.

import { spawn, type ChildProcess } from 'node:child_process';
import { chromium, type Browser, type Page, type CDPSession } from 'playwright-core';

const CHROME = '/usr/bin/google-chrome-stable';

export async function launchChrome(port: number, userDataDir: string): Promise<ChildProcess> {
  const proc = spawn(
    CHROME,
    [
      '--headless=new',
      '--no-sandbox',
      '--disable-gpu',
      `--remote-debugging-port=${port}`,
      `--user-data-dir=${userDataDir}`,
      '--no-first-run',
      '--no-default-browser-check',
      '--window-size=1280,800',
      'about:blank',
    ],
    { stdio: 'ignore' },
  );
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://localhost:${port}/json/version`);
      if (r.ok) return proc;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  proc.kill();
  throw new Error(`chrome CDP did not come up on port ${port}`);
}

export type ControlOwner = 'agent' | 'user';

export class SharedBrowser {
  browser!: Browser;
  page!: Page;
  client!: CDPSession;
  controlOwner: ControlOwner = 'agent';
  private waiters: (() => void)[] = [];

  constructor(private cdpUrl: string) {}

  async connect(): Promise<void> {
    this.browser = await chromium.connectOverCDP(this.cdpUrl);
    const ctx = this.browser.contexts()[0];
    this.page = ctx.pages()[0] ?? (await ctx.newPage());
    await this.page.setViewportSize({ width: 1280, height: 800 }).catch(() => {});
    this.client = await ctx.newCDPSession(this.page);
  }

  // ---- control-owner token ----
  grab(): void {
    this.controlOwner = 'user';
  }
  release(): void {
    this.controlOwner = 'agent';
    const w = this.waiters;
    this.waiters = [];
    w.forEach((f) => f());
  }
  private gate(): Promise<void> {
    if (this.controlOwner === 'agent') return Promise.resolve();
    return new Promise<void>((r) => this.waiters.push(r)); // HARD-PAUSE until release()
  }

  /** An agent browser action (Playwright/CDP), gated by the token. */
  async agentDo<T>(_label: string, fn: (p: Page) => Promise<T>): Promise<T> {
    await this.gate();
    return fn(this.page);
  }

  // ---- viewer: screencast ----
  async startScreencast(onFrame: (b64jpeg: string) => void): Promise<void> {
    this.client.on('Page.screencastFrame', async (e: any) => {
      onFrame(e.data);
      try {
        await this.client.send('Page.screencastFrameAck', { sessionId: e.sessionId });
      } catch {
        /* page navigating */
      }
    });
    await this.client.send('Page.startScreencast', { format: 'jpeg', quality: 70, maxWidth: 1280, maxHeight: 800, everyNthFrame: 1 });
  }
  async stopScreencast(): Promise<void> {
    await this.client.send('Page.stopScreencast').catch(() => {});
  }

  // ---- viewer: human input (only dispatched when the human holds the wheel) ----
  async userClick(x: number, y: number): Promise<void> {
    await this.client.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1, buttons: 1 });
    await this.client.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1, buttons: 1 });
  }
  async userMove(x: number, y: number): Promise<void> {
    await this.client.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
  }
  async userType(text: string): Promise<void> {
    await this.client.send('Input.insertText', { text });
  }

  async close(): Promise<void> {
    await this.browser.close().catch(() => {});
  }
}
