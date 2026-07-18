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
  const rendererRef = useRef<HTMLDivElement>(null);
  const kbdRef = useRef<HTMLTextAreaElement>(null);
  // Keep the latest callbacks without re-running the mount effect.
  const cbs = useRef({ subscribe, onData, onResize, onEngine, onFirstData });
  cbs.current = { subscribe, onData, onResize, onEngine, onFirstData };

  // Canvas/WASM terminal renderers cannot summon a phone's soft keyboard. Keep
  // a real, invisible textarea in the terminal and translate its mobile IME
  // events into the same byte stream as renderer.onData. This intentionally
  // lives in the shared component so it works for both the agent CLI and the
  // independent Terminal shell.
  const SENTINEL = ' ';
  const pendingBackspaceInputs = useRef(0);
  const resetKeyboard = () => {
    const el = kbdRef.current;
    if (!el) return;
    el.value = SENTINEL;
    el.setSelectionRange(SENTINEL.length, SENTINEL.length);
  };
  const focusKeyboard = () => {
    pendingBackspaceInputs.current = 0;
    resetKeyboard();
    kbdRef.current?.focus();
  };
  const send = (data: string) => cbs.current.onData(data);
  const onKeyboardInput = (e: React.FormEvent<HTMLTextAreaElement>) => {
    const input = e.nativeEvent as InputEvent;
    switch (input.inputType) {
      case 'insertText':
      case 'insertReplacementText':
      case 'insertCompositionText':
        pendingBackspaceInputs.current = 0;
        if (input.data) send(input.data);
        break;
      case 'insertLineBreak':
      case 'insertParagraph':
        pendingBackspaceInputs.current = 0;
        send('\r');
        break;
      case 'deleteContentBackward':
      case 'deleteWordBackward':
        if (pendingBackspaceInputs.current > 0) pendingBackspaceInputs.current -= 1;
        else send('\x7f');
        break;
      case 'deleteContentForward':
        pendingBackspaceInputs.current = 0;
        send('\x1b[3~');
        break;
    }
    resetKeyboard();
  };
  const onKeyboardKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const keys: Record<string, string> = {
      Enter: '\r', Tab: '\t', Escape: '\x1b', Delete: '\x1b[3~',
      ArrowUp: '\x1b[A', ArrowDown: '\x1b[B', ArrowRight: '\x1b[C', ArrowLeft: '\x1b[D',
      Home: '\x1b[H', End: '\x1b[F', PageUp: '\x1b[5~', PageDown: '\x1b[6~',
    };
    if (e.key === 'Backspace') {
      // Let the textarea delete its sentinel. Its subsequent input event is
      // paired and suppressed, while keyboards that omit keydown still work.
      pendingBackspaceInputs.current += 1;
      send('\x7f');
      return;
    }
    if (e.key.length === 1 || e.key === 'Unidentified' || e.key === 'Process') return;
    const data = keys[e.key];
    if (!data) return;
    e.preventDefault();
    send(data);
  };

  useEffect(() => {
    let renderer: TerminalRenderer | null = null;
    let unsub: (() => void) | null = null;
    let disposed = false;
    let ro: ResizeObserver | null = null;

    const el = rendererRef.current;
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

  return (
    <div className="term-mount">
      {/* ghostty-web makes its mount contenteditable and cancels beforeinput.
          Keep the mobile textarea outside that subtree or phone IME edits are
          canceled before onInput can forward them to the PTY. */}
      <div className="term-renderer" ref={rendererRef} />
      <button type="button" className="term-kbd-btn" onClick={focusKeyboard} title="Show keyboard to type in the terminal">
        ⌨ Keyboard
      </button>
      <textarea
        ref={kbdRef}
        className="term-kbd-catcher"
        autoCapitalize="off"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        aria-hidden="true"
        tabIndex={-1}
        defaultValue={SENTINEL}
        onInput={onKeyboardInput}
        onKeyDown={onKeyboardKeyDown}
      />
    </div>
  );
}
