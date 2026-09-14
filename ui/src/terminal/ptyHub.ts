// A non-reactive buffer + fan-out for raw_pty bytes, kept OUT of the React store
// so a stream of terminal bytes never triggers component re-renders. The Terminal
// pane subscribes on focus and rehydrates from the buffer (docs/terminal.md:
// "reconnect replays buffered raw_pty bytes"); background terminals just buffer.

const MAX_BYTES = 256 * 1024; // per-agent scrollback cap on the client side

function b64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

class PtyHub {
  private buffers = new Map<string, Uint8Array[]>();
  private sizes = new Map<string, number>();
  private listeners = new Map<string, Set<(bytes: Uint8Array) => void>>();

  push(sessionId: string, dataB64: string): void {
    const bytes = b64ToBytes(dataB64);
    const buf = this.buffers.get(sessionId) ?? [];
    buf.push(bytes);
    let size = (this.sizes.get(sessionId) ?? 0) + bytes.length;
    while (size > MAX_BYTES && buf.length > 1) size -= buf.shift()!.length;
    this.buffers.set(sessionId, buf);
    this.sizes.set(sessionId, size);
    const ls = this.listeners.get(sessionId);
    if (ls) for (const l of ls) l(bytes);
  }

  // Subscribe live; immediately replays the buffered scrollback into `cb`.
  subscribe(sessionId: string, cb: (bytes: Uint8Array) => void): () => void {
    for (const chunk of this.buffers.get(sessionId) ?? []) cb(chunk);
    const ls = this.listeners.get(sessionId) ?? new Set();
    ls.add(cb);
    this.listeners.set(sessionId, ls);
    return () => ls.delete(cb);
  }

  hasData(sessionId: string): boolean {
    return (this.sizes.get(sessionId) ?? 0) > 0;
  }

  clear(sessionId: string): void {
    this.buffers.delete(sessionId);
    this.sizes.delete(sessionId);
    this.listeners.delete(sessionId);
  }
}

export const ptyHub = new PtyHub();
