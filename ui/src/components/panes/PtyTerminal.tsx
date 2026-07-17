import { useEffect, useRef } from 'react';
import { createRenderer, selectedEngine, type EngineName, type TerminalRenderer } from '../../terminal/TerminalRenderer';

// A reusable terminal view (docs/terminal.md): mounts the TerminalRenderer
// (ghostty-web default, @xterm/xterm fallback), rehydrates from a byte hub, and
// forwards keystrokes/resizes. It is stream-agnostic — the agent CLI (raw_pty)
// and the user shell (shell_pty) both drive it through these props.
interface Props {
  // Live byte source; must immediately replay buffered scrollback into `cb`.
  subscribe: (cb: (bytes: Uint8Array) => void) => () => void;
  // Keystrokes typed into the terminal, as a UTF-8 string.
  onData: (data: string) => void;
  // Fit-to-container size, reported on initial layout and every later resize.
  onResize: (cols: number, rows: number) => void;
  onEngine?: (engine: EngineName) => void;
  onFirstData?: () => void;
}

export function PtyTerminal({ subscribe, onData, onResize, onEngine, onFirstData }: Props) {
  const mountRef = useRef<HTMLDivElement>(null);
  // Keep the latest callbacks without re-running the mount effect.
  const cbs = useRef({ subscribe, onData, onResize, onEngine, onFirstData });
  cbs.current = { subscribe, onData, onResize, onEngine, onFirstData };

  useEffect(() => {
    let renderer: TerminalRenderer | null = null;
    let unsub: (() => void) | null = null;
    let disposed = false;
    let ro: ResizeObserver | null = null;

    const el = mountRef.current;
    if (!el) return;

    (async () => {
      const created = await createRenderer(el, selectedEngine());
      if (disposed) {
        created.renderer.dispose();
        return;
      }
      renderer = created.renderer;
      cbs.current.onEngine?.(created.engine);

      // Feed buffered scrollback + live bytes into the emulator.
      unsub = cbs.current.subscribe((bytes) => {
        renderer?.write(bytes);
        if (bytes.length) cbs.current.onFirstData?.();
      });

      // Keystrokes → owner.
      renderer.onData((d) => cbs.current.onData(d));

      // Fit to container and report the size; observe future resizes.
      const doFit = () => {
        renderer?.fit();
        if (renderer) cbs.current.onResize(renderer.cols, renderer.rows);
      };
      doFit();
      ro = new ResizeObserver(doFit);
      ro.observe(el);
      renderer.focus();
    })();

    return () => {
      disposed = true;
      ro?.disconnect();
      unsub?.();
      renderer?.dispose();
    };
  }, []);

  return <div className="term-mount" ref={mountRef} />;
}
