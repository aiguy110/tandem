import { useEffect, useRef, useState } from 'react';
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

type MobileModifier = 'ctrl' | 'alt';

const MOBILE_KEYS = [
  { label: 'Esc', data: '\x1b' },
  { label: 'Tab', data: '\t' },
  { label: '/', data: '/', printable: true },
  { label: '~', data: '~', printable: true },
  { label: '-', data: '-', printable: true },
  { label: '|', data: '|', printable: true },
  { label: '←', data: '\x1b[D' },
  { label: '↑', data: '\x1b[A' },
  { label: '↓', data: '\x1b[B' },
  { label: '→', data: '\x1b[C' },
] as const;

export function PtyTerminal({ subscribe, onData, onResize, onEngine, onFirstData }: Props) {
  const rendererRef = useRef<HTMLDivElement>(null);
  const kbdRef = useRef<HTMLTextAreaElement>(null);
  const [keyboardOpen, setKeyboardOpen] = useState(false);
  const [modifiers, setModifiers] = useState<Set<MobileModifier>>(() => new Set());
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
    kbdRef.current?.focus({ preventScroll: true });
  };
  const send = (data: string) => cbs.current.onData(data);
  const clearModifiers = () => setModifiers(new Set());
  const sendWithModifiers = (data: string, printable = false) => {
    let output = data;
    if (printable && modifiers.has('ctrl') && output.length > 0) {
      const code = output.charCodeAt(0);
      let control: string | null = null;
      if (code === 32 || (code >= 64 && code <= 95)) control = String.fromCharCode(code & 0x1f);
      else if (code >= 97 && code <= 122) control = String.fromCharCode(code - 96);
      else if (output[0] === '?') control = '\x7f';
      if (control !== null) output = control + output.slice(1);
    }
    if (modifiers.has('alt')) output = '\x1b' + output;
    if (modifiers.size > 0) clearModifiers();
    send(output);
  };
  const toggleModifier = (modifier: MobileModifier) => {
    setModifiers((current) => {
      const next = new Set(current);
      if (next.has(modifier)) next.delete(modifier);
      else next.add(modifier);
      return next;
    });
    kbdRef.current?.focus();
  };
  const onKeyboardInput = (e: React.FormEvent<HTMLTextAreaElement>) => {
    const input = e.nativeEvent as InputEvent;
    switch (input.inputType) {
      case 'insertText':
      case 'insertReplacementText':
      case 'insertCompositionText':
        pendingBackspaceInputs.current = 0;
        if (input.data) sendWithModifiers(input.data, true);
        break;
      case 'insertLineBreak':
      case 'insertParagraph':
        pendingBackspaceInputs.current = 0;
        sendWithModifiers('\r');
        break;
      case 'deleteContentBackward':
      case 'deleteWordBackward':
        if (pendingBackspaceInputs.current > 0) pendingBackspaceInputs.current -= 1;
        else sendWithModifiers('\x7f');
        break;
      case 'deleteContentForward':
        pendingBackspaceInputs.current = 0;
        sendWithModifiers('\x1b[3~');
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
      sendWithModifiers('\x7f');
      return;
    }
    if (e.key.length === 1 || e.key === 'Unidentified' || e.key === 'Process') return;
    const data = keys[e.key];
    if (!data) return;
    e.preventDefault();
    sendWithModifiers(data);
  };

  useEffect(() => {
    let renderer: TerminalRenderer | null = null;
    let unsub: (() => void) | null = null;
    let disposed = false;
    let ro: ResizeObserver | null = null;

    const el = rendererRef.current;
    if (!el) return;

    // Both terminal engines install their own hidden textarea. On touch
    // devices, tapping the canvas focuses that field directly, bypassing our
    // mobile IME handling and accessory-bar state. Redirect that focus while
    // it is still part of the user's tap gesture so the soft keyboard opens
    // through the same path as the explicit Keyboard button.
    const onRendererFocusIn = (event: FocusEvent) => {
      if (
        event.target instanceof HTMLTextAreaElement
        && window.matchMedia('(hover: none) and (pointer: coarse)').matches
      ) focusKeyboard();
    };
    el.addEventListener('focusin', onRendererFocusIn);

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
      el.removeEventListener('focusin', onRendererFocusIn);
      ro?.disconnect();
      unsub?.();
      renderer?.dispose();
    };
  }, []);

  return (
    <div className={`term-mount${keyboardOpen ? ' mobile-kbd-open' : ''}`}>
      {/* ghostty-web makes its mount contenteditable and cancels beforeinput.
          Keep the mobile textarea outside that subtree or phone IME edits are
          canceled before onInput can forward them to the PTY. */}
      <div className="term-renderer" ref={rendererRef} />
      <button type="button" className="term-kbd-btn" onClick={focusKeyboard} title="Show keyboard to type in the terminal">
        ⌨ Keyboard
      </button>
      {keyboardOpen && (
        <div className="term-extra-keys" role="toolbar" aria-label="Terminal special keys">
          <button
            type="button"
            className={modifiers.has('ctrl') ? 'active' : ''}
            aria-pressed={modifiers.has('ctrl')}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => toggleModifier('ctrl')}
          >Ctrl</button>
          <button
            type="button"
            className={modifiers.has('alt') ? 'active' : ''}
            aria-pressed={modifiers.has('alt')}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => toggleModifier('alt')}
          >Alt</button>
          {MOBILE_KEYS.map((key) => (
            <button
              type="button"
              key={key.label}
              aria-label={key.label === '←' ? 'Left arrow' : key.label === '→' ? 'Right arrow' : key.label === '↑' ? 'Up arrow' : key.label === '↓' ? 'Down arrow' : key.label}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => sendWithModifiers(key.data, 'printable' in key && key.printable)}
            >{key.label}</button>
          ))}
          <button
            type="button"
            aria-label="Hide keyboard"
            title="Hide keyboard"
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => kbdRef.current?.blur()}
          >⌄</button>
        </div>
      )}
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
        onFocus={() => setKeyboardOpen(true)}
        onBlur={() => { setKeyboardOpen(false); clearModifiers(); }}
        onInput={onKeyboardInput}
        onKeyDown={onKeyboardKeyDown}
      />
    </div>
  );
}
