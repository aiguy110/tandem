import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WsClient } from './client';

// A frozen (screen-off) mobile page can come back with a WebSocket the OS
// silently killed while the client itself never saw a close event — see
// WsClient.wake()'s doc comment. These tests drive that path with a fake
// WebSocket rather than a real one, since jsdom doesn't implement real
// network behavior and we need to control readyState/close precisely.
class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly CONNECTING = FakeWebSocket.CONNECTING;
  readonly OPEN = FakeWebSocket.OPEN;
  readonly CLOSING = FakeWebSocket.CLOSING;
  readonly CLOSED = FakeWebSocket.CLOSED;

  readyState = FakeWebSocket.CONNECTING;
  url: string;
  onopen: (() => void) | null = null;
  onclose: ((e: { code: number }) => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  sent: string[] = [];

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }
  send(data: string) {
    this.sent.push(data);
  }
  // Test helper simulating the browser actually establishing the connection.
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
  close() {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code: 1000 });
  }
}

function opts() {
  return { onMessage: vi.fn(), onState: vi.fn(), onOpen: vi.fn() };
}

beforeEach(() => {
  FakeWebSocket.instances = [];
  vi.stubGlobal('WebSocket', FakeWebSocket as unknown as typeof WebSocket);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('WsClient.wake', () => {
  it('ignores delayed callbacks from a retired socket', () => {
    vi.useFakeTimers();
    const handlers = opts();
    const client = new WsClient(handlers);
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();
    // Capture already queued browser callbacks: clearing handlers alone cannot
    // cancel events whose callbacks have already been scheduled.
    const oldClose = first.onclose;
    const oldOpen = first.onopen;
    const oldMessage = first.onmessage;
    client.setToken('replacement');
    const second = FakeWebSocket.instances[1];
    second.open();
    oldClose?.({ code: 1006 });
    oldOpen?.();
    oldMessage?.({ data: JSON.stringify({ t: 'agents', sessions: [] }) });
    expect(handlers.onState).toHaveBeenLastCalledWith('connected');
    expect(handlers.onOpen).toHaveBeenCalledTimes(2);
    expect(handlers.onMessage).not.toHaveBeenCalled();
    expect(client.reconnectTimerRef()).toBeNull();
    client.send({ t: 'list_agents' });
    expect(second.sent).toContain(JSON.stringify({ t: 'list_agents' }));
    vi.runOnlyPendingTimers();
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('requires the matching pong even while browser frames arrive', () => {
    vi.useFakeTimers();
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();
    client.wake();
    first.onmessage?.({ data: JSON.stringify({ t: 'browser_frame', sessionId: 'a', dataB64: '', meta: {} }) });
    first.onmessage?.({ data: JSON.stringify({ t: 'pong', corrId: 'unrelated' }) });
    vi.advanceTimersByTime(3000);
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('cancels a closed socket probe before its replacement connects', () => {
    vi.useFakeTimers();
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();
    client.wake();
    first.onclose?.({ code: 1006 });
    client.wake();
    FakeWebSocket.instances[1].open();
    vi.advanceTimersByTime(5000);
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('assembles interleaved snapshot chunks with routed session IDs', () => {
    const handlers = opts();
    const client = new WsClient(handlers);
    client.start('tok');
    const socket = FakeWebSocket.instances[0];
    socket.open();
    const receive = (msg: object) => socket.onmessage?.({ data: JSON.stringify(msg) });
    const data = JSON.stringify({ t: 'snapshot', sessionId: 'child-local', seq: 9, transcript: [] });
    receive({ t: 'snapshot_start', sessionId: 'host-a/child-local', replayId: 'same' });
    receive({ t: 'snapshot_start', sessionId: 'host-b/child-local', replayId: 'same' });
    receive({ t: 'snapshot_chunk', sessionId: 'host-a/child-local', replayId: 'same', data: data.slice(0, 20) });
    receive({ t: 'snapshot_chunk', sessionId: 'host-b/child-local', replayId: 'same', data });
    receive({ t: 'snapshot_chunk', sessionId: 'host-a/child-local', replayId: 'same', data: data.slice(20) });
    expect(handlers.onMessage).not.toHaveBeenCalled();
    receive({ t: 'snapshot_end', sessionId: 'host-a/child-local', replayId: 'same' });
    receive({ t: 'snapshot_end', sessionId: 'host-b/child-local', replayId: 'same' });
    expect(handlers.onMessage.mock.calls.map(([msg]) => msg.sessionId)).toEqual(['host-a/child-local', 'host-b/child-local']);
  });

  it('does not replay offline mouse input into a replacement browser', () => {
    const client = new WsClient(opts());
    client.start('tok');
    client.send({ t: 'browser_input', sessionId: 'a', event: { kind: 'click', x: 1, y: 2 } });
    client.send({ t: 'list_agents' });
    const socket = FakeWebSocket.instances[0];
    socket.open();
    expect(socket.sent).toEqual([JSON.stringify({ t: 'list_agents' })]);
  });

  it('keeps a healthy socket when the probe is answered', () => {
    vi.useFakeTimers();
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();

    client.wake();

    // The probe goes out on the existing socket rather than replacing it.
    expect(FakeWebSocket.instances).toHaveLength(1);
    const probe = JSON.parse(first.sent[first.sent.length - 1]);
    expect(probe.t).toBe('ping');

    first.onmessage?.({ data: JSON.stringify({ t: 'pong', corrId: probe.corrId }) });
    vi.advanceTimersByTime(5000);

    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(first.readyState).toBe(FakeWebSocket.OPEN);
  });

  it('reconnects when a socket that still reports OPEN never answers the probe', () => {
    vi.useFakeTimers();
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();

    // The half-open case: the OS killed it while the page was frozen, so it
    // still claims OPEN but nothing comes back.
    client.wake();
    expect(FakeWebSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(5000);

    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(first.readyState).toBe(FakeWebSocket.CLOSED);
  });

  it('reconnects immediately, without probing, when the socket is already closed', () => {
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];
    first.open();
    first.readyState = FakeWebSocket.CLOSED;

    client.wake();

    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('does nothing without a token (need-token state)', () => {
    const client = new WsClient(opts());
    client.start(null);

    client.wake();

    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it('does nothing if start() was never called', () => {
    const client = new WsClient(opts());

    client.wake();

    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it('clears a pending backoff timer instead of racing a stale scheduled reconnect', () => {
    vi.useFakeTimers();
    const client = new WsClient(opts());
    client.start('tok');
    const first = FakeWebSocket.instances[0];

    // Simulate the OS having actually killed the socket: an unexpected close
    // schedules the normal backoff+jitter reconnect.
    first.onclose?.({ code: 1006 });
    expect(client.reconnectTimerRef()).not.toBeNull();

    client.wake();

    expect(client.reconnectTimerRef()).toBeNull();
    expect(FakeWebSocket.instances).toHaveLength(2);

    // The stale timer must not still be armed to fire a second reconnect
    // later and create a third, redundant socket.
    vi.runOnlyPendingTimers();
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('is a no-op while a fresh connect() is already in flight', () => {
    const client = new WsClient(opts());
    client.start('tok'); // leaves the first socket CONNECTING (open() not called)

    client.wake();

    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});
