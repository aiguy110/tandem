// TerminalRenderer (docs/terminal.md): the only terminal surface the rest of the
// UI depends on, so the emulator engine is swappable. ghostty-web is the default
// (Ghostty's libghostty-vt in WASM, xterm-API-compatible); @xterm/xterm is the
// drop-in fallback selected by a config flag. Because ghostty-web mirrors the
// xterm API, both concrete engines share this adapter with a one-line import diff.

import { ensureTermFont, selectedFontFamily } from './font';

export interface TerminalRenderer {
  write(bytes: Uint8Array): void;
  setFontSize(px: number): void;
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
  options?: { fontSize?: number };
  rows: number;
  open(el: HTMLElement): void;
  write(data: Uint8Array | string): void;
  onData(cb: (d: string) => void): void;
  resize(cols: number, rows: number): void;
  focus(): void;
  dispose(): void;
  loadAddon(addon: unknown): void;
}
interface GhosttyLike extends XtermLike {
  wasmTerm?: unknown;
  element?: HTMLElement;
  renderer?: {
    render(buffer: unknown, forceAll?: boolean, viewportY?: number, scrollbackProvider?: unknown): void;
  };
  viewportY: number;
}
interface FitLike {
  fit(): void;
}

class Adapter implements TerminalRenderer {
  constructor(
    private term: XtermLike,
    private fitAddon: FitLike,
    private forceRedraw?: () => void,
  ) {}
  setFontSize(px: number): void {
    // Both engines expose xterm.js' mutable options object; assigning fontSize
    // remeasures the cell grid. The caller refits afterwards so the new grid is
    // reported to the pty.
    if (this.term.options) this.term.options.fontSize = px;
  }
  write(bytes: Uint8Array): void {
    this.term.write(bytes);
    // ghostty-web 0.4's canvas can miss the final cursor-only update in the
    // usual shell erase echo (BS, space, BS), especially after scrollback has
    // been replayed. Its VT buffer is correct, but the old character and cursor
    // remain painted until some later output dirties the row. Redraw only for
    // chunks containing BS; xterm.js does not need this compatibility path.
    if (bytes.includes(0x08)) this.forceRedraw?.();
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
// ghostty-web 0.4 sizes a cell as Math.ceil(measureText('M').width) in CSS
// pixels and its height from the cap height of 'M' plus 2px. Both are wrong for
// the same reason: they ignore the device pixel ratio and the font's own line
// box. At 12px Cascadia the advance is 7.03px, so ceiling to 8 stretches every
// column by ~14% (visibly wider than native Ghostty), while rows come out at
// 10px against the font's 14px line box. Round the advance to the nearest
// device pixel instead -- cells still land on whole device pixels, so nothing
// seams -- and take the height and baseline from the font's bounding box.
let metricsPatched = false;
function patchCellMetrics(CanvasRenderer: { prototype: Record<string, unknown> }): void {
  if (metricsPatched) return;
  metricsPatched = true;
  CanvasRenderer.prototype.measureFont = function measureFont(this: {
    fontSize: number;
    fontFamily: string;
    devicePixelRatio: number;
  }) {
    const ctx = document.createElement('canvas').getContext('2d')!;
    ctx.font = `${this.fontSize}px ${this.fontFamily}`;
    const m = ctx.measureText('M');
    const dpr = this.devicePixelRatio || 1;
    const snap = (v: number) => Math.max(1, Math.round(v * dpr)) / dpr;
    const ascent = m.fontBoundingBoxAscent || m.actualBoundingBoxAscent || this.fontSize * 0.8;
    const descent = m.fontBoundingBoxDescent || m.actualBoundingBoxDescent || this.fontSize * 0.2;
    return { width: snap(m.width), height: snap(ascent + descent), baseline: snap(ascent) };
  };
}

export async function createRenderer(el: HTMLElement, engine: EngineName, fontSize: number): Promise<{ renderer: TerminalRenderer; engine: EngineName }> {
  // Both engines measure their cell grid from the font at construction time, so
  // the face has to be resolved first or the grid is sized to the fallback.
  const fontFamily = selectedFontFamily();
  await ensureTermFont(fontFamily, fontSize);
  if (engine === 'ghostty') {
    try {
      const g = await import('ghostty-web');
      await g.init();
      patchCellMetrics(g.CanvasRenderer as unknown as { prototype: Record<string, unknown> });
      const term = new g.Terminal({ fontSize, fontFamily, cursorBlink: true, theme: THEME } as never) as unknown as GhosttyLike;
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
      const forceRedraw = () => {
        if (term.renderer && term.wasmTerm) {
          term.renderer.render(term.wasmTerm, true, term.viewportY, term);
        }
      };
      return { renderer: new Adapter(term, fit, forceRedraw), engine: 'ghostty' };
    } catch (e) {
      console.warn('[terminal] ghostty-web failed, falling back to xterm.js:', e);
    }
  }
  const { Terminal } = await import('@xterm/xterm');
  const { FitAddon } = await import('@xterm/addon-fit');
  await import('@xterm/xterm/css/xterm.css');
  const term = new Terminal({ fontSize, fontFamily, cursorBlink: true, theme: THEME, convertEol: false }) as unknown as XtermLike;
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  return { renderer: new Adapter(term, fit as unknown as FitLike), engine: 'xterm' };
}
