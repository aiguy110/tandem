// Non-reactive fan-out for browser screencast frames, kept OUT of the React store
// so a stream of JPEG frames never triggers component re-renders (mirrors ptyHub).
// The BrowserPane subscribes on mount and paints each frame into a <canvas>.

export interface Frame {
  dataB64: string;
  meta: { deviceWidth: number; deviceHeight: number; offsetTop: number };
}

class BrowserHub {
  private last = new Map<string, Frame>();
  private listeners = new Map<string, Set<(f: Frame) => void>>();

  push(sessionId: string, f: Frame): void {
    this.last.set(sessionId, f);
    const ls = this.listeners.get(sessionId);
    if (ls) for (const l of ls) l(f);
  }

  // Subscribe live; immediately replays the last frame so a freshly mounted pane
  // shows something without waiting for the next screencast tick.
  subscribe(sessionId: string, cb: (f: Frame) => void): () => void {
    const ls = this.listeners.get(sessionId) ?? new Set();
    ls.add(cb);
    this.listeners.set(sessionId, ls);
    const last = this.last.get(sessionId);
    if (last) cb(last);
    return () => ls.delete(cb);
  }

  clear(sessionId: string): void {
    this.last.delete(sessionId);
    this.listeners.delete(sessionId);
  }
}

export const browserHub = new BrowserHub();
