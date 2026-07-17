// TerminalRenderer (docs/terminal.md): the only terminal surface the rest of the
// UI depends on, so the emulator engine is swappable. ghostty-web is the default
// (Ghostty's libghostty-vt in WASM, xterm-API-compatible); @xterm/xterm is the
// drop-in fallback selected by a config flag. Because ghostty-web mirrors the
// xterm API, both concrete engines share this adapter with a one-line import diff.

export interface TerminalRenderer {
  write(bytes: Uint8Array): void;
  onData(cb: (data: string) => void): void;
  resize(cols: number, rows: number): void;
  readonly cols: number;
  readonly rows: number;
  focus(): void;
  fit(): void;
  dispose(): void;
}

export type EngineName = 'ghostty' | 'xterm';

// Engine selection: ?term=xterm / localStorage / build flag forces the fallback.
export function selectedEngine(): EngineName {
  const q = new URLSearchParams(window.location.search).get('term');
  if (q === 'xterm' || q === 'ghostty') return q;
  const saved = localStorage.getItem('tandem.termEngine');
  if (saved === 'xterm' || saved === 'ghostty') return saved;
  const flag = import.meta.env.VITE_TERM_ENGINE as string | undefined;
  if (flag === 'xterm' || flag === 'ghostty') return flag;
  return 'ghostty';
}

const THEME = {
  background: '#0c1216',
  foreground: '#e4eaee',
  cursor: '#3fd0c4',
};

// Common shape between ghostty-web's Terminal and @xterm/xterm's Terminal.
interface XtermLike {
  cols: number;
  rows: number;
  open(el: HTMLElement): void;
  write(data: Uint8Array | string): void;
  onData(cb: (d: string) => void): void;
  resize(cols: number, rows: number): void;
  focus(): void;
  dispose(): void;
  loadAddon(addon: unknown): void;
}
interface FitLike {
  fit(): void;
}

class Adapter implements TerminalRenderer {
  constructor(private term: XtermLike, private fitAddon: FitLike) {}
  write(bytes: Uint8Array): void {
    this.term.write(bytes);
  }
  onData(cb: (data: string) => void): void {
    this.term.onData(cb);
  }
  resize(cols: number, rows: number): void {
    this.term.resize(cols, rows);
  }
  get cols(): number {
    return this.term.cols;
  }
  get rows(): number {
    return this.term.rows;
  }
  focus(): void {
    this.term.focus();
  }
  fit(): void {
    try {
      this.fitAddon.fit();
    } catch {
      /* container not laid out yet */
    }
  }
  dispose(): void {
    try {
      this.term.dispose();
    } catch {
      /* already gone */
    }
  }
}

// Create a renderer mounted into `el`. Tries ghostty-web first; on any failure
// (WASM load, init) falls back to @xterm/xterm and reports which engine won.
export async function createRenderer(el: HTMLElement, engine: EngineName): Promise<{ renderer: TerminalRenderer; engine: EngineName }> {
  if (engine === 'ghostty') {
    try {
      const g = await import('ghostty-web');
      await g.init();
      const term = new g.Terminal({ fontSize: 12, cursorBlink: true, theme: THEME } as never) as unknown as XtermLike;
      const fit = new g.FitAddon() as unknown as FitLike;
      term.loadAddon(fit);
      term.open(el);
      // ghostty-web 0.4 can recycle a freed WASM terminal handle with its old
      // screen cells intact. When React swaps the agent CLI for the independent
      // worktree shell, that makes correctly separated raw_pty bytes appear in
      // the shell renderer. Clear both the viewport and scrollback before the
      // selected hub replays its own bytes into this new renderer.
      term.write('\x1b[3J\x1b[2J\x1b[H');
      (fit as FitLike).fit();
      return { renderer: new Adapter(term, fit), engine: 'ghostty' };
    } catch (e) {
      console.warn('[terminal] ghostty-web failed, falling back to xterm.js:', e);
    }
  }
  const { Terminal } = await import('@xterm/xterm');
  const { FitAddon } = await import('@xterm/addon-fit');
  await import('@xterm/xterm/css/xterm.css');
  const term = new Terminal({ fontSize: 12, cursorBlink: true, theme: THEME, convertEol: false }) as unknown as XtermLike;
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  return { renderer: new Adapter(term, fit as unknown as FitLike), engine: 'xterm' };
}
