// A non-reactive buffer + fan-out for the user escape-hatch shell (Terminal
// tab), mirroring ptyHub but for shell_pty bytes. Kept OUT of the React store so
// a stream of terminal bytes never triggers component re-renders. The Terminal
// pane subscribes on focus and rehydrates from the buffer (reconnect replays
// buffered shell_pty bytes); background shells just buffer.

const MAX_BYTES = 256 * 1024; // per-agent scrollback cap on the client side

function b64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

class ShellHub {
  private buffers = new Map<string, Uint8Array[]>();
  private sizes = new Map<string, number>();
  private listeners = new Map<string, Set<(bytes: Uint8Array) => void>>();

  push(agentId: string, dataB64: string): void {
    const bytes = b64ToBytes(dataB64);
    const buf = this.buffers.get(agentId) ?? [];
    buf.push(bytes);
    let size = (this.sizes.get(agentId) ?? 0) + bytes.length;
    while (size > MAX_BYTES && buf.length > 1) size -= buf.shift()!.length;
    this.buffers.set(agentId, buf);
    this.sizes.set(agentId, size);
    const ls = this.listeners.get(agentId);
    if (ls) for (const l of ls) l(bytes);
  }

  // Subscribe live; immediately replays the buffered scrollback into `cb`.
  subscribe(agentId: string, cb: (bytes: Uint8Array) => void): () => void {
    for (const chunk of this.buffers.get(agentId) ?? []) cb(chunk);
    const ls = this.listeners.get(agentId) ?? new Set();
    ls.add(cb);
    this.listeners.set(agentId, ls);
    return () => ls.delete(cb);
  }

  hasData(agentId: string): boolean {
    return (this.sizes.get(agentId) ?? 0) > 0;
  }

  // Drop buffered scrollback so a fresh shell (after exit + restart) starts clean.
  reset(agentId: string): void {
    this.buffers.delete(agentId);
    this.sizes.delete(agentId);
  }

  clear(agentId: string): void {
    this.buffers.delete(agentId);
    this.sizes.delete(agentId);
    this.listeners.delete(agentId);
  }
}

export const shellHub = new ShellHub();
