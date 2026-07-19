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
type VirtualKeyboardLike = EventTarget & {
  boundingRect: DOMRectReadOnly;
  overlaysContent: boolean;
};

function virtualKeyboard(): VirtualKeyboardLike | undefined {
  return (navigator as Navigator & { virtualKeyboard?: VirtualKeyboardLike }).virtualKeyboard;
}

function rectSnapshot(rect: DOMRect | DOMRectReadOnly | undefined) {
  if (!rect) return null;
  return {
    top: Math.round(rect.top), right: Math.round(rect.right), bottom: Math.round(rect.bottom), left: Math.round(rect.left),
    width: Math.round(rect.width), height: Math.round(rect.height),
  };
}

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
  const mountRef = useRef<HTMLDivElement>(null);
  const rendererRef = useRef<HTMLDivElement>(null);
  const kbdRef = useRef<HTMLTextAreaElement>(null);
  const [keyboardOpen, setKeyboardOpen] = useState(false);
  const [keyboardInset, setKeyboardInset] = useState(0);
  const [modifiers, setModifiers] = useState<Set<MobileModifier>>(() => new Set());
  const [debugCopyState, setDebugCopyState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const debugEnabled = useRef(new URLSearchParams(window.location.search).get('termDebug') === '1').current;
  const debugLog = useRef<Record<string, unknown>[]>([]);
  const debugState = useRef({ keyboardOpen, keyboardInset });
  debugState.current = { keyboardOpen, keyboardInset };
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
  const recordGeometry = (reason: string, extra?: Record<string, unknown>) => {
    if (!debugEnabled) return;
    const viewport = window.visualViewport;
    const keyboard = virtualKeyboard();
    const mount = mountRef.current;
    const renderer = rendererRef.current;
    const catcher = kbdRef.current;
    const toolbar = mount?.querySelector<HTMLElement>('.term-extra-keys');
    const probe = document.createElement('div');
    probe.style.cssText = 'position:fixed;visibility:hidden;height:env(keyboard-inset-height, 0px)';
    document.body.appendChild(probe);
    const cssKeyboardInset = getComputedStyle(probe).height;
    probe.remove();
    const active = document.activeElement as HTMLElement | null;
    const toolbarStyle = toolbar ? getComputedStyle(toolbar) : null;
    const sample = {
      at: new Date().toISOString(),
      elapsedMs: Math.round(performance.now()),
      reason,
      state: debugState.current,
      window: {
        innerWidth: window.innerWidth,
        innerHeight: window.innerHeight,
        scrollX: window.scrollX,
        scrollY: window.scrollY,
        documentClientHeight: document.documentElement.clientHeight,
        screenWidth: window.screen.width,
        screenHeight: window.screen.height,
        orientation: window.screen.orientation?.type ?? null,
      },
      visualViewport: viewport ? {
        width: Math.round(viewport.width), height: Math.round(viewport.height),
        offsetTop: Math.round(viewport.offsetTop), offsetLeft: Math.round(viewport.offsetLeft),
        pageTop: Math.round(viewport.pageTop), pageLeft: Math.round(viewport.pageLeft), scale: viewport.scale,
      } : null,
      virtualKeyboard: keyboard ? {
        overlaysContent: keyboard.overlaysContent,
        boundingRect: rectSnapshot(keyboard.boundingRect),
      } : null,
      cssKeyboardInset,
      elements: {
        mount: rectSnapshot(mount?.getBoundingClientRect()),
        renderer: rectSnapshot(renderer?.getBoundingClientRect()),
        catcher: rectSnapshot(catcher?.getBoundingClientRect()),
        toolbar: rectSnapshot(toolbar?.getBoundingClientRect()),
        toolbarBottom: toolbarStyle?.bottom ?? null,
        toolbarDisplay: toolbarStyle?.display ?? null,
        active: active ? `${active.tagName.toLowerCase()}.${active.className}` : null,
      },
      ...extra,
    };
    debugLog.current.push(sample);
    if (debugLog.current.length > 120) debugLog.current.shift();
  };
  const resetKeyboard = () => {
    const el = kbdRef.current;
    if (!el) return;
    el.value = SENTINEL;
    el.setSelectionRange(SENTINEL.length, SENTINEL.length);
  };
  const focusKeyboard = () => {
    recordGeometry('focusKeyboard.before');
    pendingBackspaceInputs.current = 0;
    resetKeyboard();
    // Opt into explicit geometry before focus. This is required for overlay
    // and split keyboards that do not resize visualViewport.
    const keyboard = virtualKeyboard();
    if (keyboard) keyboard.overlaysContent = true;
    kbdRef.current?.focus({ preventScroll: true });
    requestAnimationFrame(() => recordGeometry('focusKeyboard.rendered'));
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

  // Prefer Chrome's Virtual Keyboard geometry for overlay/split keyboards.
  // Fall back to visualViewport for browsers that resize only the visual
  // viewport. Lift both the bar and terminal fit boundary above whichever
  // reports the higher obstruction.
  useEffect(() => {
    if (!debugEnabled) return;
    recordGeometry('debug.mount');
    requestAnimationFrame(() => recordGeometry('debug.mount.rendered'));
  }, []);

  useEffect(() => {
    if (!debugEnabled) return;
    requestAnimationFrame(() => recordGeometry('state.keyboardInset'));
  }, [keyboardInset]);

  useEffect(() => {
    if (!keyboardOpen) {
      recordGeometry('keyboard.closed');
      setKeyboardInset(0);
      return;
    }
    const viewport = window.visualViewport;
    const keyboard = virtualKeyboard();
    const updateInset = (reason: string) => {
      const terminalBottom = mountRef.current?.getBoundingClientRect().bottom ?? window.innerHeight;
      const viewportBottom = viewport ? viewport.offsetTop + viewport.height : window.innerHeight;
      const keyboardRect = keyboard?.boundingRect;
      const keyboardTop = keyboardRect && keyboardRect.height > 0 ? keyboardRect.top : window.innerHeight;
      const visibleBottom = Math.min(viewportBottom, keyboardTop);
      const covered = terminalBottom - visibleBottom;
      const nextInset = Math.max(0, Math.round(covered));
      recordGeometry(reason, { calculation: { terminalBottom, viewportBottom, keyboardTop, visibleBottom, covered, nextInset } });
      setKeyboardInset(nextInset);
      requestAnimationFrame(() => recordGeometry(`${reason}.rendered`, { nextInset }));
    };
    const onViewportResize = () => updateInset('visualViewport.resize');
    const onViewportScroll = () => updateInset('visualViewport.scroll');
    const onKeyboardGeometry = () => updateInset('virtualKeyboard.geometrychange');
    const onWindowResize = () => updateInset('window.resize');
    updateInset('keyboard.open');
    viewport?.addEventListener('resize', onViewportResize);
    viewport?.addEventListener('scroll', onViewportScroll);
    keyboard?.addEventListener('geometrychange', onKeyboardGeometry);
    window.addEventListener('resize', onWindowResize);
    return () => {
      viewport?.removeEventListener('resize', onViewportResize);
      viewport?.removeEventListener('scroll', onViewportScroll);
      keyboard?.removeEventListener('geometrychange', onKeyboardGeometry);
      window.removeEventListener('resize', onWindowResize);
      if (keyboard) keyboard.overlaysContent = false;
    };
  }, [keyboardOpen]);

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
      recordGeometry('renderer.focusin', { target: (event.target as HTMLElement | null)?.tagName ?? null });
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

  const copyDiagnostics = async () => {
    recordGeometry('diagnostics.copy');
    const safeUrl = new URL(window.location.href);
    safeUrl.searchParams.delete('t');
    safeUrl.hash = '';
    const payload = JSON.stringify({
      version: 'terminal-mobile-debug-v1',
      capturedAt: new Date().toISOString(),
      url: safeUrl.toString(),
      userAgent: navigator.userAgent,
      samples: debugLog.current,
    }, null, 2);
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
      await navigator.clipboard.writeText(payload);
      setDebugCopyState('copied');
    } catch {
      setDebugCopyState('failed');
      window.prompt('Copy the terminal diagnostics below:', payload);
    }
  };

  return (
    <div
      ref={mountRef}
      className={`term-mount${keyboardOpen ? ' mobile-kbd-open' : ''}`}
      style={{ '--term-keyboard-inset': `${keyboardInset}px` } as React.CSSProperties}
    >
      {/* ghostty-web makes its mount contenteditable and cancels beforeinput.
          Keep the mobile textarea outside that subtree or phone IME edits are
          canceled before onInput can forward them to the PTY. */}
      <div className="term-renderer" ref={rendererRef} />
      <button type="button" className="term-kbd-btn" onClick={focusKeyboard} title="Show keyboard to type in the terminal">
        ⌨ Keyboard
      </button>
      {debugEnabled && (
        <button type="button" className="term-debug-btn" onClick={() => void copyDiagnostics()}>
          {debugCopyState === 'copied' ? '✓ Copied diagnostics' : debugCopyState === 'failed' ? 'Copy failed — tap again' : 'Copy diagnostics'}
        </button>
      )}
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
        onFocus={() => { recordGeometry('catcher.focus'); setKeyboardOpen(true); }}
        onBlur={() => { recordGeometry('catcher.blur'); setKeyboardOpen(false); clearModifiers(); }}
        onInput={onKeyboardInput}
        onKeyDown={onKeyboardKeyDown}
      />
    </div>
  );
}
