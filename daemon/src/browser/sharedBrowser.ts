// The daemon's own CDP connection to one browser, used for the USER side of the
// shared browser: Page.startScreencast (view) + Input.* (control). Ported from
// spike/browser/src/sharedBrowser.ts. This connection is SEPARATE from the
// agent's Playwright-MCP connection (which goes through the broker's gated CDP
// proxy) — so screencast keeps flowing while the agent is hard-paused.

import { chromium, type Browser, type Page, type CDPSession } from 'playwright-core';

export interface ScreencastFrame {
  dataB64: string;
  meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number };
}

export class SharedBrowser {
  private browser?: Browser;
  page?: Page;
  private client?: CDPSession;
  private casting = false;
  private frameCb?: (f: ScreencastFrame) => void;

  constructor(private cdpUrl: string) {}

  async connect(): Promise<void> {
    this.browser = await chromium.connectOverCDP(this.cdpUrl);
    const ctx = this.browser.contexts()[0] ?? (await this.browser.newContext());
    this.page = ctx.pages()[0] ?? (await ctx.newPage());
    await this.page.setViewportSize({ width: 1280, height: 800 }).catch(() => {});
    this.client = await ctx.newCDPSession(this.page);
  }

  isConnected(): boolean {
    return !!this.client;
  }

  // ---- viewer: screencast ----
  // The CDP frame handler is registered ONCE per connection (in connect) and just
  // forwards to the current frameCb — so repeated start/stop cycles never stack
  // duplicate listeners. start/stop only toggle the CDP screencast + the callback.
  private handlerBound = false;
  private bindFrameHandler(): void {
    if (this.handlerBound || !this.client) return;
    this.handlerBound = true;
    this.client.on('Page.screencastFrame', async (e: any) => {
      this.frameCb?.({
        dataB64: e.data,
        meta: {
          deviceWidth: e.metadata?.deviceWidth ?? 1280,
          deviceHeight: e.metadata?.deviceHeight ?? 800,
          offsetTop: e.metadata?.offsetTop ?? 0,
          timestamp: e.metadata?.timestamp,
        },
      });
      try {
        await this.client!.send('Page.screencastFrameAck', { sessionId: e.sessionId });
      } catch {
        /* page navigating */
      }
    });
  }

  async startScreencast(onFrame: (f: ScreencastFrame) => void): Promise<void> {
    if (!this.client) throw new Error('shared browser not connected');
    this.frameCb = onFrame;
    this.bindFrameHandler();
    if (this.casting) return; // idempotent
    this.casting = true;
    await this.client.send('Page.startScreencast', { format: 'jpeg', quality: 70, maxWidth: 1280, maxHeight: 800, everyNthFrame: 1 });
  }

  async stopScreencast(): Promise<void> {
    this.frameCb = undefined;
    if (!this.casting) return;
    this.casting = false;
    await this.client?.send('Page.stopScreencast').catch(() => {});
  }

  isCasting(): boolean {
    return this.casting;
  }

  // ---- viewer: human input (dispatched only when the user holds the wheel) ----
  async dispatch(event: BrowserInputEvent): Promise<void> {
    const c = this.client;
    if (!c) return;
    const x = event.x ?? 0;
    const y = event.y ?? 0;
    switch (event.kind) {
      case 'mousemove':
        await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, buttons: event.buttons ?? 0 });
        break;
      case 'mousedown':
        await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: event.button ?? 'left', clickCount: event.clickCount ?? 1, buttons: event.buttons ?? 1 });
        break;
      case 'mouseup':
        await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: event.button ?? 'left', clickCount: event.clickCount ?? 1, buttons: event.buttons ?? 0 });
        break;
      case 'click':
        await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1, buttons: 1 });
        await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1, buttons: 1 });
        break;
      case 'wheel':
        await c.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x, y, deltaX: event.deltaX ?? 0, deltaY: event.deltaY ?? 0 });
        break;
      case 'keydown':
        await c.send('Input.dispatchKeyEvent', { type: 'keyDown', key: event.key, text: event.text, code: event.code, windowsVirtualKeyCode: event.keyCode });
        break;
      case 'keyup':
        await c.send('Input.dispatchKeyEvent', { type: 'keyUp', key: event.key, code: event.code, windowsVirtualKeyCode: event.keyCode });
        break;
      case 'text':
        await c.send('Input.insertText', { text: event.text ?? '' });
        break;
    }
  }

  async close(): Promise<void> {
    await this.stopScreencast().catch(() => {});
    // Only drop our client connection; the browser process is owned by the driver.
    await this.browser?.close().catch(() => {});
    this.client = undefined;
    this.page = undefined;
    this.browser = undefined;
  }
}

// Normalized user-input event (the WS `browser_input.event` shape).
export interface BrowserInputEvent {
  kind: 'mousemove' | 'mousedown' | 'mouseup' | 'click' | 'wheel' | 'keydown' | 'keyup' | 'text';
  x?: number;
  y?: number;
  button?: 'left' | 'middle' | 'right';
  buttons?: number;
  clickCount?: number;
  deltaX?: number;
  deltaY?: number;
  key?: string;
  code?: string;
  keyCode?: number;
  text?: string;
}
